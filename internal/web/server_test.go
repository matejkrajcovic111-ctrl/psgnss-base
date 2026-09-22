package web

import (
	"compress/gzip"
	"github.com/psgnss/psgnss-base/internal/config"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestApplicationShellIsNotCached(t *testing.T) {
	s := &Server{}
	for _, path := range []string{"/", "/index.html", "/app.js", "/app.css"} {
		rr := httptest.NewRecorder()
		s.securityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rr.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Errorf("%s Cache-Control=%q", path, got)
		}
	}
}

// Tiles are proxied by the daemon, so a configured map must not widen the page
// policy at all. This asserted the opposite until the tile proxy landed: the
// browser fetched tiles directly and OpenStreetMap blocked it for being
// anonymous, which no CSP entry could have fixed.
func TestTilePolicyStaysClosedWithAMapConfigured(t *testing.T) {
	get := func(s *Server) string {
		rr := httptest.NewRecorder()
		s.securityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
			ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		return rr.Header().Get("Content-Security-Policy")
	}
	const closed = "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; " +
		"frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'"
	withMap := &config.Config{}
	withMap.Web.MapTiles = "https://tile.openstreetmap.org/{z}/{x}/{y}.png"
	for name, srv := range map[string]*Server{
		"no configuration": {},
		"no map":           {Cfg: &config.Config{}},
		"map configured":   {Cfg: withMap},
	} {
		if got := get(srv); got != closed {
			t.Errorf("%s: policy = %q, want the closed policy", name, got)
		}
	}
}

func TestMapConfigurationIsValidated(t *testing.T) {
	base := func() *config.Config {
		c := &config.Config{}
		c.Web.MapAttribution = "© OpenStreetMap contributors"
		return c
	}
	cases := map[string]string{
		"http://tile.example.org/{z}/{x}/{y}.png": "https",
		"https://tile.example.org/tiles.png":      "placeholders",
		"not a url":                               "tile URL template",
	}
	for tiles, want := range cases {
		c := base()
		c.Web.MapTiles = tiles
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v does not mention %q", tiles, err, want)
		}
	}
	// A tile source with no attribution is refused: tile providers require credit.
	c := &config.Config{}
	c.Web.MapTiles = "https://tile.openstreetmap.org/{z}/{x}/{y}.png"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "map_attribution") {
		t.Errorf("missing attribution was accepted: %v", err)
	}
}

func TestStaticCompressionAndVendorCaching(t *testing.T) {
	payload := strings.Repeat("large javascript payload;", 1000)
	h := staticCompression(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = io.WriteString(w, payload)
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/uplot-1.6.32.min.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rr, req)
	if got := rr.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding=%q", got)
	}
	zr, err := gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	_ = zr.Close()
	if string(got) != payload {
		t.Fatal("compressed response did not round-trip")
	}

	s := &Server{}
	rr = httptest.NewRecorder()
	s.securityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rr, req)
	if got := rr.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("versioned chart library Cache-Control=%q", got)
	}
}

func TestChartEngineIsOffTheCriticalRenderPath(t *testing.T) {
	index, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(index), `src="uplot-1.6.32.min.js"`) {
		t.Fatal("index still blocks app.js on the chart library")
	}
	app, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"function loadUplot()", "script.async=true", "const ready=useUplot()"} {
		if !strings.Contains(string(app), want) {
			t.Errorf("lazy chart loader missing %q", want)
		}
	}
}

// Corners used to be square by decree, with a test that failed the build on
// any radius at all. The UI now sits on Tabler, where a rounded surface is
// part of the language, so the rule became a different one: a radius may only
// come from the three tokens, or be the 50% that makes an indicator a circle.
// A stray 3px somewhere is still the thing worth catching.
func TestSurfaceRadiiComeFromTokens(t *testing.T) {
	s := readAsset(t, "assets/app.css")
	if !strings.Contains(s, ".dot,.instrument-state i,.diagnostic-test-icon,.info-button{border-radius:50%}") {
		t.Fatal("semantic state indicators no longer retain their circular shape")
	}
	allowed := map[string]bool{
		"50%": true, "var(--r-control)": true, "var(--r-surface)": true, "var(--r-pill)": true,
	}
	for _, decl := range cssDeclarations(s, "border-radius") {
		if !allowed[decl] {
			t.Errorf("radius %q is not one of the --r-* tokens", decl)
		}
	}
}

