package unicore_test

import (
	"testing"

	"github.com/psgnss/psgnss-base/internal/unicore"
	"github.com/psgnss/psgnss-base/internal/unicore/unicoretest"
)

// A fixed position set in ECEF comes back in ECEF, and must not be presented
// as a latitude.
func TestModeInECEFIsReportedAsECEF(t *testing.T) {
	m := unicore.ParseModeText("MODE BASE 1 4000000.1234 1300000.1234 4800000.1234,")
	if m.Kind != "fixed" || !m.ECEF || m.ID != "1" || m.Lat != "4000000.1234" {
		t.Fatalf("%+v", m)
	}
	if m = unicore.ParseModeText("MODE BASE 1 48.1 17.1 300.0,"); m.Kind != "fixed" || m.ECEF {
		t.Fatalf("%+v", m)
	}
}

func TestLiveFakeRepliesAreAcknowledgedAndParsed(t *testing.T) {
	s := session(t, unicoretest.New())
	for _, cmd := range []string{"VERSIONA", "CONFIG", "UNILOGLIST", "MODE", "MASK"} {
		r, err := s.Do(cmd)
		if err != nil || !r.OK() {
			t.Fatalf("%s: %+v %v", cmd, r, err)
		}
	}
	if _, err := s.Snapshot(); err != nil {
		t.Fatal(err)
	}
}
