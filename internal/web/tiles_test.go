package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/psgnss/psgnss-base/internal/config"
)

func TestTilePathRejectsProbes(t *testing.T) {
	if _, _, _, err := parseTilePath("16", "35000", "22000"); err != nil {
		t.Fatalf("a legitimate tile was rejected: %v", err)
	}
	bad := [][3]string{
		{"16", "..", "1"},            // traversal
		{"16", "1", "../../etc/pwd"}, // traversal in the y segment
		{"16", "-1", "1"},            // negative
		{"25", "1", "1"},             // above max zoom
		{"2", "9", "1"},              // outside the pyramid for this zoom
		{"2", "1", "9"},
		{"x", "1", "1"},
	}
	for _, c := range bad {
		if _, _, _, err := parseTilePath(c[0], c[1], c[2]); err == nil {
			t.Errorf("%v was accepted", c)
		}
	}
}

func TestTileURLFillsTemplate(t *testing.T) {
	got := tileURL("https://tile.example.org/{z}/{x}/{y}.png", 16, 35000, 22000)
	if got != "https://tile.example.org/16/35000/22000.png" {
		t.Errorf("url = %q", got)
	}
	if got := tileURL("https://{s}.tile.example.org/{z}/{x}/{y}.png", 1, 1, 1); !strings.HasPrefix(got, "https://c.tile") {
		t.Errorf("subdomain not filled: %q", got)
	}
}

// The whole point of proxying is the User-Agent and the caching, so both are
// asserted against a real upstream.
func TestTileProxyIdentifiesItselfAndCaches(t *testing.T) {
	var hits atomic.Int64
	var agent atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		agent.Store(r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNGDATA"))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Station.Name = "Example"
	cfg.Web.MapTiles = upstream.URL + "/{z}/{x}/{y}.png"
	cfg.Web.MapAttribution = "© Someone"
	cfg.Web.MapCacheDir = dir
	srv := New(&Server{Cfg: cfg})
	if srv.tiles == nil {
		t.Fatal("tile cache was not created for a configured map")
	}

	body, err := srv.tiles.get(context.Background(), cfg.Web.MapTiles, srv.tileAgent(), 16, 35000, 22000)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "PNGDATA" {
		t.Fatalf("body = %q", body)
	}
	ua, _ := agent.Load().(string)
	if !strings.HasPrefix(ua, "PSGNSS/") || !strings.Contains(ua, "Example") {
		t.Errorf("User-Agent %q does not identify the application and station", ua)
	}
	if _, err := os.Stat(filepath.Join(dir, "16", "35000", "22000.png")); err != nil {
		t.Errorf("tile was not cached: %v", err)
	}
	// A second request must not reach the provider again.
	if _, err := srv.tiles.get(context.Background(), cfg.Web.MapTiles, srv.tileAgent(), 16, 35000, 22000); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("provider was hit %d times; caching is what the usage policy asks for", got)
	}
}

// The tile route is public now, because the dashboard is: a visitor who cannot
// load tiles sees a grey square where the station should be. Without a
// configured map it still serves nothing at all.
func TestTileRouteNeedsAConfiguredMap(t *testing.T) {
	cfg := &config.Config{}
	srv := New(&Server{Cfg: cfg})
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/tiles/16/35000/22000", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("tiles answered %d with no map configured", rr.Code)
	}
	// And it is limited per caller, so it cannot be used as a tile proxy.
	if tileLimit.rate > 4 || tileLimit.burst > 120 {
		t.Errorf("the public tile budget is too generous: %v/s burst %v", tileLimit.rate, tileLimit.burst)
	}
}

func TestPageNeverReceivesTheUpstreamTileURL(t *testing.T) {
	if got := tileRoute(""); got != "" {
		t.Errorf("no map should mean no tile route, got %q", got)
	}
	got := tileRoute("https://tile.openstreetmap.org/{z}/{x}/{y}.png")
	if strings.Contains(got, "openstreetmap") || strings.HasPrefix(got, "http") {
		t.Errorf("the page was given the upstream URL: %q", got)
	}
}

func TestTileRateLimitFallsBackToStale(t *testing.T) {
	dir := t.TempDir()
	c := newTileCache(dir, 30)
	// Exhaust the bucket.
	for c.allow() {
	}
	// With no cached copy the request fails rather than queueing upstream work.
	if _, err := c.get(context.Background(), "https://example.invalid/{z}/{x}/{y}.png", "test", 1, 0, 0); err == nil {
		t.Error("rate limit did not stop the fetch")
	}
	// With a stale copy on disk, a stale tile is better than a blank map.
	p := c.path(1, 0, 0)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.maxAge = 0 // everything is stale
	got, err := c.get(context.Background(), "https://example.invalid/{z}/{x}/{y}.png", "test", 1, 0, 0)
	if err != nil || string(got) != "OLD" {
		t.Errorf("stale tile not served: %q %v", got, err)
	}
}
