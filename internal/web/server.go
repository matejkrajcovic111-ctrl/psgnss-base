// Package web serves the PSGNSS dashboard and its JSON API.
package web

import (
	"compress/gzip"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/psgnss/psgnss-base/internal/archive"
	"github.com/psgnss/psgnss-base/internal/caster"
	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/coverage"
	"github.com/psgnss/psgnss-base/internal/downloader"
	"github.com/psgnss/psgnss-base/internal/ephemeris"
	"github.com/psgnss/psgnss-base/internal/hostinfo"
	"github.com/psgnss/psgnss-base/internal/hub"
	"github.com/psgnss/psgnss-base/internal/integrity"
	"github.com/psgnss/psgnss-base/internal/logbuffer"
	"github.com/psgnss/psgnss-base/internal/pushout"
	"github.com/psgnss/psgnss-base/internal/rtcmout"
	"github.com/psgnss/psgnss-base/internal/secrets"
	"github.com/psgnss/psgnss-base/internal/store"
	"github.com/psgnss/psgnss-base/internal/telemetry"
	"github.com/psgnss/psgnss-base/internal/timesync"
	"github.com/psgnss/psgnss-base/internal/version"
)

//go:embed assets
var assetsFS embed.FS

const sessionCookie = "psgnss_session"
const sessionTTL = 12 * time.Hour

// Server holds everything the API exposes.
type Server struct {
	Cfg        *config.Config
	ConfigPath string
	// Restart requests a graceful daemon restart after persisted settings
	// change. Production sends SIGHUP; tests may provide a harmless stub.
	Restart    func() error
	Store      *store.Store
	Keyring    *secrets.Keyring
	Hub        *hub.Hub
	Caster     *caster.Caster
	Collector  *telemetry.Collector
	Archives   []*archive.Writer
	Downloader *downloader.Downloader
	Log        *slog.Logger
	Logs       *logbuffer.Buffer
	Integrity  *integrity.Monitor
	Ephemeris  *ephemeris.Store
	Pushout    *pushout.Manager
	TimeSync   *timesync.Sender
	RTCMOut    *rtcmout.Manager
	// Stage0Path overrides where the Stage 0 receiver dump is read from.
	Stage0Path  string
	tiles       *tileCache
	receiverMu  sync.Mutex
	receiverTxn *pendingReceiverTxn
	// receiverControl replaces Hub.Control in tests.
	receiverControl func() (io.ReadWriteCloser, error)
	settingsMu      sync.Mutex

	mux *http.ServeMux
}

