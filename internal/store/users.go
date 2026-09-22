package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/psgnss/psgnss-base/internal/secrets"
)

var ErrNotFound = errors.New("not found")

// User is an NTRIP account.
type User struct {
	ID              int64
	Username        string
	ConnectionLimit int
	Enabled         bool
	Note            string
	Email           string
	ExpiresAt       *time.Time // nil means never
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// CreateUser stores a user with an encrypted password.
func (s *Store) CreateUser(kr *secrets.Keyring, username, password string, limit int, note string) (*User, error) {
	if username == "" {
		return nil, fmt.Errorf("username required")
	}
	if limit <= 0 {
		limit = 5
	}
	ct, nonce, err := kr.Seal(password)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO users
		(username,password_enc,password_nonce,connection_limit,enabled,note,created_at,updated_at)
		VALUES (?,?,?,?,1,?,?,?)`, username, ct, nonce, limit, note, now, now)
	if err != nil {
		return nil, fmt.Errorf("create user %q: %w", username, err)
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Username: username, ConnectionLimit: limit, Enabled: true,
		Note: note, CreatedAt: time.Unix(now, 0), UpdatedAt: time.Unix(now, 0)}, nil
}

// SetPassword replaces a user's password.
func (s *Store) SetPassword(kr *secrets.Keyring, username, password string) error {
	ct, nonce, err := kr.Seal(password)
	if err != nil {
		return err
	}
	r, err := s.db.Exec(`UPDATE users SET password_enc=?,password_nonce=?,updated_at=? WHERE username=?`,
		ct, nonce, time.Now().Unix(), username)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetPassword decrypts a user's password. This is what lets an admin read a
// password back out of the UI.
func (s *Store) GetPassword(kr *secrets.Keyring, username string) (string, error) {
	var ct, nonce []byte
	err := s.db.QueryRow(`SELECT password_enc,password_nonce FROM users WHERE username=?`,
		username).Scan(&ct, &nonce)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return kr.Open(ct, nonce)
}

// Authenticate verifies credentials in constant time relative to the password
// contents, returning the user on success.
func (s *Store) Authenticate(kr *secrets.Keyring, username, password string) (*User, error) {
	var (
		u            User
		ct, nonce    []byte
		created, upd int64
		enabled      int
		expiry       sql.NullInt64
	)
	err := s.db.QueryRow(`SELECT id,username,password_enc,password_nonce,connection_limit,
		enabled,note,created_at,updated_at,email,expires_at FROM users WHERE username=?`, username).
		Scan(&u.ID, &u.Username, &ct, &nonce, &u.ConnectionLimit, &enabled, &u.Note, &created, &upd, &u.Email, &expiry)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	want, err := kr.Open(ct, nonce)
	if err != nil {
		return nil, err
	}
	if !constantTimeEqual(want, password) {
		return nil, ErrNotFound
	}
	u.Enabled = enabled != 0
	u.CreatedAt, u.UpdatedAt = time.Unix(created, 0), time.Unix(upd, 0)
	u.ExpiresAt = expiryTime(expiry)
	return &u, nil
}

// constantTimeEqual avoids leaking password length or content through timing.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		// Still compare something so the fast path is not obviously shorter.
		var v byte
		for i := 0; i < len(a); i++ {
			v |= a[i]
		}
		_ = v
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// ListUsers returns all users.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id,username,connection_limit,enabled,note,created_at,updated_at,email,expires_at
		FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var enabled int
		var c, up int64
		var expiry sql.NullInt64
		if err := rows.Scan(&u.ID, &u.Username, &u.ConnectionLimit, &enabled, &u.Note, &c, &up, &u.Email, &expiry); err != nil {
			return nil, err
		}
		u.Enabled = enabled != 0
		u.CreatedAt, u.UpdatedAt = time.Unix(c, 0), time.Unix(up, 0)
		u.ExpiresAt = expiryTime(expiry)
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetEnabled enables or disables an account.
func (s *Store) SetEnabled(username string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE users SET enabled=?,updated_at=? WHERE username=?`,
		v, time.Now().Unix(), username)
	return err
}

// DeleteUser removes an account. Past connection rows survive, because they
// denormalise the username.
func (s *Store) DeleteUser(username string) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE username=?`, username)
	return err
}

// ---------------------------------------------------------------- accounting

// ConnStart records a new connection and returns its row id.
func (s *Store) ConnStart(userID int64, username, mountpoint, clientIP string,
	clientPort int, viaProxy bool, proxyIP, userAgent string, ntripVer int) (int64, error) {
	vp := 0
	if viaProxy {
		vp = 1
	}
	var uid any = userID
	if userID == 0 {
		uid = nil
	}
	res, err := s.db.Exec(`INSERT INTO connections
		(user_id,username,mountpoint,client_ip,client_port,via_proxy,proxy_ip,user_agent,
		 ntrip_version,started_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		uid, username, mountpoint, clientIP, clientPort, vp, proxyIP, userAgent,
		ntripVer, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ConnEnd closes out a connection with its byte counts.
func (s *Store) ConnEnd(id int64, bytesSent, bytesRecv, nmeaCount int64, reason string) error {
	_, err := s.db.Exec(`UPDATE connections SET ended_at=?,bytes_sent=?,bytes_recv=?,
		nmea_count=?,disconnect_reason=? WHERE id=?`,
		time.Now().Unix(), bytesSent, bytesRecv, nmeaCount, reason, id)
	return err
}

// ActiveConnections counts a user's currently-open connections, for limit
// enforcement.
func (s *Store) ActiveConnections(username string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM connections WHERE username=? AND ended_at IS NULL`,
		username).Scan(&n)
	return n, err
}

// ActiveConnectionsForUser counts by account identity. This avoids a newly
// created account inheriting still-open sessions from a deleted account that
// happened to use the same username.
func (s *Store) ActiveConnectionsForUser(userID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM connections WHERE user_id=? AND ended_at IS NULL`,
		userID).Scan(&n)
	return n, err
}

// CloseStaleConnections marks connections left open by an unclean shutdown.
// Without this, limit enforcement would count ghosts forever.
func (s *Store) CloseStaleConnections() (int64, error) {
	r, err := s.db.Exec(`UPDATE connections SET ended_at=started_at,
		disconnect_reason='stale: daemon restarted' WHERE ended_at IS NULL`)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// Connection is one accounting row.
type Connection struct {
	ID               int64
	Username         string
	Mountpoint       string
	ClientIP         string
	ClientPort       int
	ViaProxy         bool
	ProxyIP          string
	UserAgent        string
	NtripVersion     int
	StartedAt        time.Time
	EndedAt          *time.Time
	BytesSent        int64
	BytesReceived    int64
	NMEACount        int64
	DisconnectReason string
}

// RecentConnections returns the most recent connections, newest first.
func (s *Store) RecentConnections(limit int) ([]Connection, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id,username,mountpoint,client_ip,via_proxy,user_agent,
		started_at,ended_at,bytes_sent FROM connections ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Connection
	for rows.Next() {
		var c Connection
		var vp int
		var st int64
		var en sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Username, &c.Mountpoint, &c.ClientIP, &vp,
			&c.UserAgent, &st, &en, &c.BytesSent); err != nil {
			return nil, err
		}
		c.ViaProxy = vp != 0
		c.StartedAt = time.Unix(st, 0)
		if en.Valid {
			t := time.Unix(en.Int64, 0)
			c.EndedAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
