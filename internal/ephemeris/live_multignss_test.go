package ephemeris

import (
	"math"
	"os"
	"testing"
	"time"
)

// Live Galileo and BeiDou navigation data from this station's own receiver,
// about a hundred seconds of it. As with the GPS fixture, satellite broadcast
// data is public and a capture of it is the only way to prove a bit-level
// decoder outside the sky.
//
// Both decoders were built against these bytes and then checked field by field
// against RTKLIB's convbin over the same file: every parameter of every
// satellite agreed exactly. What the assertions below pin is the physics, which
// is what fails loudly when a bit offset moves.

func feed(t *testing.T, s *Store, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
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
	return frames
}

func TestDecodesLiveGalileoPages(t *testing.T) {
	s := New(nil)
	if frames := feed(t, s, "testdata/sfrbx-gal.ubx"); frames < 100 {
		t.Fatalf("only %d frames in the fixture", frames)
	}
	const (
		p2_19 = 1.0 / (1 << 19)
		p2_33 = 1.0 / float64(int64(1)<<33)
		p2_31 = 1.0 / (1 << 31)
	)
	checked := 0
	for sv, a := range s.gal {
		if !a.complete() {
			continue
		}
		checked++
		e := &a.eph
		// Galileo flies a 29,600 km circular orbit at 56 degrees, so sqrt(A) is
		// 5440.x and nothing else.
		if sqrtA := float64(e.sqrtA) * p2_19; sqrtA < 5435 || sqrtA > 5445 {
			t.Errorf("E%02d: sqrt(A) = %.3f, not a Galileo orbit", sv, sqrtA)
		}
		if ecc := float64(e.e) * p2_33; ecc < 0 || ecc > 0.01 {
			t.Errorf("E%02d: eccentricity = %.6f", sv, ecc)
		}
		if inc := float64(e.i0) * p2_31 * 180; inc < 50 || inc > 62 {
			t.Errorf("E%02d: inclination = %.2f degrees", sv, inc)
		}
		for name, v := range map[string]int64{"M0": e.m0, "Omega0": e.omega0, "omega": e.omega} {
			if got := math.Abs(float64(v) * p2_31); got > 1.0000001 {
				t.Errorf("E%02d: %s = %.6f semicircles, outside [-1,1]", sv, name, got)
			}
		}
		// t_oe counts 60-second steps within the week.
		if e.toe*60 >= 604800 {
			t.Errorf("E%02d: toe = %d, outside the week", sv, e.toe*60)
		}
		if e.iodNav > 1023 {
			t.Errorf("E%02d: IODnav = %d", sv, e.iodNav)
		}
		if e.sv < 1 || e.sv > 36 {
			t.Errorf("E%02d: word type 4 names satellite %d", sv, e.sv)
		}
	}
	if checked < 4 {
		t.Fatalf("only %d complete Galileo ephemerides decoded", checked)
	}

	// The week is shared by the constellation and fixes the capture date.
	// Galileo week 0 began 1999-08-22, and the broadcast field is 12 bits.
	want := uint64(time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC).
		Sub(time.Date(1999, 8, 22, 0, 0, 0, 0, time.UTC)).Hours()/24/7) % 4096
	for sv, a := range s.gal {
		if a.complete() && a.eph.week != want {
			t.Errorf("E%02d: week %d, want %d for the capture date", sv, a.eph.week, want)
		}
	}

	msgs := s.Messages(1046)
	if len(msgs) != checked {
		t.Errorf("%d messages for %d complete ephemerides", len(msgs), checked)
	}
	for _, m := range msgs {
		// 492 bits of payload is 62 bytes, plus the 3-byte header and 3-byte CRC.
		if len(m) != 68 || m[0] != 0xD3 {
			t.Errorf("frame of %d bytes starting %#02x", len(m), m[0])
		}
		if got := int(m[3])<<4 | int(m[4])>>4; got != 1046 {
			t.Errorf("frame carries type %d", got)
		}
	}
}

// The Galileo fixture also holds E5b pages. They arrive in the same eight-word
// shape as E1-B and do not decode the same way: read as E1-B they give week
// numbers centuries out. This feeds the decoder nothing else.
func TestGalileoE5bPagesAreIgnored(t *testing.T) {
	raw, err := os.ReadFile("testdata/sfrbx-gal.ubx")
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
		if raw[i+8] != galileoE1B { // sigId, anything but E1-B
			s.consume(raw[i:end])
			fed++
		}
		i = end
	}
	if fed == 0 {
		t.Fatal("the fixture has no E5b pages to test with")
	}
	if len(s.gal) != 0 {
		t.Errorf("%d satellites were built from E5b pages", len(s.gal))
	}
	if got := s.Messages(1046); len(got) != 0 {
		t.Errorf("E5b pages produced %d ephemeris messages", len(got))
	}
}

