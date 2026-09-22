package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/psgnss/psgnss-base/internal/version"
)

// Map tiles are fetched by the daemon, not by the browser.
//
// OpenStreetMap's tile usage policy requires a User-Agent that identifies the
// application and a contact, and asks that clients cache aggressively. A
// browser cannot be made to send either: it sends Chrome's User-Agent and its
// own Referer, which is exactly the anonymous traffic the policy exists to stop
// --- and OSM answers it with "Access blocked".
//
// Proxying through psgnssd fixes the cause rather than the symptom. The daemon
// sends a real identifying User-Agent, caches every tile on disk so the same
// tile is fetched once rather than on every page load, rate-limits itself, and
// only ever fetches tiles a browser actually asked for --- no prefetching and
// no bulk downloading, both of which the policy forbids. As
// a side effect the page goes back to talking only to its own origin, so the
// Content-Security-Policy no longer needs to name a remote host at all.
//
// This still does not make heavy use of OSM's donated servers acceptable. For
// anything beyond one operator glancing at a base station, point map_tiles at a
// provider you pay or one you host.
const (
	tileFetchTimeout = 20 * time.Second
	tileMaxZoom      = 19
	// tileRate and tileBurst cap outbound requests. A map drag asks for a few
	// dozen tiles at once, which the burst covers; sustained scraping is what
	// the rate stops.
	tileRate  = 4.0
	tileBurst = 40
)

type tileCache struct {
	dir    string
	maxAge time.Duration

	mu      sync.Mutex
	tokens  float64
	lastRef time.Time
	// inflight collapses concurrent requests for the same tile into one
	// upstream fetch, so a page load never multiplies into duplicate traffic.
	inflight map[string]*sync.WaitGroup
}

func newTileCache(dir string, maxAgeDays int) *tileCache {
	if maxAgeDays <= 0 {
		maxAgeDays = 30
	}
	return &tileCache{dir: dir, maxAge: time.Duration(maxAgeDays) * 24 * time.Hour,
		tokens: tileBurst, lastRef: time.Now(), inflight: map[string]*sync.WaitGroup{}}
}

// allow implements a token bucket over outbound fetches.
func (c *tileCache) allow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.tokens += now.Sub(c.lastRef).Seconds() * tileRate
	if c.tokens > tileBurst {
		c.tokens = tileBurst
	}
	c.lastRef = now
	if c.tokens < 1 {
		return false
	}
	c.tokens--
	return true
}