func New(s *Server) *Server {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.Cfg != nil && s.Cfg.Web.MapTiles != "" {
		dir := s.Cfg.Web.MapCacheDir
		if dir == "" {
			dir = filepath.Join(filepath.Dir(s.Cfg.Telemetry.DBPath), "tilecache")
		}
		s.tiles = newTileCache(dir, s.Cfg.Web.MapCacheDays)
	}
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

func (s *Server) routes() {
	// Public: the dashboard, data history and file download pages, plus what
	// they need to draw themselves. Everything registered with s.auth below is
	// administrator-only. A route added here is world-readable through the
	// tunnel -- TestPublicSurfaceIsExactlyTheReadOnlyPages pins the list.
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("GET /api/status", s.handleStatus) // read-only summary
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Authenticated
	s.mux.HandleFunc("POST /api/logout", s.auth(s.handleLogout))
	s.mux.HandleFunc("GET /api/me", s.pub(s.handleMe))
	s.mux.HandleFunc("GET /api/live", s.pub(s.handleLive))
	s.mux.HandleFunc("GET /api/history", s.pub(s.handleHistory))
	s.mux.HandleFunc("GET /api/users", s.auth(s.handleUsers))
	s.mux.HandleFunc("POST /api/users", s.auth(s.handleUserCreate))
	s.mux.HandleFunc("GET /api/users/{name}", s.auth(s.handleUserGet))
	s.mux.HandleFunc("PUT /api/users/{name}", s.auth(s.handleUserUpdate))
	s.mux.HandleFunc("DELETE /api/users/{name}", s.auth(s.handleUserDelete))
	s.mux.HandleFunc("POST /api/users/{name}/enabled", s.auth(s.handleUserEnable))
	s.mux.HandleFunc("GET /api/connections", s.auth(s.handleConnections))
	s.mux.HandleFunc("GET /api/mountpoints", s.auth(s.handleMountpoints))
	s.mux.HandleFunc("GET /api/settings", s.auth(s.handleSettings))
	s.mux.HandleFunc("PUT /api/settings/mountpoints", s.auth(s.handleSettingsMountpoints))
	s.mux.HandleFunc("POST /api/settings/position/apply", s.auth(s.handleSettingsPositionApply))
	s.mux.HandleFunc("PUT /api/settings/general", s.auth(s.handleSettingsGeneral))
	s.mux.HandleFunc("POST /api/settings/station-id/apply", s.auth(s.handleSettingsStationIDApply))
	s.mux.HandleFunc("GET /api/operations/health", s.auth(s.handleOperationsHealth))
	s.mux.HandleFunc("POST /api/operations/diagnose", s.auth(s.handleOperationsHealth))
	s.mux.HandleFunc("GET /api/operations/logs", s.auth(s.handleOperationsLogs))
	s.mux.HandleFunc("POST /api/operations/restart", s.auth(s.handleOperationsRestart))
	s.mux.HandleFunc("POST /api/system/power", s.auth(s.handleSystemPower))
	s.mux.HandleFunc("GET /api/logs", s.auth(s.handleLogs))
	s.mux.HandleFunc("GET /api/logs/download", s.auth(s.handleLogsDownload))
	s.mux.HandleFunc("GET /api/integrity", s.auth(s.handleIntegrityStatus))
	s.mux.HandleFunc("PUT /api/integrity/settings", s.auth(s.handleIntegritySettings))
	s.mux.HandleFunc("POST /api/integrity/run", s.auth(s.handleIntegrityRun))
	s.mux.HandleFunc("POST /api/integrity/cancel", s.auth(s.handleIntegrityCancel))
	s.mux.HandleFunc("GET /tiles/{z}/{x}/{y}", s.pub(s.handleTile))
	s.mux.HandleFunc("GET /api/pushout", s.auth(s.handlePushoutStatus))
	s.mux.HandleFunc("PUT /api/pushout", s.auth(s.handlePushoutSave))
	s.mux.HandleFunc("POST /api/backup", s.auth(s.handleBackupCreate))
	s.mux.HandleFunc("POST /api/backup/inspect", s.auth(s.handleBackupInspect))
	s.mux.HandleFunc("POST /api/backup/restore", s.auth(s.handleBackupRestore))
	s.mux.HandleFunc("GET /api/admins", s.auth(s.handleAdmins))
	s.mux.HandleFunc("POST /api/admins", s.auth(s.handleAdminCreate))
	s.mux.HandleFunc("PUT /api/admins/{name}/password", s.auth(s.handleAdminPassword))
	s.mux.HandleFunc("DELETE /api/admins/{name}", s.auth(s.handleAdminDelete))
	s.mux.HandleFunc("GET /api/receiver", s.auth(s.handleReceiver))
	s.mux.HandleFunc("POST /api/receiver/apply", s.auth(s.handleReceiverApply))
	s.mux.HandleFunc("POST /api/receiver/commit", s.auth(s.handleReceiverCommit))
	s.mux.HandleFunc("POST /api/receiver/revert", s.auth(s.handleReceiverRevert))
	s.mux.HandleFunc("GET /api/receiver/stage0", s.auth(s.handleStage0Preview))
	s.mux.HandleFunc("POST /api/receiver/stage0/restore", s.auth(s.handleStage0Restore))
	s.mux.HandleFunc("GET /api/downloader/available", s.pub(s.handleDlAvailable))
	s.mux.HandleFunc("POST /api/downloader/process", s.pub(s.handleDlProcess))
	s.mux.HandleFunc("GET /api/downloader/job/{id}", s.pub(s.handleDlJob))
	s.mux.HandleFunc("GET /api/downloader/jobs", s.pub(s.handleDlJobs))
	s.mux.HandleFunc("POST /api/downloader/job/{id}/cancel", s.pub(s.handleDlCancel))
	s.mux.HandleFunc("GET /api/downloader/download/{token}", s.pub(s.handleDlDownload))

	// UI
	sub, _ := fs.Sub(assetsFS, "assets")
	s.mux.Handle("GET /", staticCompression(http.FileServer(http.FS(sub))))
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Add("Vary", "Accept-Encoding")
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.gz.Write(p)
}

// staticCompression cuts the cold browser transfer from roughly 3.8 MB to
// about 1.2 MB. API responses retain their own semantics; history already has
// a dedicated compressed writer.
func staticCompression(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Range") != "" ||
			!strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		next.ServeHTTP(gw, r)
		_ = gz.Close()
	})
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Cfg.Web.Listen,
		Handler:           s.securityHeaders(s.throttle(s.mux)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sh)
	}()
	s.Log.Info("web UI listening", "addr", s.Cfg.Web.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// Everything is served from this origin, including map tiles, which the
		// daemon fetches and caches on the browser's behalf. The page itself
		// never talks to another host, so this policy stays closed even with a
		// map configured.
		h.Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; "+
				"frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
		// Only when the browser actually arrived over TLS, so a plain-http
		// session on the local network is not locked out of its own station.
		if secureRequest(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		} else if r.URL.Path == "/" || r.URL.Path == "/index.html" ||
			r.URL.Path == "/app.js" || r.URL.Path == "/app.css" || r.URL.Path == "/i18n.js" {
			// These files define the application shell. Never let a browser keep
			// running an old shell against a newly deployed API.
			h.Set("Cache-Control", "no-store, max-age=0")
		} else if r.URL.Path == "/uplot-1.6.32.min.js" || r.URL.Path == "/uplot-1.6.32.min.css" ||
			r.URL.Path == "/leaflet-1.9.4.js" || r.URL.Path == "/leaflet-1.9.4.css" ||
			r.URL.Path == "/tabler-1.4.0.min.css" || strings.HasSuffix(r.URL.Path, ".woff2") {
			// The version is part of the filename, so it is safe to keep forever.
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else if r.URL.Path == "/react.js" || r.URL.Path == "/react-dom.js" ||
			r.URL.Path == "/htm.js" {
			h.Set("Cache-Control", "public, max-age=604800")
		}
		next.ServeHTTP(w, r)
	})
}

