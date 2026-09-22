package store

import (
	"fmt"
	"strings"
	"time"
)

// PushTarget is one outbound NTRIP upload: the local mountpoint whose exact
// broadcast stream is pushed, and the remote caster it is pushed to.
//
// The shape --- several independent targets, each with its own credentials,
// remote mountpoint and protocol --- follows RTKBase's [ntrip_A]/[ntrip_B]
// arrangement in settings.conf.default. See CREDITS.md. Unlike RTKBase, the
// message selection is not repeated per target: a target names a local
// mountpoint and sends byte-for-byte what a rover connected here receives.
type PushTarget struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Enabled       bool   `json:"enabled"`
	Source        string `json:"source"`     // local mountpoint name
	Host          string `json:"host"`       // host:port of the remote caster
	Mountpoint    string `json:"mountpoint"` // remote mountpoint to publish as
	Protocol      string `json:"protocol"`   // "v1" (SOURCE) or "v2" (POST)
	Username      string `json:"username"`   // v2 only; v1 authenticates with the password alone
	PasswordEnc   []byte `json:"-"`
	PasswordNonce []byte `json:"-"`
	HasPassword   bool   `json:"has_password"`
	UpdatedAt     int64  `json:"updated_at"`
}

// ProtocolV1 and ProtocolV2 are the two upload handshakes.
const (
	ProtocolV1 = "v1"
	ProtocolV2 = "v2"
)

func (t PushTarget) hasPassword() bool {
	return len(t.PasswordEnc) > 0 && len(t.PasswordNonce) > 0
}

// PushTargets lists every configured target, ordered by name.
func (s *Store) PushTargets() ([]PushTarget, error) {
	rows, err := s.db.Query(`SELECT id,name,enabled,source,host,mountpoint,protocol,username,
		password_enc,password_nonce,updated_at FROM push_targets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushTarget
	for rows.Next() {
		var t PushTarget
		if err := rows.Scan(&t.ID, &t.Name, &t.Enabled, &t.Source, &t.Host, &t.Mountpoint,
			&t.Protocol, &t.Username, &t.PasswordEnc, &t.PasswordNonce, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.HasPassword = t.hasPassword()
		out = append(out, t)
	}
	return out, rows.Err()
}

// SavePushTargets replaces the whole set in one transaction. A target whose
// name already exists keeps its stored password when the caller passes none,
// so editing a schedule or a mountpoint never silently discards credentials.
func (s *Store) SavePushTargets(targets []PushTarget) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	existing := map[string]PushTarget{}
	rows, err := tx.Query(`SELECT name,password_enc,password_nonce FROM push_targets`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t PushTarget
		if err := rows.Scan(&t.Name, &t.PasswordEnc, &t.PasswordNonce); err != nil {
			rows.Close()
			return err
		}
		existing[t.Name] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM push_targets`); err != nil {
		return err
	}
	now := time.Now().Unix()
	seen := map[string]bool{}
	for _, t := range targets {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			return fmt.Errorf("push target name is empty")
		}
		if seen[strings.ToLower(name)] {
			return fmt.Errorf("push target %q is listed twice", name)
		}
		seen[strings.ToLower(name)] = true
		if !t.hasPassword() {
			if prev, ok := existing[name]; ok {
				t.PasswordEnc, t.PasswordNonce = prev.PasswordEnc, prev.PasswordNonce
			}
		}
		if _, err := tx.Exec(`INSERT INTO push_targets
			(name,enabled,source,host,mountpoint,protocol,username,password_enc,password_nonce,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			name, t.Enabled, t.Source, t.Host, t.Mountpoint, t.Protocol, t.Username,
			t.PasswordEnc, t.PasswordNonce, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