// handleTile serves one map tile.
//
// The route is public, because the dashboard is: a visitor who cannot load
// tiles sees a grey square where the station is. What keeps it from becoming
// someone else's tile proxy is that it serves only a configured template,
// caches everything, and is limited twice over -- per caller here, and
// globally in the cache. Point map_tiles at a provider you pay before
// publishing this to an audience.
func (s *Server) handleTile(w http.ResponseWriter, r *http.Request) {
	if s.Cfg == nil || s.Cfg.Web.MapTiles == "" {
		writeErr(w, http.StatusNotFound, "no map tile source is configured")
		return
	}
	// The map is public now, so this route is reachable by anyone with the
	// address. Per-caller limiting, on top of the cache's own global limit,
	// keeps it a station map rather than a tile proxy for the internet: a map
	// view is about thirty tiles and a drag a few more, which this allows
	// comfortably, while sustained scraping does not.
	if !isAdmin(r) && !tileLimit.allow(realIP(r)) {
		w.Header().Set("Retry-After", "10")
		writeErr(w, http.StatusTooManyRequests, "too many tile requests")
		return
	}
	z, x, y, err := parseTilePath(r.PathValue("z"), r.PathValue("x"), r.PathValue("y"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.tiles == nil {
		writeErr(w, http.StatusServiceUnavailable, "tile cache is not ready")
		return
	}
	body, err := s.tiles.get(r.Context(), s.Cfg.Web.MapTiles, s.tileAgent(), z, x, y)
	if err != nil {
		s.Log.Debug("tile fetch failed", "z", z, "x", x, "y", y, "err", err)
		// A missing tile is a grey square, not a broken page.
		writeErr(w, http.StatusBadGateway, "tile unavailable: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=604800")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body)
}

// tileAgent identifies this station to the tile provider, as the policy
// requires. The contact is whatever the operator configured; without one the
// station name still makes the traffic traceable to a person who can be asked
// to stop.
func (s *Server) tileAgent() string {
	agent := "PSGNSS/" + version.Version + " (base station map"
	if c := strings.TrimSpace(s.Cfg.Web.MapContact); c != "" {
		agent += "; " + c
	} else if n := strings.TrimSpace(s.Cfg.Station.Name); n != "" {
		agent += "; station " + n
	}
	return agent + ")"
}

func parseTilePath(zs, xs, ys string) (z, x, y int, err error) {
	ys = strings.TrimSuffix(ys, ".png")
	for _, p := range []struct {
		s   string
		out *int
	}{{zs, &z}, {xs, &x}, {ys, &y}} {
		v, e := strconv.Atoi(p.s)
		if e != nil || v < 0 {
			return 0, 0, 0, errors.New("tile coordinates must be non-negative integers")
		}
		*p.out = v
	}
	if z > tileMaxZoom {
		return 0, 0, 0, fmt.Errorf("zoom %d is above the maximum of %d", z, tileMaxZoom)
	}
	// x and y must be inside the pyramid for this zoom; anything else is a
	// probe, and turning it into a path would be the traversal bug.
	if limit := 1 << uint(z); x >= limit || y >= limit {
		return 0, 0, 0, fmt.Errorf("tile %d/%d is outside zoom %d", x, y, z)
	}
	return z, x, y, nil
}

func (c *tileCache) path(z, x, y int) string {
	return filepath.Join(c.dir, strconv.Itoa(z), strconv.Itoa(x), strconv.Itoa(y)+".png")
}

// get returns a tile from disk, fetching it upstream only when it is missing or
// older than the cache age.
func (c *tileCache) get(ctx context.Context, template, agent string, z, x, y int) ([]byte, error) {
	p := c.path(z, x, y)
	if b, ok := c.fresh(p); ok {
		return b, nil
	}
	key := p
	c.mu.Lock()
	wg, busy := c.inflight[key]
	if busy {
		c.mu.Unlock()
		wg.Wait()
		if b, ok := c.fresh(p); ok {
			return b, nil
		}
		return nil, errors.New("concurrent fetch for this tile failed")
	}
	wg = &sync.WaitGroup{}
	wg.Add(1)
	c.inflight[key] = wg
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.inflight, key)
		c.mu.Unlock()
		wg.Done()
	}()

	if !c.allow() {
		// Serve a stale tile rather than nothing: an old basemap is correct
		// enough, and this is the case the rate limit exists to create.
		if b, err := os.ReadFile(p); err == nil {
			return b, nil
		}
		return nil, errors.New("tile request rate limit reached")
	}

	body, err := fetchTile(ctx, template, agent, z, x, y)
	if err != nil {
		if b, readErr := os.ReadFile(p); readErr == nil {
			return b, nil // stale beats blank
		}
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
		tmp := p + ".tmp"
		if os.WriteFile(tmp, body, 0o644) == nil {
			_ = os.Rename(tmp, p)
		}
	}
	return body, nil
}

func (c *tileCache) fresh(p string) ([]byte, bool) {
	st, err := os.Stat(p)
	if err != nil || time.Since(st.ModTime()) > c.maxAge {
		return nil, false
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}

// tileURL fills a tile template. {s} rotates over a/b/c the way Leaflet would.
func tileURL(template string, z, x, y int) string {
	sub := string("abc"[(x+y)%3])
	r := strings.NewReplacer("{z}", strconv.Itoa(z), "{x}", strconv.Itoa(x),
		"{y}", strconv.Itoa(y), "{s}", sub, "{r}", "")
	return r.Replace(template)
}

var tileClient = &http.Client{Timeout: tileFetchTimeout}

func fetchTile(ctx context.Context, template, agent string, z, x, y int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tileURL(template, z, x, y), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", agent)
	req.Header.Set("Accept", "image/png,image/*;q=0.8")
	resp, err := tileClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tile provider answered %s", resp.Status)
	}
	// A tile is a few kilobytes; anything far larger is not a tile.
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
