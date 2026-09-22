package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/archive"
	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/store"
)

func accessFixture(t *testing.T) (*Server, *http.Cookie) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateAdmin("fixture-admin", "fixture-admin-password"); err != nil {
		t.Fatal(err)
	}
	tok, err := db.AdminLogin("fixture-admin", "fixture-admin-password", "127.0.0.1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return New(&Server{Store: db, Cfg: &config.Config{}}), &http.Cookie{Name: sessionCookie, Value: tok}
}

// The public surface is the three read-only pages and what they draw
// themselves from. Everything else must refuse an anonymous caller, because
// this UI is published through a tunnel: a route that answers here answers the
// internet.
func TestPublicSurfaceIsExactlyTheReadOnlyPages(t *testing.T) {
	srv, _ := accessFixture(t)
	public := []struct{ method, path string }{
		{"GET", "/api/status"}, {"GET", "/healthz"}, {"GET", "/api/me"},
		{"GET", "/api/live"}, {"GET", "/api/history"},
		{"GET", "/api/downloader/available"}, {"GET", "/api/downloader/jobs"},
		{"GET", "/api/downloader/job/whatever"}, {"POST", "/api/downloader/process"},
		{"POST", "/api/downloader/job/whatever/cancel"},
		{"GET", "/api/downloader/download/whatever"},
		{"GET", "/tiles/16/35000/22000"},
	}
	for _, c := range public {
		// This fixture has no collector or downloader behind it, so a public
		// handler may well fail once it is past the access check. Reaching the
		// handler at all is the assertion.
		code := func() (code int) {
			defer func() {
				if recover() != nil {
					code = http.StatusInternalServerError
				}
			}()
			rr := httptest.NewRecorder()
			srv.mux.ServeHTTP(rr, httptest.NewRequest(c.method, c.path, strings.NewReader("{}")))
			return rr.Code
		}()
		if code == http.StatusUnauthorized {
			t.Errorf("%s %s is meant to be public but demands a session", c.method, c.path)
		}
	}
	// Everything an administrator does, including reading configuration.
	private := []struct{ method, path string }{
		{"GET", "/api/settings"}, {"PUT", "/api/settings/general"},
		{"GET", "/api/users"}, {"POST", "/api/users"}, {"GET", "/api/users/someone"},
		{"DELETE", "/api/users/someone"}, {"GET", "/api/connections"},
		{"GET", "/api/mountpoints"}, {"PUT", "/api/settings/mountpoints"},
		{"GET", "/api/operations/health"}, {"POST", "/api/operations/diagnose"},
		{"POST", "/api/operations/restart"}, {"GET", "/api/operations/logs"},
		{"GET", "/api/logs"}, {"GET", "/api/logs/download"},
		{"POST", "/api/system/power"}, {"GET", "/api/receiver"},
		{"POST", "/api/receiver/apply"}, {"POST", "/api/receiver/commit"},
		{"GET", "/api/receiver/stage0"}, {"POST", "/api/receiver/stage0/restore"},
		{"GET", "/api/integrity"}, {"PUT", "/api/integrity/settings"},
		{"POST", "/api/integrity/run"}, {"POST", "/api/integrity/cancel"},
		{"GET", "/api/pushout"}, {"PUT", "/api/pushout"},
		{"GET", "/api/admins"}, {"POST", "/api/admins"}, {"DELETE", "/api/admins/someone"},
		{"POST", "/api/backup"}, {"POST", "/api/backup/inspect"}, {"POST", "/api/backup/restore"},
		{"POST", "/api/logout"},
	}
	for _, c := range private {
		rr := httptest.NewRecorder()
		srv.mux.ServeHTTP(rr, httptest.NewRequest(c.method, c.path, strings.NewReader("{}")))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s answered %d to an anonymous caller; it must be administrator-only",
				c.method, c.path, rr.Code)
		}
	}
}

// The page decides what to render from /api/me, so it has to answer a visitor
// rather than refuse them -- and it must not call a visitor an administrator.
func TestMeAnswersAnonymously(t *testing.T) {
	srv, cookie := accessFixture(t)
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/me", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"admin":false`) {
		t.Fatalf("anonymous /api/me = %d %s", rr.Code, rr.Body.String())
	}
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), `"admin":true`) {
		t.Fatalf("signed-in /api/me = %s", rr.Body.String())
	}
}

// Behind the tunnel every request arrives from cloudflared on loopback, so the
// forwarded address is the only useful one -- but only from a local peer.
// Anywhere else it is a string the caller chose, and believing it would let
// anyone spread a brute-force attempt across as many buckets as they like.
func TestForwardedAddressIsTrustedOnlyFromALocalPeer(t *testing.T) {
	cases := []struct{ peer, header, want string }{
		{"127.0.0.1:9000", "203.0.113.7", "203.0.113.7"},
		{"192.168.0.5:9000", "203.0.113.7", "203.0.113.7"},
		{"198.51.100.9:9000", "203.0.113.7", "198.51.100.9"},
		{"127.0.0.1:9000", "not-an-address", "127.0.0.1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/live", nil)
		r.RemoteAddr = c.peer
		r.Header.Set("CF-Connecting-IP", c.header)
		if got := realIP(r); got != c.want {
			t.Errorf("peer %s with header %q: realIP = %s, want %s", c.peer, c.header, got, c.want)
		}
	}
	// X-Forwarded-For names the client first and the proxies after it.
	r := httptest.NewRequest("GET", "/api/live", nil)
	r.RemoteAddr = "127.0.0.1:9000"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.1")
	if got := realIP(r); got != "203.0.113.9" {
		t.Errorf("realIP = %s, want the client entry", got)
	}
}

