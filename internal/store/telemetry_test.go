package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/telemetry"
)

func TestEpochHistoryStoresSignalsAndSamplesWholeWindow(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := time.Unix(1000, 0)
	for i := 0; i < 20; i++ {
		sats := []telemetry.Sat{{GNSSID: telemetry.GPS, SvID: 14, CNO: uint8(30 + i), Elev: 20, Azim: 100}}
		sigs := []telemetry.Signal{{GNSSID: telemetry.GPS, SvID: 14, SigID: 0, CNO: uint8(25 + i), Used: true}}
		if err := s.PutEpoch(start.Add(time.Duration(i)*time.Second), sats, sigs, 5); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.EpochRange(start, start.Add(19*time.Second), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) > 5 || len(rows) < 4 {
		t.Fatalf("sample count=%d", len(rows))
	}
	if rows[0].TS.Unix() < 1000 || rows[len(rows)-1].TS.Unix() < 1016 {
		t.Fatalf("sampling did not cover window: %v .. %v", rows[0].TS, rows[len(rows)-1].TS)
	}
	if len(rows[len(rows)-1].Signals) != 1 || rows[len(rows)-1].Signals[0].SigID != 0 {
		t.Fatalf("signals not stored: %+v", rows[len(rows)-1].Signals)
	}
}
