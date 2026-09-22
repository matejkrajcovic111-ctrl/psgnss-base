package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/store"
	"github.com/psgnss/psgnss-base/internal/telemetry"
)

func TestHistoryAPIIncludesPlaybackSignalsAndHealth(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().Add(-time.Minute).Truncate(time.Second)
	sats := []telemetry.Sat{{GNSSID: telemetry.GPS, SvID: 14, CNO: 45, Elev: 32, Azim: 120, Used: true}}
	sigs := []telemetry.Signal{{GNSSID: telemetry.GPS, SvID: 14, SigID: 6, CNO: 43, Used: true}}
	if err := db.PutEpoch(now, sats, sigs, 5); err != nil {
		t.Fatal(err)
	}
	if err := db.PutHealth(now, 1000, 4000, 6000, 2, 12.5, 48.2, 1000, 2000); err != nil {
		t.Fatal(err)
	}
	s := New(&Server{Store: db, Cfg: &config.Config{Telemetry: config.Telemetry{RetentionDays: 7}}})
	rr := httptest.NewRecorder()
	s.handleHistory(rr, httptest.NewRequest(http.MethodGet, "/api/history?hours=1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Epochs []struct {
			Satellites []liveSat    `json:"satellites"`
			Signals    []liveSignal `json:"signals"`
		} `json:"epochs"`
		Health    []map[string]any `json:"health"`
		Retention int              `json:"retention_days"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Epochs) != 1 || len(body.Epochs[0].Signals) != 1 || body.Epochs[0].Signals[0].Band != "L5 I" {
		t.Fatalf("wrong playback epoch: %+v", body.Epochs)
	}
	if len(body.Health) != 1 || body.Retention != 7 {
		t.Fatalf("health=%v retention=%d", body.Health, body.Retention)
	}
}
