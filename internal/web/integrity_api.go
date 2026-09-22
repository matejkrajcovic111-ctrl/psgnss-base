package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/psgnss/psgnss-base/internal/store"
)

func (s *Server) handleIntegrityStatus(w http.ResponseWriter, _ *http.Request) {
	settings, err := s.Store.IntegritySettings()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	latest, err := s.Store.LatestIntegrityRun()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var progress any
	if s.Integrity != nil {
		progress = s.Integrity.Progress()
	}
	writeJSON(w, 200, map[string]any{
		"settings": map[string]any{"enabled": settings.Enabled, "schedule": settings.Schedule,
			"duration_minutes": settings.DurationMinutes, "tolerance_horizontal_mm": settings.ToleranceHorizontalMM,
			"tolerance_vertical_mm": settings.ToleranceVerticalMM, "host": settings.Host,
			"mountpoint": settings.Mountpoint, "username": settings.Username,
			"has_password": settings.HasPassword()},
		"running": s.Integrity != nil && s.Integrity.Running(), "progress": progress, "latest": latest,
	})
}

var scheduleRE = regexp.MustCompile(`^(?:[01][0-9]|2[0-3]):[0-5][0-9]$`)

func (s *Server) handleIntegritySettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
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
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		writeErr(w, 400, "bad request: "+err.Error())
		return
	}
	if !scheduleRE.MatchString(in.Schedule) || in.DurationMinutes < 5 || in.DurationMinutes > 120 ||
		in.ToleranceHorizontalMM < 1 || in.ToleranceHorizontalMM > 10000 || in.ToleranceVerticalMM < 1 || in.ToleranceVerticalMM > 10000 {
		writeErr(w, 400, "schedule, duration or tolerance is outside the allowed range")
		return
	}
	if _, _, err := net.SplitHostPort(in.Host); err != nil {
		writeErr(w, 400, "reference caster host must be host:port")
		return
	}
	if strings.TrimSpace(in.Mountpoint) == "" || strings.ContainsAny(in.Mountpoint, "\r\n@/#") || strings.ContainsAny(in.Username, "\r\n@/#") {
		writeErr(w, 400, "reference username or mountpoint contains unsupported characters")
		return
	}
	old, err := s.Store.IntegritySettings()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	next := store.IntegritySettings{Enabled: in.Enabled, Schedule: in.Schedule, DurationMinutes: in.DurationMinutes,
		ToleranceHorizontalMM: in.ToleranceHorizontalMM, ToleranceVerticalMM: in.ToleranceVerticalMM,
		Host: strings.TrimSpace(in.Host), Mountpoint: strings.TrimSpace(in.Mountpoint), Username: strings.TrimSpace(in.Username),
		PasswordEnc: old.PasswordEnc, PasswordNonce: old.PasswordNonce}
	if in.Password != "" {
		if strings.ContainsAny(in.Password, "\r\n@/#") {
			writeErr(w, 400, "reference password contains a character RTKLIB cannot encode")
			return
		}
		next.PasswordEnc, next.PasswordNonce, err = s.Keyring.Seal(in.Password)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if next.Enabled && (next.Username == "" || !next.HasPassword()) {
		writeErr(w, 400, "reference username and password are required before enabling monitoring")
		return
	}
	if err := s.Store.SaveIntegritySettings(next); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.Log.Warn("external integrity settings changed", "enabled", next.Enabled, "schedule", next.Schedule, "by", adminOf(r))
	s.handleIntegrityStatus(w, r)
}

func (s *Server) handleIntegrityCancel(w http.ResponseWriter, r *http.Request) {
	if s.Integrity == nil {
		writeErr(w, 503, "external integrity monitor is unavailable")
		return
	}
	if !s.Integrity.Cancel() {
		writeErr(w, 409, "no external integrity check is running")
		return
	}
	s.Log.Warn("external integrity check cancelled", "by", adminOf(r))
	s.handleIntegrityStatus(w, r)
}

func (s *Server) handleIntegrityRun(w http.ResponseWriter, r *http.Request) {
	if s.Integrity == nil {
		writeErr(w, 503, "external integrity monitor is unavailable")
		return
	}
	if s.Integrity.Running() {
		writeErr(w, 409, "an external integrity check is already running")
		return
	}
	s.Log.Info("manual external integrity check requested", "by", adminOf(r))
	go func() { _, _ = s.Integrity.Run(context.Background()) }()
	writeJSON(w, http.StatusAccepted, map[string]bool{"running": true})
}
