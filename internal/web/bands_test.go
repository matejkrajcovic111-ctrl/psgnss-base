package web

import "testing"

func TestEveryUnicoreSignalHasALabel(t *testing.T) {
	cases := []struct{ gnss, sig uint8; want string }{
		{3, 32, "B1C"}, {3, 33, "B2b I"}, {0, 34, "L2P"}, {0, 32, "L1C P"}, {6, 33, "G3 Q"}, {1, 32, "L5 I"},
		{3, 5, "B3I D2"}, // the X20P meaning is untouched
	}
	for _, c := range cases {
		if got := signalBand(c.gnss, c.sig); got != c.want {
			t.Errorf("signalBand(%d,%d) = %q, want %q", c.gnss, c.sig, got, c.want)
		}
	}
}

func TestSignalsOnOneFrequencyCountAsOneCarrier(t *testing.T) {
	for _, g := range [][]string{{"L1 C/A", "E1 C", "B1C", "L1C P"}, {"L5 Q", "E5a Q", "B2a"}, {"E5b Q", "B2I D1", "B2b I"}} {
		for _, b := range g[1:] {
			if carrierOf(b) != carrierOf(g[0]) {
				t.Errorf("%s and %s should share a carrier", b, g[0])
			}
		}
	}
	if carrierOf("L1 OF") == carrierOf("L1 C/A") {
		t.Error("GLONASS G1 is not GPS L1")
	}
}

func TestCoverageIgnoresConstellationsNoMountpointCarries(t *testing.T) {
	sats := []liveSat{{System: "GPS", CNO: 40}, {System: "Galileo", CNO: 40},
		{System: "NavIC", CNO: 40}, {System: "QZSS", CNO: 40}, {System: "SBAS", CNO: 40}}
	sigs := []liveSignal{{System: "GPS", Band: "L1 C/A", CNO: 40}, {System: "GPS", Band: "L2 CL", CNO: 40},
		{System: "NavIC", Band: "L5 A", CNO: 40}}
	r := liveCoverage(sats, sigs)
	if r.Constellations != 2 || r.Bands != 2 {
		t.Fatalf("%+v", r)
	}
}
