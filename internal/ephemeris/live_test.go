package ephemeris

import (
	"math"
	"os"
	"testing"
	"time"
)

// Ninety seconds of real RXM-SFRBX from the station's own receiver, GPS only.
// Satellite broadcast data is public, and a fixture of it is the only way to
// prove a bit-level decoder outside the sky.
//
// A wrong bit offset does not produce a slightly wrong orbit; it produces
// nonsense. These assertions are the physics: every GPS satellite is in a
// roughly circular 26,560 km orbit inclined about 55°, so the square root of
// the semi-major axis is near 5153.7 and eccentricity is a few thousandths.
// No misaligned field survives that across twelve satellites.
func TestDecodesLiveReceiverSubframes(t *testing.T) {
	raw, err := os.ReadFile("testdata/sfrbx-gps.ubx")
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil)
	frames := 0
	for i := 0; i+8 <= len(raw); {
		if raw[i] != 0xB5 || raw[i+1] != 0x62 {
			i++
			continue
		}
		n := int(raw[i+4]) | int(raw[i+5])<<8
		end := i + 6 + n + 2
		if end > len(raw) {
			break
		}
		s.consume(raw[i:end])
		frames++
		i = end
	}
	if frames < 100 {
		t.Fatalf("only %d frames in the fixture", frames)
	}
	if s.framesBad != 0 {
		t.Errorf("%d frames were malformed", s.framesBad)
	}

	const (
		p2_19 = 1.0 / (1 << 19)
		p2_33 = 1.0 / (1 << 33)
		p2_31 = 1.0 / (1 << 31)
	)
	checked := 0
	for sv, a := range s.gps {
		if !a.eph.complete(a.have) {
			continue
		}
		checked++
		e := &a.eph
		// √A in units of 2^-19: a GPS orbit is 26,560 km, so this is 5153.x.
		if sqrtA := float64(e.sqrtA) * p2_19; sqrtA < 5150 || sqrtA > 5160 {
			t.Errorf("sv %d: sqrt(A) = %.3f, not a GPS orbit", sv, sqrtA)
		}
		if ecc := float64(e.e) * p2_33; ecc < 0 || ecc > 0.03 {
			t.Errorf("sv %d: eccentricity = %.6f", sv, ecc)
		}
		// i0 is in semicircles; the constellation sits near 55 degrees.
		if inc := float64(e.i0) * p2_31 * 180; inc < 50 || inc > 60 {
			t.Errorf("sv %d: inclination = %.2f degrees", sv, inc)
		}
		// M0, Omega0 and omega are angles in semicircles: |value| <= 1.
		for name, v := range map[string]int64{"M0": e.m0, "Omega0": e.omega0, "omega": e.omega} {
			if a := math.Abs(float64(v) * p2_31); a > 1.0000001 {
				t.Errorf("sv %d: %s = %.6f semicircles, outside [-1,1]", sv, name, a)
			}
		}
		// toe and toc are multiples of 16 s within the week, and the two are
		// equal in a healthy broadcast data set.
		if e.toe*16 >= 604800 {
			t.Errorf("sv %d: toe = %d, outside the week", sv, e.toe*16)
		}
		if e.toe != e.toc {
			t.Logf("sv %d: toe %d and toc %d differ, which is legal but unusual", sv, e.toe, e.toc)
		}
		if e.ura > 15 || e.health != 0 {
			t.Logf("sv %d: URA index %d, health %d", sv, e.ura, e.health)
		}
		if e.iodc&0xFF != e.iode2 || e.iode2 != e.sv3 {
			t.Errorf("sv %d: issue-of-data mismatch %d/%d/%d", sv, e.iodc, e.iode2, e.sv3)
		}
	}
	if checked < 4 {
		t.Fatalf("only %d complete ephemerides decoded from the fixture", checked)
	}
	t.Logf("%d satellites decoded from %d live subframe frames", checked, frames)

	// The week number is shared by the whole constellation and must match the
	// capture date. GPS week 0 began 1980-01-06; the broadcast field is the
	// low ten bits.
	want := uint64((time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC).
		Sub(time.Date(1980, 1, 6, 0, 0, 0, 0, time.UTC)).Hours() / 24 / 7)) % 1024
	for sv, a := range s.gps {
		if a.eph.complete(a.have) && a.eph.week != want {
			t.Errorf("sv %d: week %d, want %d for the capture date", sv, a.eph.week, want)
		}
	}

	// Every complete set must produce a well-formed 1019.
	msgs := s.Messages(1019)
	if len(msgs) != checked {
		t.Errorf("%d messages for %d complete ephemerides", len(msgs), checked)
	}
	for _, m := range msgs {
		if len(m) != 67 || m[0] != 0xD3 {
			t.Errorf("frame of %d bytes starting %#02x", len(m), m[0])
		}
		if got := int(m[3])<<4 | int(m[4])>>4; got != 1019 {
			t.Errorf("frame carries type %d", got)
		}
	}
}

// The same fixture holds L2 CM and L5 frames. They arrive in the same shape as
// L1 C/A and carry CNAV, which shares nothing with LNAV's layout, so accepting
// them yields an ephemeris assembled from the wrong bits. This test feeds the
// decoder nothing but those frames.
func TestCNAVSubframesAreIgnored(t *testing.T) {
	raw, err := os.ReadFile("testdata/sfrbx-gps.ubx")
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil)
	fed := 0
	for i := 0; i+8 <= len(raw); {
		if raw[i] != 0xB5 || raw[i+1] != 0x62 {
			i++
			continue
		}
		n := int(raw[i+4]) | int(raw[i+5])<<8
		end := i + 6 + n + 2
		if end > len(raw) {
			break
		}
		if sigID := raw[i+8]; sigID != 0 { // L2 CM and L5 only
			s.consume(raw[i:end])
			fed++
		}
		i = end
	}
	if fed == 0 {
		t.Fatal("the fixture has no CNAV frames to test with")
	}
	if len(s.gps) != 0 {
		t.Errorf("%d satellites were built from CNAV frames", len(s.gps))
	}
	if got := s.Messages(1019); len(got) != 0 {
		t.Errorf("CNAV frames produced %d ephemeris messages", len(got))
	}
	t.Logf("%d CNAV frames correctly ignored", fed)
}
