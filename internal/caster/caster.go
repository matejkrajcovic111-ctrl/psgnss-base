// Package caster implements an NTRIP caster supporting protocol versions 1
// (ICY) and 2 (HTTP), with basic auth, per-user connection limits, per-
// connection accounting and PROXY-protocol awareness.
package caster

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/psgnss/psgnss-base/internal/hub"
	"github.com/psgnss/psgnss-base/internal/rtcm"
	"github.com/psgnss/psgnss-base/internal/secrets"
	"github.com/psgnss/psgnss-base/internal/store"
)

// Mount is a servable mountpoint backed by a hub subscription.
type Mount struct {
	Entry  MountEntry
	Filter *hub.Filter
	// PushIn marks a mountpoint fed by an external NTRIP server rather than
	// the local hub.
	PushIn bool
	// Generated lists message types this mountpoint must synthesise because
	// the receiver never emits them (1006, 1008, 1033), with their intervals
	// in seconds.
	Generated map[int]int
	// Terminator is the MSM type that must carry the end-of-epoch marker for
	// this mountpoint. Because a mountpoint serves a subset, the receiver's own
	// terminator may be filtered out; without one the rover waits a full epoch
	// before using the corrections.
	Terminator int

	Terminated atomic.Int64

	mu      sync.RWMutex
	clients map[*client]struct{}
	taps    map[*Tap]struct{}
	// last holds the most recent frame of each type, so a joining rover gets
	// station and antenna info immediately instead of waiting up to 10 s.
	lastMu sync.Mutex
	last   map[int][]byte

	BytesOut atomic.Int64
	Joined   atomic.Int64
	LastData atomic.Int64 // Unix milliseconds
}

type client struct {
	conn  net.Conn
	ch    chan []byte
	once  sync.Once
	sent  atomic.Int64
	rowID int64
	user  string
}

func (c *client) close() { c.once.Do(func() { close(c.ch); c.conn.Close() }) }

// Options configures a Caster.
type Options struct {
	Listen      string
	ProxyListen string
	Trusted     *TrustList
	AllowV1     bool
	AllowV2     bool
	Net         *NetEntry
	Store       *store.Store
	Keyring     *secrets.Keyring
	// Ephemeris synthesises navigation messages from the receiver's broadcast
	// subframes. One message type expands to one message per satellite, which
	// is why it is not just another Station.Generate case.
	Ephemeris EphemerisSource
	// Station supplies the values for generated station-description messages.
	Station    rtcm.Station
	Hub        *hub.Hub
	PushInPass string
	Logger     *slog.Logger
}

// Caster serves mountpoints to NTRIP clients.
type Caster struct {
	o      Options
	log    *slog.Logger
	mu     sync.RWMutex
	mounts map[string]*Mount
	// admissionMu makes the connection-limit check and accounting insert one
	// admission operation. Without it, simultaneous connections can both see
	// the same old count and exceed a user's limit.
	admissionMu sync.Mutex

	Connections atomic.Int64
	Rejected    atomic.Int64
}

func New(o Options) (*Caster, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Store == nil {
		return nil, errors.New("caster: store required")
	}
	if !o.AllowV1 && !o.AllowV2 {
		return nil, errors.New("caster: both NTRIP v1 and v2 are disabled")
	}
	return &Caster{o: o, log: o.Logger, mounts: make(map[string]*Mount)}, nil
}

// AddMount registers a mountpoint fed from the hub.
func (c *Caster) AddMount(e MountEntry, f *hub.Filter) *Mount {
	m := &Mount{Entry: e, Filter: f, clients: make(map[*client]struct{}), last: make(map[int][]byte)}
	c.mu.Lock()
	c.mounts[e.Name] = m
	c.mu.Unlock()
	return m
}

// Mounts returns a snapshot of registered mountpoints.
func (c *Caster) Mounts() []*Mount {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*Mount, 0, len(c.mounts))
	for _, m := range c.mounts {
		out = append(out, m)
	}
	return out
}

// ActiveClients reports the current number of rover connections.
func (m *Mount) ActiveClients() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.clients)
}

