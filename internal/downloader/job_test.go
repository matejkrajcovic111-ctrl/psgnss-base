package downloader

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestProgressIsMonotonic guards the bug seen in testing: convbin makes more
// passes over the file than expected, and a bar that jumps backwards (96% ->
// 50%) is worse than one that stalls.
func TestProgressIsMonotonic(t *testing.T) {
	start := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	end := start.Add(24*time.Hour - time.Second)
	p := newProgress(start, end)

	last := 0.0
	// Four passes over the same day, each walking the clock forward.
	for pass := 0; pass < 4; pass++ {
		for h := 0; h < 24; h += 2 {
			p.feed("scanning: " + start.Add(time.Duration(h)*time.Hour).
				Format("2006/01/02 15:04:05") + " GESC")
			f, _ := p.read()
			if f < last {
				t.Fatalf("progress went backwards: %.3f -> %.3f (pass %d, hour %d)",
					last, f, pass+1, h)
			}
			if f > 1 {
				t.Fatalf("progress exceeded 100%%: %.3f", f)
			}
			last = f
		}
	}
	if last >= 1.0 {
		t.Errorf("progress reached %.3f before the process exited; should stay below 1", last)
	}
	if last < 0.5 {
		t.Errorf("after four full passes progress is only %.3f; the bar barely moves", last)
	}
}

func TestKindFilesForMultiDayWindow(t *testing.T) {
	dir := t.TempDir()
	for _, day := range []string{"20260918", "20260919", "20260920", "20260921"} {
		if err := os.WriteFile(filepath.Join(dir, "Base1_"+day+"0000.ubx"), []byte(day), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d := New(Options{Sources: []Source{{Kind: "rtcm", Dir: dir, Prefix: "Base1_"}}})
	got := d.kindFilesForWindow("20260918", Window{StartMin: 23 * 60, EndMin: 3*1440 + 5*60}, "rtcm")
	if len(got) != 4 {
		t.Fatalf("multi-day source count = %d, want 4: %v", len(got), got)
	}
	// An exclusive end exactly at midnight must not pull in an unused day.
	got = d.kindFilesForWindow("20260918", Window{StartMin: 0, EndMin: 3 * 1440}, "rtcm")
	if len(got) != 3 {
		t.Fatalf("midnight-exclusive source count = %d, want 3: %v", len(got), got)
	}
}

func TestProgressStageChanges(t *testing.T) {
	start := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	p := newProgress(start, start.Add(24*time.Hour))
	p.feed("scanning: 2026/09/13 01:00:00 GESC")
	if _, stage := p.read(); stage != "scanning" {
		t.Errorf("first pass stage = %q, want scanning", stage)
	}
	p.feed("scanning: 2026/09/13 20:00:00 GESC")
	p.feed("scanning: 2026/09/13 00:10:00 GESC") // wrapped: new pass
	if _, stage := p.read(); stage != "converting" {
		t.Errorf("after wrap stage = %q, want converting", stage)
	}
}

func TestProgressIgnoresNonProgressLines(t *testing.T) {
	start := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	p := newProgress(start, start.Add(24*time.Hour))
	for _, l := range []string{"", "input file : /mnt/x", "->rinex obs"} {
		p.feed(l)
	}
	if f, _ := p.read(); f != 0 {
		t.Errorf("progress moved on non-progress output: %.3f", f)
	}
}

func TestParseOutputs(t *testing.T) {
	for in, want := range map[string]Outputs{
		"obs":  {Obs: true},
		"nav":  {Nav: true},
		"both": {Obs: true, Nav: true},
		"":     {Obs: true, Nav: true}, // default is both
		"junk": {Obs: true, Nav: true},
	} {
		if got := ParseOutputs(in); got != want {
			t.Errorf("ParseOutputs(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestOutputsString(t *testing.T) {
	for _, c := range []struct {
		o    Outputs
		want string
	}{
		{Outputs{Obs: true}, "obs"},
		{Outputs{Nav: true}, "nav"},
		{Outputs{Obs: true, Nav: true}, "both"},
	} {
		if got := c.o.String(); got != c.want {
			t.Errorf("%+v.String() = %q, want %q", c.o, got, c.want)
		}
	}
}

func TestWindowValidity(t *testing.T) {
	for _, c := range []struct {
		name string
		w    Window
		ok   bool
	}{
		{"whole day", Window{0, 1440}, true},
		{"morning", Window{540, 660}, true},
		{"crosses midnight", Window{1380, 1740}, true}, // 23:00 -> 05:00
		{"exactly 24h across midnight", Window{720, 2160}, true},
		{"end before start", Window{600, 540}, false},
		{"zero length", Window{600, 600}, false},
		{"longer than 24h", Window{0, 1500}, true},
		{"multiple days", Window{1380, 3*1440 + 300}, true},
		{"maximum", Window{0, 31 * 1440}, true},
		{"over maximum", Window{0, 31*1440 + 1}, false},
		{"start past midnight", Window{1440, 1500}, false},
	} {
		if got := c.w.Valid(); got != c.ok {
			t.Errorf("%s: Valid() = %v, want %v", c.name, got, c.ok)
		}
	}
}

func TestWindowCrossesMidnight(t *testing.T) {
	if !(Window{1380, 1740}).CrossesMidnight() {
		t.Error("23:00-05:00 should cross midnight")
	}
	if (Window{540, 660}).CrossesMidnight() {
		t.Error("09:00-11:00 should not cross midnight")
	}
}

func TestWindowLabel(t *testing.T) {
	for _, c := range []struct {
		w    Window
		want string
	}{
		{Window{540, 660}, "0900-1100"},
		{Window{1380, 1740}, "2300-0500+1"},
		{Window{1380, 3*1440 + 300}, "2300-0500+3"},
		{Window{0, 1440}, "0000-0000"},
	} {
		if got := c.w.Label(); got != c.want {
			t.Errorf("Label() = %q, want %q", got, c.want)
		}
	}
}
