// Package secrets manages the master key and the reversible encryption used
// for NTRIP passwords.
//
// NTRIP passwords are encrypted, not hashed, because the operator must be able
// to read them back to configure field equipment. That trade-off is documented
// in the README. Administrator login passwords are hashed separately and never
// pass through this package.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const KeyLen = 32 // AES-256

var ErrNoKey = errors.New("master key not found")

// Keyring holds the master key in memory.
type Keyring struct{ aead cipher.AEAD }

// Load reads the master key from path. The file must be root-only (0600);
// looser permissions are refused rather than silently accepted, since the key
// is equivalent to every NTRIP password at once.
func Load(path string) (*Keyring, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoKey
		}
		return nil, err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("master key %s has mode %04o; must be 0600 "+
			"(it is equivalent to every NTRIP password)", path, perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("master key is not hex: %w", err)
	}
	if len(key) != KeyLen {
		return nil, fmt.Errorf("master key is %d bytes, want %d", len(key), KeyLen)
	}
	return newKeyring(key)
}

// Generate creates a new master key at path with mode 0600. It refuses to
// overwrite an existing key, because doing so would make every stored password
// permanently unrecoverable.
func Generate(path string) (*Keyring, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("refusing to overwrite existing master key at %s "+
			"(every stored password would become unrecoverable)", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, KeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return newKeyring(key)
}

// LoadOrGenerate loads the key, creating it on first run.
func LoadOrGenerate(path string) (*Keyring, bool, error) {
	k, err := Load(path)
	if err == nil {
		return k, false, nil
	}
	if !errors.Is(err, ErrNoKey) {
		return nil, false, err
	}
	k, err = Generate(path)
	return k, true, err
}

func newKeyring(key []byte) (*Keyring, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Keyring{aead: aead}, nil
}

// Seal encrypts a password, returning ciphertext and nonce separately so they
// can be stored in distinct columns.
func (k *Keyring) Seal(plain string) (ct, nonce []byte, err error) {
	nonce = make([]byte, k.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return k.aead.Seal(nil, nonce, []byte(plain), nil), nonce, nil
}

// Open decrypts a password. GCM authenticates, so a tampered row fails rather
// than returning garbage.
func (k *Keyring) Open(ct, nonce []byte) (string, error) {
	if len(nonce) != k.aead.NonceSize() {
		return "", fmt.Errorf("bad nonce length %d", len(nonce))
	}
	pt, err := k.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt failed (wrong key or tampered record): %w", err)
	}
	return string(pt), nil
}
