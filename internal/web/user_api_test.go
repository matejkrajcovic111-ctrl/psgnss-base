package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/secrets"
	"github.com/psgnss/psgnss-base/internal/store"
)

type userAPIFixture struct {
	server *Server
	store  *store.Store
	key    *secrets.Keyring
	cookie *http.Cookie
	userID int64
}

func newUserAPIFixture(t *testing.T) userAPIFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "web.db"))
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
	u, err := db.CreateUser(key, "test-rover", "fixture-old", 5, "original")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.DB().Exec(`INSERT INTO connections
		(user_id,username,mountpoint,client_ip,client_port,via_proxy,proxy_ip,user_agent,
		 ntrip_version,started_at,ended_at,bytes_sent,bytes_recv,nmea_count,disconnect_reason)
		VALUES
		(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?),
		(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.Username, "Test_MSM7", "192.0.2.2", 1002, 0, "", "agent-2", 2, 200, 230, 2000, 20, 2, "done",
		u.ID, u.Username, "Test_MSM4", "192.0.2.1", 1001, 1, "198.51.100.1", "agent-1", 1, 100, 110, 1000, 10, 1, "done")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(&Server{Store: db, Keyring: key})
	return userAPIFixture{server: srv, store: db, key: key,
		cookie: &http.Cookie{Name: sessionCookie, Value: token}, userID: u.ID}
}

func (f userAPIFixture) request(t *testing.T, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(f.cookie)
	rr := httptest.NewRecorder()
	f.server.mux.ServeHTTP(rr, req)
	return rr
}

func TestUserDetailAndUpdateAPI(t *testing.T) {
	f := newUserAPIFixture(t)
	update := []byte(`{
		"limit":2,"enabled":true,"note":"field rover","email":"rover@example.com",
		"expires_at":"2026-12-31","ip_rules":["192.0.2.42/24"],
		"mountpoints":["Test_MSM7"]
	}`)
	rr := f.request(t, http.MethodPut, "/api/users/test-rover", update)
	if rr.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rr.Code, rr.Body.String())
	}
	u, err := f.store.GetUser("test-rover")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != f.userID || u.ConnectionLimit != 2 || u.Email != "rover@example.com" ||
		u.ExpiresAt == nil || u.ExpiresAt.Format(time.RFC3339) != "2027-01-01T00:00:00Z" {
		t.Fatalf("wrong updated account: %+v", u)
	}
	if _, err := f.store.Authenticate(f.key, "test-rover", "fixture-old"); err != nil {
		t.Fatal("omitted password was not preserved")
	}

	rr = f.request(t, http.MethodGet, "/api/users/test-rover?limit=1&offset=1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response struct {
		User struct {
			Username string  `json:"username"`
			Password *string `json:"password"`
		} `json:"user"`
		Access struct {
			IPs         []string `json:"ip_rules"`
			Mountpoints []string `json:"mountpoints"`
		} `json:"access"`
		Stats struct {
			Count    int64    `json:"connection_count"`
			Bytes    int64    `json:"total_bytes_sent"`
			Duration int64    `json:"total_duration_s"`
			IPs      []string `json:"ips_seen"`
			Last     string   `json:"last_connection"`
		} `json:"stats"`
		History []struct {
			ID            int64  `json:"id"`
			BytesReceived int64  `json:"bytes_received"`
			ProxyIP       string `json:"proxy_ip"`
		} `json:"history"`
		Page struct {
			Limit, Offset int
			Total         int64
		} `json:"page"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.User.Username != "test-rover" || response.User.Password != nil ||
		response.Stats.Count != 2 || response.Stats.Bytes != 3000 || response.Stats.Duration != 40 ||
		len(response.Stats.IPs) != 2 || response.Stats.Last == "" ||
		len(response.History) != 1 || response.History[0].BytesReceived != 10 ||
		response.History[0].ProxyIP != "198.51.100.1" ||
		response.Page.Limit != 1 || response.Page.Offset != 1 || response.Page.Total != 2 {
		t.Fatalf("wrong detail response: %+v", response)
	}
	if len(response.Access.IPs) != 1 || response.Access.IPs[0] != "192.0.2.0/24" ||
		len(response.Access.Mountpoints) != 1 || response.Access.Mountpoints[0] != "Test_MSM7" {
		t.Fatalf("wrong access response: %+v", response.Access)
	}
}

func TestUserUpdateAPIValidationAndPassword(t *testing.T) {
	f := newUserAPIFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/users/test-rover", nil)
	rr := httptest.NewRecorder()
	f.server.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated detail status=%d", rr.Code)
	}
	for name, body := range map[string]string{
		"missing fields": `{"limit":2}`,
		"unknown field":  `{"limit":2,"enabled":true,"note":"","email":"","expires_at":null,"ip_rules":[],"mountpoints":[],"extra":1}`,
		"bad expiry":     `{"limit":2,"enabled":true,"note":"","email":"","expires_at":"tomorrow","ip_rules":[],"mountpoints":[]}`,
		"bad IP":         `{"limit":2,"enabled":true,"note":"","email":"","expires_at":null,"ip_rules":["bad"],"mountpoints":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			rr := f.request(t, http.MethodPut, "/api/users/test-rover", []byte(body))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
	u, _ := f.store.GetUser("test-rover")
	if u.ConnectionLimit != 5 || u.Note != "original" {
		t.Fatal("invalid API request changed account")
	}
	passwordUpdate := []byte(`{
		"password":"fixture-new","limit":3,"enabled":true,"note":"updated","email":"",
		"expires_at":null,"ip_rules":[],"mountpoints":[]
	}`)
	rr = f.request(t, http.MethodPut, "/api/users/test-rover", passwordUpdate)
	if rr.Code != http.StatusOK {
		t.Fatalf("password update status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := f.store.Authenticate(f.key, "test-rover", "fixture-old"); err != store.ErrNotFound {
		t.Fatal("old password still accepted")
	}
	if _, err := f.store.Authenticate(f.key, "test-rover", "fixture-new"); err != nil {
		t.Fatal("new password not accepted")
	}
	rr = f.request(t, http.MethodGet, "/api/users/missing", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing user status=%d", rr.Code)
	}
	rr = f.request(t, http.MethodGet, "/api/users/test-rover?limit=0", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad page status=%d", rr.Code)
	}
}
