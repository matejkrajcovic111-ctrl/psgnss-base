package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"net/netip"
	"strings"
	"time"

	"github.com/psgnss/psgnss-base/internal/secrets"
)

var ErrInvalidUser = errors.New("invalid user settings")

func invalidUser(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidUser, fmt.Sprintf(format, args...))
}

// UserAccess is an allowlist enforced by the caster. Empty lists mean
// unrestricted access. Addresses are stored as canonical CIDRs, including
// individual IPv4 and IPv6 hosts.
type UserAccess struct {
	IPs         []string
	Mountpoints []string
}

// AllowsMountpoint reports whether a mountpoint passes this allowlist. An
// empty allowlist means all mountpoints, matching the schema's documented
// backwards-compatible default.
func (a UserAccess) AllowsMountpoint(name string) bool {
	if len(a.Mountpoints) == 0 {
		return true
	}
	for _, allowed := range a.Mountpoints {
		if allowed == name {
			return true
		}
	}
	return false
}

// AllowsIP reports whether an address passes this allowlist. The caster passes
// the real client address here when a trusted PROXY header is in use. An empty
// allowlist means every address.
func (a UserAccess) AllowsIP(value string) (bool, error) {
	if len(a.IPs) == 0 {
		return true, nil
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return false, fmt.Errorf("invalid client IP %q", value)
	}
	addr = addr.WithZone("").Unmap()
	for _, value := range a.IPs {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return false, fmt.Errorf("invalid stored IP rule %q", value)
		}
		if prefix.Contains(addr) {
			return true, nil
		}
	}
	return false, nil
}

// UserEdit replaces editable account settings without replacing the account.
// Password nil preserves the password; an empty password is rejected.
// ExpiresAt nil means never. Username and creation time cannot be changed.
type UserEdit struct {
	Password        *string
	ConnectionLimit int
	Enabled         bool
	Note            string
	Email           string
	ExpiresAt       *time.Time
	Access          UserAccess
}

func expiryTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}

// GetUser returns account metadata, never the password.
func (s *Store) GetUser(username string) (*User, error) {
	var u User
	var created, updated int64
	var expiry sql.NullInt64
	err := s.db.QueryRow(`SELECT id,username,connection_limit,enabled,note,email,expires_at,created_at,updated_at FROM users WHERE username=?`, username).
		Scan(&u.ID, &u.Username, &u.ConnectionLimit, &u.Enabled, &u.Note, &u.Email, &expiry, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt, u.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	u.ExpiresAt = expiryTime(expiry)
	return &u, nil
}

// GetUserAccess reads both allowlists from one consistent snapshot.
func (s *Store) GetUserAccess(username string) (UserAccess, error) {
	var userID int64
	if err := s.db.QueryRow(`SELECT id FROM users WHERE username=?`, username).Scan(&userID); err != nil {
		if err == sql.ErrNoRows {
			return UserAccess{}, ErrNotFound
		}
		return UserAccess{}, err
	}
	return s.GetUserAccessForUser(userID)
}

// GetUserAccessForUser reads allowlists by stable account identity.
func (s *Store) GetUserAccessForUser(userID int64) (UserAccess, error) {
	a := UserAccess{IPs: []string{}, Mountpoints: []string{}}
	rows, err := s.db.Query(`SELECT 'account', '' FROM users WHERE id=?
		UNION ALL SELECT 'ip', cidr FROM user_ip_rules WHERE user_id=?
		UNION ALL SELECT 'mount', mountpoint FROM user_mountpoints WHERE user_id=?
		ORDER BY 1,2`, userID, userID, userID)
	if err != nil {
		return a, err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var kind, value string
		if err := rows.Scan(&kind, &value); err != nil {
			return a, err
		}
		switch kind {
		case "account":
			found = true
		case "ip":
			a.IPs = append(a.IPs, value)
		case "mount":
			a.Mountpoints = append(a.Mountpoints, value)
		}
	}
	if err := rows.Err(); err != nil {
		return a, err
	}
	if !found {
		return a, ErrNotFound
	}
	return a, nil
}

// UpdateUser commits settings, optional password and allowlists together.
// Any validation or database failure leaves the whole account unchanged.
func (s *Store) UpdateUser(kr *secrets.Keyring, username string, edit UserEdit) error {
	if edit.ConnectionLimit < 1 {
		return invalidUser("connection limit must be positive")
	}
	if edit.Password != nil && *edit.Password == "" {
		return invalidUser("password must not be empty")
	}
	edit.Email = strings.TrimSpace(edit.Email)
	if edit.Email != "" {
		addr, err := mail.ParseAddress(edit.Email)
		if err != nil || addr.Address != edit.Email {
			return invalidUser("email must be a single email address")
		}
	}
	ips := make(map[string]bool)
	for _, value := range edit.Access.IPs {
		value = strings.TrimSpace(value)
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			addr, addrErr := netip.ParseAddr(value)
			if addrErr != nil || addr.Zone() != "" {
				return invalidUser("invalid IP address or CIDR: %q", value)
			}
			addr = addr.Unmap()
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		// Canonicalise IPv4-mapped IPv6 ranges to match ordinary IPv4 peers.
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return invalidUser("IPv4-mapped CIDR prefix must be at least 96 bits")
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		ips[prefix.Masked().String()] = true
	}
	mounts := make(map[string]bool)
	for _, name := range edit.Access.Mountpoints {
		if name == "" || strings.ContainsAny(name, "/; \t\r\n\x00") {
			return invalidUser("invalid mountpoint name")
		}
		mounts[name] = true
	}
	var expiry any
	if edit.ExpiresAt != nil {
		expiry = edit.ExpiresAt.Unix()
	}
	var ct, nonce []byte
	if edit.Password != nil {
		if kr == nil {
			return fmt.Errorf("password change requires a keyring")
		}
		var err error
		ct, nonce, err = kr.Seal(*edit.Password)
		if err != nil {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRow(`SELECT id FROM users WHERE username=?`, username).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if _, err := tx.Exec(`UPDATE users SET connection_limit=?,enabled=?,note=?,email=?,expires_at=?,updated_at=? WHERE id=?`,
		edit.ConnectionLimit, edit.Enabled, edit.Note, edit.Email, expiry, time.Now().Unix(), id); err != nil {
		return err
	}
	if edit.Password != nil {
		if _, err := tx.Exec(`UPDATE users SET password_enc=?,password_nonce=? WHERE id=?`, ct, nonce, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM user_ip_rules WHERE user_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM user_mountpoints WHERE user_id=?`, id); err != nil {
		return err
	}
	for cidr := range ips {
		if _, err := tx.Exec(`INSERT INTO user_ip_rules(user_id,cidr) VALUES (?,?)`, id, cidr); err != nil {
			return err
		}
	}
	for name := range mounts {
		if _, err := tx.Exec(`INSERT INTO user_mountpoints(user_id,mountpoint) VALUES (?,?)`, id, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}