func TestDecodesLiveBeiDouSubframes(t *testing.T) {
	s := New(nil)
	if frames := feed(t, s, "testdata/sfrbx-bds.ubx"); frames < 50 {
		t.Fatalf("only %d frames in the fixture", frames)
	}
	const (
		p2_19 = 1.0 / (1 << 19)
		p2_33 = 1.0 / float64(int64(1)<<33)
		p2_31 = 1.0 / (1 << 31)
	)
	checked, geo := 0, 0
	for sv, a := range s.bds {
		if !a.complete() {
			continue
		}
		checked++
		e := &a.eph
		// BeiDou flies two orbits: 27,906 km MEO (sqrt(A) 5282.x) and 42,164 km
		// IGSO (6493.x). Anything else is a misaligned field.
		sqrtA := float64(e.sqrtA) * p2_19
		switch {
		case sqrtA > 5275 && sqrtA < 5290:
		case sqrtA > 6485 && sqrtA < 6500:
			geo++
		default:
			t.Errorf("C%02d: sqrt(A) = %.3f, not a BeiDou orbit", sv, sqrtA)
		}
		if ecc := float64(e.e) * p2_33; ecc < 0 || ecc > 0.02 {
			t.Errorf("C%02d: eccentricity = %.6f", sv, ecc)
		}
		if inc := float64(e.i0) * p2_31 * 180; inc < 50 || inc > 62 {
			t.Errorf("C%02d: inclination = %.2f degrees", sv, inc)
		}
		for name, v := range map[string]int64{"M0": e.m0, "Omega0": e.omega0, "omega": e.omega} {
			if got := math.Abs(float64(v) * p2_31); got > 1.0000001 {
				t.Errorf("C%02d: %s = %.6f semicircles, outside [-1,1]", sv, name, got)
			}
		}
		// t_oe and t_oc count 8-second steps within the week.
		if e.toe*8 >= 604800 || e.toe == 0 {
			t.Errorf("C%02d: toe = %d, outside the week", sv, e.toe*8)
		}
		if e.toc*8 >= 604800 {
			t.Errorf("C%02d: toc = %d, outside the week", sv, e.toc*8)
		}
	}
	if checked < 4 {
		t.Fatalf("only %d complete BeiDou ephemerides decoded", checked)
	}
	if geo == 0 {
		t.Log("no inclined-geosynchronous satellite in this capture")
	}

	// BeiDou week 0 began 2006-01-01.
	want := uint64(time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC).
		Sub(time.Date(2006, 1, 1, 0, 0, 0, 0, time.UTC)).Hours() / 24 / 7)
	for sv, a := range s.bds {
		if a.complete() && a.eph.week != want {
			t.Errorf("C%02d: week %d, want %d for the capture date", sv, a.eph.week, want)
		}
	}

	msgs := s.Messages(1042)
	if len(msgs) != checked {
		t.Errorf("%d messages for %d complete ephemerides", len(msgs), checked)
	}
	for _, m := range msgs {
		// 511 bits of payload is 64 bytes, plus header and CRC.
		if len(m) != 70 || m[0] != 0xD3 {
			t.Errorf("frame of %d bytes starting %#02x", len(m), m[0])
		}
		if got := int(m[3])<<4 | int(m[4])>>4; got != 1042 {
			t.Errorf("frame carries type %d", got)
		}
	}
}

// The geostationary satellites broadcast D2, which has a different frame
// layout; decoding it as D1 is the same class of error as reading CNAV as LNAV.
func TestBeiDouGeostationarySatellitesAreSkipped(t *testing.T) {
	for _, sv := range []int{1, 2, 3, 4, 5, 59, 60, 61, 62, 63} {
		if !beidouGEO(sv) {
			t.Errorf("C%02d is geostationary but would be decoded as D1", sv)
		}
	}
	for _, sv := range []int{6, 11, 20, 32, 41, 58} {
		if beidouGEO(sv) {
			t.Errorf("C%02d is not geostationary but would be skipped", sv)
		}
	}
}
