package downloader

import "testing"

// A preset exists to match what a post-processing service will accept, so the
// parameters are the feature. These are the values RTKBase uses.
func TestPresetsMatchTheServices(t *testing.T) {
	byID := map[string]Preset{}
	for _, p := range Presets() {
		byID[p.ID] = p
	}
	ign := byID["ign"]
	if ign.Version != "2.11" || ign.Frequencies != 2 || ign.IntervalSec != 30 {
		t.Errorf("IGN preset = %+v", ign)
	}
	if len(ign.Exclude) != 6 { // everything except GPS
		t.Errorf("IGN keeps %d systems excluded, want GPS only", len(ign.Exclude))
	}
	nrcan := byID["nrcan"]
	if nrcan.Version != "3.04" || nrcan.IntervalSec != 30 {
		t.Errorf("NRCan preset = %+v", nrcan)
	}
	for _, sys := range nrcan.Exclude {
		if sys == "G" || sys == "R" || sys == "E" {
			t.Errorf("NRCan excludes %s, which the service wants", sys)
		}
	}
	if byID["full1"].IntervalSec != 1 || byID["full30"].IntervalSec != 30 {
		t.Error("the full presets do not differ in sampling interval alone")
	}
	// The station default must not impose anything: it is the previous
	// behaviour, and the configured version has to win.
	if d := byID["station"]; d.Version != "" || d.Frequencies != 0 || d.IntervalSec != 0 || len(d.Exclude) != 0 {
		t.Errorf("station default overrides configuration: %+v", d)
	}
	// An unknown id must not silently drop constellations or decimate.
	if ParsePreset("nonsense").ID != "station" {
		t.Error("an unknown preset does not fall back to the station default")
	}
}