// Run starts the plain listener and, if configured, the PROXY-protocol one.
func (c *Caster) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	errc := make(chan error, 2)

	start := func(addr string, proxy bool) {
		if addr == "" {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.serve(ctx, addr, proxy); err != nil && ctx.Err() == nil {
				errc <- err
			}
		}()
	}
	start(c.o.Listen, false)
	start(c.o.ProxyListen, true)

	// Feed mountpoints from the hub.
	for _, m := range c.Mounts() {
		if m.PushIn {
			continue
		}
		wg.Add(1)
		go func(m *Mount) {
			defer wg.Done()
			c.feed(ctx, m)
		}(m)
		// Synthesise the station-description messages the receiver does not
		// emit. Without 1006 a rover has no base coordinate and RTK cannot
		// resolve, so this is not optional.
		for typ, iv := range m.Generated {
			wg.Add(1)
			go func(m *Mount, typ, iv int) {
				defer wg.Done()
				c.generate(ctx, m, typ, iv)
			}(m, typ, iv)
		}
	}

	go func() { wg.Wait(); close(errc) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errc:
		return err
	}
}

func (c *Caster) feed(ctx context.Context, m *Mount) {
	sub := c.o.Hub.Subscribe("caster:"+m.Entry.Name, m.Filter, 512)
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-sub.C():
			if !ok {
				return
			}
			// Mark the last MSM message of each epoch for this mountpoint, so
			// the rover can close the epoch immediately.
			if m.Terminator != 0 && rtcm.MSMType(b) == m.Terminator {
				if out, changed := rtcm.ClearMultipleMessageBit(b); changed {
					b = out
					m.Terminated.Add(1)
				}
			}
			m.broadcast(b)
		}
	}
}

// EphemerisSource supplies synthesised navigation messages by type.
type EphemerisSource interface {
	Messages(msgType int) [][]byte
	Supports(msgType int) bool
}

// generate emits one synthesised message type on its interval.
func (c *Caster) generate(ctx context.Context, m *Mount, msgType, interval int) {
	if interval <= 0 {
		interval = 1
	}
	// Emit once immediately so a mountpoint is usable the moment it comes up.
	ephemeris := c.o.Ephemeris != nil && c.o.Ephemeris.Supports(msgType)
	emit := func() {
		if ephemeris {
			// Nothing to send until the receiver has collected a complete data
			// set, which takes about half a minute from a cold start. Silence
			// here is normal and must not be logged as a failure.
			for _, b := range c.o.Ephemeris.Messages(msgType) {
				m.broadcast(b)
			}
			return
		}
		b, err := c.o.Station.Generate(msgType)
		if err != nil {
			c.log.Error("cannot generate message", "type", msgType,
				"mount", m.Entry.Name, "err", err)
			return
		}
		m.broadcast(b)
	}
	emit()
	t := time.NewTicker(time.Duration(interval) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			emit()
		}
	}
}

func (m *Mount) broadcast(b []byte) {
	// Remember station-description messages for immediate replay to joiners.
	if len(b) > 5 && b[0] == 0xD3 {
		t := int(b[3])<<4 | int(b[4])>>4
		switch t {
		case 1005, 1006, 1007, 1008, 1013, 1033:
			cp := make([]byte, len(b))
			copy(cp, b)
			m.lastMu.Lock()
			m.last[t] = cp
			m.lastMu.Unlock()
		}
	}
	m.mu.RLock()
	for cl := range m.clients {
		select {
		case cl.ch <- b:
		default:
			go cl.close() // too slow; drop it rather than stall the mountpoint
		}
	}
	for t := range m.taps {
		select {
		case t.ch <- b:
		default:
			t.Dropped.Add(1) // a slow push must not delay the rovers served here
		}
	}
	m.mu.RUnlock()
	m.BytesOut.Add(int64(len(b)))
	m.LastData.Store(time.Now().UnixMilli())
}

func (c *Caster) serve(ctx context.Context, addr string, proxy bool) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	kind := "plain"
	if proxy {
		kind = "proxy-protocol"
	}
	c.log.Info("caster listening", "addr", addr, "mode", kind)
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.log.Warn("caster accept failed", "addr", addr, "err", err)
			continue
		}
		go c.handle(ctx, conn, proxy)
	}
}

