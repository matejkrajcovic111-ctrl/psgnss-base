// Package backup is the settings snapshot: everything an operator configured,
// in one file they can keep, and enough of it to rebuild this station on new
// hardware.
//
// # What is in it
//
// Configuration, administrators, NTRIP accounts and their access rules, the
// integrity monitor and the push-out targets. Not the telemetry history, the
// connection log, the receiver audit trail or the integrity run history: those
// are records of what happened, they are large, and restoring them onto another
// machine would be fiction. Not the active sessions either -- a restore must
// not hand out a login.
//
// # Why it is encrypted
//
// NTRIP passwords in this system are recoverable by design: administrators can
// read them in the UI, so they are stored as ciphertext under the station's
// master key rather than as hashes. A settings backup therefore contains live
// credentials. Two ways to handle that are wrong: shipping the master key with
// them (the file then unlocks itself), and leaving them out (the restore is
// useless). What happens instead is that the credentials travel as plaintext
// inside an envelope encrypted with a passphrase the operator chooses, and the
// receiving station re-seals them under its own master key. The station's key
// never leaves the machine, and the passphrase -- which is never stored -- is
// the only thing protecting the file.
//
// Say that plainly wherever this is offered: whoever has the file and the
// passphrase has every NTRIP password in it.
package backup

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/psgnss/psgnss-base/internal/config"
)

// FormatVersion is the envelope and document version. A file from a newer
// version is refused rather than half-read.
const FormatVersion = 1

// MinPassphrase is short enough to type on a phone and long enough that the
// argon2 cost is what an attacker has to pay. The file is only as safe as this.
const MinPassphrase = 12

var magic = [8]byte{'P', 'S', 'G', 'N', 'S', 'S', 'B', 'K'}

// Document is the snapshot itself.
type Document struct {
	Version     int            `json:"version"`
	Created     time.Time      `json:"created"`
	Station     string         `json:"station"`
	AppVersion  string         `json:"app_version"`
	Config      *config.Config `json:"config"`
	Admins      []Admin        `json:"admins"`
	Users       []User         `json:"users"`
	Integrity   *Integrity     `json:"integrity,omitempty"`
	PushTargets []PushTarget   `json:"push_targets,omitempty"`
}

// Admin carries the argon2id hash, not a password: administrator logins are
// hashed and there is nothing to recover.
type Admin struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	CreatedAt    int64  `json:"created_at"`
}

// User is one NTRIP account, with its password in the clear inside the
// envelope, because that is what the caster has to be able to check.
type User struct {
	Username        string   `json:"username"`
	Password        string   `json:"password"`
	ConnectionLimit int      `json:"connection_limit"`
	Enabled         bool     `json:"enabled"`
	Note            string   `json:"note"`
	Email           string   `json:"email"`
	ExpiresAt       *int64   `json:"expires_at,omitempty"`
	CreatedAt       int64    `json:"created_at"`
	Mountpoints     []string `json:"mountpoints,omitempty"`
	IPRules         []string `json:"ip_rules,omitempty"`
}

type Integrity struct {
	Enabled               bool   `json:"enabled"`
	Schedule              string `json:"schedule"`
	DurationMinutes       int    `json:"duration_minutes"`
	ToleranceHorizontalMM int    `json:"tolerance_horizontal_mm"`
	ToleranceVerticalMM   int    `json:"tolerance_vertical_mm"`
	Host                  string `json:"host"`
	Mountpoint            string `json:"mountpoint"`
	Username              string `json:"username"`
	Password              string `json:"password"`
}

type PushTarget struct {
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	Source     string `json:"source"`
	Host       string `json:"host"`
	Mountpoint string `json:"mountpoint"`
	Protocol   string `json:"protocol"`
	Username   string `json:"username"`
	Password   string `json:"password"`
}

// Summary is what can be said about a file before it is applied, so the
// operator confirms a restore against its actual contents.
type Summary struct {
	Version     int       `json:"version"`
	Created     time.Time `json:"created"`
	Station     string    `json:"station"`
	AppVersion  string    `json:"app_version"`
	Admins      int       `json:"admins"`
	Users       int       `json:"users"`
	Mountpoints int       `json:"mountpoints"`
	PushTargets int       `json:"push_targets"`
	HasConfig   bool      `json:"has_config"`
}

