package ephemeris

import (
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/rtcm"
)

// packSubframe builds the ten 24-bit words of one subframe from a bit layout,
// so a test can drive the decoder the way the satellite does.
type field struct {
	pos, width int
	value      int64
}

func packSubframe(id int, fields []field) []uint32 {
	buf := make([]byte, gpsSubframeBytes)
	set := func(pos, width int, v int64) {
		u := uint64(v) & (1<<uint(width) - 1)
		for i := 0; i < width; i++ {
			if u>>(uint(width-1-i))&1 == 1 {
				buf[(pos+i)/8] |= 1 << (7 - uint((pos+i)%8))
			}
		}
	}
	set(24+19, 3, int64(id)) // subframe ID in the handover word
	for _, f := range fields {
		set(f.pos, f.width, f.value)
	}
	words := make([]uint32, 10)
	for i := range words {
		words[i] = uint32(buf[i*3])<<16 | uint32(buf[i*3+1])<<8 | uint32(buf[i*3+2])
	}
	return words
}

// The broadcast subframes and RTCM 1019 carry the same integers, so a value
// written into a subframe must come out of the message bit for bit. Negative
// values are included deliberately: a sign-extension error is the likeliest
// way to put a satellite somewhere plausible but wrong.
func TestGPSSubframesRoundTripThrough1019(t *testing.T) {
	const i = gpsDataStart
	a := &gpsAssembly{}
	a.eph.sv = 7
	// Subframe 1: clock and health.
	if _, err := a.addSubframe(packSubframe(1, []field{
		{i, 10, 291}, {i + 10, 2, 1}, {i + 12, 4, 2}, {i + 16, 6, 0}, {i + 22, 2, 0b10},
		{i + 24, 1, 1},
		{i + 25 + 87, 8, -13}, {i + 25 + 87 + 8, 8, 0x5A}, {i + 25 + 87 + 16, 16, 45000},
		{i + 25 + 87 + 32, 8, -2}, {i + 25 + 87 + 40, 16, -3411}, {i + 25 + 87 + 56, 22, -123456},
	})); err != nil {
		t.Fatal(err)
	}
	// Subframe 2: orbit, first half. IODE must equal the low byte of IODC.
	if _, err := a.addSubframe(packSubframe(2, []field{
		{i, 8, 0x5A}, {i + 8, 16, -142}, {i + 24, 16, 11234}, {i + 40, 32, -1234567890},
		{i + 72, 16, -2048}, {i + 88, 32, 41943040}, {i + 120, 16, 3000},
		{i + 136, 32, 2702092800}, {i + 168, 16, 45000}, {i + 184, 1, 0},
	})); err != nil {
		t.Fatal(err)
	}
	// Subframe 3: orbit, second half.
	if _, err := a.addSubframe(packSubframe(3, []field{
		{i, 16, 120}, {i + 16, 32, -987654321}, {i + 48, 16, -60}, {i + 64, 32, 700000000},
		{i + 96, 16, 4321}, {i + 112, 32, -1700000000}, {i + 144, 24, -21000},
		{i + 168, 8, 0x5A}, {i + 176, 14, -1234},
	})); err != nil {
		t.Fatal(err)
	}
	if !a.eph.complete(a.have) {
		t.Fatalf("data set not complete: %+v have=%v", a.eph, a.have)
	}

	w := rtcm.NewWriter()
	a.eph.encode1019(w)
	frame, err := w.Frame()
	if err != nil {
		t.Fatal(err)
	}
	// 1019 is 488 bits of payload: 61 bytes, plus the 3-byte header and 3-byte CRC.
	if len(frame) != 67 {
		t.Errorf("frame is %d bytes, want 67", len(frame))
	}
	if frame[0] != 0xD3 {
		t.Fatal("frame does not start with the RTCM preamble")
	}
	r := bitReader{frame[3:]}
	pos := 0
	nextU := func(n int) uint64 { v := r.u(pos, n); pos += n; return v }
	nextS := func(n int) int64 { v := r.s(pos, n); pos += n; return v }
	if got := nextU(12); got != 1019 {
		t.Fatalf("message type %d", got)
	}
	checks := []struct {
		name string
		got  int64
		want int64
	}{
		{"sv", int64(nextU(6)), 7},
		{"week", int64(nextU(10)), 291},
		{"ura", int64(nextU(4)), 2},
		{"codeL2", int64(nextU(2)), 1},
		{"idot", nextS(14), -1234},
		{"iode", int64(nextU(8)), 0x5A},
		{"toc", int64(nextU(16)), 45000},
		{"af2", nextS(8), -2},
		{"af1", nextS(16), -3411},
		{"af0", nextS(22), -123456},
		{"iodc", int64(nextU(10)), 0b10<<8 | 0x5A},
		{"crs", nextS(16), -142},
		{"deltaN", nextS(16), 11234},
		{"m0", nextS(32), -1234567890},
		{"cuc", nextS(16), -2048},
		{"e", int64(nextU(32)), 41943040},
		{"cus", nextS(16), 3000},
		{"sqrtA", int64(nextU(32)), 2702092800},
		{"toe", int64(nextU(16)), 45000},
		{"cic", nextS(16), 120},
		{"omega0", nextS(32), -987654321},
		{"cis", nextS(16), -60},
		{"i0", nextS(32), 700000000},
		{"crc", nextS(16), 4321},
		{"omega", nextS(32), -1700000000},
		{"omegaDot", nextS(24), -21000},
		{"tgd", nextS(8), -13},
		{"health", int64(nextU(6)), 0},
		{"l2p", int64(nextU(1)), 1},
		{"fit", int64(nextU(1)), 0},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if pos != 488 {
		t.Errorf("wrote %d bits, RTCM 1019 is 488", pos)
	}
}

// A data set assembled across an ephemeris change would be a satellite in the
// wrong place. The issue-of-data fields are what prevent it.
func TestMismatchedIssueOfDataIsNotEmitted(t *testing.T) {
	const i = gpsDataStart
	a := &gpsAssembly{}
	_, _ = a.addSubframe(packSubframe(1, []field{{i + 22, 2, 0}, {i + 25 + 87 + 8, 8, 0x11}}))
	_, _ = a.addSubframe(packSubframe(2, []field{{i, 8, 0x11}}))
	_, _ = a.addSubframe(packSubframe(3, []field{{i + 168, 8, 0x22}})) // a newer set
	if a.eph.complete(a.have) {
		t.Error("subframes from two different data sets were accepted as one ephemeris")
	}
}

func TestIncompleteAndStaleSetsAreNotServed(t *testing.T) {
	s := New(nil)
	a := &gpsAssembly{}
	a.eph.sv = 3
	s.gps[3] = a
	if got := s.Messages(1019); len(got) != 0 {
		t.Errorf("an incomplete set produced %d messages", len(got))
	}
	a.have = [4]bool{false, true, true, true}
	a.eph.updated = time.Now().Add(-3 * time.Hour)
	if got := s.Messages(1019); len(got) != 0 {
		t.Errorf("a stale set produced %d messages", len(got))
	}
	if got := s.Messages(1020); got != nil {
		t.Error("an unimplemented constellation returned messages")
	}
}
