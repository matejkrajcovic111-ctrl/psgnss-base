package integrity

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/hub"
	"github.com/psgnss/psgnss-base/internal/store"
)

func TestParseAndPreferFixedSolutions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.pos")
	b := "% header\n2026/09/18 03:00:00 48.123 17.456 201.2 2 18\n2026/09/18 03:00:01 48.124 17.457 201.3 1 20\n2026/09/18 03:00:02 48.124 17.457 201.3 1 20\n2026/09/18 03:00:03 48.124 17.457 201.3 1 20\n"
	if err := os.WriteFile(p, []byte(b), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := parseSolutions(p)
	if err != nil {
		t.Fatal(err)
	}
	selected, quality := selectSolutions(v)
	if quality != "fixed" || len(selected) != 3 {
		t.Fatalf("%s %d", quality, len(selected))
	}
}

func TestHorizontalDistance(t *testing.T) {
	if got := horizontalDistanceMM(48, 17, 48, 17); got != 0 {
		t.Fatalf("same point = %f", got)
	}
	if got := horizontalDistanceMM(48, 17, 48.000001, 17); got < 100 || got > 120 {
		t.Fatalf("one microdegree = %.1f mm", got)
	}
}

// The progress snapshot is what the web UI draws its bar and live figures
// from, so it has to track the solution file while rtkrcv is still writing it
// and report the same offsets the final result will record.
func TestProgressTracksAGrowingSolutionFile(t *testing.T) {
	cfg := &config.Config{}
	cfg.Station.Position.Latitude, cfg.Station.Position.Longitude, cfg.Station.Position.Height = 48.124, 17.457, 201.3
	m := New(nil, nil, cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.mu.Lock()
	m.running, m.prog = true, Progress{StartedAt: time.Now().Add(-90 * time.Second).Unix(), PlannedSec: 900, Phase: "connecting"}
	m.mu.Unlock()

	if p := m.Progress(); !p.Running || p.Phase != "connecting" || p.ElapsedSec < 89 || p.ElapsedSec > 95 {
		t.Fatalf("before any solution: %+v", p)
	}
	// Two float epochs are not enough to select anything: no offset may be shown.
	m.observe([]solution{{48.124, 17.457, 201.3, 2}, {48.124, 17.457, 201.3, 2}})
	p := m.Progress()
	if p.Phase != "observing" || p.Epochs != 2 || p.Float != 2 || p.Solution != "none" || p.HorizontalMM != nil {
		t.Fatalf("premature offset: %+v", p)
	}
	// Three fixed epochs are, and the offset is measured from the broadcast position.
	fixed := solution{48.124, 17.457, 201.5, 1}
	m.observe([]solution{{48.124, 17.457, 201.3, 2}, fixed, fixed, fixed})
	p = m.Progress()
	if p.Solution != "fixed" || p.Fixed != 3 || p.Epochs != 4 {
		t.Fatalf("selection: %+v", p)
	}
	if p.VerticalMM == nil || *p.VerticalMM < 199 || *p.VerticalMM > 201 {
		t.Fatalf("vertical offset of 0.2 m not reported as ~200 mm: %+v", p.VerticalMM)
	}

	// Elapsed time is not clamped to the planned window: an overrun is
	// information the operator should see.
	m.mu.Lock()
	m.prog.StartedAt = time.Now().Add(-20 * time.Minute).Unix()
	m.mu.Unlock()
	if p := m.Progress(); p.ElapsedSec <= p.PlannedSec {
		t.Fatalf("overrun hidden: %+v", p)
	}
	// A finished run stops claiming to be running.
	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
	if m.Progress().Running {
		t.Fatal("progress still reports a running check")
	}
}

// watch must survive the window before rtkrcv has created the file at all.
func TestWatchToleratesAMissingSolutionFile(t *testing.T) {
	m := New(nil, nil, &config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() { time.Sleep(50 * time.Millisecond); close(done) }()
	m.watch(filepath.Join(t.TempDir(), "absent.pos"), done)
	if p := m.Progress(); p.Epochs != 0 {
		t.Fatalf("invented epochs: %+v", p)
	}
}

// A check holds one login on the reference network for its whole window, and
// many subscriptions allow only one connection at a time, so stopping it has to
// work while it runs -- not merely refuse politely.
func TestCancelIsRejectedWhenNothingIsRunning(t *testing.T) {
	m := New(nil, nil, &config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if m.Cancel() {
		t.Fatal("cancel claimed to stop a check that was never started")
	}
	// Running with no cancel function published yet (still preparing) is also
	// not cancellable, and must not panic.
	m.mu.Lock()
	m.running = true
	m.mu.Unlock()
	if m.Cancel() {
		t.Fatal("cancel claimed to stop a check that has no context yet")
	}
}

func TestCancelStopsTheObservationWindow(t *testing.T) {
	m := New(nil, nil, &config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.mu.Lock()
	m.running, m.cancel, m.prog = true, cancel, Progress{Phase: "observing"}
	m.mu.Unlock()

	if !m.Cancel() {
		t.Fatal("a running check refused to cancel")
	}
	if p := m.Progress(); p.Phase != "cancelling" {
		t.Errorf("phase = %q, want the UI to see it stopping", p.Phase)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the observation context was not cancelled")
	}
	if !m.wasCancelled() {
		t.Error("the run would be recorded as a failure rather than a cancellation")
	}
}

// The check ran for a full window and produced nothing because neither input
// stream carried navigation data: the station's RTCM is 1005 plus MSM
// observations, and this receiver family has no CFG-MSGOUT key for RTCM
// 1019/1020/1042/1046, so no configuration could have added it. rtkrcv is fed
// raw UBX instead, and these assertions are what stops that regressing
// silently.
func TestIntegrityFeedCarriesEphemerisAndObservations(t *testing.T) {
	f, err := integrityFilter()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cases := []struct {
		name string
		fr   hub.Frame
		want bool
	}{
		{"RXM-SFRBX ephemeris", hub.Frame{Proto: hub.ProtoUBX, Type: 0x0213}, true},
		{"RXM-RAWX observations", hub.Frame{Proto: hub.ProtoUBX, Type: 0x0215}, true},
		{"NAV-PVT", hub.Frame{Proto: hub.ProtoUBX, Type: 0x0107}, false},
		{"RTCM MSM7", hub.Frame{Proto: hub.ProtoRTCM3, Type: 1077}, false},
		{"NMEA", hub.Frame{Proto: hub.ProtoNMEA}, false},
	}
	for _, tc := range cases {
		if got := f.Pass(tc.fr, now); got != tc.want {
			t.Errorf("%s: passed=%v, want %v", tc.name, got, tc.want)
		}
	}
	// Neither message may be decimated: RTKLIB needs every subframe and every
	// epoch.
	for _, typ := range []int{0x0213, 0x0215} {
		if !f.Pass(hub.Frame{Proto: hub.ProtoUBX, Type: typ}, now.Add(time.Millisecond)) {
			t.Errorf("type %#04x was decimated", typ)
		}
	}
}

func TestRTKConfigDeclaresTheStreamFormats(t *testing.T) {
	m := New(nil, nil, &config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	conf := m.rtkConfig(store.IntegritySettings{Host: "caster.example.org:2101",
		Mountpoint: "REF_MSM7", Username: "operator"}, "unused", "127.0.0.1:1", "/tmp/out.pos")
	if !strings.Contains(conf, "inpstr1-format =ubx") {
		t.Error("the station feed is not declared as UBX, so rtkrcv gets no ephemeris")
	}
	if !strings.Contains(conf, "inpstr2-format =rtcm3") {
		t.Error("the reference stream is not declared as RTCM3")
	}
}

// The first completed run was float-only with an AR ratio of 0.0 on every epoch
// and reported a 204 mm vertical offset. Both came from the processing options,
// so they are pinned here: the ionosphere-free combination has non-integer
// ambiguities and cannot be fixed, and an estimated zenith tropospheric delay
// absorbs height on a short baseline.
func TestRTKConfigSuitsAShortBaseline(t *testing.T) {
	m := New(nil, nil, &config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	conf := m.rtkConfig(store.IntegritySettings{Host: "caster.example.org:2101",
		Mountpoint: "REF_MSM7", Username: "operator"}, "unused", "127.0.0.1:1", "/tmp/out.pos")
	for _, want := range []string{"pos1-ionoopt =off", "pos1-tropopt =saas", "pos2-armode =continuous"} {
		if !strings.Contains(conf, want) {
			t.Errorf("configuration is missing %q", want)
		}
	}
	// A network caster puts its virtual station where the GGA says the receiver
	// is, including its height, and the latlon form cannot carry one.
	if !strings.Contains(conf, "inpstr2-nmeareq =single") {
		t.Error("the reference network is not told this antenna's height")
	}
	for _, unwanted := range []string{"dual-freq", "est-ztd", "nmealat"} {
		if strings.Contains(conf, unwanted) {
			t.Errorf("%q is back; it prevents an ambiguity fix or absorbs height", unwanted)
		}
	}
}

// Static mode accumulates, so the measurement belongs to the end of the window,
// not to the convergence at the start.
func TestSelectionMeasuresFromTheConvergedTail(t *testing.T) {
	var all []solution
	for i := 0; i < 100; i++ {
		// The first half is a converging float run, the second half sits on the
		// true position.
		if i < 50 {
			all = append(all, solution{48.0 + float64(50-i)*1e-6, 17.0, 200 + float64(50-i)*0.02, 2})
			continue
		}
		all = append(all, solution{48.0, 17.0, 200, 2})
	}
	chosen, quality := selectSolutions(all)
	if quality != "float" {
		t.Fatalf("quality = %q", quality)
	}
	if len(chosen) != 25 {
		t.Errorf("selected %d epochs, want the last quarter", len(chosen))
	}
	_, _, h := medianPosition(chosen)
	if h != 200 {
		t.Errorf("median height %.3f includes the convergence transient", h)
	}
	// A run that only just clears the threshold keeps every epoch it has.
	short := []solution{{48, 17, 200, 1}, {48, 17, 200, 1}, {48, 17, 200, 1}}
	if got, q := selectSolutions(short); q != "fixed" || len(got) != 3 {
		t.Errorf("minimum fixed run: %d epochs, quality %q", len(got), q)
	}
}
