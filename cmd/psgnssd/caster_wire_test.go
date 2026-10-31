package main

import (
	"testing"

	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/hub"
)

func msgs(pairs ...int) []config.MessageSel {
	var out []config.MessageSel
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, config.MessageSel{Type: pairs[i], Interval: pairs[i+1]})
	}
	return out
}

// A u-blox mountpoint synthesises the station messages and ephemeris, and
// filters the observations from the stream at their configured intervals.
func TestUBXMountSynthesisesEphemeris(t *testing.T) {
	specs, generated := mountMessagePlan(msgs(1006, 10, 1019, 30, 1077, 1, 1046, 30), false)
	if generated[1006] != 10 || generated[1019] != 30 || generated[1046] != 30 {
		t.Fatalf("generated = %v", generated)
	}
	if len(specs) != 1 || specs[0] != (hub.FilterSpec{Type: 1077, Interval: 1}) {
		t.Fatalf("specs = %v", specs)
	}
}

// A UM980 emits its own ephemeris, one message per second per type rotating
// through the satellites. Decimating that to one message per 30 s would drop
// most satellites, so native ephemeris passes on every occurrence -- including
// GLONASS 1020, which nothing synthesises.
func TestNativeEphemerisPassesUndecimated(t *testing.T) {
	specs, generated := mountMessagePlan(msgs(1006, 10, 1019, 30, 1020, 30, 1046, 30, 1087, 1, 1230, 10), true)
	if len(generated) != 1 || generated[1006] != 10 {
		t.Fatalf("generated = %v, want only the station message", generated)
	}
	want := map[int]int{1019: 0, 1020: 0, 1046: 0, 1087: 1, 1230: 10}
	if len(specs) != len(want) {
		t.Fatalf("specs = %v", specs)
	}
	for _, s := range specs {
		if iv, ok := want[s.Type]; !ok || iv != s.Interval {
			t.Errorf("type %d interval %d, want %d", s.Type, s.Interval, want[s.Type])
		}
	}
}
