package store

import (
	"path/filepath"
	"testing"
)

func TestAdminManagementKeepsOneAdministrator(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "admins.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateAdmin("one", "password-one"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAdmin("one"); err == nil {
		t.Fatal("deleted last administrator")
	}
	if err := s.CreateAdmin("two", "password-two"); err != nil {
		t.Fatal(err)
	}
	admins, err := s.ListAdmins()
	if err != nil || len(admins) != 2 {
		t.Fatalf("admins=%v err=%v", admins, err)
	}
	if err := s.SetAdminPassword("two", "replacement-password"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAdmin("one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminLogin("two", "replacement-password", "127.0.0.1", 1); err != nil {
		t.Fatal(err)
	}
}
