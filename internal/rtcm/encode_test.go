package rtcm

import (
	"math"
	"testing"
)

// bitReader mirrors bitWriter, so tests decode what was encoded rather than
// asserting against a hand-copied byte string.
type bitReader struct {
	b []byte
	p int
}

func (r *bitReader) u(n int) uint64 {
	var v uint64
	for i := 0; i < n; i++ {
		v = v<<1 | uint64((r.b[r.p/8]>>(7-r.p%8))&1)
		r.p++
	}
	return v
}

func (r *bitReader) s(n int) int64 {
	v := r.u(n)
	if v&(1<<uint(n-1)) != 0 {
		return int64(v) - (1 << uint(n))
	}
	return int64(v)
}

func (r *bitReader) str() string {
	n := int(r.u(8))
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(r.u(8))
	}
	return string(out)
}

// unwrap validates the RTCM frame and returns the payload.
func unwrap(t *testing.T, f []byte) []byte {
	t.Helper()
	if f[0] != 0xD3 {
		t.Fatalf("bad sync byte %#x", f[0])
	}
	ln := int(f[1]&0x03)<<8 | int(f[2])
	if len(f) != 3+ln+3 {
		t.Fatalf("frame length %d, header says %d+6", len(f), ln)
	}
	got := uint32(f[3+ln])<<16 | uint32(f[4+ln])<<8 | uint32(f[5+ln])
	if want := crc24q(f[:3+ln]); want != got {
		t.Fatalf("CRC mismatch: got %06X want %06X", got, want)
	}
	return f[3 : 3+ln]
}

func testStation() Station {
	x, y, z := LLHToECEF(48.12345678, 17.98765432, 287.275)
	return Station{
		ID: 1, X: x, Y: y, Z: z, AntennaHeight: 0,
		AntennaDescriptor: "ADVNULLANTENNA", AntennaSetupID: 0,
		ReceiverType: "ZED-X20P", ReceiverFirmware: "HPG 2.00",
		GPS: true, Galileo: true,
	}
}

func TestEncode1006RoundTrip(t *testing.T) {
	s := testStation()
	f, err := s.Encode1006()
	if err != nil {
		t.Fatal(err)
	}
	p := unwrap(t, f)
	r := &bitReader{b: p}
	if mt := r.u(12); mt != 1006 {
		t.Fatalf("message type = %d, want 1006", mt)
	}
	if id := r.u(12); id != 1 {
		t.Errorf("station ID = %d, want 1", id)
	}
	r.u(6) // ITRF year
	gps, glo, gal := r.u(1), r.u(1), r.u(1)
	if gps != 1 || glo != 0 || gal != 1 {
		t.Errorf("constellation bits = %d/%d/%d, want 1/0/1", gps, glo, gal)
	}
	r.u(1) // reference-station indicator
	x := float64(r.s(38)) / 1e4
	r.u(1)
	r.u(1)
	y := float64(r.s(38)) / 1e4
	r.u(2)
	z := float64(r.s(38)) / 1e4
	h := float64(r.u(16)) / 1e4

	// Round-tripping must stay within the 0.1 mm encoding resolution.
	for _, c := range []struct {
		name     string
		got, exp float64
	}{{"X", x, s.X}, {"Y", y, s.Y}, {"Z", z, s.Z}} {
		if math.Abs(c.got-c.exp) > 1e-4 {
			t.Errorf("%s = %.4f, want %.4f (delta %.6f m)", c.name, c.got, c.exp, c.got-c.exp)
		}
	}
	if h != 0 {
		t.Errorf("antenna height = %v, want 0", h)
	}
}