func TestStylesheetUsesTokens(t *testing.T) {
	s := readAsset(t, "assets/app.css")
	body := s
	if i := strings.Index(s, "*{box-sizing:border-box}"); i > 0 {
		body = s[i:] // everything after the :root/theme blocks
	} else {
		t.Fatal("cannot find the end of the token block")
	}
	if m := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgba?\(`).FindString(body); m != "" {
		t.Errorf("colour literal %q outside the token block", m)
	}
	if m := regexp.MustCompile(`font-size:[0-9.]+px`).FindString(body); m != "" {
		t.Errorf("type step %q is not one of the --fs-* steps", m)
	}
	for _, prop := range []string{"padding", "margin", "gap"} {
		for _, decl := range cssDeclarations(body, prop) {
			for _, part := range strings.Fields(decl) {
				if !strings.HasSuffix(part, "px") || strings.HasPrefix(part, "-") {
					continue
				}
				if part != "2px" && part != "8px" { // a hairline nudge and one bleed
					t.Errorf("%s value %q is not a --s* spacing step", prop, part)
				}
			}
		}
	}
}

func readAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := assetsFS.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// cssDeclarations returns every value assigned to prop, ignoring longhand
// properties that merely start with the same word.
func cssDeclarations(css, prop string) []string {
	var out []string
	re := regexp.MustCompile(`(?:^|[;{\s])` + prop + `:([^;}!]+)`)
	for _, m := range re.FindAllStringSubmatch(css, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

func TestOperationsNumberFormatterIsBundled(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "fmtNum(") && !strings.Contains(s, "const fmtNum =") {
		t.Fatal("Operations uses fmtNum without defining it")
	}
}

func TestHistoryChartsDoNotRequireWebGL(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "scattergl") {
		t.Fatal("history charts must use the SVG scatter renderer so they work without WebGL")
	}
}

func TestDashboardInteractionDefaults(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"!hiddenSystems.includes(k)",
		"display_hidden_constellations",
		"<span>CEST ${fmtCESTTime(live.time)}</span>",
		"<span>UTC ${fmtUTCTime(live.time)}</span>",
		"GLONASS: { color:",
		"setInterval(()=>load(false),60000)",
		"/api/operations/diagnose",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("app.js is missing interaction default %q", want)
		}
	}
	if strings.Contains(s, ">LOCAL ${") {
		t.Error("dashboard clock still exposes an unwanted LOCAL time")
	}
	if strings.Contains(s, "fmtTime(") {
		t.Error("app.js calls removed fmtTime helper")
	}
	if !strings.Contains(s, "const {has_password,...payload}=form") {
		t.Error("external integrity settings save does not strip the read-only password-state field")
	}
	if strings.Contains(s, "Files close at 00:00 UTC") {
		t.Error("file download still claims current-day files are unavailable")
	}
}

func TestBrandRenderingAndHistoryTrailsRemoved(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// The satellites are generated from ORBIT_GROUPS now, so they are counted
	// there rather than in the markup.
	groups := s[strings.Index(s, "const ORBIT_GROUPS"):]
	groups = groups[:strings.Index(groups, "];")]
	if got := strings.Count(groups, "',"); got != 10 {
		t.Fatalf("brand has %d satellites, want 10", got)
	}
	if !strings.Contains(s, `r="1.35"`) || strings.Count(s, `r="1.35"`) != 1 {
		t.Fatal("brand satellite markers are not one uniform size")
	}
	// Satellites bunched because each had its own period; they now share one
	// and are spaced by phase. A second duration anywhere in the mark brings
	// the drift back.
	if strings.Count(s, "dur=") != 1 || !strings.Contains(s, "const ORBIT_PERIOD = 60;") {
		t.Error("brand orbits no longer share a single period")
	}
	for _, oldDuration := range []string{`dur="24s"`, `dur="31s"`, `dur="37s"`, `dur="41s"`, `dur="48s"`, `dur="52s"`} {
		if strings.Contains(s, oldDuration) {
			t.Errorf("brand still contains per-satellite orbit duration %s", oldDuration)
		}
	}
	if !strings.Contains(s, "animation.beginElementAt(-phase)") {
		t.Fatal("brand does not start dynamically inserted SMIL orbits at their phase")
	}
	if strings.Contains(s, "trailEpochs") || strings.Contains(s, ">Trails<") {
		t.Fatal("removed history trails are still present")
	}
}

func TestSignalBandProductionSignals(t *testing.T) {
	tests := []struct {
		gnss, sig uint8
		want      string
	}{
		{0, 0, "L1 C/A"}, {0, 3, "L2 CL"}, {0, 7, "L5 Q"},
		{2, 0, "E1 C"}, {2, 4, "E5a Q"}, {2, 8, "E6 B"},
		{3, 4, "B3I D1"}, {3, 5, "B3I D2"}, {3, 7, "B2a"},
		{5, 0, "L1 C/A"}, {5, 5, "L2 CL"}, {5, 9, "L5 Q"},
		{6, 0, "L1 OF"}, {6, 2, "L2 OF"},
		{7, 0, "L5 A"},
		{1, 0, "L1 C/A"}, {3, 99, "signal 99"},
	}
	for _, tt := range tests {
		if got := signalBand(tt.gnss, tt.sig); got != tt.want {
			t.Errorf("signalBand(%d, %d) = %q, want %q", tt.gnss, tt.sig, got, tt.want)
		}
	}
}

// The integrity monitor works against any NTRIP network covering the area. The
// original deployment's provider is not a requirement, so its name must not
// appear anywhere an operator reads.
func TestIntegrityUIDoesNotNameOneReferenceNetwork(t *testing.T) {
	for _, name := range []string{"assets/app.js", "assets/app.css", "assets/index.html"} {
		if strings.Contains(strings.ToUpper(readAsset(t, name)), "SKPOS") {
			t.Errorf("%s still names SKPOS", name)
		}
	}
}

// A check observes for a quarter of an hour by default. The UI has to show it
// progressing with figures from the run, not a spinner.
func TestIntegrityRunShowsLiveProgress(t *testing.T) {
	s := readAsset(t, "assets/app.js")
	for _, want := range []string{
		"data.progress",             // the snapshot the daemon publishes
		"elapsedPct",                // bar width from elapsed against planned
		"diag-meter-track",          // the project's existing meter, not a new one
		"prog.epochs",               // epochs solved so far
		"fmtMM(prog.horizontal_mm)", // the offsets as they settle
		"INTEGRITY_PHASES",          // which stage the run is in
		"setInterval(load,2000)",    // polled fast enough for the bar to move
	} {
		if !strings.Contains(s, want) {
			t.Errorf("integrity progress panel is missing %q", want)
		}
	}
}

// Data History renders the same instrument panels as the dashboard. The
// dashboard fills the panel beside the skyplot with the station map; History
// has no map, and left that space empty until this panel. Since the panels
// became arrangeable it is the tile grid, not a fixed row, that gives both of
// them the skyplot's height.
func TestHistorySkyplotHasASecondColumn(t *testing.T) {
	s := readAsset(t, "assets/app.js")
	if strings.Count(s, "<${GNSSPlots}") != strings.Count(s, "side=${html`") {
		t.Fatal("a GNSSPlots row renders without a second column, leaving space beside the skyplot empty")
	}
	if !strings.Contains(s, "<${EpochSummary} epoch=${epoch}") {
		t.Error("history does not pass the replayed epoch to the detail panel")
	}
	css := readAsset(t, "assets/app.css")
	if !strings.Contains(css, ".tile .plot-host,.tile .map-host,.tile .epoch-summary{height:var(--tile-h)}") {
		t.Error("the history panel is not sized with the skyplot by the panel grid")
	}
}

