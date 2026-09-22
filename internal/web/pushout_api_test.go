package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/secrets"
	"github.com/psgnss/psgnss-base/internal/store"
)

// pushFixture is a server with two configured mountpoints, one of them
// disabled, so the API's "must name an enabled local mountpoint" rule is
// exercised against something realistic.
func newPushFixture(t *testing.T) (userAPIFixture, *Server) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "push.db"))
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
	token, err := db.AdminLogin("fixture-admin", "fixture-admin-password", "127.0.0.1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Caster.Mountpoint = []config.Mountpoint{
		{Name: "Example_MSM7"},
		{Name: "Example_OLD", Disabled: true},
	}
	srv := New(&Server{Store: db, Keyring: key, Cfg: cfg})
	return userAPIFixture{server: srv, store: db, key: key,
		cookie: &http.Cookie{Name: sessionCookie, Value: token}}, srv
}

func TestPushoutSaveEncryptsPasswordAndLists(t *testing.T) {
	f, _ := newPushFixture(t)
	body := []byte(`{"targets":[{"name":"centipede","enabled":true,"source":"Example_MSM7",
		"host":"caster.example.org:2101","mountpoint":"REMOTE1","protocol":"v2",
		"username":"pushuser","password":"push-secret"}]}`)
	rr := f.request(t, http.MethodPut, "/api/pushout", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	stored, err := f.store.PushTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || !stored[0].HasPassword {
		t.Fatalf("target not stored: %+v", stored)
	}
	if string(stored[0].PasswordEnc) == "push-secret" {
		t.Fatal("password was stored in clear")
	}
	pw, err := f.key.Open(stored[0].PasswordEnc, stored[0].PasswordNonce)
	if err != nil || pw != "push-secret" {
		t.Fatalf("password does not open: %q %v", pw, err)
	}
	// The listing must never carry the secret back out.
	if body := rr.Body.String(); contains(body, "push-secret") {
		t.Fatal("response echoed the password")
	}
	var out struct {
		Targets []map[string]any `json:"targets"`
		Sources []string         `json:"sources"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) != 1 || out.Sources[0] != "Example_MSM7" {
		t.Errorf("disabled mountpoint offered as a source: %v", out.Sources)
	}
	if out.Targets[0]["has_password"] != true || out.Targets[0]["state"] != "stopped" {
		t.Errorf("unexpected row: %+v", out.Targets[0])
	}
}

func TestPushoutRejectsBadInput(t *testing.T) {
	f, _ := newPushFixture(t)
	cases := []struct{ name, body, want string }{
		{"no port", `{"targets":[{"name":"a","source":"Example_MSM7","host":"caster.example.org","mountpoint":"M","protocol":"v1"}]}`, "host:port"},
		{"disabled source", `{"targets":[{"name":"a","source":"Example_OLD","host":"h:1","mountpoint":"M","protocol":"v1"}]}`, "not an enabled local mountpoint"},
		{"unknown source", `{"targets":[{"name":"a","source":"Nope","host":"h:1","mountpoint":"M","protocol":"v1"}]}`, "not an enabled local mountpoint"},
		{"bad protocol", `{"targets":[{"name":"a","source":"Example_MSM7","host":"h:1","mountpoint":"M","protocol":"v3"}]}`, "protocol must be"},
		{"v2 without user", `{"targets":[{"name":"a","source":"Example_MSM7","host":"h:1","mountpoint":"M","protocol":"v2"}]}`, "needs a username"},
		{"injection in mountpoint", `{"targets":[{"name":"a","source":"Example_MSM7","host":"h:1","mountpoint":"M HTTP/1.1","protocol":"v1"}]}`, "unsupported characters"},
		{"enabled without password", `{"targets":[{"name":"a","enabled":true,"source":"Example_MSM7","host":"h:1","mountpoint":"M","protocol":"v1"}]}`, "password is required"},
		{"unknown field", `{"targets":[{"name":"a","source":"Example_MSM7","host":"h:1","mountpoint":"M","protocol":"v1","rtcm_msg":"1004"}]}`, "bad request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := f.request(t, http.MethodPut, "/api/pushout", []byte(c.body))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			if !contains(rr.Body.String(), c.want) {
				t.Errorf("error %q does not mention %q", rr.Body.String(), c.want)
			}
		})
	}
	stored, err := f.store.PushTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Errorf("a rejected save still wrote %d targets", len(stored))
	}
}

func TestPushoutEditKeepsPasswordAndCanDisable(t *testing.T) {
	f, _ := newPushFixture(t)
	create := []byte(`{"targets":[{"name":"net","enabled":true,"source":"Example_MSM7","host":"h:2101",
		"mountpoint":"M1","protocol":"v1","password":"pw"}]}`)
	if rr := f.request(t, http.MethodPut, "/api/pushout", create); rr.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	edit := []byte(`{"targets":[{"name":"net","enabled":false,"source":"Example_MSM7","host":"h:2101",
		"mountpoint":"M2","protocol":"v1"}]}`)
	if rr := f.request(t, http.MethodPut, "/api/pushout", edit); rr.Code != http.StatusOK {
		t.Fatalf("edit status=%d body=%s", rr.Code, rr.Body.String())
	}
	stored, err := f.store.PushTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Mountpoint != "M2" || stored[0].Enabled {
		t.Fatalf("edit did not apply: %+v", stored)
	}
	if !stored[0].HasPassword {
		t.Error("editing a target discarded its stored password")
	}
}

func TestPushoutRequiresAuth(t *testing.T) {
	f, srv := newPushFixture(t)
	_ = f
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rr := anonymousRequest(t, srv, method, "/api/pushout")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s /api/pushout without a session: status=%d", method, rr.Code)
		}
	}
}

// anonymousRequest calls an endpoint with no session cookie.
func anonymousRequest(t *testing.T, srv *Server, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader([]byte(`{"targets":[]}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req)
	return rr
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