// TestEncode1006MatchesConfiguredPosition is the assertion that matters
// operationally: the broadcast coordinate must be the surveyed one. If this
// drifts, every rover silently shifts.
func TestEncode1006MatchesConfiguredPosition(t *testing.T) {
	const lat, lon, ht = 48.12345678, 17.98765432, 287.275
	x, y, z := LLHToECEF(lat, lon, ht)
	s := Station{ID: 1, X: x, Y: y, Z: z, AntennaDescriptor: "ADVNULLANTENNA", GPS: true}
	f, _ := s.Encode1006()
	p := unwrap(t, f)
	r := &bitReader{b: p}
	r.u(12 + 12 + 6 + 1 + 1 + 1 + 1)
	gx := float64(r.s(38)) / 1e4
	r.u(1)
	r.u(1)
	gy := float64(r.s(38)) / 1e4
	r.u(2)
	gz := float64(r.s(38)) / 1e4

	// Convert back and compare in geodetic terms.
	blat, blon, bh := ecefToLLH(gx, gy, gz)
	if d := math.Abs(blat-lat) * 111320 * 1000; d > 1 {
		t.Errorf("latitude drifted %.3f mm", d)
	}
	if d := math.Abs(blon-lon) * 111320 * math.Cos(lat*math.Pi/180) * 1000; d > 1 {
		t.Errorf("longitude drifted %.3f mm", d)
	}
	if d := math.Abs(bh-ht) * 1000; d > 1 {
		t.Errorf("height drifted %.3f mm", d)
	}
}

func ecefToLLH(x, y, z float64) (lat, lon, h float64) {
	const a = 6378137.0
	const f = 1 / 298.257223563
	e2 := f * (2 - f)
	lon = math.Atan2(y, x)
	p := math.Hypot(x, y)
	lat = math.Atan2(z, p*(1-e2))
	for i := 0; i < 100; i++ {
		n := a / math.Sqrt(1-e2*math.Sin(lat)*math.Sin(lat))
		h = p/math.Cos(lat) - n
		lat = math.Atan2(z, p*(1-e2*n/(n+h)))
	}
	n := a / math.Sqrt(1-e2*math.Sin(lat)*math.Sin(lat))
	h = p/math.Cos(lat) - n
	return lat * 180 / math.Pi, lon * 180 / math.Pi, h
}

func TestEncode1008RoundTrip(t *testing.T) {
	s := testStation()
	s.AntennaSerial = "SN12345"
	f, err := s.Encode1008()
	if err != nil {
		t.Fatal(err)
	}
	r := &bitReader{b: unwrap(t, f)}
	if mt := r.u(12); mt != 1008 {
		t.Fatalf("message type = %d, want 1008", mt)
	}
	r.u(12)
	if d := r.str(); d != "ADVNULLANTENNA" {
		t.Errorf("antenna descriptor = %q, want ADVNULLANTENNA", d)
	}
	r.u(8)
	if sn := r.str(); sn != "SN12345" {
		t.Errorf("serial = %q, want SN12345", sn)
	}
}

func TestEncode1033RoundTrip(t *testing.T) {
	s := testStation()
	f, err := s.Encode1033()
	if err != nil {
		t.Fatal(err)
	}
	r := &bitReader{b: unwrap(t, f)}
	if mt := r.u(12); mt != 1033 {
		t.Fatalf("message type = %d, want 1033", mt)
	}
	r.u(12)
	if d := r.str(); d != "ADVNULLANTENNA" {
		t.Errorf("antenna descriptor = %q", d)
	}
	r.u(8)
	r.str() // antenna serial
	if rt := r.str(); rt != "ZED-X20P" {
		t.Errorf("receiver type = %q, want ZED-X20P", rt)
	}
	if fw := r.str(); fw != "HPG 2.00" {
		t.Errorf("receiver firmware = %q, want HPG 2.00", fw)
	}
}

func TestLLHToECEFKnownPoint(t *testing.T) {
	// Equator on the prime meridian at zero height: X must be the semi-major
	// axis, Y and Z zero.
	x, y, z := LLHToECEF(0, 0, 0)
	if math.Abs(x-6378137.0) > 1e-6 || math.Abs(y) > 1e-6 || math.Abs(z) > 1e-6 {
		t.Errorf("LLHToECEF(0,0,0) = %.6f,%.6f,%.6f want 6378137,0,0", x, y, z)
	}
	// North pole: Z must be the semi-minor axis.
	_, _, z = LLHToECEF(90, 0, 0)
	if want := 6356752.314245; math.Abs(z-want) > 1e-3 {
		t.Errorf("polar Z = %.6f, want %.6f", z, want)
	}
}

