package store

import (
	"database/sql"
	"time"
)

// UserStats aggregates all connection history belonging to one account ID.
// Using the ID keeps a deleted and later recreated username from inheriting
// the old account's statistics.
type UserStats struct {
	ConnectionCount int64
	Active          int
	BytesSent       int64
	BytesReceived   int64
	DurationSeconds int64
	FirstConnection *time.Time
	LastConnection  *time.Time
	IPs             []string
}

// UserStats returns lifetime totals. Active sessions contribute elapsed time
// up to the instant this method begins.
func (s *Store) UserStats(userID int64) (UserStats, error) {
	var out UserStats
	now := time.Now().Unix()
	var first, last sql.NullInt64
	err := s.db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN ended_at IS NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(bytes_sent),0), COALESCE(SUM(bytes_recv),0),
		COALESCE(SUM(CASE WHEN COALESCE(ended_at,?) > started_at
			THEN COALESCE(ended_at,?)-started_at ELSE 0 END),0),
		MIN(started_at), MAX(started_at)
		FROM connections WHERE user_id=?`, now, now, userID).
		Scan(&out.ConnectionCount, &out.Active, &out.BytesSent, &out.BytesReceived,
			&out.DurationSeconds, &first, &last)
	if err != nil {
		return out, err
	}
	out.FirstConnection = expiryTime(first)
	out.LastConnection = expiryTime(last)
	rows, err := s.db.Query(`SELECT DISTINCT client_ip FROM connections
		WHERE user_id=? ORDER BY client_ip`, userID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.IPs = []string{}
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return out, err
		}
		out.IPs = append(out.IPs, ip)
	}
	return out, rows.Err()
}

// UserConnections returns one page of an account's connection history.
func (s *Store) UserConnections(userID int64, limit, offset int) ([]Connection, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(`SELECT id,username,mountpoint,client_ip,client_port,
		via_proxy,proxy_ip,user_agent,ntrip_version,started_at,ended_at,
		bytes_sent,bytes_recv,nmea_count,disconnect_reason
		FROM connections WHERE user_id=?
		ORDER BY started_at DESC,id DESC LIMIT ? OFFSET ?`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Connection{}
	for rows.Next() {
		var c Connection
		var viaProxy int
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Username, &c.Mountpoint, &c.ClientIP,
			&c.ClientPort, &viaProxy, &c.ProxyIP, &c.UserAgent, &c.NtripVersion,
			&started, &ended, &c.BytesSent, &c.BytesReceived, &c.NMEACount,
			&c.DisconnectReason); err != nil {
			return nil, err
		}
		c.ViaProxy = viaProxy != 0
		c.StartedAt = time.Unix(started, 0)
		c.EndedAt = expiryTime(ended)
		out = append(out, c)
	}
	return out, rows.Err()
}
