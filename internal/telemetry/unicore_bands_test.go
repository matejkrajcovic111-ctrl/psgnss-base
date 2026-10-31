package telemetry

import "testing"

// Every frequency in the manual's table has its own id, and no two signals on
// different carriers share one. BeiDou B1C once shared an id with GEO B3I.
func TestEverySignalInTheManualHasADistinctID(t *testing.T) {
	cases := []struct {
		gnss, sv uint8
		freqs    []uint8
	}{
		{GPS, 13, []uint8{0, 17, 14, 6, 3, 11, 9}},
		{Galileo, 7, []uint8{1, 2, 12, 17, 18, 22}},
		{GLONASS, 11, []uint8{0, 5, 6, 7}},
		{SBAS, 123, []uint8{0, 6}},
		{NavIC, 2, []uint8{6, 14}},
		{QZSS, 1, []uint8{0, 6, 14, 17, 18, 24, 27}},
		{BeiDou, 13, []uint8{0, 17, 21, 8, 12, 13, 4, 5, 6}},
		{BeiDou, 2, []uint8{0, 17, 21, 8, 12, 13, 4, 5, 6}},
	}
	for _, c := range cases {
		seen := map[uint8]uint8{}
		for _, f := range c.freqs {
			id, ok := sigID(c.gnss, c.sv, f)
			if !ok {
				t.Errorf("gnss %d freq %d is not mapped", c.gnss, f)
				continue
			}
			if prev, dup := seen[id]; dup {
				t.Errorf("gnss %d freqs %d and %d share id %d", c.gnss, prev, f, id)
			}
			seen[id] = f
		}
	}
}

func TestBeiDouB1CDoesNotTakeAUbloxID(t *testing.T) {
	if id, _ := sigID(BeiDou, 13, 8); id != 32 {
		t.Fatalf("B1C id %d", id)
	}
	if id, _ := sigID(BeiDou, 2, 21); id != 5 {
		t.Fatalf("GEO B3I id %d", id)
	}
}
