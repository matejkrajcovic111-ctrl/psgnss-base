package telemetry

import (
	"encoding/binary"
	"testing"
)

// buildNavSat constructs a UBX-NAV-SAT payload for testing.
func buildNavSat(sats []Sat) []byte {
	p := make([]byte, 8+12*len(sats))
	binary.LittleEndian.PutUint32(p[0:4], 123456)
	p[4] = 1
	p[5] = byte(len(sats))
	for i, s := range sats {
		o := 8 + 12*i
		p[o], p[o+1], p[o+2] = s.GNSSID, s.SvID, s.CNO
		p[o+3] = byte(s.Elev)
		binary.LittleEndian.PutUint16(p[o+4:o+6], uint16(s.Azim))
		var flags uint32 = uint32(s.Health) << 4
		if s.Used {
			flags |= 1 << 3
		}
		binary.LittleEndian.PutUint32(p[o+8:o+12], flags)
	}
	return p
}

func TestParseNavSat(t *testing.T) {
	in := []Sat{
		{GNSSID: GPS, SvID: 14, CNO: 47, Elev: 62, Azim: 210, Used: true, Health: 1},
		{GNSSID: Galileo, SvID: 31, CNO: 41, Elev: 18, Azim: 95, Used: false, Health: 1},
		{GNSSID: BeiDou, SvID: 60, CNO: 0, Elev: -5, Azim: 300, Used: false},
	}
	got, err := ParseNavSat(buildNavSat(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(in) {
		t.Fatalf("got %d satellites, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("sat %d = %+v, want %+v", i, got[i], in[i])
		}
	}
}

func TestParseNavSatRejectsTruncated(t *testing.T) {
	p := buildNavSat([]Sat{{GNSSID: GPS, SvID: 1}})
	if _, err := ParseNavSat(p[:10]); err == nil {
		t.Error("expected an error for a truncated payload")
	}
	if _, err := ParseNavSat(p[:4]); err == nil {
		t.Error("expected an error for a payload shorter than the header")
	}
}

func TestParseNavSig(t *testing.T) {
	p := make([]byte, 8+32)
	p[5] = 2
	p[8], p[9], p[10], p[14] = GPS, 14, 0, 47 // GPS 14, L1 C/A
	binary.LittleEndian.PutUint16(p[18:20], 1<<3)
	p[24], p[25], p[26], p[30] = Galileo, 31, 7, 41 // Galileo 31, E6 C
	got, err := ParseNavSig(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].CNO != 47 || !got[0].Used || got[1].SigID != 7 || got[1].Used {
		t.Fatalf("ParseNavSig() = %+v", got)
	}
	if _, err := ParseNavSig(p[:20]); err == nil {
		t.Error("expected truncated NAV-SIG to fail")
	}
}

// TestPackUnpackRoundTrip guards the compact blob format. Getting this wrong
// would silently corrupt a week of history.
func TestPackUnpackRoundTrip(t *testing.T) {
	in := []Sat{
		{GNSSID: GPS, SvID: 14, CNO: 47, Elev: 62, Azim: 210, Used: true, Health: 1},
		{GNSSID: Galileo, SvID: 31, CNO: 41, Elev: 18, Azim: 94, Used: false, Health: 1},
		{GNSSID: BeiDou, SvID: 60, CNO: 33, Elev: -5, Azim: 300, Used: true, Health: 2},
	}
	out := Unpack(Pack(in))
	if len(out) != len(in) {
		t.Fatalf("round trip produced %d satellites, want %d", len(out), len(in))
	}
	for i := range in {
		a, b := in[i], out[i]
		if a.GNSSID != b.GNSSID || a.SvID != b.SvID || a.CNO != b.CNO ||
			a.Elev != b.Elev || a.Used != b.Used || a.Health != b.Health {
			t.Errorf("sat %d: got %+v want %+v", i, b, a)
		}
		// Azimuth is stored at 2-degree resolution.
		if d := int(a.Azim) - int(b.Azim); d < -2 || d > 2 {
			t.Errorf("sat %d azimuth %d -> %d, drift beyond 2 degrees", i, a.Azim, b.Azim)
		}
	}
}

func TestPackIsCompact(t *testing.T) {
	sats := make([]Sat, 30)
	if n := len(Pack(sats)); n != 180 {
		t.Errorf("30 satellites pack to %d bytes, want 180 (6 per satellite)", n)
	}
}

func TestPackSignalsRoundTrip(t *testing.T) {
	in := []Signal{{GNSSID: GPS, SvID: 14, SigID: 0, CNO: 47, Used: true}, {GNSSID: Galileo, SvID: 31, SigID: 7, CNO: 41}}
	b := PackSignals(in)
	if len(b) != 10 {
		t.Fatalf("signal blob is %d bytes, want 10", len(b))
	}
	out := UnpackSignals(b)
	if len(out) != len(in) {
		t.Fatalf("got %d signals", len(out))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("signal %d = %+v, want %+v", i, out[i], in[i])
		}
	}
}

func TestParseNavPVT(t *testing.T) {
	p := make([]byte, 92)
	p[20] = 5 // fixType: time only
	p[23] = 24
	binary.LittleEndian.PutUint32(p[40:44], 8) // hAcc 8 mm
	binary.LittleEndian.PutUint32(p[44:48], 6) // vAcc 6 mm
	got, err := ParseNavPVT(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.FixType != 5 || got.NumSV != 24 {
		t.Errorf("got fix=%d numSV=%d, want 5/24", got.FixType, got.NumSV)
	}
	if got.HAcc != 0.008 || got.VAcc != 0.006 {
		t.Errorf("got hAcc=%v vAcc=%v, want 0.008/0.006 m", got.HAcc, got.VAcc)
	}
}

func TestFixNameCoversBaseCase(t *testing.T) {
	// A fixed base reports fix type 5; the dashboard must not call that "no fix".
	if got := FixName(5); got != "time only (fixed base)" {
		t.Errorf("FixName(5) = %q", got)
	}
}

func TestGNSSName(t *testing.T) {
	for id, want := range map[uint8]string{
		GPS: "GPS", Galileo: "Galileo", BeiDou: "BeiDou",
		SBAS: "SBAS", GLONASS: "GLONASS", QZSS: "QZSS", NavIC: "NavIC",
	} {
		if got := GNSSName(id); got != want {
			t.Errorf("GNSSName(%d) = %q, want %q", id, got, want)
		}
	}
}
