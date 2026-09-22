package web

// Public access, and the hardening that has to come with it.
//
// The dashboard, data history and file download pages are readable by anyone;
// everything else -- settings, diagnostics, users, receiver control, power,
// logs -- needs an administrator session. The owner publishes this UI through
// a Cloudflare tunnel, so every assumption here is that the caller is
// anonymous, automated and hostile until a session says otherwise:
//
//   - `pub` attaches a session when one is present but never demands one;
//     handlers decide what an anonymous caller may see. `auth` still refuses.
//   - Rate limits are per caller address, and the caller's address is the one
//     the tunnel reports -- but only when the immediate peer is local, because
//     anywhere else those headers are simply strings the client chose.
//   - Sign-in has its own much tighter limit, because it is the one endpoint
//     where guessing pays.
//   - Unsafe methods must come from this origin, which stops a page on another
//     site from driving an administrator's browser.
//
// None of this replaces the tunnel's own protection; it is what keeps the
// daemon safe if the tunnel is bypassed or misconfigured.

import (
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const visitorCookie = "psgnss_visitor"

// pub wraps a handler anonymous callers may reach. A valid session still
// identifies the administrator, so one handler can serve both.
func (s *Server) pub(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			if name, ok := s.Store.AdminBySession(c.Value); ok {
				r = r.WithContext(withAdmin(r.Context(), name))
			}
		}
		next(w, r)
	}
}

// visitorID identifies an anonymous caller across requests, so a conversion
// they started stays theirs. It is not a credential and grants nothing: it
// only scopes the job list, and every privileged route ignores it.
func (s *Server) visitorID(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(visitorCookie); err == nil && len(c.Value) >= 16 && len(c.Value) <= 64 {
		if _, err := base64.RawURLEncoding.DecodeString(c.Value); err == nil {
			return c.Value
		}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	id := base64.RawURLEncoding.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{Name: visitorCookie, Value: id, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureRequest(r),
		MaxAge: int((24 * time.Hour).Seconds())})
	return id
}

// jobOwner names who a conversion belongs to: an administrator by name, or an
// anonymous visitor by cookie. The two namespaces cannot collide.
func (s *Server) jobOwner(w http.ResponseWriter, r *http.Request) string {
	if name := adminOf(r); name != "" {
		return "admin:" + name
	}
	if id := s.visitorID(w, r); id != "" {
		return "visitor:" + id
	}
	return "visitor:anonymous"
}

// secureRequest reports whether the browser reached us over TLS, so the
// cookies can be marked Secure through the tunnel without breaking a plain
// http:// session on the local network.
func secureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return trustedPeer(clientIP(r)) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// trustedPeer reports whether the immediate connection came from something
// that may speak for someone else: the loopback interface, where cloudflared
// runs, or the local network. A forwarded-for header from anywhere else is
// attacker-controlled and is ignored.
func trustedPeer(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// realIP is the address rate limits and the audit log are keyed on.
func realIP(r *http.Request) string {
	peer := clientIP(r)
	if !trustedPeer(peer) {
		return peer
	}
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); net.ParseIP(v) != nil {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		// The client is the first entry; every later one was added by a proxy
		// on the way here.
		first := strings.TrimSpace(strings.Split(v, ",")[0])
		if net.ParseIP(first) != nil {
			return first
		}
	}
	return peer
}

// ------------------------------------------------------------- rate limits

// limiter is a token bucket per key. Buckets are cheap and are swept when
// they have been full and idle for long enough to be indistinguishable from a
// new one, so a flood of distinct addresses cannot grow the map without bound.
type limiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	keys   map[string]*bucketState
	swept  time.Time
	maxLen int
}

type bucketState struct {
	tokens float64
	at     time.Time
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{rate: rate, burst: burst, keys: map[string]*bucketState{},
		swept: time.Now(), maxLen: 4096}
}

func (l *limiter) allow(key string) bool { return l.allowN(key, 1, time.Now()) }

func (l *limiter) allowN(key string, n float64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	b, ok := l.keys[key]
	if !ok {
		if len(l.keys) >= l.maxLen {
			// The table is full of active callers. Refusing is the safe answer:
			// it costs a legitimate new visitor a retry and costs a flood
			// everything.
			return false
		}
		b = &bucketState{tokens: l.burst, at: now}
		l.keys[key] = b
	}
	b.tokens += now.Sub(b.at).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.at = now
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// refund returns tokens to a bucket: a successful sign-in should not count
// against the attempt limit.
func (l *limiter) refund(key string, n float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.keys[key]; ok {
		if b.tokens += n; b.tokens > l.burst {
			b.tokens = l.burst
		}
	}
}

func (l *limiter) sweep(now time.Time) {
	if now.Sub(l.swept) < time.Minute {
		return
	}
	l.swept = now
	full := l.burst / l.rate
	for k, b := range l.keys {
		if b.tokens+now.Sub(b.at).Seconds()*l.rate >= l.burst && now.Sub(b.at) > time.Duration(full)*time.Second {
			delete(l.keys, k)
		}
	}
}

// Request budgets. The dashboard polls twice a second and a map drag pulls a
// screenful of tiles, so the general budget is generous; the point is to stop
// a flood, not to police a browser. Sign-in gets one attempt a minute with a
// burst of five, which is invisible to someone typing a password and useless
// to someone guessing one.
var (
	generalLimit = newLimiter(12, 240)
	loginLimit   = newLimiter(1.0/60.0, 5)
	// A RINEX conversion costs real CPU for minutes, so an anonymous caller
	// gets four an hour and no more than two may run at once across all of
	// them. Administrators are not limited.
	convertLimit = newLimiter(4.0/3600.0, 4)
)

// A map view is about thirty tiles; a visitor may draw several and pan, but
// not scrape.
var tileLimit = newLimiter(3, 90)

const anonymousConversions = 2

// throttle is the outermost middleware: it caps request rate per caller,
// refuses cross-origin writes and bounds request bodies.
func (s *Server) throttle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := realIP(r)
		if !generalLimit.allow(ip) {
			w.Header().Set("Retry-After", "5")
			writeErr(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !sameOrigin(r) {
				writeErr(w, http.StatusForbidden, "cross-origin request refused")
				return
			}
			// Every write handler parses a small JSON document; none needs more
			// than this, and an unbounded body is free memory for an attacker.
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin rejects a write driven from another site. A browser always sends
// Origin on a cross-origin write and Sec-Fetch-Site on every fetch it makes;
// a request with neither did not come from a page, so it is left to the
// session check.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
