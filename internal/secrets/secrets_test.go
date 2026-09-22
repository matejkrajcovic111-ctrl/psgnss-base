package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	kr, err := Generate(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pw := range []string{"130599", "", "a very long passphrase with spaces", "ünïcødé"} {
		ct, nonce, err := kr.Seal(pw)
		if err != nil {
			t.Fatal(err)
		}
		if string(ct) == pw && pw != "" {
			t.Error("ciphertext equals plaintext")
		}
		got, err := kr.Open(ct, nonce)
		if err != nil {
			t.Fatal(err)
		}
		if got != pw {
			t.Errorf("round trip = %q, want %q", got, pw)
		}
	}
}

func TestTamperedCiphertextFails(t *testing.T) {
	kr, _ := Generate(filepath.Join(t.TempDir(), "k"))
	ct, nonce, _ := kr.Seal("secret")
	ct[0] ^= 0xFF
	if _, err := kr.Open(ct, nonce); err == nil {
		t.Error("tampered ciphertext decrypted without error; GCM auth not enforced")
	}
}

func TestRefusesLoosePermissions(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "k")
	if _, err := Generate(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("loaded a world-readable master key; permissions must be enforced")
	}
}

func TestRefusesToOverwriteKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	if _, err := Generate(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(p); err == nil {
		t.Error("overwrote an existing master key; every stored password would be lost")
	}
}

func TestDifferentKeysCannotDecrypt(t *testing.T) {
	d := t.TempDir()
	a, _ := Generate(filepath.Join(d, "a"))
	b, _ := Generate(filepath.Join(d, "b"))
	ct, nonce, _ := a.Seal("secret")
	if _, err := b.Open(ct, nonce); err == nil {
		t.Error("a different key decrypted the ciphertext")
	}
}
