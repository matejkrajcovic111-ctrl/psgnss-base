package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/psgnss/psgnss-base/internal/store"
)

type userEditRequest struct {
	Password        *string         `json:"password"`
	ConnectionLimit *int            `json:"limit"`
	Enabled         *bool           `json:"enabled"`
	Note            *string         `json:"note"`
	Email           *string         `json:"email"`
	ExpiresAt       json.RawMessage `json:"expires_at"`
	IPRules         *[]string       `json:"ip_rules"`
	Mountpoints     *[]string       `json:"mountpoints"`
}

func decodeUserEdit(w http.ResponseWriter, r *http.Request) (store.UserEdit, error) {
	var body userEditRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return store.UserEdit{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return store.UserEdit{}, errors.New("request body must contain one JSON object")
	}
	if body.ConnectionLimit == nil || body.Enabled == nil || body.Note == nil ||
		body.Email == nil || body.ExpiresAt == nil || body.IPRules == nil || body.Mountpoints == nil {
		return store.UserEdit{}, errors.New("limit, enabled, note, email, expires_at, ip_rules and mountpoints are required")
	}
	expires, err := parseUserExpiry(body.ExpiresAt)
	if err != nil {
		return store.UserEdit{}, err
	}
	return store.UserEdit{
		Password: body.Password, ConnectionLimit: *body.ConnectionLimit,
		Enabled: *body.Enabled, Note: *body.Note, Email: *body.Email,
		ExpiresAt: expires,
		Access:    store.UserAccess{IPs: *body.IPRules, Mountpoints: *body.Mountpoints},
	}, nil
}

// parseUserExpiry accepts an exact RFC3339 instant, or a calendar date. A date
// remains valid through that day and expires at 00:00 UTC the following day.
func parseUserExpiry(raw json.RawMessage) (*time.Time, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return nil, errors.New("expires_at must be null, YYYY-MM-DD or RFC3339")
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		t = t.UTC()
		return &t, nil
	}
	t, err := time.Parse("2006-01-02", value)
	if err != nil {
		return nil, errors.New("expires_at must be null, YYYY-MM-DD or RFC3339")
	}
	t = t.AddDate(0, 0, 1)
	return &t, nil
}

func detailPage(r *http.Request) (limit, offset int, err error) {
	limit = 100
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 500 {
			return 0, 0, errors.New("limit must be between 1 and 500")
		}
	}
	if value := r.URL.Query().Get("offset"); value != "" {
		offset, err = strconv.Atoi(value)
		if err != nil || offset < 0 {
			return 0, 0, errors.New("offset must be zero or greater")
		}
	}
	return limit, offset, nil
}

func connectionJSON(c store.Connection) map[string]any {
	now := time.Now()
	end := now
	if c.EndedAt != nil {
		end = *c.EndedAt
	}
	duration := int64(end.Sub(c.StartedAt).Seconds())
	if duration < 0 {
		duration = 0
	}
	return map[string]any{
		"id": c.ID, "username": c.Username, "mountpoint": c.Mountpoint,
		"client_ip": c.ClientIP, "client_port": c.ClientPort,
		"via_proxy": c.ViaProxy, "proxy_ip": c.ProxyIP,
		"agent": c.UserAgent, "ntrip_version": c.NtripVersion,
		"started": c.StartedAt.UTC(), "ended": c.EndedAt,
		"active": c.EndedAt == nil, "duration_s": duration,
		"bytes_sent": c.BytesSent, "bytes_received": c.BytesReceived,
		"nmea_count": c.NMEACount, "disconnect_reason": c.DisconnectReason,
	}
}

func (s *Server) userDetail(name string, limit, offset int, showPassword bool) (map[string]any, error) {
	u, err := s.Store.GetUser(name)
	if err != nil {
		return nil, err
	}
	access, err := s.Store.GetUserAccessForUser(u.ID)
	if err != nil {
		return nil, err
	}
	stats, err := s.Store.UserStats(u.ID)
	if err != nil {
		return nil, err
	}
	history, err := s.Store.UserConnections(u.ID, limit, offset)
	if err != nil {
		return nil, err
	}
	connections := make([]map[string]any, 0, len(history))
	for _, c := range history {
		connections = append(connections, connectionJSON(c))
	}
	user := map[string]any{
		"username": u.Username, "limit": u.ConnectionLimit,
		"enabled": u.Enabled, "note": u.Note, "email": u.Email,
		"expires_at": u.ExpiresAt, "created": u.CreatedAt.UTC(),
		"updated": u.UpdatedAt.UTC(),
	}
	if showPassword {
		password, err := s.Store.GetPassword(s.Keyring, name)
		if err != nil {
			return nil, err
		}
		user["password"] = password
	}
	return map[string]any{
		"user": user,
		"access": map[string]any{
			"ip_rules": access.IPs, "mountpoints": access.Mountpoints,
			"all_ips":         len(access.IPs) == 0,
			"all_mountpoints": len(access.Mountpoints) == 0,
		},
		"stats": map[string]any{
			"connection_count": stats.ConnectionCount, "active": stats.Active,
			"total_bytes_sent":     stats.BytesSent,
			"total_bytes_received": stats.BytesReceived,
			"total_duration_s":     stats.DurationSeconds,
			"first_connection":     stats.FirstConnection,
			"last_connection":      stats.LastConnection, "ips_seen": stats.IPs,
		},
		"history": connections,
		"page": map[string]any{
			"limit": limit, "offset": offset, "total": stats.ConnectionCount,
		},
	}, nil
}

func (s *Server) handleUserGet(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := detailPage(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := s.userDetail(r.PathValue("name"), limit, offset, r.URL.Query().Get("passwords") == "1")
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not load user")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	edit, err := decodeUserEdit(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name := r.PathValue("name")
	err = s.Store.UpdateUser(s.Keyring, name, edit)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	}
	if errors.Is(err, store.ErrInvalidUser) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not update user")
		return
	}
	s.Log.Info("NTRIP user updated", "user", name, "by", adminOf(r),
		"password_changed", edit.Password != nil)
	out, err := s.userDetail(name, 100, 0, false)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "user updated but could not reload it")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
