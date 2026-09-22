package downloader

// RINEX presets for the online post-processing services.
//
// The parameters mirror RTKBase's tools/convbin.sh (© Stéphane Péneau,
// AGPL-3.0); see CREDITS.md. Each service publishes what it will accept, and
// getting it wrong means the upload is rejected after the file has been made:
// IGN wants RINEX 2.11 GPS-only at 30 s, NRCan wants 3.04 with GPS, GLONASS and
// Galileo at 30 s. The full presets keep every constellation this receiver
// tracks and differ only in the sampling interval.
//
// This receiver's firmware has no GLONASS, so the NRCan preset yields GPS and
// Galileo here. That is the service's constraint, not a missing feature.
type Preset struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Note string `json:"note"`
	// Version is the RINEX version convbin writes; empty means the station
	// default from the configuration.
	Version string `json:"version"`
	// Frequencies is convbin -f; zero means the station default.
	Frequencies int `json:"frequencies"`
	// Exclude lists convbin -y system letters (G R E J S C I).
	Exclude []string `json:"exclude"`
	// IntervalSec is convbin -ti; zero keeps every epoch.
	IntervalSec float64 `json:"interval_s"`
	// ToleranceSec is convbin -tt, the time tolerance when decimating.
	ToleranceSec float64 `json:"tolerance_s"`
}

var presets = []Preset{
	{ID: "station", Name: "Station default",
		Note: "Every epoch, every constellation, at the configured RINEX version. Largest files."},
	{ID: "ign", Name: "IGN (France)", Version: "2.11", Frequencies: 2,
		Exclude: []string{"R", "E", "J", "S", "C", "I"}, IntervalSec: 30, ToleranceSec: 0.005,
		Note: "RINEX 2.11, GPS only, L1/L2, 30 s. What the IGN service accepts."},
	{ID: "nrcan", Name: "NRCan PPP (Canada)", Version: "3.04", Frequencies: 2,
		Exclude: []string{"J", "S", "C", "I"}, IntervalSec: 30,
		Note: "RINEX 3.04, GPS + GLONASS + Galileo, L1/L2, 30 s. This receiver has no GLONASS."},
	{ID: "full30", Name: "Full, 30 s", Version: "3.04", IntervalSec: 30,
		Note: "RINEX 3.04, every constellation, decimated to 30 s. A good general submission."},
	{ID: "full1", Name: "Full, 1 s", Version: "3.04", IntervalSec: 1,
		Note: "RINEX 3.04, every constellation, one epoch per second."},
}

// Presets lists what the UI may offer, in the order it should offer them.
func Presets() []Preset { return append([]Preset(nil), presets...) }

// ParsePreset resolves an id, falling back to the station default so an unknown
// value produces a larger file rather than an error.
func ParsePreset(id string) Preset {
	for _, p := range presets {
		if p.ID == id {
			return p
		}
	}
	return presets[0]
}
