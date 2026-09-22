package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/psgnss/psgnss-base/internal/backup"
	"github.com/psgnss/psgnss-base/internal/secrets"
)

// ExportSettings fills a backup document with everything an operator
// configured. Credentials come out unsealed, because the receiving station will
// re-seal them under its own master key -- see the package comment on
// internal/backup for why the key itself is never exported.
//
// History is not settings and is left behind: telemetry, the connection log,
// integrity runs and the receiver audit trail stay with the machine that
// recorded them. Sessions are left behind too; a restore must not hand anyone a
// login.
func (s *Store) ExportSettings(kr *secrets.Keyring, doc *backup.Document) error {
	if kr == nil {
		return errors.New("no master key: stored passwords cannot be read")
	}
	rows, err := s.db.Query(`SELECT username,password_hash,created_at FROM admins ORDER BY username`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a backup.Admin
		if err := rows.Scan(&a.Username, &a.PasswordHash, &a.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		doc.Admins = append(doc.Admins, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = s.db.Query(`SELECT id,username,password_enc,password_nonce,connection_limit,
		enabled,note,email,expires_at,created_at FROM users ORDER BY username`)
	if err != nil {
		return err
	}
	type pending struct {
		id   int64
		user backup.User
	}
	var users []pending
	for rows.Next() {
		var p pending
		var ct, nonce []byte
		var expiry sql.NullInt64
		if err := rows.Scan(&p.id, &p.user.Username, &ct, &nonce, &p.user.ConnectionLimit,
			&p.user.Enabled, &p.user.Note, &p.user.Email, &expiry, &p.user.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		pw, err := kr.Open(ct, nonce)
		if err != nil {
			rows.Close()
			return fmt.Errorf("read password for %q: %w", p.user.Username, err)
		}
		p.user.Password = pw
		if expiry.Valid {
			v := expiry.Int64
			p.user.ExpiresAt = &v
		}
		users = append(users, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range users {
		access, err := s.GetUserAccessForUser(p.id)
		if err != nil {
			return err
		}
		// No rows means every mountpoint, which round-trips as no rows.
		p.user.Mountpoints = access.Mountpoints
		p.user.IPRules = access.IPs
		doc.Users = append(doc.Users, p.user)
	}

	settings, err := s.IntegritySettings()
	if err != nil {
		return err
	}
	in := &backup.Integrity{Enabled: settings.Enabled, Schedule: settings.Schedule,
		DurationMinutes: settings.DurationMinutes, ToleranceHorizontalMM: settings.ToleranceHorizontalMM,
		ToleranceVerticalMM: settings.ToleranceVerticalMM, Host: settings.Host,
		Mountpoint: settings.Mountpoint, Username: settings.Username}
	if settings.HasPassword() {
		pw, err := kr.Open(settings.PasswordEnc, settings.PasswordNonce)
		if err != nil {
			return fmt.Errorf("read integrity reference password: %w", err)
		}
		in.Password = pw
	}
	doc.Integrity = in

	targets, err := s.PushTargets()
	if err != nil {
		return err
	}
	for _, t := range targets {
		out := backup.PushTarget{Name: t.Name, Enabled: t.Enabled, Source: t.Source,
			Host: t.Host, Mountpoint: t.Mountpoint, Protocol: t.Protocol, Username: t.Username}
		if t.hasPassword() {
			pw, err := kr.Open(t.PasswordEnc, t.PasswordNonce)
			if err != nil {
				return fmt.Errorf("read push-out password for %q: %w", t.Name, err)
			}
			out.Password = pw
		}
		doc.PushTargets = append(doc.PushTargets, out)
	}
	return nil
}

// ImportSettings replaces the configured state with a backup's, in one
// transaction: either the whole document applies or none of it does.
//
// Two consequences are deliberate. Replacing the administrators drops every
// session, including the one asking for the restore, so whoever runs it is
// signed out and signs in with the credentials from the backup. And a document
// with no administrators is refused, because applying it would lock everyone
// out of the station for good.
func (s *Store) ImportSettings(kr *secrets.Keyring, doc *backup.Document) error {
	if kr == nil {
		return errors.New("no master key: passwords cannot be stored")
	}
	if doc == nil || len(doc.Admins) == 0 {
		return errors.New("this backup has no administrators; restoring it would lock you out")
	}
	for _, a := range doc.Admins {
		if a.Username == "" || a.PasswordHash == "" {
			return errors.New("this backup has an administrator with no credentials")
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	for _, stmt := range []string{
		`DELETE FROM user_ip_rules`, `DELETE FROM user_mountpoints`,
		`DELETE FROM users`, `DELETE FROM push_targets`, `DELETE FROM admins`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("clear existing settings: %w", err)
		}
	}
	for _, a := range doc.Admins {
		created := a.CreatedAt
		if created == 0 {
			created = now
		}
		if _, err := tx.Exec(`INSERT INTO admins (username,password_hash,created_at) VALUES (?,?,?)`,
			a.Username, a.PasswordHash, created); err != nil {
			return fmt.Errorf("restore administrator %q: %w", a.Username, err)
		}
	}
	for _, u := range doc.Users {
		if u.Username == "" {
			return errors.New("this backup has an NTRIP account with no name")
		}
		ct, nonce, err := kr.Seal(u.Password)
		if err != nil {
			return err
		}
		limit := u.ConnectionLimit
		if limit <= 0 {
			limit = 5
		}
		created := u.CreatedAt
		if created == 0 {
			created = now
		}
		var expiry any
		if u.ExpiresAt != nil {
			expiry = *u.ExpiresAt
		}
		res, err := tx.Exec(`INSERT INTO users
			(username,password_enc,password_nonce,connection_limit,enabled,note,email,expires_at,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, u.Username, ct, nonce, limit, u.Enabled, u.Note,
			u.Email, expiry, created, now)
		if err != nil {
			return fmt.Errorf("restore NTRIP account %q: %w", u.Username, err)
		}
		id, _ := res.LastInsertId()
		for _, m := range u.Mountpoints {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO user_mountpoints (user_id,mountpoint) VALUES (?,?)`,
				id, m); err != nil {
				return err
			}
		}
		for _, c := range u.IPRules {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO user_ip_rules (user_id,cidr) VALUES (?,?)`,
				id, c); err != nil {
				return err
			}
		}
	}
	for _, t := range doc.PushTargets {
		var ct, nonce []byte
		if t.Password != "" {
			ct, nonce, err = kr.Seal(t.Password)
			if err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO push_targets
			(name,enabled,source,host,mountpoint,protocol,username,password_enc,password_nonce,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, t.Name, t.Enabled, t.Source, t.Host, t.Mountpoint,
			t.Protocol, t.Username, ct, nonce, now); err != nil {
			return fmt.Errorf("restore push-out target %q: %w", t.Name, err)
		}
	}
	if in := doc.Integrity; in != nil {
		var ct, nonce []byte
		if in.Password != "" {
			ct, nonce, err = kr.Seal(in.Password)
			if err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE integrity_settings SET enabled=?,schedule=?,duration_minutes=?,
			tolerance_horizontal_mm=?,tolerance_vertical_mm=?,host=?,mountpoint=?,username=?,
			password_enc=?,password_nonce=?,updated_at=? WHERE id=1`, in.Enabled, in.Schedule,
			in.DurationMinutes, in.ToleranceHorizontalMM, in.ToleranceVerticalMM, in.Host,
			in.Mountpoint, in.Username, ct, nonce, now); err != nil {
			return fmt.Errorf("restore integrity settings: %w", err)
		}
	}
	return tx.Commit()
}