// tileRoute is the same-origin path the page fetches tiles from, or empty when
// no map is configured.
func tileRoute(upstream string) string {
	if upstream == "" {
		return ""
	}
	return "tiles/{z}/{x}/{y}"
}

// auth wraps a handler so it requires a valid session.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "not signed in")
			return
		}
		name, ok := s.Store.AdminBySession(c.Value)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "session expired")
			return
		}
		r = r.WithContext(withAdmin(r.Context(), name))
		next(w, r)
	}
}

type ctxAdmin struct{}

func withAdmin(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, ctxAdmin{}, name)
}

// isAdmin reports whether this request carries an administrator session. On a
// public route that is the difference between the whole page and the parts
// anyone may see.
func isAdmin(r *http.Request) bool { return adminOf(r) != "" }

func adminOf(r *http.Request) string {
	v, _ := r.Context().Value(ctxAdmin{}).(string)
	return v
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeJSONCompressed(w http.ResponseWriter, r *http.Request, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Vary", "Accept-Encoding")
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.WriteHeader(code)
	gz := gzip.NewWriter(w)
	_ = json.NewEncoder(gz).Encode(v)
	_ = gz.Close()
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// ---------------------------------------------------------------- handlers

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct{ Username, Password string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	ip := realIP(r)
	// Sign-in is the one endpoint where guessing pays, and it is now reachable
	// from the internet. A successful attempt gives its token back, so a
	// working password is never throttled.
	if !loginLimit.allow(ip) {
		s.Log.Warn("admin login rate limited", "user", body.Username, "ip", ip)
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "too many sign-in attempts; wait a minute")
		return
	}
	tok, err := s.Store.AdminLogin(body.Username, body.Password, ip, sessionTTL)
	if err != nil {
		s.Log.Warn("admin login failed", "user", body.Username, "ip", ip)
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	loginLimit.refund(ip, 1)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: secureRequest(r),
		MaxAge: int(sessionTTL.Seconds()),
	})
	s.Log.Info("admin signed in", "user", body.Username, "ip", ip)
	writeJSON(w, http.StatusOK, map[string]string{"username": body.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.Store.AdminLogout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secureRequest(r), MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleMe is how the page decides what to render. It answers for an
// anonymous visitor too -- with admin false -- because a public dashboard must
// not send anyone to a sign-in form.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"username": adminOf(r), "admin": isAdmin(r)})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	_, at := s.Collector.Latest()
	fresh := time.Since(at) < 15*time.Second
	code := http.StatusOK
	if !fresh {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"ok":                fresh,
		"last_epoch_age_s":  int(time.Since(at).Seconds()),
		"hub_frames":        s.Hub.Stats.FramesIn.Load(),
		"hub_bytes_dropped": s.Hub.Stats.BytesDropped.Load(),
	})
}

// handleStatus is the unauthenticated summary: enough to see the station is
// alive, with nothing sensitive in it.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ep, at := s.Collector.Latest()
	writeJSON(w, http.StatusOK, map[string]any{
		"station":     s.Cfg.Station.Name,
		"version":     version.String(),
		"fix":         telemetry.FixName(ep.FixType),
		"satellites":  len(ep.Sats),
		"last_update": at.UTC(),
		"online":      time.Since(at) < 15*time.Second,
	})
}