const readHeaderTimeout = 15 * time.Second

func (c *Caster) handle(ctx context.Context, conn net.Conn, proxy bool) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	conn.SetReadDeadline(time.Now().Add(readHeaderTimeout))
	br := bufio.NewReaderSize(conn, 4096)

	realIP, realPort := splitHostPort(conn.RemoteAddr().String())
	viaProxy, proxyIP := false, ""
	if proxy {
		pi, err := ReadProxyHeader(br, conn.RemoteAddr(), c.o.Trusted)
		if err != nil {
			c.log.Warn("bad PROXY header", "peer", conn.RemoteAddr().String(), "err", err)
			return
		}
		if pi.Used {
			proxyIP = realIP
			realIP, realPort = pi.SrcIP, pi.SrcPort
			viaProxy = true
		}
	}

	req, err := readRequest(br)
	if err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})

	switch req.Method {
	case "GET":
		c.handleGet(ctx, conn, br, req, realIP, realPort, viaProxy, proxyIP)
	case "SOURCE", "POST":
		c.handlePushIn(ctx, conn, br, req, realIP)
	default:
		writeErr(conn, req, 501, "Not Implemented")
	}
}

type request struct {
	Method, Path, Proto string
	Headers             map[string]string
	// NtripVersion is 2 when the client announced Ntrip/2.0, else 1.
	NtripVersion int
	// SourcePass carries the password from an NTRIP v1 SOURCE line.
	SourcePass string
}

func readRequest(br *bufio.Reader) (*request, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	f := strings.Fields(line)
	if len(f) < 2 {
		return nil, fmt.Errorf("malformed request line %q", line)
	}
	r := &request{Method: strings.ToUpper(f[0]), Headers: map[string]string{}, NtripVersion: 1}
	if r.Method == "SOURCE" {
		// NTRIP v1 push-in: SOURCE <password> <mountpoint>
		r.SourcePass = f[1]
		if len(f) > 2 {
			r.Path = f[2]
		}
	} else {
		r.Path = f[1]
		if len(f) > 2 {
			r.Proto = f[2]
		}
	}
	for i := 0; i < 64; i++ {
		h, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			break
		}
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			continue
		}
		r.Headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	if v := r.Headers["ntrip-version"]; strings.EqualFold(v, "Ntrip/2.0") {
		r.NtripVersion = 2
	}
	return r, nil
}