// The map card is a column flex container, and in one of those the flex basis
// is the main size -- height is ignored. `flex:1` is a basis of zero, so the
// height the panel grid sets went nowhere: Leaflet initialised, fetched its
// tiles and drew them into a container 0 pixels high, and the dashboard showed
// a heading and a coordinate readout with no map between them. Nothing in the
// console said so. A basis of auto is what makes the height apply.
func TestTheMapFillsItsPanel(t *testing.T) {
	css := readAsset(t, "assets/app.css")
	if !strings.Contains(css, ".map-host{width:100%;flex:1 1 auto;min-height:0;") {
		t.Error("the map host is back to a flex basis that discards the height the panel gives it")
	}
	if !strings.Contains(css, ".tile .plot-host,.tile .map-host,.tile .epoch-summary{height:var(--tile-h)}") {
		t.Error("nothing gives the map its height")
	}
}

// Both gestures end outside the panel that started them: pulling an edge takes
// the pointer past it, and a drag lifts the panel away. While either is running
// the handle has to stay visible and keep the pointer, or the gesture appears
// to come apart in the hand.
func TestPanelHandlesSurviveTheGesture(t *testing.T) {
	css := readAsset(t, "assets/app.css")
	if !strings.Contains(css, ".tile-handle:hover,.tile-handle.active{opacity:1}") {
		t.Error("a handle fades out from under the pointer once the gesture leaves its panel")
	}
	if !strings.Contains(css, ".tile.dragging .tile-grip{opacity:1}") {
		t.Error("the grip disappears the moment a panel is picked up")
	}
	if !strings.Contains(css, "touch-action:none") {
		t.Error("dragging a handle on a touch screen scrolls the page instead")
	}
	js := readAsset(t, "assets/app.js")
	for _, want := range []string{
		"handle.setPointerCapture(e.pointerId)",         // keep the pointer across the map and the charts
		"window.addEventListener('pointercancel',stop)", // and let go if the browser takes it away
		"if(!resizing)saveTiles(scope,layout)",          // storage once on release, not every pointer move
	} {
		if !strings.Contains(js, want) {
			t.Errorf("panel resize is missing %q", want)
		}
	}
}

