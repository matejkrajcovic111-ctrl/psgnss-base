package web

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/psgnss/psgnss-base/internal/pushout"
	"github.com/psgnss/psgnss-base/internal/store"
)

// handlePushoutStatus lists every configured target merged with the live state
// of the ones that are running.
func (s *Server) handlePushoutStatus(w http.ResponseWriter, _ *http.Request) {
	targets, err := s.Store.PushTargets()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	live := map[string]pushout.Status{}
	if s.Pushout != nil {
		for _, st := range s.Pushout.Status() {
			live[st.Name] = st
		}
	}
	rows := make([]map[string]any, 0, len(targets))
	for _, t := range targets {
		row := map[string]any{"name": t.Name, "enabled": t.Enabled, "source": t.Source,
			"host": t.Host, "mountpoint": t.Mountpoint, "protocol": t.Protocol,
			"username": t.Username, "has_password": t.HasPassword, "updated_at": t.UpdatedAt,
			"state": "stopped"}
		if st, ok := live[t.Name]; ok {
			row["state"] = st.State
			row["connected_at"] = st.ConnectedAt
			row["bytes_sent"] = st.BytesSent
			row["connects"] = st.Connects
			row["dropped"] = st.Dropped
			row["last_data_at"] = st.LastDataAt
			row["last_error"] = st.LastError
			row["last_error_at"] = st.LastErrorAt
			row["next_attempt_in"] = st.NextAttemptIn
		}
		rows = append(rows, row)
	}
	sources := make([]string, 0, len(s.Cfg.Caster.Mountpoint))
	for _, m := range s.Cfg.Caster.Mountpoint {
		if !m.Disabled {
			sources = append(sources, m.Name)
		}
	}
	writeJSON(w, 200, map[string]any{"targets": rows, "sources": sources})
}

type pushTargetInput struct {
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	Source     string `json:"source"`
	Host       string `json:"host"`
	Mountpoint string `json:"mountpoint"`
	Protocol   string `json:"protocol"`
	Username   string `json:"username"`
	Password   string `json:"password"`
}

// A mountpoint name travels in an NTRIP request line, and a username and
// password in a SOURCE line or a Basic credential. None of them may carry the
// characters that would let a value break out of its field.
const forbiddenInField = "\r\n\x00 \t@/#:"

func (s *Server) handlePushoutSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Targets []pushTargetInput `json:"targets"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		writeErr(w, 400, "bad request: "+err.Error())
		return
	}
	if len(in.Targets) > 8 {
		writeErr(w, 400, "at most 8 push targets are supported")
		return
	}
	sources := map[string]bool{}
	for _, m := range s.Cfg.Caster.Mountpoint {
		if !m.Disabled {
			sources[strings.ToLower(m.Name)] = true
		}
	}
	targets := make([]store.PushTarget, 0, len(in.Targets))
	for _, t := range in.Targets {
		name := strings.TrimSpace(t.Name)
		if name == "" || len(name) > 40 || strings.ContainsAny(name, "\r\n\x00") {
			writeErr(w, 400, "each push target needs a name of at most 40 characters")
			return
		}
		if _, _, err := net.SplitHostPort(strings.TrimSpace(t.Host)); err != nil {
			writeErr(w, 400, fmt.Sprintf("target %q: caster address must be host:port", name))
			return
		}
		if !sources[strings.ToLower(strings.TrimSpace(t.Source))] {
			writeErr(w, 400, fmt.Sprintf("target %q: %q is not an enabled local mountpoint", name, t.Source))
			return
		}
		mount := strings.TrimSpace(t.Mountpoint)
		if mount == "" || strings.ContainsAny(mount, forbiddenInField) {
			writeErr(w, 400, fmt.Sprintf("target %q: remote mountpoint is empty or has unsupported characters", name))
			return
		}
		if t.Protocol != store.ProtocolV1 && t.Protocol != store.ProtocolV2 {
			writeErr(w, 400, fmt.Sprintf("target %q: protocol must be %q or %q", name, store.ProtocolV1, store.ProtocolV2))
			return
		}
		user := strings.TrimSpace(t.Username)
		if strings.ContainsAny(user, forbiddenInField) {
			writeErr(w, 400, fmt.Sprintf("target %q: username has unsupported characters", name))
			return
		}
		if t.Protocol == store.ProtocolV2 && user == "" {
			writeErr(w, 400, fmt.Sprintf("target %q: NTRIP v2 upload needs a username", name))
			return
		}
		out := store.PushTarget{Name: name, Enabled: t.Enabled, Source: strings.TrimSpace(t.Source),
			Host: strings.TrimSpace(t.Host), Mountpoint: mount, Protocol: t.Protocol, Username: user}
		if t.Password != "" {
			if strings.ContainsAny(t.Password, "\r\n\x00") {
				writeErr(w, 400, fmt.Sprintf("target %q: password has unsupported characters", name))
				return
			}
			enc, nonce, err := s.Keyring.Seal(t.Password)
			if err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			out.PasswordEnc, out.PasswordNonce = enc, nonce
		}
		targets = append(targets, out)
	}
	// An enabled target with no password anywhere would connect and be refused
	// once a second; fail the save instead.
	stored, err := s.Store.PushTargets()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	had := map[string]bool{}
	for _, t := range stored {
		had[t.Name] = t.HasPassword
	}
	for _, t := range targets {
		if t.Enabled && len(t.PasswordEnc) == 0 && !had[t.Name] {
			writeErr(w, 400, fmt.Sprintf("target %q: a password is required before enabling it", t.Name))
			return
		}
	}
	if err := s.Store.SavePushTargets(targets); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.Log.Warn("NTRIP push-out configuration changed", "by", adminOf(r), "targets", len(targets))
	// Reload rather than restart: a restart would drop every rover to change
	// where a copy of the stream is sent.
	if s.Pushout != nil {
		if err := s.Pushout.Reload(); err != nil {
			writeErr(w, 500, "saved, but could not apply: "+err.Error())
			return
		}
	}
	s.handlePushoutStatus(w, r)
}