func (c *Caster) handleGet(ctx context.Context, conn net.Conn, br *bufio.Reader,
	req *request, ip string, port int, viaProxy bool, proxyIP string) {

	mountName := strings.TrimPrefix(req.Path, "/")
	if mountName == "" {
		// A browser asks for text/html and sends no NTRIP version header.
		// Answering it with "SOURCETABLE 200 OK" is not valid HTTP, so the
		// browser reports an invalid response. Serve a real page instead --
		// the caster being replaced did the same.
		if isBrowser(req) {
			c.writeBrowserPage(conn)
			return
		}
		c.writeSourcetable(conn, req)
		return
	}
	c.mu.RLock()
	m := c.mounts[mountName]
	c.mu.RUnlock()
	if m == nil {
		// The spec says an unknown mountpoint gets the sourcetable, which is
		// how clients discover what is actually available.
		c.writeSourcetable(conn, req)
		return
	}

	user, pass, ok := basicAuth(req.Headers["authorization"])
	if !ok {
		c.Rejected.Add(1)
		writeUnauthorized(conn, req)
		return
	}
	u, err := c.o.Store.Authenticate(c.o.Keyring, user, pass)
	if err != nil {
		c.Rejected.Add(1)
		c.log.Warn("auth failed", "user", user, "mount", mountName, "ip", ip)
		writeUnauthorized(conn, req)
		return
	}
	if status, reason, err := c.checkUserAccess(u, mountName, ip, time.Now()); status != 0 {
		c.Rejected.Add(1)
		attrs := []any{"user", user, "mount", mountName, "ip", ip, "reason", reason}
		if err != nil {
			attrs = append(attrs, "err", err)
		}
		c.log.Warn("account access refused", attrs...)
		writeErr(conn, req, status, http.StatusText(status))
		return
	}

	// Keep limit checking and accounting atomic within this caster process.
	c.admissionMu.Lock()
	n, countErr := c.o.Store.ActiveConnectionsForUser(u.ID)
	if countErr != nil {
		c.admissionMu.Unlock()
		c.Rejected.Add(1)
		c.log.Error("connection admission failed", "user", user, "err", countErr)
		writeErr(conn, req, http.StatusServiceUnavailable, http.StatusText(http.StatusServiceUnavailable))
		return
	}
	if n >= u.ConnectionLimit {
		c.admissionMu.Unlock()
		c.Rejected.Add(1)
		c.log.Warn("connection limit reached",
			"user", user, "active", n, "limit", u.ConnectionLimit, "ip", ip)
		writeErr(conn, req, http.StatusConflict, http.StatusText(http.StatusConflict))
		return
	}

	rowID, err := c.o.Store.ConnStart(u.ID, u.Username, mountName, ip, port,
		viaProxy, proxyIP, req.Headers["user-agent"], req.NtripVersion)
	c.admissionMu.Unlock()
	if err != nil {
		c.Rejected.Add(1)
		c.log.Error("accounting insert failed", "err", err)
		writeErr(conn, req, http.StatusServiceUnavailable, http.StatusText(http.StatusServiceUnavailable))
		return
	}

	writeStreamOK(conn, req)
	c.Connections.Add(1)
	m.Joined.Add(1)
	c.log.Info("client connected", "user", u.Username, "mount", mountName,
		"ip", ip, "via_proxy", viaProxy, "ntrip", req.NtripVersion)

	cl := &client{conn: conn, ch: make(chan []byte, 512), rowID: rowID, user: u.Username}
	m.mu.Lock()
	m.clients[cl] = struct{}{}
	m.mu.Unlock()

	// Replay cached station messages so the rover does not wait for the next
	// 10-second 1006/1008/1033 cycle before it can use the stream.
	m.lastMu.Lock()
	for _, t := range []int{1005, 1006, 1007, 1008, 1013, 1033} {
		if b, ok := m.last[t]; ok {
			select {
			case cl.ch <- b:
			default:
			}
		}
	}
	m.lastMu.Unlock()

	reason := "client closed"
	var recv int64
	done := make(chan struct{})
	go func() { // drain client->server (NMEA GGA from rovers)
		defer close(done)
		buf := make([]byte, 1024)
		for {
			n, err := br.Read(buf)
			recv += int64(n)
			if err != nil {
				return
			}
		}
	}()

	for b := range cl.ch {
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		n, err := conn.Write(b)
		cl.sent.Add(int64(n))
		if err != nil {
			reason = "write error: " + err.Error()
			break
		}
	}

	m.mu.Lock()
	delete(m.clients, cl)
	m.mu.Unlock()
	cl.close()
	<-done
	c.Connections.Add(-1)
	if rowID != 0 {
		if err := c.o.Store.ConnEnd(rowID, cl.sent.Load(), recv, 0, reason); err != nil {
			c.log.Error("accounting close failed", "err", err)
		}
	}
	c.log.Info("client disconnected", "user", u.Username, "mount", mountName,
		"ip", ip, "bytes", cl.sent.Load(), "reason", reason)
}

// checkUserAccess applies account policy before a connection consumes a slot.
// A zero status means allowed. Database and malformed-address errors fail
// closed with 503, while deliberate policy refusals return 403.
func (c *Caster) checkUserAccess(u *store.User, mountpoint, ip string, now time.Time) (int, string, error) {
	if !u.Enabled {
		return http.StatusForbidden, "disabled", nil
	}
	if u.ExpiresAt != nil && !now.Before(*u.ExpiresAt) {
		return http.StatusForbidden, "expired", nil
	}
	access, err := c.o.Store.GetUserAccessForUser(u.ID)
	if err != nil {
		return http.StatusServiceUnavailable, "access lookup failed", err
	}
	if !access.AllowsMountpoint(mountpoint) {
		return http.StatusForbidden, "mountpoint not allowed", nil
	}
	allowed, err := access.AllowsIP(ip)
	if err != nil {
		return http.StatusServiceUnavailable, "IP policy evaluation failed", err
	}
	if !allowed {
		return http.StatusForbidden, "IP not allowed", nil
	}
	return 0, "", nil
}

