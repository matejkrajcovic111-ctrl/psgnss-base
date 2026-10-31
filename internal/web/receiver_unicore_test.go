package web

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/unicore/unicoretest"
)

func unicoreFixture(t *testing.T) (userAPIFixture, *unicoretest.Fake) {
	t.Helper()
	f := newUserAPIFixture(t)
	cfg := settingsTestConfig()
	cfg.Receiver.Profile, cfg.Receiver.Model = "simplertk3b-budget", "UM980"
	cfg.Station.StationID = 1
	cfg.Receiver.RevertTimeout = 60
	path := filepath.Join(t.TempDir(), "psgnss.toml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	f.server.Cfg, f.server.ConfigPath = cfg, path
	fake := unicoretest.New()
	f.server.receiverControl = fake.Open
	return f, fake
}

func consoleBody(cmd string, confirm bool) []byte {
	c := "false"
	if confirm {
		c = "true"
	}
	return []byte(`{"command":"` + cmd + `","confirm":` + c + `}`)
}

func auditRows(t *testing.T, f userAPIFixture) []string {
	t.Helper()
	rows, err := f.store.DB().Query(`SELECT key_name || ' | ' || note || ' | ' || reverted FROM receiver_config_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestUnicoreReceiverPageReadsVersionModeOutputsConfig(t *testing.T) {
	f, _ := unicoreFixture(t)
	rr := f.request(t, http.MethodGet, "/api/receiver", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	out := body(t, rr.Body)
	if out["driver"] != "unicore" || out["model"] != "UM980" || out["firmware"] != "R4.10Build20739" ||
		out["serial"] != "MD22C5260607530" || out["auth_date"] != "2025/08/12" {
		t.Fatalf("info: %+v", out)
	}
	if mode := out["mode"].(map[string]any); mode["role"] != "BASE" || mode["id"] != "1" || mode["kind"] != "time" {
		t.Fatalf("mode: %+v", mode)
	}
	if len(out["outputs"].([]any)) != 5 || len(out["config"].([]any)) != 6 || len(out["masks"].([]any)) != 1 {
		t.Fatalf("lists: %s", rr.Body)
	}
}

func TestUnicoreConsoleQueryRunsDirectlyAndIsAudited(t *testing.T) {
	f, fake := unicoreFixture(t)
	rr := f.request(t, http.MethodPost, "/api/receiver/console", consoleBody("versiona", false))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	out := body(t, rr.Body)
	if out["ok"] != true || out["class"] != "query" || !strings.Contains(out["text"].(string), `"UM980"`) {
		t.Fatalf("%+v", out)
	}
	if got := fake.Commands(); len(got) != 1 {
		t.Fatalf("a query must send exactly itself, got %v", got)
	}
	if a := auditRows(t, f); len(a) != 1 || !strings.Contains(a[0], "versiona | query") {
		t.Fatalf("audit %v", a)
	}
}

func TestUnicoreConsoleChangeIsPendingThenKeptWithSaveconfig(t *testing.T) {
	f, fake := unicoreFixture(t)
	rr := f.request(t, http.MethodPost, "/api/receiver/console", consoleBody("CONFIG PPS DISABLE", false))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	out := body(t, rr.Body)
	cs := out["changes"].([]any)
	if out["driver"] != "unicore" || len(cs) != 1 || cs[0].(map[string]any)["display"] != "CONFIG PPS DISABLE" {
		t.Fatalf("%+v", out)
	}
	if fake.Saved {
		t.Fatal("saved before Keep")
	}
	// a second change and a page read both see the pending state
	if rr = f.request(t, http.MethodPost, "/api/receiver/console", consoleBody("MASK 10", false)); rr.Code != http.StatusConflict {
		t.Fatalf("second change: %d %s", rr.Code, rr.Body)
	}
	rr = f.request(t, http.MethodGet, "/api/receiver", nil)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"pending"`) {
		t.Fatalf("read while pending: %d %s", rr.Code, rr.Body)
	}
	if rr = f.request(t, http.MethodPost, "/api/receiver/commit", nil); rr.Code != http.StatusOK {
		t.Fatalf("commit %d: %s", rr.Code, rr.Body)
	}
	if !fake.Saved {
		t.Fatal("Keep did not SAVECONFIG")
	}
}

func TestUnicoreConsoleRevertRestoresAndMarksAudit(t *testing.T) {
	f, fake := unicoreFixture(t)
	mode0, out0 := fake.State()
	if rr := f.request(t, http.MethodPost, "/api/receiver/console", consoleBody("UNLOG COM1 RTCM1077", false)); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	if rr := f.request(t, http.MethodPost, "/api/receiver/revert", nil); rr.Code != http.StatusOK {
		t.Fatalf("revert %d: %s", rr.Code, rr.Body)
	}
	mode1, out1 := fake.State()
	if mode0 != mode1 || strings.Join(out0, ",") != strings.Join(out1, ",") {
		t.Fatalf("not restored: %v -> %v", out0, out1)
	}
	if fake.Saved {
		t.Fatal("revert saved")
	}
	a := auditRows(t, f)
	if len(a) != 1 || !strings.HasSuffix(a[0], "| 1") {
		t.Fatalf("audit not marked reverted: %v", a)
	}
}

func TestUnicoreAutoRevertWhenNotKept(t *testing.T) {
	f, fake := unicoreFixture(t)
	f.server.Cfg.Receiver.RevertTimeout = 1
	if rr := f.request(t, http.MethodPost, "/api/receiver/console", consoleBody("CONFIG PPS DISABLE", false)); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.server.receiverMu.Lock()
		gone := f.server.receiverTxn == nil
		f.server.receiverMu.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending change never reverted")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if fake.Config["PPS"] != "CONFIG PPS ENABLE GPS POSITIVE 500000 1000 0 0" {
		t.Fatalf("PPS = %q", fake.Config["PPS"])
	}
}

