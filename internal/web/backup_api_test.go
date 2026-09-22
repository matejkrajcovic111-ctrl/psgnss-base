package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/secrets"
	"github.com/psgnss/psgnss-base/internal/store"
)

type backupFixture struct {
	srv        *Server
	cookie     *http.Cookie
	configPath string
}

func newBackupFixture(t *testing.T) backupFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	key, err := secrets.Generate(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateAdmin("fixture-admin", "fixture-admin-password"); err != nil {
		t.Fatal(err)
	}
	tok, err := db.AdminLogin("fixture-admin", "fixture-admin-password", "127.0.0.1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser(key, "rover", "rover-secret", 4, "kept"); err != nil {
		t.Fatal(err)
	}
	carrier := 3
	cfg := &config.Config{
		Station:  config.Station{Name: "Example", Position: config.Position{Mode: "fixed", Format: "llh", Latitude: 48, Longitude: 17, Height: 250}},
		Receiver: config.Receiver{Device: "/dev/ttyACM0", Baud: 460800, Model: "ZED-X20P"},
		Hub:      config.Hub{Input: "serial"},
		Caster: config.Caster{NtripV1: true, FormatString: "RTCM 3.2", Carrier: 3,
			Mountpoint: []config.Mountpoint{{Name: "TEST_MSM7", SourceID: 1, Carrier: &carrier,
				NavSystem: "GPS+GAL", Auth: "B", Fee: "N",
				Messages: []config.MessageSel{{Type: 1006, Interval: 10}, {Type: 1077, Interval: 1}}}}},
		RINEX:     config.RINEX{Frequencies: 4},
		Telemetry: config.Telemetry{DBPath: filepath.Join(dir, "state.db")},
	}
	path := filepath.Join(dir, "psgnss.toml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	srv := New(&Server{Store: db, Keyring: key, Cfg: cfg, ConfigPath: path})
	return backupFixture{srv: srv, cookie: &http.Cookie{Name: sessionCookie, Value: tok}, configPath: path}
}

func (f backupFixture) post(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(f.cookie)
	rr := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rr, req)
	return rr
}

// The whole point: take a backup, change the station, put the backup back, and
// find the station as it was -- including a credential the caster can still use.
func TestBackupDownloadInspectAndRestore(t *testing.T) {
	f := newBackupFixture(t)
	const pass = "a-long-enough-passphrase"

	rr := f.post(t, "/api/backup", map[string]string{"passphrase": pass})
	if rr.Code != 200 {
		t.Fatalf("backup = %d %s", rr.Code, rr.Body.String())
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "psgnssb-Example-") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	blob := rr.Body.Bytes()
	if bytes.Contains(blob, []byte("rover-secret")) {
		t.Fatal("the downloaded file carries a password in the clear")
	}
	encoded := base64.StdEncoding.EncodeToString(blob)

	rr = f.post(t, "/api/backup/inspect", map[string]string{"passphrase": pass, "data": encoded})
	if rr.Code != 200 {
		t.Fatalf("inspect = %d %s", rr.Code, rr.Body.String())
	}
	var sum struct {
		Station string `json:"station"`
		Users   int    `json:"users"`
		Admins  int    `json:"admins"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Station != "Example" || sum.Users != 1 || sum.Admins != 1 {
		t.Fatalf("inspect reported %+v", sum)
	}
	// A wrong passphrase must not say which part was wrong, and must not apply.
	if rr := f.post(t, "/api/backup/inspect", map[string]string{"passphrase": "a-long-wrong-passphrase",
		"data": encoded}); rr.Code != 400 {
		t.Errorf("inspect with a wrong passphrase = %d", rr.Code)
	}

	// Drift: an account added after the backup, and a changed station name.
	if _, err := f.srv.Store.CreateUser(f.srv.Keyring, "temporary", "temp-secret", 1, ""); err != nil {
		t.Fatal(err)
	}
	changed := *f.srv.Cfg
	changed.Station.Name = "Renamed"
	if err := config.Save(f.configPath, &changed); err != nil {
		t.Fatal(err)
	}

	rr = f.post(t, "/api/backup/restore", map[string]string{"passphrase": pass, "data": encoded})
	if rr.Code != 200 {
		t.Fatalf("restore = %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"signed_out":true`) {
		t.Error("the restore did not warn that it drops every session")
	}
	if _, err := f.srv.Store.GetUser("temporary"); err == nil {
		t.Error("the account added after the backup survived the restore")
	}
	if pw, err := f.srv.Store.GetPassword(f.srv.Keyring, "rover"); err != nil || pw != "rover-secret" {
		t.Errorf("restored password = %q, %v", pw, err)
	}
	written, err := config.Load(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if written.Station.Name != "Example" {
		t.Errorf("the configuration was not restored: station = %q", written.Station.Name)
	}
	// And a rollback snapshot of what was replaced exists, readable only by the
	// daemon's user.
	var snapshot struct {
		Rollback string `json:"rollback_snapshot"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(snapshot.Rollback)
	if err != nil {
		t.Fatalf("no rollback snapshot was written: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("rollback snapshot mode = %04o", st.Mode().Perm())
	}
}

// A short passphrase must be refused where it is chosen, and a file that is not
// a backup must not reach the database at all.
func TestBackupRefusesWeakPassphraseAndRubbish(t *testing.T) {
	f := newBackupFixture(t)
	if rr := f.post(t, "/api/backup", map[string]string{"passphrase": "short"}); rr.Code != 400 {
		t.Errorf("a five-character passphrase was accepted: %d", rr.Code)
	}
	rubbish := base64.StdEncoding.EncodeToString([]byte("this is not a backup"))
	if rr := f.post(t, "/api/backup/restore", map[string]string{"passphrase": "a-long-enough-passphrase",
		"data": rubbish}); rr.Code != 400 {
		t.Errorf("arbitrary bytes were accepted for restore: %d", rr.Code)
	}
	if _, err := f.srv.Store.GetUser("rover"); err != nil {
		t.Errorf("the refused restore still touched the database: %v", err)
	}
}
