package store

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/secrets"
)

func testStore(t *testing.T) (*Store, *secrets.Keyring) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	kr, err := secrets.Generate(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	return s, kr
}

func TestUserEditPreservesIdentityAndHistory(t *testing.T) {
	s, kr := testStore(t)
	before, err := s.CreateUser(kr, "test-rover", "fixture-old", 5, "original")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.ConnStart(before.ID, before.Username, "Test_MSM7", "192.0.2.10", 1234, false, "", "fixture", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConnEnd(conn, 12345, 0, 0, "fixture"); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	edit := UserEdit{ConnectionLimit: 2, Enabled: true, Email: "rover@example.com", Note: "updated", ExpiresAt: &expiry,
		Access: UserAccess{IPs: []string{"192.0.2.42/24", "192.0.2.0/24", "2001:db8::1", "::ffff:198.51.100.7"}, Mountpoints: []string{"Test_MSM7", "Test_MSM7"}}}
	if err := s.UpdateUser(nil, before.Username, edit); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetUser(before.Username)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID || !after.CreatedAt.Equal(before.CreatedAt) || after.ConnectionLimit != 2 || after.Email != edit.Email || after.Note != edit.Note || !after.ExpiresAt.Equal(expiry) {
		t.Fatalf("metadata not preserved/updated: %+v", after)
	}
	auth, err := s.Authenticate(kr, before.Username, "fixture-old")
	if err != nil || auth.Email != edit.Email || !auth.ExpiresAt.Equal(expiry) {
		t.Fatalf("authenticate metadata: %v", err)
	}
	users, err := s.ListUsers()
	if err != nil || len(users) != 1 || !users[0].ExpiresAt.Equal(expiry) {
		t.Fatalf("list: %v", err)
	}
	access, err := s.GetUserAccess(before.Username)
	if err != nil {
		t.Fatal(err)
	}
	want := UserAccess{IPs: []string{"192.0.2.0/24", "198.51.100.7/32", "2001:db8::1/128"}, Mountpoints: []string{"Test_MSM7"}}
	if !reflect.DeepEqual(access, want) {
		t.Fatalf("access got %+v want %+v", access, want)
	}
	history, err := s.RecentConnections(10)
	if err != nil || len(history) != 1 || history[0].ID != conn || history[0].BytesSent != 12345 {
		t.Fatalf("history changed: %v", err)
	}
	pw := "fixture-new"
	edit.Password = &pw
	edit.ExpiresAt = nil
	edit.Access = UserAccess{}
	if err := s.UpdateUser(kr, before.Username, edit); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(kr, before.Username, "fixture-old"); err != ErrNotFound {
		t.Fatal("old password still accepted")
	}
	if _, err := s.Authenticate(kr, before.Username, pw); err != nil {
		t.Fatal(err)
	}
	after, _ = s.GetUser(before.Username)
	access, _ = s.GetUserAccess(before.Username)
	if after.ExpiresAt != nil || len(access.IPs) != 0 || len(access.Mountpoints) != 0 {
		t.Fatal("defaults not restored")
	}
}

func TestUserAccessPolicy(t *testing.T) {
	a := UserAccess{}
	if !a.AllowsMountpoint("Any") {
		t.Fatal("empty mountpoint list must allow all")
	}
	if ok, err := a.AllowsIP("not-an-address"); !ok || err != nil {
		t.Fatalf("empty IP list must remain unrestricted: ok=%v err=%v", ok, err)
	}
	a = UserAccess{
		IPs:         []string{"192.0.2.0/24", "2001:db8::/48"},
		Mountpoints: []string{"Test_MSM7"},
	}
	for _, test := range []struct {
		ip   string
		want bool
	}{
		{"192.0.2.42", true}, {"192.0.3.1", false},
		{"2001:db8::5", true}, {"2001:db9::5", false},
	} {
		got, err := a.AllowsIP(test.ip)
		if err != nil || got != test.want {
			t.Errorf("AllowsIP(%q)=%v,%v want %v,nil", test.ip, got, err, test.want)
		}
	}
	if a.AllowsMountpoint("Test_MSM4") || !a.AllowsMountpoint("Test_MSM7") {
		t.Fatal("mountpoint allowlist not enforced")
	}
	if _, err := a.AllowsIP("invalid"); err == nil {
		t.Fatal("restricted policy accepted malformed client IP")
	}
	if _, err := (UserAccess{IPs: []string{"invalid"}}).AllowsIP("192.0.2.1"); err == nil {
		t.Fatal("malformed stored rule was silently ignored")
	}
}

func TestUserStatsAndHistory(t *testing.T) {
	s, kr := testStore(t)
	u, err := s.CreateUser(kr, "test-rover", "fixture-password", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(kr, "other-rover", "fixture-password", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO connections
		(id,user_id,username,mountpoint,client_ip,client_port,via_proxy,proxy_ip,
		 user_agent,ntrip_version,started_at,ended_at,bytes_sent,bytes_recv,nmea_count,disconnect_reason)
		VALUES
		(10,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?),
		(11,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?),
		(12,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.Username, "Test_MSM7", "192.0.2.2", 1002, 0, "", "agent-2", 2, 200, 230, 2000, 20, 2, "done",
		u.ID, u.Username, "Test_MSM4", "192.0.2.1", 1001, 1, "198.51.100.1", "agent-1", 1, 100, 110, 1000, 10, 1, "done",
		other.ID, other.Username, "Other", "203.0.113.1", 1000, 0, "", "other", 1, 1, 999, 9999, 99, 0, "done")
	if err != nil {
		t.Fatal(err)
	}
	stats, err := s.UserStats(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ConnectionCount != 2 || stats.Active != 0 || stats.BytesSent != 3000 ||
		stats.BytesReceived != 30 || stats.DurationSeconds != 40 ||
		stats.FirstConnection.Unix() != 100 || stats.LastConnection.Unix() != 200 ||
		!reflect.DeepEqual(stats.IPs, []string{"192.0.2.1", "192.0.2.2"}) {
		t.Fatalf("wrong stats: %+v", stats)
	}
	page, err := s.UserConnections(u.ID, 1, 0)
	if err != nil || len(page) != 1 || page[0].ID != 10 || page[0].BytesReceived != 20 || page[0].NtripVersion != 2 {
		t.Fatalf("first history page: %+v err=%v", page, err)
	}
	page, err = s.UserConnections(u.ID, 1, 1)
	if err != nil || len(page) != 1 || page[0].ID != 11 || page[0].ProxyIP != "198.51.100.1" {
		t.Fatalf("second history page: %+v err=%v", page, err)
	}
}

func TestUserEditRollback(t *testing.T) {
	s, kr := testStore(t)
	u, err := s.CreateUser(kr, "test-rover", "fixture-old", 5, "original")
	if err != nil {
		t.Fatal(err)
	}
	original := UserEdit{ConnectionLimit: 5, Enabled: true, Note: "original", Access: UserAccess{IPs: []string{"192.0.2.1"}, Mountpoints: []string{"Test_MSM4"}}}
	if err := s.UpdateUser(nil, u.Username, original); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetUser(u.Username)
	accessBefore, _ := s.GetUserAccess(u.Username)
	check := func() {
		t.Helper()
		after, _ := s.GetUser(u.Username)
		access, _ := s.GetUserAccess(u.Username)
		if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(accessBefore, access) {
			t.Fatal("failed edit modified account")
		}
		if _, err := s.Authenticate(kr, u.Username, "fixture-old"); err != nil {
			t.Fatal("password changed on failed edit")
		}
	}
	empty := ""
	cases := []UserEdit{
		{ConnectionLimit: 0}, {ConnectionLimit: 1, Password: &empty}, {ConnectionLimit: 1, Email: "not-an-email"},
		{ConnectionLimit: 1, Access: UserAccess{IPs: []string{"invalid"}}},
		{ConnectionLimit: 1, Access: UserAccess{Mountpoints: []string{"bad/name"}}},
	}
	for _, edit := range cases {
		if err := s.UpdateUser(kr, u.Username, edit); err == nil {
			t.Fatal("invalid edit accepted")
		}
		check()
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_test_mount BEFORE INSERT ON user_mountpoints BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	pw := "fixture-new"
	edit := UserEdit{Password: &pw, ConnectionLimit: 1, Enabled: false, Access: UserAccess{Mountpoints: []string{"Test_MSM7"}}}
	if err := s.UpdateUser(kr, u.Username, edit); err == nil {
		t.Fatal("expected injected failure")
	}
	check()
	if err := s.UpdateUser(nil, "missing", original); err != ErrNotFound {
		t.Fatalf("missing account: %v", err)
	}
	if _, err := s.GetUserAccess("missing"); err != ErrNotFound {
		t.Fatalf("missing access: %v", err)
	}
	if err := s.DeleteUser(u.Username); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_ip_rules`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cascade: n=%d err=%v", n, err)
	}
}

// Fingerprint only legacy columns, without ever logging account data.
func legacyFingerprint(t *testing.T, db *sql.DB) [32]byte {
	t.Helper()
	h := sha256.New()
	for _, q := range []string{
		`SELECT id,username,password_enc,password_nonce,connection_limit,enabled,note,created_at,updated_at FROM users ORDER BY id`,
		`SELECT * FROM connections ORDER BY id`, `SELECT * FROM user_mountpoints ORDER BY user_id,mountpoint`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(h, "%#v\n", vals)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func verifyV1Migration(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("expected v1 fixture: %d %v", version, err)
	}
	before := legacyFingerprint(t, db)
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if after := legacyFingerprint(t, s.db); after != before {
		t.Fatal("migration altered legacy account/history data")
	}
	users, err := s.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Email != "" || u.ExpiresAt != nil {
			t.Fatal("migration did not preserve unrestricted defaults")
		}
		a, err := s.GetUserAccess(u.Username)
		if err != nil || len(a.IPs) != 0 {
			t.Fatal("migration added IP restrictions")
		}
	}
	var check string
	if err := s.db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity: %s %v", check, err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := s.Version()
	if err != nil || v != SchemaVersion {
		t.Fatalf("version %d %v", v, err)
	}
	if after := legacyFingerprint(t, s.db); after != before {
		t.Fatal("reopen altered legacy data")
	}
	t.Logf("v1 to v%d migration and reopen preserved %d users and all legacy account/history data; integrity_check=ok", v, len(users))
}

func TestV1Migration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version VALUES(1,1); INSERT INTO users VALUES(7,'fixture',X'0102',X'0304',5,1,'original',1,1); INSERT INTO user_mountpoints VALUES(7,'Test_MSM7'); INSERT INTO connections(id,user_id,username,mountpoint,client_ip,started_at,ended_at,bytes_sent) VALUES(9,7,'fixture','Test_MSM7','192.0.2.1',1,4,12345)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	verifyV1Migration(t, path)
}

func TestFutureSchemaRejectedWithoutChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER, applied_at INTEGER); INSERT INTO schema_version VALUES(999,1)`); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("accepted newer schema")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rejected migration mutated schema: %d %v", n, err)
	}
}

// Opt-in on-device check. Use only a disposable SQLite backup, never the live DB.
func TestProductionCopyMigration(t *testing.T) {
	path := os.Getenv("PSGNSS_TEST_DB_COPY")
	if path == "" {
		t.Skip("set PSGNSS_TEST_DB_COPY to a disposable v1 backup")
	}
	if filepath.Base(path) != "store-v1-copy.db" {
		t.Fatal("refusing path without expected disposable-copy filename")
	}
	verifyV1Migration(t, path)
}