func TestUnicoreConsoleDangerousNeedsConfirmation(t *testing.T) {
	f, fake := unicoreFixture(t)
	for _, cmd := range []string{"UNLOG", "UNLOG COM1", "MODE ROVER", "MODE BASE 1 48.1 17.1 300", "CONFIG COM1 460800", "FRESET", "RESET"} {
		rr := f.request(t, http.MethodPost, "/api/receiver/console", consoleBody(cmd, false))
		out := body(t, rr.Body)
		if rr.Code != http.StatusConflict || out["dangerous"] != true || out["error"] == "" {
			t.Errorf("%s: %d %s", cmd, rr.Code, rr.Body)
		}
	}
	if len(fake.Commands()) != 0 {
		t.Fatalf("unconfirmed dangerous commands reached the receiver: %v", fake.Commands())
	}
	// confirmed and revertable: goes pending, and reverting brings the outputs back
	_, out0 := fake.State()
	if rr := f.request(t, http.MethodPost, "/api/receiver/console", consoleBody("UNLOG COM1", true)); rr.Code != http.StatusOK {
		t.Fatalf("confirmed UNLOG: %d %s", rr.Code, rr.Body)
	}
	if _, o := fake.State(); len(o) != 0 {
		t.Fatalf("UNLOG COM1 left %v", o)
	}
	if rr := f.request(t, http.MethodPost, "/api/receiver/revert", nil); rr.Code != http.StatusOK {
		t.Fatalf("revert: %d %s", rr.Code, rr.Body)
	}
	if _, o := fake.State(); strings.Join(o, ",") != strings.Join(out0, ",") {
		t.Fatalf("outputs not restored: %v vs %v", o, out0)
	}
}

func TestUnicoreConsoleConfirmedIrreversibleIsSentDirectly(t *testing.T) {
	f, fake := unicoreFixture(t)
	rr := f.request(t, http.MethodPost, "/api/receiver/console", consoleBody("RESET", true))
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	if got := fake.Commands(); len(got) != 1 || got[0] != "RESET" {
		t.Fatalf("sent %v", got)
	}
	f.server.receiverMu.Lock()
	defer f.server.receiverMu.Unlock()
	if f.server.receiverTxn != nil {
		t.Fatal("an irreversible command was held pending")
	}
}

func TestUnicoreConsoleRefusesFirmwareCommandsEvenConfirmed(t *testing.T) {
	f, fake := unicoreFixture(t)
	for _, cmd := range []string{"UPGRADE", "loader", "FWUPDATE 115200"} {
		rr := f.request(t, http.MethodPost, "/api/receiver/console", consoleBody(cmd, true))
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", cmd, rr.Code, rr.Body)
		}
	}
	if len(fake.Commands()) != 0 {
		t.Fatalf("reached the receiver: %v", fake.Commands())
	}
	if a := auditRows(t, f); len(a) != 3 || !strings.Contains(a[0], "refused") {
		t.Fatalf("audit %v", a)
	}
}

