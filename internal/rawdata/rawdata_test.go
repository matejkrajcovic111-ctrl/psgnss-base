package rawdata

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// writer builds synthetic messages. The fixtures are invented numbers: nothing
// here comes from a real station, so no position can leak through a test.
type writer struct {
	b []byte
	n int
}

func (w *writer) put(v uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.n%8 == 0 {
			w.b = append(w.b, 0)
		}
		w.b[w.n/8] |= byte(v>>uint(i)&1) << (7 - uint(w.n%8))
		w.n++
	}
}

func (w *writer) signed(v int64, n int) { w.put(uint64(v)&(1<<uint(n)-1), n) }

// msm7 writes a GPS 1077 with one satellite and the signals 1C and 5Q.
func msm7(tow uint64, lock uint64) bits {
	w := &writer{}
	w.put(1077, 12)
	w.put(1, 12)
	w.put(tow, 30)
	w.put(0, 73-54)
	w.put(1<<(64-5), 64) // G05
	w.put(1<<(32-2)|1<<(32-23), 32)
	w.put(3, 2) // both cells
	w.put(70, 8)
	w.put(0, 4)
	w.put(512, 10) // 70.5 ms
	w.signed(-300, 14)
	w.signed(1<<12, 20) // pr
	w.signed(-1<<12, 20)
	w.signed(1<<14, 24) // cp
	w.signed(-1<<14, 24)
	w.put(lock, 10)
	w.put(lock, 10)
	w.put(0, 1)
	w.put(1, 1)
	w.put(45*16, 10)
	w.put(40*16, 10)
	w.signed(5000, 15) // 0.5 m/s
	w.signed(-1<<14, 15)
	return w.b
}