// A panel can be pulled by any of its four edges or four corners. An edge
// moves one dimension and must leave the other alone -- the bug worth catching
// here is an edge that quietly resets the dimension it does not own.
func TestEveryPanelEdgeAndCornerResizes(t *testing.T) {
	js := readAsset(t, "assets/app.js")
	for _, d := range []string{"'n'", "'s'", "'e'", "'w'", "'nw'", "'ne'", "'sw'", "'se'"} {
		if !strings.Contains(js, "{d:"+d+",x:") {
			t.Errorf("no handle for %s", d)
		}
	}
	if !strings.Contains(js, "span:span===null?t.span:span,h:h===null?t.h:h") {
		t.Error("an edge no longer leaves the dimension it does not own untouched")
	}
	css := readAsset(t, "assets/app.css")
	for _, want := range []string{
		".tile-n,.tile-s{", ".tile-e,.tile-w{", ".tile-nw{", ".tile-ne{", ".tile-sw{", ".tile-se{",
		"cursor:ns-resize", "cursor:ew-resize", "cursor:nwse-resize", "cursor:nesw-resize",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("panel handle styling is missing %q", want)
		}
	}
}

// Dropping one panel on another exchanges the two, and each keeps the size it
// was given. Two things this guards: the exchange happens on the drop rather
// than while passing over -- swapping on every dragover trades places with
// every panel on the way to the one that was meant -- and it is done by id,
// since History renders no map and its layout is not what is on screen.
func TestPanelsChangePlacesOnDrop(t *testing.T) {
	js := readAsset(t, "assets/app.js")
	if !strings.Contains(js, "const n=l.slice(); n[ia]=l[ib]; n[ib]=l[ia]; return n;") {
		t.Error("panels no longer exchange whole entries, so a swap moves sizes about with them")
	}
	if !strings.Contains(js, "const ia=l.findIndex(t=>t.id===a), ib=l.findIndex(t=>t.id===b);") {
		t.Error("the swap is back to indexing a layout that may hold panels this page does not render")
	}
	if !strings.Contains(js, "if(from.current&&from.current!==l.id)swap(from.current,l.id);") {
		t.Error("the exchange is not on the drop")
	}
	if strings.Contains(js, "onDragOver=${e=>{e.preventDefault();\n          if(from.current") {
		t.Error("dragging over a panel rearranges it again")
	}
}