func TestUnicoreConsoleReceiverRejectionLeavesNothingPending(t *testing.T) {
	f, fake := unicoreFixture(t)
	fake.Reject["CONFIG PPS"] = true
	rr := f.request(t, http.MethodPost, "/api/receiver/console", consoleBody("CONFIG PPS ENABLE X", false))
	out := body(t, rr.Body)
	if rr.Code != http.StatusBadGateway || !strings.Contains(out["text"].(string), "PARSING FAILD") {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	if rr = f.request(t, http.MethodPost, "/api/receiver/commit", nil); rr.Code != http.StatusConflict {
		t.Fatalf("something was pending: %d", rr.Code)
	}
}

func TestUnicoreConsoleOnUBXProfileIsNotImplemented(t *testing.T) {
	f := newUserAPIFixture(t)
	rr := f.request(t, http.MethodPost, "/api/receiver/console", consoleBody("VERSIONA", false))
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
}

func TestUnicoreStationIDApplyKeepsModeAndUpdatesConfigOnKeep(t *testing.T) {
	f, fake := unicoreFixture(t)
	rr := f.request(t, http.MethodPost, "/api/settings/station-id/apply", []byte(`{"station_id":7}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	if mode, _ := fake.State(); mode != "MODE BASE 7 TIME 60 0" {
		t.Fatalf("mode %q", mode)
	}
	if rr = f.request(t, http.MethodPost, "/api/receiver/commit", nil); rr.Code != http.StatusOK {
		t.Fatalf("commit %d %s", rr.Code, rr.Body)
	}
	got, err := config.Load(f.server.ConfigPath)
	if err != nil || got.Station.StationID != 7 || !fake.Saved {
		t.Fatalf("config id %d err %v saved %v", got.Station.StationID, err, fake.Saved)
	}
}

func TestUnicoreStationIDOutOfRangeIsRefused(t *testing.T) {
	f, fake := unicoreFixture(t)
	rr := f.request(t, http.MethodPost, "/api/settings/station-id/apply", []byte(`{"station_id":4096}`))
	if rr.Code != http.StatusBadRequest || len(fake.Commands()) != 0 {
		t.Fatalf("%d %s %v", rr.Code, rr.Body, fake.Commands())
	}
}

func TestUnicorePositionApplySendsModeBaseAndUpdatesConfigOnKeep(t *testing.T) {
	f, fake := unicoreFixture(t)
	req := []byte(`{"latitude":48.0001,"longitude":17.0001,"height":300.25}`)
	rr := f.request(t, http.MethodPost, "/api/settings/position/apply", req)
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	out := body(t, rr.Body)
	if mode, _ := fake.State(); mode != "MODE BASE 1 48.00010000000 17.00010000000 300.2500" {
		t.Fatalf("mode %q", mode)
	}
	if out["height_reference"] != "ellipsoidal" || out["distance_m"].(float64) > 20 {
		t.Fatalf("response %+v", out)
	}
	if rr = f.request(t, http.MethodPost, "/api/receiver/commit", nil); rr.Code != http.StatusOK {
		t.Fatalf("commit %d %s", rr.Code, rr.Body)
	}
	got, _ := config.Load(f.server.ConfigPath)
	if got.Station.Position.Mode != "fixed" || got.Station.Position.Height != 300.25 {
		t.Fatalf("config %+v", got.Station.Position)
	}
}

func TestUnicorePositionApplyConvertsAnMSLHeight(t *testing.T) {
	f, fake := unicoreFixture(t)
	fake.HeightIsMSL = true
	rr := f.request(t, http.MethodPost, "/api/settings/position/apply", []byte(`{"latitude":48.0001,"longitude":17.0001,"height":300}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	if mode, _ := fake.State(); !strings.HasSuffix(mode, " 257.0000") {
		t.Fatalf("height not converted to MSL (300 - undulation 43): %q", mode)
	}
	if out := body(t, rr.Body); out["height_reference"] != "msl-converted" {
		t.Fatalf("%+v", out)
	}
}

func TestUnicorePositionApplyRefusesAPositionOver300mAway(t *testing.T) {
	f, fake := unicoreFixture(t)
	rr := f.request(t, http.MethodPost, "/api/settings/position/apply", []byte(`{"latitude":48.01,"longitude":17,"height":300}`))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "no RTCM") {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	for _, c := range fake.Commands() {
		if strings.HasPrefix(c, "MODE BASE") {
			t.Fatalf("MODE BASE was sent: %v", fake.Commands())
		}
	}
}
