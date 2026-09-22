package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/psgnss/psgnss-base/internal/receiver/receivertest"
)

func receiverFixture(t *testing.T) (userAPIFixture, *receivertest.Fake) {
	t.Helper()
	f := newUserAPIFixture(t)
	fake := receivertest.New(map[uint32][]byte{
		0x20030001: {2},          // CFG-TMODE-MODE FIXED
		0x30210001: {0xE8, 0x03}, // CFG-RATE-MEAS
		0x1031001F: {1},          // CFG-SIGNAL-GPS_ENA
		0x10310004: {1},          // CFG-SIGNAL-GPS_L5_ENA
		0x209102CF: {1},          // RTCM 1077 USB
		0x209102CD: {0},          // RTCM 1077 UART1
		0x20910137: {0},          // undocumented
	})
	f.server.receiverControl = fake.Open
	return f, fake
}

func body(t *testing.T, b *bytes.Buffer) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(b.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", b.String(), err)
	}
	return out
}

func TestReceiverReadIncludesNamesUptimeAndControls(t *testing.T) {
	f, _ := receiverFixture(t)
	rr := f.request(t, http.MethodGet, "/api/receiver", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	out := body(t, rr.Body)
	if sys := out["system"].(map[string]any); sys["run_time_s"].(float64) != 3725 {
		t.Fatalf("uptime: %+v", sys)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["label"] != "RTCM 1077" {
		t.Fatalf("messages: %+v", msgs)
	}
	if sig := out["signals"].([]any); len(sig) != 1 {
		t.Fatalf("signals: %+v", sig)
	}
	s := rr.Body.String()
	for _, want := range []string{`"name":"CFG-TMODE-MODE"`, `"display":"FIXED (2)"`, `"key":"0x20910137","value":"00","display":"00","documented":false`} {
		if !strings.Contains(s, want) {
			t.Errorf("response lacks %s", want)
		}
	}
	if out["stage0_available"] != false {
		t.Error("stage0 reported available without a snapshot")
	}
}

func TestReceiverTypedMultiKeyApplyCommitAndAudit(t *testing.T) {
	f, fake := receiverFixture(t)
	req := []byte(`{"label":"RTCM 1077 rates","changes":[{"key":"0x209102CF","text":"0"},{"key":"0x209102CD","text":"1"}]}`)
	rr := f.request(t, http.MethodPost, "/api/receiver/apply", req)
	if rr.Code != http.StatusOK {
		t.Fatalf("apply %d: %s", rr.Code, rr.Body)
	}
	if got := fake.Get(0x209102CD); got[0] != 1 || fake.Flash[0x209102CD][0] != 0 {
		t.Fatalf("RAM %x Flash %x: RAM must change and Flash must not before Keep", got, fake.Flash[0x209102CD])
	}
	// Reads are refused while pending, but report the pending change.
	rr = f.request(t, http.MethodGet, "/api/receiver", nil)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"label":"RTCM 1077 rates"`) {
		t.Fatalf("pending read %d: %s", rr.Code, rr.Body)
	}
	if rr = f.request(t, http.MethodPost, "/api/receiver/commit", nil); rr.Code != http.StatusOK {
		t.Fatalf("commit %d: %s", rr.Code, rr.Body)
	}
	if fake.Flash[0x209102CD][0] != 1 || fake.Flash[0x209102CF][0] != 0 {
		t.Fatal("Keep did not persist both keys to Flash")
	}
	var names []string
	rows, err := f.store.DB().Query(`SELECT key_name FROM receiver_config_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	if len(names) != 2 || names[0] != "CFG-MSGOUT-RTCM_3X_TYPE1077_USB — RTCM 1077 rates" {
		t.Fatalf("audit names %q", names)
	}
}

func TestReceiverApplyRejectsBadInput(t *testing.T) {
	f, fake := receiverFixture(t)
	for _, req := range []string{
		`{"changes":[{"key":"0x209102CF","text":"300"}]}`,
		`{"changes":[{"key":"0x20910137","text":"1"}]}`,
		`{"changes":[{"key":"0x209102CF","text":"1"},{"key":"0x209102cf","text":"0"}]}`,
		`{"changes":[{"key":"0x209102CF","text":"1","value":"01"}]}`,
		`{}`,
	} {
		if rr := f.request(t, http.MethodPost, "/api/receiver/apply", []byte(req)); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", req, rr.Code)
		}
	}
	if fake.ValSets != 0 {
		t.Fatal("invalid request reached the receiver")
	}
}

func TestStage0PreviewRestoreAndRevert(t *testing.T) {
	f, fake := receiverFixture(t)
	dump := `{"mon_ver":{"swVersion":"EXT HPG 2.00 (test)","extensions":["MOD=ZED-X20P"]},
	 "layers":{"RAM":{"0x91":[["0x209102CF","00"],["0x209102CD","01"],["0x20910137","00"]],"0x21":[["0x30210001","e803"]]}}}`
	path := filepath.Join(t.TempDir(), "stage0.json")
	if err := os.WriteFile(path, []byte(dump), 0o600); err != nil {
		t.Fatal(err)
	}
	f.server.Stage0Path = path

	rr := f.request(t, http.MethodGet, "/api/receiver/stage0", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview %d: %s", rr.Code, rr.Body)
	}
	out := body(t, rr.Body)
	if n := len(out["differences"].([]any)); n != 2 || out["model_matches"] != true {
		t.Fatalf("preview: %s", rr.Body)
	}
	if rr = f.request(t, http.MethodPost, "/api/receiver/stage0/restore", []byte(`{"expected":3}`)); rr.Code != http.StatusConflict {
		t.Fatalf("stale preview accepted: %d %s", rr.Code, rr.Body)
	}
	if fake.ValSets != 0 {
		t.Fatal("refused restore wrote to the receiver")
	}
	if rr = f.request(t, http.MethodPost, "/api/receiver/stage0/restore", []byte(`{"expected":2}`)); rr.Code != http.StatusOK {
		t.Fatalf("restore %d: %s", rr.Code, rr.Body)
	}
	if fake.Get(0x209102CF)[0] != 0 || fake.Get(0x209102CD)[0] != 1 {
		t.Fatal("restore not applied to RAM")
	}
	if rr = f.request(t, http.MethodPost, "/api/receiver/revert", nil); rr.Code != http.StatusOK {
		t.Fatalf("revert %d: %s", rr.Code, rr.Body)
	}
	if fake.Get(0x209102CF)[0] != 1 || fake.Get(0x209102CD)[0] != 0 {
		t.Fatal("revert did not restore the pre-restore values")
	}
	fake.Sw = "EXT HPG 2.10 (other)"
	if rr = f.request(t, http.MethodPost, "/api/receiver/stage0/restore", []byte(`{"expected":2}`)); rr.Code != http.StatusConflict {
		t.Fatalf("restore across firmware versions: %d %s", rr.Code, rr.Body)
	}
}