// Data History's satellites come from the telemetry payload, which has no
// per-signal map on a satellite and can name a constellation the plot palette
// does not (IMES, or a bare GNSS<id>). Both were unguarded when the epoch panel
// shipped: Object.values(undefined) threw and unmounted the page.
func TestEpochPanelSurvivesTheTelemetryPayloadShape(t *testing.T) {
	s := readAsset(t, "assets/app.js")
	if !strings.Contains(s, "Object.values(s.signals || {})") {
		t.Error("bestCNO assumes a per-signal map that history satellites do not carry")
	}
	if !strings.Contains(s, "const systemStyle = sys => PLOT_SYSTEMS[sys] ||") {
		t.Fatal("there is no fallback for a constellation the palette does not name")
	}
	// Any direct PLOT_SYSTEMS[...] index outside the plot itself is the bug
	// again: the plot filters to known systems first, the panels do not.
	panel := s[strings.Index(s, "function EpochSummary("):]
	panel = panel[:strings.Index(panel, "\nfunction ")] // just this component
	if i := strings.Index(panel, "PLOT_SYSTEMS["); i >= 0 {
		t.Errorf("EpochSummary indexes the palette directly: %q", panel[i:i+40])
	}
}

// Integrity moved out of Settings: it runs a verification and reports a result,
// which is what the Diagnostics page is, rather than configuration.
func TestIntegrityLivesOnTheDiagnosticsPage(t *testing.T) {
	s := readAsset(t, "assets/app.js")
	sections := s[strings.Index(s, "const SETTINGS_SECTIONS"):]
	sections = sections[:strings.Index(sections, "};")]
	if strings.Contains(sections, "integrity:") {
		t.Error("Settings still offers an integrity section")
	}
	ops := s[strings.Index(s, "function Operations()"):]
	ops = ops[:strings.Index(ops, "\nconst PAGE_HELP")]
	if !strings.Contains(ops, "<${IntegrityMonitor}/>") {
		t.Error("the Diagnostics page does not render the integrity monitor")
	}
	// The help text has to follow the component, or the page help describes a
	// panel that is not there.
	help := s[strings.Index(s, "const PAGE_HELP"):]
	opsHelp := help[strings.Index(help, "operations:["):]
	opsHelp = opsHelp[:strings.Index(opsHelp, "]],")]
	if !strings.Contains(opsHelp, "External integrity") {
		t.Error("Diagnostics page help does not mention integrity")
	}
}

// The dashboard used to print the antenna coordinates twice: once as a strip
// beside the constellation toggles and again under the map. The map keeps
// them; the strip is gone, and with it the widest full-precision readout on
// the page.
func TestDashboardShowsThePositionOnceAndDrawsTheWorkingRange(t *testing.T) {
	s := readAsset(t, "assets/app.js")
	if strings.Contains(s, "control-note") {
		t.Error("the duplicate coordinate readout is back on the dashboard")
	}
	for _, want := range []string{
		"coverage=${live.coverage}", // the range comes from the live stream
		"ring.current=L.circle(",    // and is drawn as a circle
		"fitted.current!==rangeKM",  // the view is fitted once per radius
		"Usable range ≈ ${rangeKM} km",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("app.js is missing working-range element %q", want)
		}
	}
	css := readAsset(t, "assets/app.css")
	if strings.Contains(css, ".control-note{") {
		t.Error("the removed coordinate strip still has a rule")
	}
}

// Connection history is a record of the same accounts, so it is a sub-page of
// Users rather than a sixth top-level tab.
func TestConnectionsLiveInsideUsers(t *testing.T) {
	s := readAsset(t, "assets/app.js")
	if strings.Contains(s, "connections: 'Connections'") {
		t.Error("Connections is a top-level tab again")
	}
	if !strings.Contains(s, "${admin && shown === 'users' ? html`<${UsersPage}/>` : null}") {
		t.Error("the Users tab no longer renders the combined page")
	}
	if !strings.Contains(s, "${view === 'accounts' ? html`<${Users}/>` : html`<${Connections}/>`}") {
		t.Error("the Users sub-nav does not reach both views")
	}
	if strings.Contains(s, "  connections:[") {
		t.Error("page help still has a section for a page that no longer exists")
	}
}