// handlePushIn accepts an external NTRIP server feeding a mountpoint.
func (c *Caster) handlePushIn(ctx context.Context, conn net.Conn, br *bufio.Reader,
	req *request, ip string) {

	mountName := strings.TrimPrefix(req.Path, "/")
	pass := req.SourcePass
	if req.Method == "POST" {
		_, p, ok := basicAuth(req.Headers["authorization"])
		if ok {
			pass = p
		}
	}
	if c.o.PushInPass == "" || pass != c.o.PushInPass {
		c.log.Warn("push-in rejected: bad password", "mount", mountName, "ip", ip)
		if req.NtripVersion == 2 || req.Method == "POST" {
			writeErr(conn, req, 401, "Unauthorized")
		} else {
			conn.Write([]byte("ERROR - Bad Password\r\n"))
		}
		return
	}
	c.mu.RLock()
	m := c.mounts[mountName]
	c.mu.RUnlock()
	if m == nil || !m.PushIn {
		c.log.Warn("push-in rejected: unknown mountpoint", "mount", mountName, "ip", ip)
		conn.Write([]byte("ERROR - Bad Mountpoint\r\n"))
		return
	}

	if req.Method == "POST" {
		conn.Write([]byte("HTTP/1.1 200 OK\r\nNtrip-Version: Ntrip/2.0\r\n\r\n"))
	} else {
		conn.Write([]byte("ICY 200 OK\r\n\r\n"))
	}
	c.log.Info("push-in feed connected", "mount", mountName, "ip", ip)

	sc := hub.NewScanner(br, 32768)
	for {
		if ctx.Err() != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		f, err := sc.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.log.Warn("push-in feed ended", "mount", mountName, "err", err)
			}
			return
		}
		cp := make([]byte, len(f.Raw))
		copy(cp, f.Raw)
		m.broadcast(cp)
	}
}

// isBrowser reports whether this looks like a person with a web browser
// rather than an NTRIP client.
func isBrowser(req *request) bool {
	if req.Headers["ntrip-version"] != "" {
		return false
	}
	if strings.Contains(strings.ToLower(req.Headers["user-agent"]), "ntrip") {
		return false
	}
	return strings.Contains(req.Headers["accept"], "text/html")
}

// writeBrowserPage serves a plain status page over valid HTTP.
func (c *Caster) writeBrowserPage(conn net.Conn) {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>NTRIP Caster</title><style>` +
		`body{font:15px/1.6 system-ui,sans-serif;max-width:46rem;margin:3rem auto;padding:0 1rem;` +
		`background:#0f1418;color:#e6edf3}h1{font-size:1.25rem}` +
		`table{border-collapse:collapse;width:100%;margin:1rem 0}` +
		`th,td{text-align:left;padding:.4rem .6rem;border-bottom:1px solid #2a353f;font-size:14px}` +
		`th{color:#8b9aa8;font-size:12px;text-transform:uppercase;letter-spacing:.5px}` +
		`code{background:#1e262e;padding:.1rem .35rem;border-radius:4px}` +
		`.m{color:#8b9aa8}` +
		`@media(prefers-color-scheme:light){body{background:#fff;color:#17212b}` +
		`code{background:#eef2f5}th,td{border-color:#d5dde4}}` +
		`</style></head><body>`)
	b.WriteString("<h1>NTRIP Caster</h1>")
	b.WriteString(`<p class="m">This is an NTRIP caster, not a website. ` +
		`Point GNSS rover software at this address and port; a browser cannot ` +
		`use the correction streams.</p>`)

	c.mu.RLock()
	names := make([]string, 0, len(c.mounts))
	for n := range c.mounts {
		names = append(names, n)
	}
	c.mu.RUnlock()
	sort.Strings(names)

	if len(names) == 0 {
		b.WriteString(`<p class="m">No mountpoints are currently available.</p>`)
	} else {
		b.WriteString("<table><tr><th>Mountpoint</th><th>Format</th><th>Systems</th><th>Auth</th></tr>")
		c.mu.RLock()
		for _, n := range names {
			m := c.mounts[n]
			fmt.Fprintf(&b, "<tr><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td></tr>",
				htmlEsc(m.Entry.Name), htmlEsc(m.Entry.Format),
				htmlEsc(m.Entry.NavSystem), "required")
		}
		c.mu.RUnlock()
		b.WriteString("</table>")
	}
	b.WriteString(`<p class="m">All mountpoints require a username and password. ` +
		`The machine-readable sourcetable is served to NTRIP clients on this ` +
		`same address.</p>`)
	b.WriteString(`<p class="m">Served by PSGNSS_base.</p></body></html>`)

	body := b.String()
	fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\n"+
		"Server: PSGNSS_base\r\n"+
		"Content-Type: text/html; charset=utf-8\r\n"+
		"Content-Length: %d\r\n"+
		"Cache-Control: no-store\r\n"+
		"Connection: close\r\n\r\n%s", len(body), body)
}

func htmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func (c *Caster) writeSourcetable(conn net.Conn, req *request) {
	entries := make([]MountEntry, 0, len(c.mounts))
	c.mu.RLock()
	for _, m := range c.mounts {
		entries = append(entries, m.Entry)
	}
	c.mu.RUnlock()
	body := Sourcetable(entries, c.o.Net)

	if req.NtripVersion == 2 && c.o.AllowV2 {
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\n"+
			"Ntrip-Version: Ntrip/2.0\r\n"+
			"Server: PSGNSS_base\r\n"+
			"Content-Type: gnss/sourcetable\r\n"+
			"Content-Length: %d\r\n"+
			"Connection: close\r\n\r\n%s", len(body), body)
		return
	}
	fmt.Fprintf(conn, "SOURCETABLE 200 OK\r\n"+
		"Server: PSGNSS_base\r\n"+
		"Content-Type: text/plain\r\n"+
		"Content-Length: %d\r\n\r\n%s", len(body), body)
}

func writeStreamOK(conn net.Conn, req *request) {
	if req.NtripVersion == 2 {
		conn.Write([]byte("HTTP/1.1 200 OK\r\n" +
			"Ntrip-Version: Ntrip/2.0\r\n" +
			"Server: PSGNSS_base\r\n" +
			"Content-Type: gnss/data\r\n" +
			"Cache-Control: no-store, no-cache, max-age=0\r\n" +
			"Connection: close\r\n\r\n"))
		return
	}
	conn.Write([]byte("ICY 200 OK\r\n\r\n"))
}

func writeUnauthorized(conn net.Conn, req *request) {
	if req.NtripVersion == 2 {
		conn.Write([]byte("HTTP/1.1 401 Unauthorized\r\n" +
			"Ntrip-Version: Ntrip/2.0\r\n" +
			"WWW-Authenticate: Basic realm=\"NTRIP\"\r\n" +
			"Connection: close\r\n\r\n"))
		return
	}
	conn.Write([]byte("HTTP/1.0 401 Unauthorized\r\n" +
		"WWW-Authenticate: Basic realm=\"NTRIP\"\r\n\r\n"))
}

func writeErr(conn net.Conn, req *request, code int, msg string) {
	proto := "HTTP/1.0"
	if req.NtripVersion == 2 {
		proto = "HTTP/1.1"
	}
	fmt.Fprintf(conn, "%s %d %s\r\nConnection: close\r\n\r\n", proto, code, msg)
}

func basicAuth(h string) (user, pass string, ok bool) {
	const p = "basic "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(p):]))
	if err != nil {
		return "", "", false
	}
	u, pw, ok := strings.Cut(string(raw), ":")
	return u, pw, ok
}

func splitHostPort(s string) (string, int) {
	h, p, err := net.SplitHostPort(s)
	if err != nil {
		return s, 0
	}
	n, _ := strconv.Atoi(p)
	return h, n
}