func TestOversizedDescriptorRejected(t *testing.T) {
	s := testStation()
	s.AntennaDescriptor = "THIS_DESCRIPTOR_IS_MUCH_TOO_LONG_FOR_RTCM"
	if _, err := s.Encode1008(); err == nil {
		t.Error("expected an error for a descriptor over 31 characters")
	}
}

// buildMSM makes a minimal MSM frame with a given type and DF393 value.
func buildMSM(t *testing.T, msgType int, multipleMessage bool) []byte {
	t.Helper()
	var w bitWriter
	w.put(uint64(msgType), 12) // DF002
	w.put(0, 12)               // DF003
	w.put(123456000, 30)       // epoch time
	if multipleMessage {
		w.put(1, 1) // DF393
	} else {
		w.put(0, 1)
	}
	w.put(0, 3+7+2+2+1+3) // remaining header fields
	w.put(0, 64)          // satellite mask
	w.put(0, 32)          // signal mask
	w.pad()
	f, err := wrap(w.buf)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func df393(f []byte) int {
	off := 12 + 12 + 30
	b := f[3+off/8]
	if b&(1<<(7-off%8)) != 0 {
		return 1
	}
	return 0
}

// TestClearMultipleMessageBit is the fix for correction age on a filtered
// mountpoint: the terminator must flip from 1 to 0 and the CRC must stay valid.
func TestClearMultipleMessageBit(t *testing.T) {
	f := buildMSM(t, 1124, true)
	if df393(f) != 1 {
		t.Fatal("fixture should start with DF393=1")
	}
	out, changed := ClearMultipleMessageBit(f)
	if !changed {
		t.Fatal("expected the bit to be cleared")
	}
	if df393(out) != 0 {
		t.Error("DF393 was not cleared")
	}
	// CRC must be repaired, or every rover drops the message.
	ln := int(out[1]&0x03)<<8 | int(out[2])
	want := crc24q(out[:3+ln])
	got := uint32(out[3+ln])<<16 | uint32(out[4+ln])<<8 | uint32(out[5+ln])
	if want != got {
		t.Errorf("CRC not repaired: got %06X want %06X", got, want)
	}
	// The input must not be mutated: the archives and raw outputs share it.
	if df393(f) != 1 {
		t.Error("input frame was modified in place")
	}
}

func TestClearMultipleMessageBitIdempotent(t *testing.T) {
	f := buildMSM(t, 1127, false)
	out, changed := ClearMultipleMessageBit(f)
	if changed {
		t.Error("an already-terminated message should not be rewritten")
	}
	if df393(out) != 0 {
		t.Error("DF393 should still be 0")
	}
}

func TestClearMultipleMessageBitIgnoresNonMSM(t *testing.T) {
	s := testStation()
	f, _ := s.Encode1006()
	if _, changed := ClearMultipleMessageBit(f); changed {
		t.Error("1006 is not an MSM message and must not be rewritten")
	}
}

func TestEpochTerminator(t *testing.T) {
	for _, c := range []struct {
		name  string
		types []int
		want  int
	}{
		{"MSM7 set", []int{1006, 1008, 1033, 1077, 1097, 1127}, 1127},
		{"MSM4 set", []int{1006, 1008, 1033, 1074, 1094, 1124}, 1124},
		{"GPS only", []int{1006, 1077}, 1077},
		{"no MSM", []int{1006, 1008, 1033}, 0},
	} {
		if got := EpochTerminator(c.types); got != c.want {
			t.Errorf("%s: EpochTerminator = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestIsMSM(t *testing.T) {
	for _, m := range []int{1074, 1077, 1094, 1097, 1124, 1127, 1084, 1087} {
		if !IsMSM(m) {
			t.Errorf("IsMSM(%d) = false, want true", m)
		}
	}
	for _, m := range []int{1005, 1006, 1008, 1033, 1230, 0} {
		if IsMSM(m) {
			t.Errorf("IsMSM(%d) = true, want false", m)
		}
	}
}