type liveSat struct {
	System string `json:"system"`
	SvID   int    `json:"sv"`
	CNO    int    `json:"cno"`
	Elev   int    `json:"elev"`
	Azim   int    `json:"azim"`
	Used   bool   `json:"used"`
}

type liveSignal struct {
	System string `json:"system"`
	SvID   int    `json:"sv"`
	Band   string `json:"band"`
	CNO    int    `json:"cno"`
	Used   bool   `json:"used"`
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	ep, at := s.Collector.Latest()
	sats := make([]liveSat, 0, len(ep.Sats))
	bySystem := map[string]map[string]int{}
	for _, t := range ep.Sats {
		name := telemetry.GNSSName(t.GNSSID)
		sats = append(sats, liveSat{name, int(t.SvID), int(t.CNO),
			int(t.Elev), int(t.Azim), t.Used})
		m, ok := bySystem[name]
		if !ok {
			m = map[string]int{}
			bySystem[name] = m
		}
		m["tracked"]++
		if t.Used {
			m["used"]++
		}
	}
	signals := make([]liveSignal, 0)
	for _, sig := range s.Collector.LatestSignals() {
		signals = append(signals, liveSignal{telemetry.GNSSName(sig.GNSSID),
			int(sig.SvID), signalBand(sig.GNSSID, sig.SigID), int(sig.CNO), sig.Used})
	}

	mounts := []map[string]any{}
	if s.Caster != nil {
		for _, m := range s.Caster.Mounts() {
			mounts = append(mounts, map[string]any{
				"name":      m.Entry.Name,
				"bytes_out": m.BytesOut.Load(),
				"joined":    m.Joined.Load(),
			})
		}
	}

	var listeners []map[string]any
	for _, sub := range s.Hub.Subs() {
		listeners = append(listeners, map[string]any{
			"name": sub.Name, "sent": sub.Sent.Load(), "dropped": sub.Dropped.Load(),
		})
	}

	payload := map[string]any{
		"time":        time.Now().UTC(),
		"last_update": at.UTC(),
		"online":      time.Since(at) < 15*time.Second,
		"fix":         telemetry.FixName(ep.FixType),
		"fix_type":    ep.FixType,
		"satellites":  sats,
		"signals":     signals,
		"by_system":   bySystem,
		"host":        hostinfo.Read(s.Cfg.Archive.MountPoint),
		"hub": map[string]any{
			"bytes_in":      s.Hub.Stats.BytesIn.Load(),
			"frames":        s.Hub.Stats.FramesIn.Load(),
			"rtcm_frames":   s.Hub.Stats.RTCMFrames.Load(),
			"ubx_frames":    s.Hub.Stats.UBXFrames.Load(),
			"bytes_dropped": s.Hub.Stats.BytesDropped.Load(),
			"resyncs":       s.Hub.Stats.Resyncs.Load(),
			"reconnects":    s.Hub.Stats.Reconnects.Load(),
			"last_drop_age": dropAge(s.Hub.Stats.LastDropAt.Load()),
		},
		"mountpoints":                   mounts,
		"listeners":                     listeners,
		"archives":                      s.archiveStats(),
		"gps_time":                      s.gpsTime(),
		"ntrip_clients":                 casterConns(s.Caster),
		"ntrip_rejected":                casterRejects(s.Caster),
		"version":                       version.String(),
		"station":                       s.Cfg.Station.Name,
		"display_hidden_constellations": displayHiddenConstellations(s.Cfg),
		"station_position": map[string]any{
			"latitude":  s.Cfg.Station.Position.Latitude,
			"longitude": s.Cfg.Station.Position.Longitude,
			"height":    s.Cfg.Station.Position.Height,
		},
		"coverage": liveCoverage(sats, signals),
		"map": map[string]any{
			// The page is given this daemon's own tile route, never the
			// upstream URL: the browser must not talk to the provider, because
			// it cannot identify itself the way the usage policy requires.
			"tiles":       tileRoute(s.Cfg.Web.MapTiles),
			"attribution": s.Cfg.Web.MapAttribution,
		},
	}
	// An anonymous visitor gets what the public pages draw and nothing else.
	// The hub counters, the subscriber list and the per-mountpoint byte totals
	// describe how the daemon is put together inside, which is reconnaissance
	// rather than station information.
	if !isAdmin(r) {
		for _, internal := range []string{"hub", "listeners", "mountpoints"} {
			delete(payload, internal)
		}
		// The capture readouts stay -- they are the file download page's
		// subject -- but without the local and network paths they are written
		// to, which say more about this machine than about the data.
		payload["archives"] = publicArchives(s.archiveStats())
	}
	writeJSON(w, http.StatusOK, payload)
}