func (d *Document) Summary() Summary {
	s := Summary{Version: d.Version, Created: d.Created, Station: d.Station,
		AppVersion: d.AppVersion, Admins: len(d.Admins), Users: len(d.Users),
		PushTargets: len(d.PushTargets), HasConfig: d.Config != nil}
	if d.Config != nil {
		s.Mountpoints = len(d.Config.Caster.Mountpoint)
	}
	return s
}

// KDF cost. These are the same parameters the admin password hash uses, which
// is a deliberate 64 MiB on a Pi: it is paid once per backup or restore, and it
// is what stands between a stolen file and every password in it.
const (
	kdfTime    = 1
	kdfMemory  = 64 * 1024
	kdfThreads = 4
	keyLen     = 32
	saltLen    = 16
)

// Seal writes the encrypted envelope:
//
//	magic | version | salt | nonce | AES-256-GCM(gzip(JSON))
//
// The header is authenticated as additional data, so a file cannot be
// downgraded to an older format or have its salt swapped without the open
// failing.
func Seal(doc *Document, passphrase string) ([]byte, error) {
	if len([]rune(passphrase)) < MinPassphrase {
		return nil, fmt.Errorf("passphrase must be at least %d characters", MinPassphrase)
	}
	if doc == nil {
		return nil, errors.New("nothing to back up")
	}
	doc.Version = FormatVersion
	plain, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var zipped bytes.Buffer
	gz := gzip.NewWriter(&zipped)
	if _, err := gz.Write(plain); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	salt := make([]byte, saltLen)
	nonce := make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	aead, err := aeadFor(passphrase, salt)
	if err != nil {
		return nil, err
	}
	head := header(salt, nonce)
	return append(head, aead.Seal(nil, nonce, zipped.Bytes(), head)...), nil
}

// Open decrypts an envelope. A wrong passphrase and a tampered file are the
// same error on purpose: neither tells the caller which it was.
func Open(blob []byte, passphrase string) (*Document, error) {
	const headLen = 8 + 1 + saltLen + 12
	if len(blob) < headLen+16 {
		return nil, errors.New("this is not a PSGNSSB backup file")
	}
	if !bytes.Equal(blob[:8], magic[:]) {
		return nil, errors.New("this is not a PSGNSSB backup file")
	}
	if v := blob[8]; v != FormatVersion {
		return nil, fmt.Errorf("backup format version %d, but this build understands %d", v, FormatVersion)
	}
	salt := blob[9 : 9+saltLen]
	nonce := blob[9+saltLen : headLen]
	aead, err := aeadFor(passphrase, salt)
	if err != nil {
		return nil, err
	}
	zipped, err := aead.Open(nil, nonce, blob[headLen:], blob[:headLen])
	if err != nil {
		return nil, errors.New("wrong passphrase, or the file has been altered")
	}
	gz, err := gzip.NewReader(bytes.NewReader(zipped))
	if err != nil {
		return nil, err
	}
	// A compressed document that expands without bound is still a denial of
	// service even after it has been authenticated by the right passphrase.
	plain, err := io.ReadAll(io.LimitReader(gz, 32<<20))
	if err != nil {
		return nil, err
	}
	var doc Document
	if err := json.Unmarshal(plain, &doc); err != nil {
		return nil, fmt.Errorf("backup contents are not readable: %w", err)
	}
	if doc.Version != FormatVersion {
		return nil, fmt.Errorf("backup document version %d, but this build understands %d",
			doc.Version, FormatVersion)
	}
	if doc.Config != nil {
		// A restore writes this straight to the configuration file, so it has
		// to be a configuration this binary would accept at startup.
		if err := doc.Config.Validate(); err != nil {
			return nil, fmt.Errorf("the configuration in this backup is not valid: %w", err)
		}
	}
	return &doc, nil
}

func header(salt, nonce []byte) []byte {
	h := make([]byte, 0, 8+1+len(salt)+len(nonce))
	h = append(h, magic[:]...)
	h = append(h, byte(FormatVersion))
	h = append(h, salt...)
	return append(h, nonce...)
}

func aeadFor(passphrase string, salt []byte) (cipher.AEAD, error) {
	key := argon2.IDKey([]byte(passphrase), salt, kdfTime, kdfMemory, kdfThreads, keyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Filename is the name the browser saves, with the station and the day in it so
// a folder of these can be told apart.
func Filename(station string, at time.Time) string {
	name := "station"
	for _, r := range station {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			continue
		}
		if name == "station" {
			name = ""
		}
		name += string(r)
	}
	if name == "" {
		name = "station"
	}
	return fmt.Sprintf("psgnssb-%s-%s.psbk", name, at.UTC().Format("20060102-1504"))
}
