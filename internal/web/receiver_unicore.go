package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/psgnss/psgnss-base/internal/boardprofile"
	"github.com/psgnss/psgnss-base/internal/hub"
	"github.com/psgnss/psgnss-base/internal/store"
	"github.com/psgnss/psgnss-base/internal/unicore"
)

// The Unicore side of the Receiver page and the Settings writes. It mirrors
// the UBX transaction: a change is applied to the running configuration,
// read back, and held pending Keep (SAVECONFIG) or auto-revert, with every
// step in the audit log. Only the receiver dialect differs.

// unicoreProfile reports whether this server drives a Unicore receiver.
func (s *Server) unicoreProfile() (boardprofile.Profile, bool) {
	if s.Cfg == nil {
		return boardprofile.Profile{}, false
	}
	p, err := boardprofile.Resolve(s.Cfg.Receiver.Profile, s.Cfg.Receiver.Model)
	return p, err == nil && p.Unicore()
}

// unicoreSession opens the hub's control pipe for a Unicore conversation.
func (s *Server) unicoreSession() (*unicore.Session, error) {
	open := s.receiverControl
	if open == nil {
		if s.Hub == nil {
			return nil, errors.New("receiver control is unavailable without a hub")
		}
		open = func() (io.ReadWriteCloser, error) { return s.Hub.Control(hub.ProtoNMEA, hub.ProtoUnicoreText) }
	}
	p, err := open()
	if err != nil {
		return nil, err
	}
	rx := unicore.NewSession(p)
	rx.Log = s.Log
	return rx, nil
}

// lockUnicore takes the receiver for one request; it fails while a change is
// awaiting Keep or Revert, because that transaction owns the control session.
func (s *Server) lockUnicore(w http.ResponseWriter) (*unicore.Session, func(), bool) {
	s.receiverMu.Lock()
	if s.receiverTxn != nil {
		s.receiverMu.Unlock()
		writeErr(w, http.StatusConflict, "a receiver change is awaiting Keep or auto-revert")
		return nil, nil, false
	}
	rx, err := s.unicoreSession()
	if err != nil {
		s.receiverMu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return nil, nil, false
	}
	return rx, func() { rx.Close(); s.receiverMu.Unlock() }, true
}

// handleUnicoreReceiver answers GET /api/receiver for a Unicore profile.
func (s *Server) handleUnicoreReceiver(w http.ResponseWriter, profile boardprofile.Profile) {
	s.receiverMu.Lock()
	if p := s.receiverTxn; p != nil {
		s.receiverMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "a receiver change is awaiting Keep or auto-revert",
			"pending": pendingView(p)})
		return
	}
	s.receiverMu.Unlock()
	rx, release, ok := s.lockUnicore(w)
	if !ok {
		return
	}
	defer release()
	r, err := rx.Do("VERSIONA")
	if err == nil {
		err = r.Err()
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "receiver VERSIONA: "+err.Error())
		return
	}
	v, err := unicore.ParseVersion(r)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "receiver VERSIONA: "+err.Error())
		return
	}
	if v.Model != "" && !strings.EqualFold(v.Model, profile.Receiver) {
		writeErr(w, http.StatusConflict, "receiver reports "+v.Model+" but board profile expects "+profile.Receiver)
		return
	}
	snap, err := rx.Snapshot()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"driver": "unicore", "model": v.Model, "firmware": v.Firmware,
		"serial": v.Serial, "pn": v.PN, "auth": v.Auth, "auth_date": v.Date, "efuse_id": v.EfuseID,
		"mode": snap.Mode, "outputs": snap.Outputs, "config": snap.Config, "masks": snap.Masks,
		"board_profile": profile, "pending": nil, "read_at": time.Now().UTC()})
}

// unicorePlan is one change to the receiver.
type unicorePlan struct {
	cmds  []string
	label string
	// note is the audit prefix, e.g. "console" or "settings".
	note string
	// check runs on the locked session first and may refuse or replace cmds.
	check func(*unicore.Session) ([]string, int, string)
	// post runs after the commands were read back; it may veto or add commands.
	post  func(*unicore.Session) ([]string, error)
	hooks receiverTxnHooks
	// extra adds fields to the response.
	extra map[string]any
}

func replyText(rs []unicore.Reply) string {
	var parts []string
	for _, r := range rs {
		parts = append(parts, "> "+unicore.Redact(r.Command)+"\n"+unicore.Redact(r.Text()))
	}
	return strings.Join(parts, "\n")
}