func TestDecodeMSM7(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) // a Wednesday
	lock := map[string]uint64{}
	sats, err := decodeMSM7(msm7(302400000, 100), now, lock)
	if err != nil {
		t.Fatal(err)
	}
	if len(sats) != 1 || sats[0].Sat != "G05" || len(sats[0].Signals) != 2 {
		t.Fatalf("sats = %+v", sats)
	}
	// Highest frequency first, so columns line up across satellites.
	l1, l5 := sats[0].Signals[0], sats[0].Signals[1]
	if l1.Code != "1C" || l5.Code != "5Q" {
		t.Fatalf("order = %s %s", l1.Code, l5.Code)
	}
	near := func(name string, got, want, tol float64) {
		t.Helper()
		if math.Abs(got-want) > tol {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	const rough = 70.5
	near("P", l1.P, (rough+4096*math.Pow(2, -29))*clight/1000, 1e-6)
	near("L", l1.L, (rough+16384*math.Pow(2, -31))*1e-3*1575.42e6, 1e-3)
	near("D", l1.D, -(-300.0+0.5)*1575.42e6/clight, 1e-6)
	near("S", l5.CN0, 40, 0)
	if l1.LLI != 0 || l5.LLI != 2 {
		t.Errorf("LLI = %d %d", l1.LLI, l5.LLI)
	}
	if l5.D != 0 {
		t.Errorf("an invalid rate gave D = %v", l5.D)
	}
	if want := "2026/10/28 12:00:00.000"; sats[0].Time != want { // Wednesday noon in GPST
		t.Errorf("time = %s, want %s", sats[0].Time, want)
	}
	// A lock indicator going down is a slip.
	sats, err = decodeMSM7(msm7(302401000, 50), now, lock)
	if err != nil {
		t.Fatal(err)
	}
	if sats[0].Signals[0].LLI&1 != 1 {
		t.Error("the slip was not flagged")
	}
}

func TestMSM7EveryTruncationIsAnError(t *testing.T) {
	full := msm7(0, 1)
	for n := range full {
		if _, err := decodeMSM7(full[:n], time.Now(), map[string]uint64{}); err == nil {
			t.Errorf("%d of %d bytes decoded", n, len(full))
		}
	}
}

func TestEpochTimeAcrossSystems(t *testing.T) {
	now := time.Date(2026, 10, 31, 22, 0, 0, 0, time.UTC) // a Saturday
	gps := now.Add(leapSeconds * time.Second)
	sunday := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	tow := uint64(gps.Sub(sunday).Milliseconds())
	if got := epochTime("GPS", tow, now); !got.Equal(gps) {
		t.Errorf("GPS %v, want %v", got, gps)
	}
	if got := epochTime("BeiDou", tow-14000, now); !got.Equal(gps) {
		t.Errorf("BeiDou %v, want %v", got, gps)
	}
	moscow := now.Add(3 * time.Hour) // past midnight there
	tod := uint64(moscow.Sub(moscow.Truncate(24 * time.Hour)).Milliseconds())
	if got := epochTime("GLONASS", 6<<27|tod, now); !got.Equal(gps) {
		t.Errorf("GLONASS %v, want %v", got, gps)
	}
	// The week turns over between the message and the clock.
	late := time.Date(2026, 11, 1, 0, 0, 1, 0, time.UTC).Add(-leapSeconds * time.Second)
	last := uint64(7*24*3600*1000 - 2000)
	if got := epochTime("GPS", last, late); got.After(late.Add(leapSeconds*time.Second)) || late.Add(leapSeconds*time.Second).Sub(got) > 5*time.Second {
		t.Errorf("rollover gave %v", got)
	}
}

// Every layout is written field by field and read back, so a wrong width
// anywhere would shift everything after it.
func TestEphemerisLayouts(t *testing.T) {
	for typ, layout := range layouts {
		w := &writer{}
		w.put(uint64(typ), 12)
		// A distinct non-zero value that fits the field and keeps its sign bit clear.
		value := func(k int, f field) uint64 {
			if v := uint64(k+1) % (1 << uint(f.bits-1)); v != 0 {
				return v
			}
			return 1
		}
		for k, f := range layout {
			w.put(value(k, f), f.bits)
		}
		m, err := read(w.b, layout)
		if err != nil {
			t.Fatalf("%d: %v", typ, err)
		}
		for k, f := range layout {
			if got, want := m[f.name], float64(value(k, f))*f.scale; math.Abs(got-want) > math.Abs(want)*1e-12 {
				t.Errorf("%d %s = %v, want %v", typ, f.name, got, want)
			}
		}
		view, sat, row, err := decodeNav(w.b)
		if err != nil {
			t.Fatalf("%d: %v", typ, err)
		}
		if view == "" || sat == "" || len(row) != len(keplerColumns) {
			t.Errorf("%d: view %q sat %q, %d columns", typ, view, sat, len(row))
		}
		if _, _, _, err := decodeNav(w.b[:10]); err == nil {
			t.Errorf("%d: a truncated message decoded", typ)
		}
	}
}

func TestGLONASSEphemeris(t *testing.T) {
	w := &writer{}
	w.put(1020, 12)
	w.put(5, 6)
	w.put(7+3, 5) // channel +3
	w.put(0, 4+12)
	w.put(0, 1) // healthy
	w.put(0, 1)
	w.put(44, 7)
	for k := 0; k < 3; k++ {
		w.put(1, 1) // negative
		w.put(1024, 23)
		w.put(1, 1)
		w.put(2048, 26)
		w.put(0, 5)
	}
	w.put(0, 1)
	w.put(0, 11)
	w.put(0, 3)
	w.put(1, 1)
	w.put(1<<10, 21)
	view, sat, row, err := decodeNav(w.b)
	if err != nil {
		t.Fatal(err)
	}
	if view != "GLONASS" || sat != "R05" || len(row) != len(navViews[3].Columns) {
		t.Fatalf("%s %s %d", view, sat, len(row))
	}
	if row[2] != "3" || row[3] != "660" || row[4] != "-1000.000" || row[7] != "-0.976562" {
		t.Errorf("row = %v", row)
	}
	if !strings.HasPrefix(row[13], "-9.5367") {
		t.Errorf("tau = %s", row[13])
	}
}

func TestMonitorTablesAndCache(t *testing.T) {
	m := New(nil)
	frame := append(append([]byte{0xD3, 0, 0}, msm7(302400000, 100)...), 0, 0, 0)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	m.consume(frame, now)
	body, ok := m.JSON("obs", now)
	if !ok {
		t.Fatal("no obs table")
	}
	var got struct {
		Sats []Sat `json:"sats"`
	}
	if err := json.Unmarshal(body, &got); err != nil || len(got.Sats) != 1 {
		t.Fatalf("%v %s", err, body)
	}
	// Within a second the same bytes are served, whatever changed.
	m.sats = map[string]Sat{}
	if again, _ := m.JSON("obs", now.Add(100*time.Millisecond)); string(again) != string(body) {
		t.Error("the table was rendered again inside the cache window")
	}
	// A satellite that stopped being reported goes away.
	m.consume(frame, now)
	if later, _ := m.JSON("obs", now.Add(satExpiry+time.Second)); strings.Contains(string(later), "G05") {
		t.Error("an expired satellite is still listed")
	}
	if _, ok := m.JSON("../etc", now); ok {
		t.Error("an unknown view was accepted")
	}
	if body, ok := m.JSON("GLONASS", now); !ok || !strings.Contains(string(body), "tb (min)") {
		t.Errorf("nav table = %s", body)
	}
}