// Data History lists every constellation the palette knows, whether or not the
// replayed epoch saw it: a missing row cannot be told apart from a row of
// zeros. The receiver's own fix type is not shown at all -- the base is in
// static survey mode and always reports the same value.
func TestEpochPanelListsEveryConstellation(t *testing.T) {
	s := readAsset(t, "assets/app.js")
	if strings.Contains(s, "Receiver fix") || strings.Contains(s, "FIX_TYPES") {
		t.Error("the receiver fix readout is back in the epoch panel")
	}
	if !strings.Contains(s, "const known=Object.keys(PLOT_SYSTEMS);") {
		t.Error("the epoch constellation table no longer starts from the full palette")
	}
}

// The working range is computed from what is actually being tracked. SBAS is
// a correction service, not a ranging constellation, so counting it would
// widen the circle for no RTK benefit.
func TestLiveCoverageCountsRangingConstellationsOnly(t *testing.T) {
	sats := []liveSat{{System: "GPS", CNO: 44}, {System: "GPS", CNO: 40}, {System: "Galileo", CNO: 41},
		{System: "SBAS", CNO: 38}, {System: "BeiDou", CNO: 0}}
	signals := []liveSignal{{System: "GPS", Band: "L1", CNO: 44}, {System: "GPS", Band: "L2", CNO: 30},
		{System: "SBAS", Band: "L1", CNO: 38}, {System: "Galileo", Band: "E1", CNO: 0}}
	got := liveCoverage(sats, signals)
	if got.Constellations != 2 || got.Bands != 2 {
		t.Fatalf("capability = %d constellations on %d bands", got.Constellations, got.Bands)
	}
	if got.RangeKM <= 0 || got.Limit == "" {
		t.Fatalf("no usable range from a tracked dual-frequency stream: %+v", got)
	}
	if empty := liveCoverage(nil, nil); empty.RangeKM != 0 {
		t.Errorf("a range was drawn with nothing tracked: %+v", empty)
	}
}

// The backup panel has to state the trade-off it makes, because the operator is
// choosing the only thing that protects the file.
func TestBackupPanelStatesWhatTheFileHolds(t *testing.T) {
	s := readAsset(t, "assets/app.js")
	for _, want := range []string{
		"backup:'Backup and restore'",
		"const BACKUP_MIN_PASSPHRASE = 12;",
		"every NTRIP password in usable form",   // what is in the file
		"the station's master key is not in it", // and what is not
		"There is no way to recover a forgotten passphrase.",
		"Every administrator session ends, this one included",
		"typed!=='RESTORE'", // a restore is typed out, not clicked past
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the backup panel is missing %q", want)
		}
	}
	// The passphrase must be confirmed: a backup nobody can open is worse than
	// no backup.
	if !strings.Contains(s, "pass!==confirmPass") {
		t.Error("the backup passphrase is not confirmed")
	}
}

// The tab icon. A browser asks for it before anything else on the page and, on
// the public dashboard, asks for it without a session -- so it has to be
// served, and served anonymously, or every visitor gets the grey globe.
func TestTheTabIconIsServedAndAnonymous(t *testing.T) {
	index := readAsset(t, "assets/index.html")
	for _, want := range []string{
		`<link rel="icon" href="icon.svg" type="image/svg+xml">`,
		`<link rel="icon" href="favicon-32.png" sizes="32x32" type="image/png">`,
		`<link rel="apple-touch-icon" href="apple-touch-icon.png">`,
	} {
		if !strings.Contains(index, want) {
			t.Errorf("index.html does not link %s", want)
		}
	}
	// Each linked file exists and is what it claims to be.
	svg := readAsset(t, "assets/icon.svg")
	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "viewBox") {
		t.Error("icon.svg is not an SVG")
	}
	for _, name := range []string{"assets/favicon-32.png", "assets/apple-touch-icon.png"} {
		b, err := assetsFS.ReadFile(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(b) < 8 || string(b[1:4]) != "PNG" {
			t.Errorf("%s is not a PNG", name)
		}
	}
}
