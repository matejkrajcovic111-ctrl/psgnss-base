package store

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// Admin login passwords are HASHED, unlike NTRIP passwords. Nobody needs to
// read an admin password back, so there is no reason to keep it recoverable.

const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16
)

type Admin struct {
	Username  string     `json:"username"`
	CreatedAt time.Time  `json:"created_at"`
	LastLogin *time.Time `json:"last_login,omitempty"`
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$%d$%d$%d$%s$%s", argonTime, argonMemory, argonThreads,
		hex.EncodeToString(salt), hex.EncodeToString(key)), nil
}

func verifyPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[0] != "argon2id" {
		return false
	}
	var t, m, p int
	fmt.Sscanf(parts[1], "%d", &t)
	fmt.Sscanf(parts[2], "%d", &m)
	fmt.Sscanf(parts[3], "%d", &p)
	salt, err1 := hex.DecodeString(parts[4])
	want, err2 := hex.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, uint32(t), uint32(m), uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// CreateAdmin adds an administrator.
func (s *Store) CreateAdmin(username, password string) error {
	h, err := hashPassword(password)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO admins (username,password_hash,created_at) VALUES (?,?,?)`,
		username, h, time.Now().Unix())
	return err
}

// SetAdminPassword changes an administrator's password.
func (s *Store) SetAdminPassword(username, password string) error {
	h, err := hashPassword(password)
	if err != nil {
		return err
	}
	r, err := s.db.Exec(`UPDATE admins SET password_hash=? WHERE username=?`, h, username)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// HasAdmin reports whether any administrator exists.
func (s *Store) HasAdmin() (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM admins`).Scan(&n)
	return n > 0, err
}

func (s *Store) ListAdmins() ([]Admin, error) {
	rows, err := s.db.Query(`SELECT username,created_at,last_login FROM admins ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Admin
	for rows.Next() {
		var a Admin
		var created int64
		var last sql.NullInt64
		if err := rows.Scan(&a.Username, &created, &last); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(created, 0).UTC()
		if last.Valid {
			t := time.Unix(last.Int64, 0).UTC()
			a.LastLogin = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAdmin(username string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM admins`).Scan(&count); err != nil {
		return err
	}
	if count <= 1 {
		return errors.New("cannot delete the last administrator")
	}
	r, err := tx.Exec(`DELETE FROM admins WHERE username=?`, username)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// AdminLogin verifies credentials and issues a session token.
func (s *Store) AdminLogin(username, password, clientIP string, ttl time.Duration) (string, error) {
	var id int64
	var hash string
	err := s.db.QueryRow(`SELECT id,password_hash FROM admins WHERE username=?`,
		username).Scan(&id, &hash)
	if err == sql.ErrNoRows {
		// Hash anyway so a missing user and a wrong password take similar time.
		verifyPassword("argon2id$1$65536$4$00$00", password)
		return "", errors.New("invalid credentials")
	}
	if err != nil {
		return "", err
	}
	if !verifyPassword(hash, password) {
		return "", errors.New("invalid credentials")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now()
	if _, err := s.db.Exec(`INSERT INTO sessions (token,admin_id,created_at,expires_at,client_ip)
		VALUES (?,?,?,?,?)`, tok, id, now.Unix(), now.Add(ttl).Unix(), clientIP); err != nil {
		return "", err
	}
	_, _ = s.db.Exec(`UPDATE admins SET last_login=? WHERE id=?`, now.Unix(), id)
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now.Unix())
	return tok, nil
}

// AdminBySession resolves a session token to a username.
func (s *Store) AdminBySession(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	var name string
	var exp int64
	err := s.db.QueryRow(`SELECT a.username, s.expires_at FROM sessions s
		JOIN admins a ON a.id = s.admin_id WHERE s.token = ?`, token).Scan(&name, &exp)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	return name, true
}

// AdminLogout invalidates a session.
func (s *Store) AdminLogout(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token=?`, token)
	return err
}