// A write driven from another site must not reach a handler at all, whatever
// cookies the browser attaches.
func TestCrossOriginWritesAreRefused(t *testing.T) {
	srv, _ := accessFixture(t)
	h := srv.throttle(srv.mux)
	write := func(origin, fetchSite string) int {
		r := httptest.NewRequest("POST", "/api/login", strings.NewReader("{}"))
		r.Host = "base.example.org"
		r.RemoteAddr = "203.0.113.55:4000" // its own rate-limit bucket
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if fetchSite != "" {
			r.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr.Code
	}
	if got := write("https://evil.example", "cross-site"); got != http.StatusForbidden {
		t.Errorf("a cross-origin write answered %d", got)
	}
	if got := write("https://base.example.org", "same-origin"); got == http.StatusForbidden {
		t.Error("the page's own write was refused")
	}
	if got := write("", ""); got == http.StatusForbidden {
		t.Error("a non-browser client with no Origin was refused")
	}
}

// Sign-in is the one place guessing pays, and it is now exposed. A wrong
// password must run out of attempts; a right one must not.
func TestSignInAttemptsRunOut(t *testing.T) {
	srv, _ := accessFixture(t)
	attempt := func(password string) int {
		r := httptest.NewRequest("POST", "/api/login",
			strings.NewReader(`{"Username":"fixture-admin","Password":"`+password+`"}`))
		r.RemoteAddr = "203.0.113.77:5000"
		rr := httptest.NewRecorder()
		srv.mux.ServeHTTP(rr, r)
		return rr.Code
	}
	seen := 0
	for i := 0; i < 12; i++ {
		if attempt("wrong-password") == http.StatusTooManyRequests {
			seen++
		}
	}
	if seen == 0 {
		t.Fatal("an attacker could guess without limit")
	}
	// The correct password refunds its attempt, so an operator who mistypes
	// once is never locked out of their own station.
	l := newLimiter(1.0/60.0, 5)
	l.allow("operator")
	l.refund("operator", 1)
	for i := 0; i < 5; i++ {
		if !l.allow("operator") {
			t.Fatalf("attempt %d was refused after a successful sign-in", i+1)
		}
	}
}

// The capture readouts stay public -- they are what the file download page is
// about -- but not the local and network paths they are written to.
func TestPublicCaptureReadoutsCarryNoPaths(t *testing.T) {
	in := []archive.Stats{{Name: "rtcm", File: "/var/spool/gnss/rtcm-2026.log",
		Dest: "/mnt/gnssraw/RAW", Bytes: 4096, StartSec: 100, SwapAtSec: 200, Failures: 3}}
	out := publicArchives(in)
	if len(out) != 1 {
		t.Fatalf("publicArchives dropped a row: %+v", out)
	}
	for _, hidden := range []string{"dest", "file", "failures", "synced", "last_sync"} {
		if _, ok := out[0][hidden]; ok {
			t.Errorf("%q is exposed to anonymous callers", hidden)
		}
	}
	if out[0]["bytes"] != int64(4096) || out[0]["name"] != "rtcm" {
		t.Errorf("the readout lost what the page draws: %+v", out[0])
	}
}

// The page has to agree with the server about what is public: an admin-only
// component rendered for a visitor would fill with 401s, and a sign-in form in
// front of the dashboard would defeat the point of publishing it.
func TestPageRendersThePublicViewWithoutASession(t *testing.T) {
	s := readAsset(t, "assets/app.js")
	for _, want := range []string{
		"const PUBLIC_TABS = { dashboard: 'Dashboard', history: 'Data History', files: 'File download' };",
		"const ADMIN_TABS = { users: 'Users', operations: 'Diagnostics' };",
		"${admin && shown === 'settings' ? html`<${Settings}/>` : null}",
		"${admin && shown === 'operations' ? html`<${Operations}/>` : null}",
		"onClick=${() => setSignin(true)}", // a visitor is offered sign-in, not sent to it
	} {
		if !strings.Contains(s, want) {
			t.Errorf("app.js is missing public-view element %q", want)
		}
	}
	// The old behaviour: no session meant the login form replaced everything.
	if strings.Contains(s, "if (!authed) return html`<${Login}") {
		t.Error("the sign-in form is a gate in front of the public pages again")
	}
	// The gear and the sign-out button belong to an administrator only.
	if !strings.Contains(s, "${admin ? html`<button class=${'icon-button settings-button '") {
		t.Error("the settings gear is offered to anonymous visitors")
	}
}
