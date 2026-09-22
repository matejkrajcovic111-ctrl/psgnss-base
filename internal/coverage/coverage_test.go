package coverage

import "testing"

func TestNothingBroadcastYieldsNoCircle(t *testing.T) {
	if e := Estimate(Capability{}); e.RangeKM != 0 || e.Limit != "" {
		t.Fatalf("a range was invented from no observations: %+v", e)
	}
}

// A single-frequency stream is short-baseline work however many constellations
// it carries: the rover cannot model the ionosphere at all.
func TestSingleFrequencyIsHeldShort(t *testing.T) {
	e := Estimate(Capability{Constellations: 4, Bands: 1})
	if e.RangeKM != 10 || e.Limit != "accuracy" {
		t.Fatalf("%+v", e)
	}
}

func TestMoreConstellationsReachFurther(t *testing.T) {
	one := Estimate(Capability{Constellations: 1, Bands: 2})
	four := Estimate(Capability{Constellations: 4, Bands: 3})
	if one.RangeKM != 20 || four.RangeKM != 41 {
		t.Fatalf("one=%+v four=%+v", one, four)
	}
	if four.Limit != "ambiguity" {
		t.Errorf("the dual-frequency limit should be the fix, not the error budget: %+v", four)
	}
}

// The ceiling exists so a receiver reporting an implausible constellation count
// cannot draw a circle across a continent.
func TestRangeIsCapped(t *testing.T) {
	if e := Estimate(Capability{Constellations: 20, Bands: 3}); e.RangeKM != 49 {
		t.Fatalf("%+v", e)
	}
}
