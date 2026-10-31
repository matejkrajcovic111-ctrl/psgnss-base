package telemetry

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixtures are one epoch (GPS week 2442, 579556 s) cut from a live UM980 by
// TestGenerateUnicoreFixtures: SATSINFOB with its ASCII twin SATSINFOA, BESTSATB,
// and BESTNAVB with the position replaced by 48, 17, 300.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func findSat(sats []Sat, gnss, sv uint8) (Sat, bool) {
	for _, s := range sats {
		if s.GNSSID == gnss && s.SvID == sv {
			return s, true
		}
	}
	return Sat{}, false
}

func sigCNO(sigs []Signal, gnss, sv, sig uint8) (uint8, bool) {
	for _, s := range sigs {
		if s.GNSSID == gnss && s.SvID == sv && s.SigID == sig {
			return s.CNO, true
		}
	}
	return 0, false
}

func TestParseSatsInfoDecodesTheReceiversOwnEpoch(t *testing.T) {
	sats, sigs, err := ParseSatsInfo(fixture(t, "um980_satsinfo.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sats) != 44 {
		t.Fatalf("got %d satellites, want 44", len(sats))
	}
	// Values read off the ASCII twin of the same epoch.
	cases := []struct {
		name     string
		gnss, sv uint8
		azim     int16
		elev     int8
		cno      uint8
		signals  map[uint8]uint8 // u-blox sigId -> C/N0
	}{
		// GPS 13: L1 C/A 47, L2C(L) 47, L5 pilot 50 (the best), L1C 47, L2P 43.
		{"GPS 13", GPS, 13, 99, 50, 50, map[uint8]uint8{0: 47, 3: 47, 7: 50, 32: 47, 34: 43}},
		// Unicore GLONASS PRN 48 is slot 11; L1 49, L2 47, G3 Q 44.
		{"GLONASS slot 11", GLONASS, 11, 112, 31, 49, map[uint8]uint8{0: 49, 2: 47, 33: 44}},
		// BeiDou 2 is a GEO satellite, so B1I and B3I are the D2 signals; B2b 40 is the best.
		{"BeiDou 2", BeiDou, 2, 112, 8, 40, map[uint8]uint8{1: 39, 5: 38, 33: 40}},
		// BeiDou 13 is not GEO: D1 signals, B1C pilot 49, B2a pilot 49, B2b 52 is the best.
		{"BeiDou 13", BeiDou, 13, 142, 67, 52, map[uint8]uint8{0: 52, 4: 52, 7: 49, 32: 49, 33: 52}},
		{"Galileo 30", Galileo, 30, 51, 78, 53, map[uint8]uint8{0: 48, 4: 50, 6: 52, 7: 53}},
		{"SBAS 136", SBAS, 136, 196, 32, 44, map[uint8]uint8{0: 44}},
		{"NavIC 2", NavIC, 2, 111, 46, 41, map[uint8]uint8{0: 41}},
	}
	for _, c := range cases {
		s, ok := findSat(sats, c.gnss, c.sv)
		if !ok {
			t.Errorf("%s is missing", c.name)
			continue
		}
		if s.Azim != c.azim || s.Elev != c.elev || s.CNO != c.cno {
			t.Errorf("%s = az %d el %d cno %d, want az %d el %d cno %d",
				c.name, s.Azim, s.Elev, s.CNO, c.azim, c.elev, c.cno)
		}
		for sig, want := range c.signals {
			if got, ok := sigCNO(sigs, c.gnss, c.sv, sig); !ok || got != want {
				t.Errorf("%s signal %d = %d (present %v), want %d", c.name, sig, got, ok, want)
			}
		}
		n := 0
		for _, sg := range sigs {
			if sg.GNSSID == c.gnss && sg.SvID == c.sv {
				n++
			}
		}
		if n != len(c.signals) {
			t.Errorf("%s has %d signals, want %d", c.name, n, len(c.signals))
		}
	}
	for _, s := range sats {
		if s.Used {
			t.Errorf("%+v is marked used before BESTSAT was applied", s)
		}
	}
}

// asciiSat is one satellite as the receiver's ASCII SATSINFOA renders it.
type asciiSat struct {
	prn, azim, elev, sys int
	cno                  int // best of the records
	records              int
}

func parseSatsInfoA(t *testing.T, line string) []asciiSat {
	t.Helper()
	body := line[strings.Index(line, ";")+1 : strings.LastIndex(line, "*")]
	f := strings.Split(body, ",")
	num := func(i int) int {
		v, err := strconv.Atoi(f[i])
		if err != nil {
			t.Fatalf("field %d %q: %v", i, f[i], err)
		}
		return v
	}
	var out []asciiSat
	o := 6
	for i := 0; i < num(0); i++ {
		s := asciiSat{prn: num(o), azim: num(o + 1), elev: num(o + 2), sys: num(o + 3), records: num(o + 6)}
		for q := 0; q < s.records; q++ {
			if v := num(o + 3 + 4*q + 1); v > s.cno {
				s.cno = v
			}
		}
		out = append(out, s)
		o += 3 + 4*s.records
	}
	if o != len(f) {
		t.Fatalf("ASCII SATSINFO has %d fields, consumed %d", len(f), o)
	}
	return out
}

func TestSatsInfoBinaryMatchesItsAsciiTwinFieldByField(t *testing.T) {
	bin := fixture(t, "um980_satsinfo.bin")
	txt := string(fixture(t, "um980_satsinfo.txt"))
	h, _, err := SplitUnicore(bin)
	if err != nil {
		t.Fatal(err)
	}
	// Same epoch: the ASCII header carries the same week and ms.
	if want := "FINE," + strconv.Itoa(int(h.Week)) + "," + strconv.Itoa(int(h.MS)) + ","; !strings.Contains(txt, want) {
		t.Fatalf("ASCII twin is not the same epoch as the binary log (%s)", want)
	}
	asc := parseSatsInfoA(t, txt)
	sats, _, err := ParseSatsInfo(bin)
	if err != nil {
		t.Fatal(err)
	}
	if len(asc) != len(sats) {
		t.Fatalf("ASCII has %d satellites, binary decodes to %d", len(asc), len(sats))
	}
	for i, a := range asc {
		gnss, sv, ok := satID(uint8(a.sys), uint8(a.prn))
		if !ok {
			t.Fatalf("ASCII satellite %d (system %d, PRN %d) has no u-blox identity", i, a.sys, a.prn)
		}
		s := sats[i]
		if s.GNSSID != gnss || s.SvID != sv || int(s.Azim) != a.azim || int(s.Elev) != a.elev || int(s.CNO) != a.cno {
			t.Errorf("satellite %d: binary %+v, ASCII %+v", i, s, a)
		}
	}
}

func TestBestSatMarksTheUsedSatellites(t *testing.T) {
	used, err := ParseBestSat(fixture(t, "um980_bestsat.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(used) != 33 {
		t.Errorf("%d satellites used, want 33", len(used))
	}
	sats, sigs, _ := ParseSatsInfo(fixture(t, "um980_satsinfo.bin"))
	MarkUsed(sats, sigs, used)
	want := map[[2]uint8]bool{
		{GPS, 13}: true, {GPS, 24}: false, // GPS 24 is tracked at 6 degrees, not used
		{GLONASS, 11}: true, {GLONASS, 22}: true, // PRN 48 and 59
		{Galileo, 30}: true, {BeiDou, 13}: true, {BeiDou, 2}: false, {SBAS, 136}: false,
	}
	for k, w := range want {
		s, ok := findSat(sats, k[0], k[1])
		if !ok || s.Used != w {
			t.Errorf("%s %d used = %v (present %v), want %v", GNSSName(k[0]), k[1], s.Used, ok, w)
		}
	}
	for _, sg := range sigs {
		if sg.GNSSID == GPS && sg.SvID == 13 && !sg.Used {
			t.Error("a signal of a used satellite is not marked used")
		}
	}
}

func TestBestNavGivesFixTypeAndSatellitesUsed(t *testing.T) {
	bn, err := ParseBestNav(fixture(t, "um980_bestnav.bin"))
	if err != nil {
		t.Fatal(err)
	}
	// The receiver was a fixed base: SOL_COMPUTED, FIXEDPOS, 28 satellites in the
	// solution (the ASCII twin says 40 tracked, 28 used).
	if bn.FixType != 5 || FixName(bn.FixType) != "time only (fixed base)" {
		t.Errorf("fix type = %d %q, want 5 time only (fixed base)", bn.FixType, FixName(bn.FixType))
	}
	if bn.NumSV != 28 {
		t.Errorf("NumSV = %d, want 28", bn.NumSV)
	}
	h := bn.Header
	if h.Week != 2442 || h.MS != 579556000 || h.LeapSec != 18 || !h.Fine() {
		t.Errorf("header = %+v", h)
	}
	if want := time.Date(2026, 10, 31, 16, 58, 58, 0, time.UTC); !h.EpochUTC().Equal(want) {
		t.Errorf("epoch UTC = %s, want %s", h.EpochUTC(), want)
	}
}

// sealed builds a BESTNAVB frame from the fixture with the given solution
// status, position type and header bytes, and a valid CRC.
func sealedBestNav(t *testing.T, status, pos uint32, edit func(f []byte)) []byte {
	t.Helper()
	f := fixture(t, "um980_bestnav.bin")
	binary.LittleEndian.PutUint32(f[unicoreHeaderLen:], status)
	binary.LittleEndian.PutUint32(f[unicoreHeaderLen+4:], pos)
	if edit != nil {
		edit(f)
	}
	binary.LittleEndian.PutUint32(f[len(f)-4:], UnicoreCRC32(f[:len(f)-4]))
	return f
}

func TestBestNavPositionTypesMapToUbloxFixTypes(t *testing.T) {
	cases := []struct {
		status, pos uint32
		want        uint8
	}{
		{0, 0, 0},  // NONE
		{1, 16, 0}, // INSUFFICIENT_OBS even with a type
		{0, 1, 5},  // FIXEDPOS
		{0, 2, 2},  // FIXEDHEIGHT
		{0, 16, 3}, // SINGLE
		{0, 17, 3}, // PSRDIFF
		{0, 18, 3}, // SBAS
		{0, 34, 3}, // NARROW_FLOAT
		{0, 50, 3}, // NARROW_INT
		{0, 70, 3}, // PPP_AR
	}
	for _, c := range cases {
		bn, err := ParseBestNav(sealedBestNav(t, c.status, c.pos, nil))
		if err != nil || bn.FixType != c.want {
			t.Errorf("status %d type %d = %d (%v), want %d", c.status, c.pos, bn.FixType, err, c.want)
		}
	}
}

func TestUnicoreDecodersRejectCorruptFrames(t *testing.T) {
	good := fixture(t, "um980_satsinfo.bin")
	bad := append([]byte(nil), good...)
	bad[40] ^= 0xFF
	if _, _, err := ParseSatsInfo(bad); err == nil {
		t.Error("a frame with a bad CRC was decoded")
	}
	if _, _, err := ParseSatsInfo(good[:len(good)-10]); err == nil {
		t.Error("a truncated frame was decoded")
	}
	if _, err := ParseBestNav(good); err == nil {
		t.Error("SATSINFO was accepted as BESTNAV")
	}
	// A declared satellite count that the data cannot hold.
	short := append([]byte(nil), good...)
	short[unicoreHeaderLen] = 200
	binary.LittleEndian.PutUint32(short[len(short)-4:], UnicoreCRC32(short[:len(short)-4]))
	if _, _, err := ParseSatsInfo(short); err == nil {
		t.Error("a SATSINFO count beyond its data was decoded")
	}
}

func TestSatelliteNumberingMatchesUblox(t *testing.T) {
	cases := []struct {
		sys, prn uint8
		g, sv    uint8
		ok       bool
	}{
		{ucGPS, 13, GPS, 13, true},
		{ucGLONASS, 38, GLONASS, 1, true},
		{ucGLONASS, 61, GLONASS, 24, true},
		{ucGLONASS, 37, 0, 0, false},
		{ucQZSS, 193, QZSS, 1, true},
		{ucQZSS, 202, QZSS, 10, true},
		{ucSBAS, 123, SBAS, 123, true},
		{ucGalileo, 36, Galileo, 36, true},
		{ucBeiDou, 59, BeiDou, 59, true},
		{ucNavIC, 2, NavIC, 2, true},
		{9, 1, 0, 0, false},
	}
	for _, c := range cases {
		g, sv, ok := satID(c.sys, c.prn)
		if ok != c.ok || (ok && (g != c.g || sv != c.sv)) {
			t.Errorf("system %d PRN %d = %d/%d/%v, want %d/%d/%v", c.sys, c.prn, g, sv, ok, c.g, c.sv, c.ok)
		}
	}
}

type memStore struct {
	mu     sync.Mutex
	epochs []memEpoch
}

type memEpoch struct {
	sats    []Sat
	signals []Signal
	fix     uint8
}

func (m *memStore) PutEpoch(ts time.Time, sats []Sat, signals []Signal, fixType uint8) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.epochs = append(m.epochs, memEpoch{sats, signals, fixType})
	return nil
}
func (m *memStore) DecimateOld(time.Duration, int) (int64, error) { return 0, nil }
func (m *memStore) PruneTelemetry(int) (int64, error)             { return 0, nil }

func TestCollectorBuildsAnEpochFromUnicoreLogs(t *testing.T) {
	store := &memStore{}
	c := NewCollector(store, time.Hour, 30, 7, nil)
	frames := make(chan []byte, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx, frames); close(done) }()

	// BESTSAT and BESTNAV first, then SATSINFO twice in the same second: the
	// store gets one row.
	frames <- fixture(t, "um980_bestsat.bin")
	frames <- fixture(t, "um980_bestnav.bin")
	frames <- fixture(t, "um980_satsinfo.bin")
	frames <- fixture(t, "um980_satsinfo.bin")
	frames <- []byte{0xB5, 0x62, 0x01, 0x07, 0, 0, 0x08, 0x2B} // UBX still passes through untouched
	close(frames)
	<-done
	cancel()

	ep, at := c.Latest()
	if at.IsZero() {
		t.Fatal("no epoch published")
	}
	if len(ep.Sats) != 44 || ep.FixType != 5 || ep.NumSV != 28 {
		t.Errorf("latest = %d sats, fix %d, numSV %d; want 44, 5, 28", len(ep.Sats), ep.FixType, ep.NumSV)
	}
	gps13, ok := findSat(ep.Sats, GPS, 13)
	if !ok || gps13.CNO != 50 || gps13.Azim != 99 || gps13.Elev != 50 || !gps13.Used {
		t.Errorf("GPS 13 = %+v (present %v)", gps13, ok)
	}
	if n := len(c.LatestSignals()); n < 100 {
		t.Errorf("%d signals published, want the per-band records", n)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.epochs) != 1 {
		t.Fatalf("store got %d rows from two epochs inside one second, want 1", len(store.epochs))
	}
	if store.epochs[0].fix != 5 || len(store.epochs[0].signals) == 0 {
		t.Errorf("stored row = fix %d, %d signals", store.epochs[0].fix, len(store.epochs[0].signals))
	}
	// The stored blob format is the one UBX history uses.
	if got := Unpack(Pack(store.epochs[0].sats)); len(got) != 44 {
		t.Errorf("packed history round trip has %d satellites", len(got))
	}

	leap, week, tow, gat := c.GPSTime()
	if leap != 18 || week != 2442 || math.Abs(tow-579556) > 1e-9 || gat.IsZero() {
		t.Errorf("GPSTime = %d %d %v %v", leap, week, tow, gat)
	}
}
