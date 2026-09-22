package store

import (
	"path/filepath"
	"testing"

	"github.com/psgnss/psgnss-base/internal/backup"
	"github.com/psgnss/psgnss-base/internal/secrets"
)

func backupFixture(t *testing.T) (*Store, *secrets.Keyring) {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	kr, err := secrets.Generate(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	return db, kr
}

// The point of a settings backup is that the station comes back: accounts,
// their access rules and every credential, usable against the new machine's own
// master key.
func TestSettingsSurviveExportAndImport(t *testing.T) {
	db, kr := backupFixture(t)
	if err := db.CreateAdmin("matej", "an-admin-password"); err != nil {
		t.Fatal(err)
	}
	u, err := db.CreateUser(kr, "rover", "rover-secret", 3, "field kit")
	if err != nil {
		t.Fatal(err)
	}
	_ = u
	if err := db.UpdateUser(kr, "rover", UserEdit{ConnectionLimit: 3, Enabled: true,
		Note: "field kit", Access: UserAccess{Mountpoints: []string{"Example_MSM7"},
			IPs: []string{"192.0.2.0/24"}}}); err != nil {
		t.Fatal(err)
	}
	ct, nonce, err := kr.Seal("reference-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveIntegritySettings(IntegritySettings{Enabled: true, Schedule: "03:00",
		DurationMinutes: 15, ToleranceHorizontalMM: 30, ToleranceVerticalMM: 50,
		Host: "caster.example:2101", Mountpoint: "REF_MSM7", Username: "op",
		PasswordEnc: ct, PasswordNonce: nonce}); err != nil {
		t.Fatal(err)
	}

	doc := &backup.Document{}
	if err := db.ExportSettings(kr, doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Admins) != 1 || len(doc.Users) != 1 {
		t.Fatalf("export found %d admins and %d users", len(doc.Admins), len(doc.Users))
	}
	if doc.Users[0].Password != "rover-secret" {
		t.Fatalf("the NTRIP password was not exported in a usable form: %q", doc.Users[0].Password)
	}
	if doc.Integrity == nil || doc.Integrity.Password != "reference-secret" {
		t.Fatalf("integrity credential not exported: %+v", doc.Integrity)
	}

	// A different station: its own database and, crucially, its own master key.
	other, otherKey := backupFixture(t)
	if err := other.ImportSettings(otherKey, doc); err != nil {
		t.Fatal(err)
	}
	pw, err := other.GetPassword(otherKey, "rover")
	if err != nil {
		t.Fatal(err)
	}
	if pw != "rover-secret" {
		t.Errorf("restored password = %q; it was not re-sealed under the new key", pw)
	}
	if _, err := other.AdminLogin("matej", "an-admin-password", "127.0.0.1", 3600); err != nil {
		t.Errorf("the administrator cannot sign in after a restore: %v", err)
	}
	access, err := other.GetUserAccess("rover")
	if err != nil {
		t.Fatal(err)
	}
	if len(access.Mountpoints) != 1 || len(access.IPs) != 1 {
		t.Errorf("access rules did not survive: %+v", access)
	}
	in, err := other.IntegritySettings()
	if err != nil {
		t.Fatal(err)
	}
	if !in.Enabled || in.Host != "caster.example:2101" || !in.HasPassword() {
		t.Errorf("integrity settings did not survive: %+v", in)
	}
}

// Import replaces rather than merges, and a document with no administrators
// would lock the station's owner out permanently.
func TestImportReplacesAndRefusesToLockYouOut(t *testing.T) {
	db, kr := backupFixture(t)
	if err := db.CreateAdmin("existing", "an-admin-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser(kr, "old-rover", "old-secret", 5, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.ImportSettings(kr, &backup.Document{}); err == nil {
		t.Fatal("a backup with no administrators was applied")
	}
	// Nothing may have changed after a refusal.
	if _, err := db.GetUser("old-rover"); err != nil {
		t.Fatalf("the refused import still altered the database: %v", err)
	}

	doc := &backup.Document{
		Admins: []backup.Admin{{Username: "restored", PasswordHash: mustHash(t, "another-password")}},
		Users:  []backup.User{{Username: "new-rover", Password: "new-secret", Enabled: true}},
	}
	if err := db.ImportSettings(kr, doc); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetUser("old-rover"); err == nil {
		t.Error("the previous NTRIP account is still present; import must replace, not merge")
	}
	if _, err := db.AdminLogin("existing", "an-admin-password", "127.0.0.1", 3600); err == nil {
		t.Error("the previous administrator still exists")
	}
	if _, err := db.AdminLogin("restored", "another-password", "127.0.0.1", 3600); err != nil {
		t.Errorf("the restored administrator cannot sign in: %v", err)
	}
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := hashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