// beginUnicoreTxn applies plan and holds the result pending Keep.
func (s *Server) beginUnicoreTxn(w http.ResponseWriter, r *http.Request, plan unicorePlan) {
	s.receiverMu.Lock()
	if s.receiverTxn != nil {
		s.receiverMu.Unlock()
		writeErr(w, http.StatusConflict, "a receiver change is already pending")
		return
	}
	rx, err := s.unicoreSession()
	if err != nil {
		s.receiverMu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	fail := func(code int, msg string, extra map[string]any) {
		rx.Close()
		s.receiverMu.Unlock()
		body := map[string]any{"error": msg}
		for k, v := range extra {
			body[k] = v
		}
		writeJSON(w, code, body)
	}
	actor := adminOf(r)
	notApplied := func(cmds []string, why string) {
		for _, c := range cmds {
			_, _ = s.Store.LogReceiverConfig(store.ReceiverConfigLog{Actor: actor, KeyID: "console", KeyName: c,
				Reverted: true, Note: "not applied: " + why})
		}
	}
	if profile, ok := s.unicoreProfile(); ok {
		rep, e := rx.Do("VERSIONA")
		if e == nil {
			e = rep.Err()
		}
		if e != nil {
			fail(http.StatusBadGateway, "receiver VERSIONA: "+e.Error(), nil)
			return
		}
		if v, e := unicore.ParseVersion(rep); e == nil && v.Model != "" && !strings.EqualFold(v.Model, profile.Receiver) {
			fail(http.StatusConflict, "receiver reports "+v.Model+" but board profile expects "+profile.Receiver, nil)
			return
		}
	}
	cmds := plan.cmds
	if plan.check != nil {
		var code int
		var msg string
		if cmds, code, msg = plan.check(rx); code != 0 {
			fail(code, msg, nil)
			return
		}
	}
	d := time.Duration(s.revertSeconds()) * time.Second
	s.Log.Info("unicore change requested", "by", actor, "label", plan.label, "commands", len(cmds))
	txn, replies, err := rx.Apply(cmds, d, s.Log, plan.post)
	if err != nil {
		notApplied(cmds, err.Error())
		fail(http.StatusBadGateway, err.Error(), map[string]any{"text": replyText(replies)})
		return
	}
	changes := txn.Changes()
	var ids []int64
	note := plan.note + ": awaiting explicit commit"
	for _, c := range changes {
		id, e := s.Store.LogReceiverConfig(store.ReceiverConfigLog{Actor: actor, KeyID: c.Kind,
			KeyName: c.Name + " (" + strings.Join(txn.Cmds, "; ") + ")", OldValue: c.Old, NewValue: c.New,
			Verified: true, Note: note})
		if e == nil {
			ids = append(ids, id)
		}
	}
	if len(changes) == 0 { // accepted, but nothing read back differently
		id, e := s.Store.LogReceiverConfig(store.ReceiverConfigLog{Actor: actor, KeyID: "console",
			KeyName: strings.Join(txn.Cmds, "; "), Verified: true, Note: note + " (no observable change)"})
		if e == nil {
			ids = append(ids, id)
		}
	}
	p := &pendingReceiverTxn{uni: txn, pipe: rx, logIDs: ids, until: time.Now().Add(d), label: plan.label,
		beforeCommit: plan.hooks.beforeCommit, rollbackCommit: plan.hooks.rollbackCommit, afterCommit: plan.hooks.afterCommit}
	s.receiverTxn = p
	go s.watchReceiverTxn(p)
	s.receiverMu.Unlock()
	out := pendingView(p)
	out["text"] = replyText(replies)
	for k, v := range plan.extra {
		out[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// unicorePendingView is pendingView for a Unicore transaction. The change rows
// carry the same fields the UBX banner reads.
func unicorePendingView(p *pendingReceiverTxn) map[string]any {
	cs := p.uni.Changes()
	rows := make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		old, now := c.Old, c.New
		if old == "" {
			old = "(not active)"
		}
		if now == "" {
			now = "(removed)"
		}
		rows = append(rows, map[string]any{"key": c.Kind + ":" + c.Name, "name": c.Name, "kind": c.Kind,
			"old_display": old, "display": now, "verified": true, "documented": true})
	}
	return map[string]any{"ok": true, "verified": true, "driver": "unicore", "revert_at": p.until.UTC(),
		"label": p.label, "changes": rows, "commands": p.uni.Cmds}
}

// handleReceiverConsole runs one operator command.
func (s *Server) handleReceiverConsole(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.unicoreProfile(); !ok {
		writeErr(w, http.StatusNotImplemented, "the command console is available for Unicore receivers only")
		return
	}
	var in struct {
		Command string `json:"command"`
		Confirm bool   `json:"confirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	cmd := strings.TrimSpace(in.Command)
	v := unicore.Classify(cmd)
	actor := adminOf(r)
	s.Log.Info("receiver console command", "by", actor, "cmd", unicore.Redact(cmd), "class", v.Class.String(), "confirmed", in.Confirm)
	switch {
	case v.Class == unicore.ClassRefused:
		_, _ = s.Store.LogReceiverConfig(store.ReceiverConfigLog{Actor: actor, KeyID: "console", KeyName: cmd,
			Reverted: true, Note: "refused: " + v.Reason})
		writeJSON(w, http.StatusForbidden, map[string]any{"error": v.Reason, "class": "refused"})
		return
	case v.Class == unicore.ClassDangerous && !in.Confirm:
		writeJSON(w, http.StatusConflict, map[string]any{"error": v.Reason, "class": "dangerous",
			"dangerous": true, "revertable": v.Revertable})
		return
	case v.Class == unicore.ClassChange || (v.Class == unicore.ClassDangerous && v.Revertable):
		s.beginUnicoreTxn(w, r, unicorePlan{cmds: []string{cmd}, label: cmd, note: "console"})
		return
	}
	// Queries, SAVECONFIG and confirmed commands the snapshot cannot undo.
	rx, release, ok := s.lockUnicore(w)
	if !ok {
		return
	}
	defer release()
	rep, err := rx.Do(cmd)
	note := map[unicore.Class]string{unicore.ClassQuery: "query", unicore.ClassSave: "saved to flash (SAVECONFIG)",
		unicore.ClassDangerous: "confirmed, sent directly (cannot be auto-reverted)"}[v.Class]
	if err != nil {
		_, _ = s.Store.LogReceiverConfig(store.ReceiverConfigLog{Actor: actor, KeyID: "console", KeyName: cmd,
			Reverted: true, Note: "failed: " + err.Error()})
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if !rep.OK() {
		note += "; receiver answered: " + rep.Status
		if !rep.Acked {
			note += "no acknowledgement"
		}
	}
	_, _ = s.Store.LogReceiverConfig(store.ReceiverConfigLog{Actor: actor, KeyID: "console", KeyName: cmd,
		Verified: rep.OK(), Note: note})
	writeJSON(w, http.StatusOK, map[string]any{"command": cmd, "class": v.Class.String(), "ok": rep.OK(),
		"acked": rep.Acked, "status": rep.Status, "text": rep.Text(), "lines": rep.Lines, "logs": rep.Logs})
}

// --- Settings: base position and station ID ---

const maxModeBaseDistance = 300.0 // metres; beyond it the receiver outputs no RTCM (manual, MODE BASE)

// distanceMetres is the flat-earth distance, ample at the 300 m scale.
func distanceMetres(lat1, lon1, lat2, lon2 float64) float64 {
	dLat := (lat2 - lat1) * 111320
	dLon := (lon2 - lon1) * 111320 * math.Cos((lat1+lat2)/2*math.Pi/180)
	return math.Hypot(dLat, dLon)
}

// unicorePositionApply sends MODE BASE <id> <lat> <lon> <height>.
//
// The manual does not say whether the height is ellipsoidal or above mean sea
// level (BASEPOS and BESTNAV report height above MSL plus the undulation). The
// configured height is ellipsoidal, so the first attempt sends it as is and
// reads BASEPOSA back: a receiver that took it as MSL reports exactly the
// number it was given, and the command is then repeated with the height
// converted using the undulation of the receiver's own solution
// (ellipsoidal = MSL + undulation, as in BESTNAV).
func (s *Server) unicorePositionApply(w http.ResponseWriter, r *http.Request, in positionInput) {
	if err := checkPosition(in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	lat, _ := strconv.ParseFloat(fmt.Sprintf("%.11f", in.Latitude), 64)
	lon, _ := strconv.ParseFloat(fmt.Sprintf("%.11f", in.Longitude), 64)
	h, _ := strconv.ParseFloat(fmt.Sprintf("%.4f", in.Height), 64)
	actual := positionInput{Latitude: lat, Longitude: lon, Height: h}
	next := *s.Cfg
	next.Station.Position.Mode = "fixed"
	next.Station.Position.Format = "llh"
	next.Station.Position.Latitude, next.Station.Position.Longitude, next.Station.Position.Height = lat, lon, h
	if err := next.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id := int(s.Cfg.Station.StationID)
	if id > 4095 {
		writeErr(w, http.StatusBadRequest, "the Unicore base station ID is 0-4095")
		return
	}
	cmd := func(height float64) string {
		return fmt.Sprintf("MODE BASE %d %.11f %.11f %.4f", id, actual.Latitude, actual.Longitude, height)
	}
	extra := map[string]any{"height_reference": "unverified"}
	var undulation float64
	var haveUndulation bool
	old := *s.Cfg
	s.beginUnicoreTxn(w, r, unicorePlan{cmds: []string{cmd(h)}, label: "Base position", note: "settings", extra: extra,
		check: func(rx *unicore.Session) ([]string, int, string) {
			rep, err := rx.Do("BESTNAVA")
			if err == nil {
				if fix, e := unicore.ParseBestNav(rep); e == nil && strings.HasPrefix(fix.SolStatus, "SOL_COMPUTED") {
					d := distanceMetres(fix.Lat, fix.Lon, lat, lon)
					extra["distance_m"] = math.Round(d*10) / 10
					if d > maxModeBaseDistance {
						return nil, http.StatusConflict, fmt.Sprintf("the position is %.0f m from the receiver's own solution; "+
							"beyond %.0f m the receiver outputs no RTCM at all", d, maxModeBaseDistance)
					}
					undulation, haveUndulation = fix.Und, true
					return []string{cmd(h)}, 0, ""
				}
			}
			s.Log.Warn("base position: receiver has no solution to compare with; the 300 m check was skipped")
			return []string{cmd(h)}, 0, ""
		},
		post: func(rx *unicore.Session) ([]string, error) {
			rep, err := rx.Do("BASEPOSA")
			var bp unicore.Fix
			if err == nil {
				bp, err = unicore.ParseBasePos(rep)
			}
			if err != nil || !haveUndulation {
				s.Log.Warn("base position: could not verify the height reference", "err", err, "undulation_known", haveUndulation)
				return nil, nil
			}
			if math.Abs(bp.Hgt-h) < 0.05 && math.Abs(undulation) > 0.5 {
				s.Log.Warn("receiver took the MODE BASE height as height above mean sea level; resending converted",
					"undulation", undulation)
				extra["height_reference"] = "msl-converted"
				return []string{cmd(h - undulation)}, nil
			}
			extra["height_reference"] = "ellipsoidal"
			return nil, nil
		},
		hooks: receiverTxnHooks{
			beforeCommit: func() error { return s.saveConfig(&next) },
			rollbackCommit: func() {
				if err := s.saveConfig(&old); err != nil {
					s.Log.Error("could not restore config after receiver commit failure", "err", err)
				}
			},
			afterCommit: s.restartSoon,
		}})
}

// unicoreStationIDApply sends MODE BASE with the new ID and the mode the
// receiver reports now (a TIME survey or the fixed position, digit for digit).
func (s *Server) unicoreStationIDApply(w http.ResponseWriter, r *http.Request, stationID uint16) {
	if stationID > 4095 {
		writeErr(w, http.StatusBadRequest, "the Unicore base station ID is 0-4095")
		return
	}
	next := *s.Cfg
	next.Station.StationID = stationID
	old := *s.Cfg
	want := strconv.Itoa(int(stationID))
	s.beginUnicoreTxn(w, r, unicorePlan{label: "RTCM station ID", note: "settings",
		check: func(rx *unicore.Session) ([]string, int, string) {
			rep, err := rx.Do("MODE")
			if err == nil {
				err = rep.Err()
			}
			var m unicore.Mode
			if err == nil {
				m, err = unicore.ParseMode(rep)
			}
			if err != nil {
				return nil, http.StatusBadGateway, "receiver MODE: " + err.Error()
			}
			switch {
			case m.Role != "BASE":
				return nil, http.StatusConflict, "the receiver is not in base mode (" + m.Raw + "); the station ID is part of MODE BASE"
			case m.Kind == "fixed":
				return []string{fmt.Sprintf("MODE BASE %s %s %s %s", want, m.Lat, m.Lon, m.Height)}, 0, ""
			case m.Kind == "time":
				c := "MODE BASE " + want + " TIME " + m.TimeSeconds
				if m.Distance != "" {
					c += " " + m.Distance
				}
				return []string{c}, 0, ""
			}
			return []string{"MODE BASE " + want + " TIME 60"}, 0, ""
		},
		post: func(rx *unicore.Session) ([]string, error) {
			rep, err := rx.Do("MODE")
			var m unicore.Mode
			if err == nil {
				m, err = unicore.ParseMode(rep)
			}
			if err != nil {
				return nil, fmt.Errorf("read MODE back: %w", err)
			}
			if m.ID != want {
				return nil, fmt.Errorf("receiver reports station ID %q after the change, not %s", m.ID, want)
			}
			return nil, nil
		},
		hooks: receiverTxnHooks{beforeCommit: func() error { return s.saveConfig(&next) }, rollbackCommit: func() {
			if err := s.saveConfig(&old); err != nil {
				s.Log.Error("could not restore config after station ID commit failure", "err", err)
			}
		}, afterCommit: s.restartSoon}})
}