// liveCoverage turns what the receiver is tracking into the usable-range
// figure the dashboard draws as a circle. SBAS is excluded: it is a correction
// service, not a ranging constellation for RTK.
func liveCoverage(sats []liveSat, signals []liveSignal) coverage.Range {
	systems, bands := map[string]bool{}, map[string]bool{}
	for _, s := range sats {
		if s.CNO > 0 && s.System != "SBAS" {
			systems[s.System] = true
		}
	}
	for _, sig := range signals {
		if sig.CNO > 0 && sig.System != "SBAS" && sig.Band != "" {
			bands[sig.Band] = true
		}
	}
	return coverage.Estimate(coverage.Capability{Constellations: len(systems), Bands: len(bands)})
}

// signalBand gives an operator-facing band label for the UBX-NAV-SIG signal
// identifiers used by the constellations this receiver supports. Unknown
// identifiers are intentionally retained rather than guessed.
func signalBand(gnss, sig uint8) string {
	labels := map[uint8]map[uint8]string{
		0: {0: "L1 C/A", 3: "L2 CL", 4: "L2 CM", 6: "L5 I", 7: "L5 Q"},
		1: {0: "L1 C/A"},
		2: {0: "E1 C", 1: "E1 B", 3: "E5a I", 4: "E5a Q", 5: "E5b I", 6: "E5b Q", 7: "E6 C", 8: "E6 B"},
		3: {0: "B1I D1", 1: "B1I D2", 2: "B2I D1", 3: "B2I D2", 4: "B3I D1", 5: "B3I D2", 7: "B2a"},
		5: {0: "L1 C/A", 1: "L1S", 4: "L2 CM", 5: "L2 CL", 8: "L5 I", 9: "L5 Q"},
		6: {0: "L1 OF", 2: "L2 OF"},
		7: {0: "L5 A"},
	}
	if byID, ok := labels[gnss]; ok {
		if label, ok := byID[sig]; ok {
			return label
		}
	}
	return fmt.Sprintf("signal %d", sig)
}

// dropAge returns seconds since unframed bytes were last discarded, or -1 if
// that has never happened.
func dropAge(unix int64) int64 {
	if unix == 0 {
		return -1
	}
	return int64(time.Since(time.Unix(unix, 0)).Seconds())
}

// archiveStats reports what each archive is capturing right now.
// publicArchives is the capture progress with the filesystem out of it.
func publicArchives(in []archive.Stats) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, a := range in {
		out = append(out, map[string]any{"name": a.Name, "bytes": a.Bytes,
			"start": a.StartSec, "swap_at": a.SwapAtSec})
	}
	return out
}

func (s *Server) archiveStats() []archive.Stats {
	if s.Archives == nil {
		return nil
	}
	out := make([]archive.Stats, 0, len(s.Archives))
	for _, w := range s.Archives {
		out = append(out, w.Snapshot())
	}
	return out
}

// gpsTime exposes the receiver's GPS time reference so the UI can show GPS
// alongside UTC and local time without hardcoding the leap-second offset.
func (s *Server) gpsTime() map[string]any {
	leap, week, tow, at := s.Collector.GPSTime()
	if week == 0 {
		return map[string]any{"valid": false}
	}
	return map[string]any{
		"valid":     true,
		"leap_secs": leap,
		"week":      week,
		"tow":       tow,
		"observed":  at.UTC(),
	}
}

func casterConns(c *caster.Caster) int64 {
	if c == nil {
		return 0
	}
	return c.Connections.Load()
}

func casterRejects(c *caster.Caster) int64 {
	if c == nil {
		return 0
	}
	return c.Rejected.Load()
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	hours := 1
	maxHours := s.Cfg.Telemetry.RetentionDays * 24
	if maxHours < 1 {
		maxHours = 24
	}
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= maxHours {
			hours = n
		}
	}
	to := time.Now()
	from := to.Add(-time.Duration(hours) * time.Hour)

	// Longer windows get more points, but never enough to make the browser
	// repeatedly parse and redraw a multi-megabyte uncompressed response.
	limit := 240
	switch {
	case hours >= 168:
		limit = 720
	case hours >= 72:
		limit = 600
	case hours >= 24:
		limit = 480
	case hours >= 6:
		limit = 360
	}
	epochCount, err := s.Store.EpochCount(from, to)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	epochs, err := s.Store.EpochRange(from, to, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type point struct {
		T       int64          `json:"t"`
		N       int            `json:"n"`
		Fix     int            `json:"fix"`
		Sys     map[string]int `json:"sys"`
		AvgC    int            `json:"avg_cno"`
		Sats    []liveSat      `json:"satellites"`
		Signals []liveSignal   `json:"signals"`
	}
	pts := make([]point, 0, len(epochs))
	for _, e := range epochs {
		sys := map[string]int{}
		sum, cnt := 0, 0
		for _, t := range e.Sats {
			sys[telemetry.GNSSName(t.GNSSID)]++
			if t.CNO > 0 {
				sum += int(t.CNO)
				cnt++
			}
		}
		sats := make([]liveSat, 0, len(e.Sats))
		for _, s := range e.Sats {
			sats = append(sats, liveSat{telemetry.GNSSName(s.GNSSID), int(s.SvID), int(s.CNO), int(s.Elev), int(s.Azim), s.Used})
		}
		signals := make([]liveSignal, 0, len(e.Signals))
		for _, s := range e.Signals {
			signals = append(signals, liveSignal{telemetry.GNSSName(s.GNSSID), int(s.SvID), signalBand(s.GNSSID, s.SigID), int(s.CNO), s.Used})
		}
		avg := 0
		if cnt > 0 {
			avg = sum / cnt
		}
		pts = append(pts, point{e.TS.Unix(), e.SatCount, int(e.FixType), sys, avg, sats, signals})
	}
	healthRows, _ := s.Store.HealthRange(from, to, limit)
	health := make([]map[string]any, 0, len(healthRows))
	for _, h := range healthRows {
		health = append(health, map[string]any{"t": h.TS.Unix(), "rtcm_bps": h.RTCMBps, "ubx_bps": h.UBXBps, "clients": h.Clients, "cpu_pct": h.CPUPct, "temp_c": h.TempC, "disk_free_mb": h.DiskFreeMB, "mem_free_mb": h.MemFreeMB})
	}
	writeJSONCompressed(w, r, http.StatusOK, map[string]any{
		"from": from.UTC(), "to": to.UTC(), "epochs": pts, "health": health,
		"epoch_count":                   epochCount,
		"retention_days":                s.Cfg.Telemetry.RetentionDays,
		"position":                      s.Cfg.Station.Position,
		"display_hidden_constellations": displayHiddenConstellations(s.Cfg),
	})
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.ListUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	showPw := r.URL.Query().Get("passwords") == "1"
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		stats, err := s.Store.UserStats(u.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "could not load user statistics")
			return
		}
		row := map[string]any{
			"username": u.Username, "limit": u.ConnectionLimit,
			"enabled": u.Enabled, "active": stats.Active,
			"created": u.CreatedAt.UTC(), "updated": u.UpdatedAt.UTC(),
			"note": u.Note, "email": u.Email,
			"expires_at": u.ExpiresAt, "connection_count": stats.ConnectionCount,
			"total_bytes_sent": stats.BytesSent,
			"last_connection":  stats.LastConnection,
		}
		if showPw {
			// Reversible storage exists precisely so an operator can read a
			// rover's password back to configure field equipment.
			if p, err := s.Store.GetPassword(s.Keyring, u.Username); err == nil {
				row["password"] = p
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Limit    int    `json:"limit"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if body.Username == "" || body.Password == "" {
		writeErr(w, http.StatusBadRequest, "username and password are required")
		return
	}
	u, err := s.Store.CreateUser(s.Keyring, body.Username, body.Password, body.Limit, body.Note)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	s.Log.Info("NTRIP user created", "user", u.Username, "by", adminOf(r))
	writeJSON(w, http.StatusCreated, map[string]any{"username": u.Username})
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.Store.DeleteUser(name); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("NTRIP user deleted", "user", name, "by", adminOf(r))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleUserEnable(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body)
	name := r.PathValue("name")
	if err := s.Store.SetEnabled(name, body.Enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("NTRIP user toggled", "user", name, "enabled", body.Enabled, "by", adminOf(r))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	n := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 && x <= 1000 {
			n = x
		}
	}
	conns, err := s.Store.RecentConnections(n)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(conns))
	for _, c := range conns {
		row := map[string]any{
			"username": c.Username, "mountpoint": c.Mountpoint,
			"client_ip": c.ClientIP, "via_proxy": c.ViaProxy,
			"started": c.StartedAt.UTC(), "bytes": c.BytesSent,
			"agent": c.UserAgent, "active": c.EndedAt == nil,
		}
		if c.EndedAt != nil {
			row["duration_s"] = int(c.EndedAt.Sub(c.StartedAt).Seconds())
		} else {
			row["duration_s"] = int(time.Since(c.StartedAt).Seconds())
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleMountpoints(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	if s.Caster != nil {
		for _, m := range s.Caster.Mounts() {
			var last any
			if ms := m.LastData.Load(); ms > 0 {
				last = time.UnixMilli(ms).UTC()
			}
			out = append(out, map[string]any{
				"name": m.Entry.Name, "source_id": m.Entry.SourceID,
				"format": m.Entry.Format, "messages": m.Entry.Messages,
				"nav_system": m.Entry.NavSystem, "bytes_out": m.BytesOut.Load(),
				"joined": m.Joined.Load(), "clients": m.ActiveClients(), "last_data": last,
				"generated": sortedKeys(m.Generated), "sourcetable": m.Entry.STR(),
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func sortedKeys(m map[int]int) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// ------------------------------------------------------------- downloader
//
// Folded in from the GNSS_Dashboard blueprint: list days, convert one to
// RINEX, hand back a short-lived zip.

func (s *Server) handleDlAvailable(w http.ResponseWriter, r *http.Request) {
	if s.Downloader == nil {
		writeErr(w, http.StatusServiceUnavailable, "downloader not configured")
		return
	}
	days, err := s.Downloader.Available()
	if err != nil {
		// Still return whatever was found; the UI shows the reason alongside.
		writeJSON(w, http.StatusOK, map[string]any{"days": days, "presets": downloader.Presets(), "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "presets": downloader.Presets()})
}

func (s *Server) handleDlProcess(w http.ResponseWriter, r *http.Request) {
	if s.Downloader == nil {
		writeErr(w, http.StatusServiceUnavailable, "downloader not configured")
		return
	}
	var body struct {
		Date     string `json:"date"`
		Outputs  string `json:"outputs"`   // "obs" | "nav" | "both"
		Preset   string `json:"preset"`    // see downloader.Presets()
		Start    string `json:"start"`     // RFC3339 UTC; preferred range API
		End      string `json:"end"`       // RFC3339 UTC, exclusive
		StartMin *int   `json:"start_min"` // UTC minutes from the day's midnight
		// EndMin may exceed 1440, running into the following day.
		EndMin *int `json:"end_min"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	out := downloader.ParseOutputs(body.Outputs)
	preset := downloader.ParsePreset(body.Preset)
	win := downloader.FullDay()
	if body.Start != "" || body.End != "" {
		start, startErr := time.Parse(time.RFC3339, body.Start)
		end, endErr := time.Parse(time.RFC3339, body.End)
		if startErr != nil || endErr != nil || start.Location() != time.UTC || end.Location() != time.UTC {
			writeErr(w, http.StatusBadRequest, "start and end must be RFC3339 UTC timestamps")
			return
		}
		start, end = start.UTC(), end.UTC()
		body.Date = start.Format("2006-01-02")
		midnight := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
		win.StartMin = int(start.Sub(midnight) / time.Minute)
		win.EndMin = int(end.Sub(midnight) / time.Minute)
	} else if body.StartMin != nil {
		win.StartMin = *body.StartMin
		if body.EndMin != nil {
			win.EndMin = *body.EndMin
		}
	}
	s.Log.Info("RINEX conversion requested", "date", body.Date,
		"outputs", out.String(), "window", win.Label(), "preset", preset.ID, "by", adminOf(r))
	// Start it as a background job and return immediately: a full day takes
	// around a minute, and the browser must not have to stay on the tab.
	owner := s.jobOwner(w, r)
	// A conversion is minutes of CPU on a Pi that is also running the caster.
	// An administrator may start as many as they like; anonymous callers share
	// a small budget, so a public page cannot be used to grind the station to
	// a halt.
	if !isAdmin(r) {
		if s.Downloader.RunningFor(owner) > 0 {
			writeErr(w, http.StatusTooManyRequests, "you already have a conversion running; wait for it to finish")
			return
		}
		if s.Downloader.RunningAnonymous() >= anonymousConversions {
			writeErr(w, http.StatusServiceUnavailable, "the station is already converting as much as it can; try again shortly")
			return
		}
		if !convertLimit.allow(realIP(r)) {
			w.Header().Set("Retry-After", "900")
			writeErr(w, http.StatusTooManyRequests, "conversion limit reached for now; try again later")
			return
		}
	}
	job, err := s.Downloader.StartJob(owner, body.Date, out, win, preset)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleDlJob(w http.ResponseWriter, r *http.Request) {
	if s.Downloader == nil {
		writeErr(w, http.StatusServiceUnavailable, "downloader not configured")
		return
	}
	j, ok := s.Downloader.JobStatus(r.PathValue("id"))
	// A job belongs to whoever asked for it. To anyone else it does not exist,
	// which is also what stops a visitor learning another's download token.
	if !ok || !s.ownsJob(w, r, j) {
		writeErr(w, http.StatusNotFound, "job not found or expired")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) handleDlCancel(w http.ResponseWriter, r *http.Request) {
	if s.Downloader == nil {
		writeErr(w, http.StatusServiceUnavailable, "downloader not configured")
		return
	}
	id := r.PathValue("id")
	j, ok := s.Downloader.JobStatus(id)
	if !ok || !s.ownsJob(w, r, j) {
		writeErr(w, http.StatusNotFound, "job not found or expired")
		return
	}
	if !s.Downloader.Cancel(id) {
		writeErr(w, http.StatusConflict, "job is not running")
		return
	}
	s.Log.Info("RINEX conversion cancelled", "job", id, "by", requesterOf(r))
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
}

func (s *Server) handleDlJobs(w http.ResponseWriter, r *http.Request) {
	if s.Downloader == nil {
		writeErr(w, http.StatusServiceUnavailable, "downloader not configured")
		return
	}
	// Administrators see every conversion; a visitor sees their own.
	owner := ""
	if !isAdmin(r) {
		owner = s.jobOwner(w, r)
	}
	writeJSON(w, http.StatusOK, s.Downloader.Jobs(owner))
}

// ownsJob reports whether this caller may see a conversion. Administrators
// may see all of them.
func (s *Server) ownsJob(w http.ResponseWriter, r *http.Request, j *downloader.Job) bool {
	if isAdmin(r) {
		return true
	}
	return j.Owner != "" && j.Owner == s.jobOwner(w, r)
}

// requesterOf names the caller for the log: an administrator by name, or the
// address an anonymous visitor came from.
func requesterOf(r *http.Request) string {
	if name := adminOf(r); name != "" {
		return name
	}
	return "visitor " + realIP(r)
}

func (s *Server) handleDlDownload(w http.ResponseWriter, r *http.Request) {
	if s.Downloader == nil {
		writeErr(w, http.StatusServiceUnavailable, "downloader not configured")
		return
	}
	path, name, ok := s.Downloader.Take(r.PathValue("token"))
	if !ok {
		writeErr(w, http.StatusNotFound, "download link expired or not found")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "download no longer available")
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	if fi != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	}
	io.Copy(w, f)
}

// AssetNames lists the embedded web assets. It exists so the dependency
// inventory can read each vendored work's version out of the filename it is
// served under, without this package having to carry a second copy of those
// version numbers for the inventory to disagree with.
func AssetNames() []string {
	entries, err := assetsFS.ReadDir("assets")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}
