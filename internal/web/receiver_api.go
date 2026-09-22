package web

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/psgnss/psgnss-base/internal/boardprofile"
	"github.com/psgnss/psgnss-base/internal/receiver"
	"github.com/psgnss/psgnss-base/internal/store"
)

// defaultStage0Path is where the Stage 0 receiver dump is deployed. It holds
// the base position at full precision, so it lives in the state directory,
// root-only, and is never embedded in the binary.
const defaultStage0Path = "/var/lib/psgnss/receiver-stage0.json"

// maxReceiverChanges bounds one operator transaction. A Stage 0 restore is
// not bounded by it; its size is whatever the receiver has drifted by.
const maxReceiverChanges = 64

type pendingReceiverTxn struct {
	txn            *receiver.Txn
	pipe           io.Closer
	logIDs         []int64
	until          time.Time
	label          string
	beforeCommit   func() error
	rollbackCommit func()
	afterCommit    func()
}

var receiverGroups = []struct {
	id   byte
	name string
}{
	{0x03, "Base mode and fixed position"}, {0x09, "RTCM station settings"},
	{0x21, "Measurement rate"}, {0x31, "Constellations and signals"},
	{0x91, "Output messages and rates"},
}

func (s *Server) receiverSession() (*receiver.Session, io.Closer, error) {
	if s.Cfg != nil {
		profile, err := boardprofile.Resolve(s.Cfg.Receiver.Profile, s.Cfg.Receiver.Model)
		if err != nil {
			return nil, nil, err
		}
		if !profile.Implemented || profile.Driver != boardprofile.DriverUBXVal {
			return nil, nil, fmt.Errorf("%s receiver control is not implemented", profile.Name)
		}
	}
	open := s.receiverControl
	if open == nil {
		if s.Hub == nil {
			return nil, nil, errors.New("receiver control is unavailable without a hub")
		}
		open = s.Hub.Control
	}
	p, err := open()
	if err != nil {
		return nil, nil, err
	}
	r := receiver.NewSession(p)
	r.Timeout = 4 * time.Second
	return r, p, nil
}

func (s *Server) stage0Path() string {
	if s.Stage0Path != "" {
		return s.Stage0Path
	}
	return defaultStage0Path
}

type receiverRow struct {
	Key        string               `json:"key"`
	Value      string               `json:"value"`
	Display    string               `json:"display"`
	Name       string               `json:"name,omitempty"`
	Type       string               `json:"type,omitempty"`
	Unit       string               `json:"unit,omitempty"`
	Scale      string               `json:"scale,omitempty"`
	Desc       string               `json:"desc,omitempty"`
	Enum       []receiver.EnumValue `json:"enum,omitempty"`
	Documented bool                 `json:"documented"`
}

func receiverRowFor(k uint32, v []byte) receiverRow {
	row := receiverRow{Key: fmt.Sprintf("0x%08X", k), Value: hex.EncodeToString(v),
		Display: receiver.DisplayValue(k, v)}
	if info, ok := receiver.LookupKey(k); ok {
		row.Name, row.Type, row.Unit, row.Scale, row.Desc, row.Enum, row.Documented =
			info.Name, info.Type, info.Unit, info.Scale, info.Desc, info.Enum, true
	}
	return row
}

func receiverRows(kv map[uint32][]byte) []receiverRow {
	keys := receiver.SortedKeys(kv)
	out := make([]receiverRow, 0, len(keys))
	for _, k := range keys {
		out = append(out, receiverRowFor(k, kv[k]))
	}
	return out
}

// lockReceiver takes the receiver for one request. It fails while a change is
// awaiting Keep or Revert: that transaction holds the control session.
func (s *Server) lockReceiver(w http.ResponseWriter) (*receiver.Session, func(), bool) {
	s.receiverMu.Lock()
	if s.receiverTxn != nil {
		s.receiverMu.Unlock()
		writeErr(w, http.StatusConflict, "a receiver change is awaiting Keep or auto-revert")
		return nil, nil, false
	}
	rx, pipe, err := s.receiverSession()
	if err != nil {
		s.receiverMu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return nil, nil, false
	}
	return rx, func() { pipe.Close(); s.receiverMu.Unlock() }, true
}

// handleReceiver reads live receiver state through the hub's serial owner.
// It never opens the USB device itself, so polling cannot steal the stream.
func (s *Server) handleReceiver(w http.ResponseWriter, r *http.Request) {
	// Tests and embedders may provide only a receiver-control hook. Preserve
	// that long-standing path as the fully supported X20P profile.
	profile, err := boardprofile.Resolve("", "ZED-X20P")
	if s.Cfg != nil {
		profile, err = boardprofile.Resolve(s.Cfg.Receiver.Profile, s.Cfg.Receiver.Model)
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if !profile.Implemented || profile.Driver != boardprofile.DriverUBXVal {
		writeErr(w, http.StatusNotImplemented, profile.Name+" receiver control is not implemented; stream forwarding remains available")
		return
	}
	s.receiverMu.Lock()
	if p := s.receiverTxn; p != nil {
		s.receiverMu.Unlock()
		// The UI needs to recover the pending state after a reload.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "a receiver change is awaiting Keep or auto-revert",
			"pending": pendingView(p)})
		return
	}
	s.receiverMu.Unlock()
	rx, release, ok := s.lockReceiver(w)
	if !ok {
		return
	}
	defer release()
	v, err := rx.Version()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "receiver MON-VER: "+err.Error())
		return
	}
	if v.Model() != "" && !strings.EqualFold(v.Model(), profile.Receiver) {
		writeErr(w, http.StatusConflict, "receiver reports "+v.Model()+" but board profile expects "+profile.Receiver)
		return
	}
	resp := map[string]any{"model": v.Model(), "firmware": v.Firmware(),
		"software": v.SwVersion, "hardware": v.HwVersion, "protocol": v.Protver(),
		"constellations": v.Constellations(), "pending": nil, "read_at": time.Now().UTC(),
		"board_profile": profile}
	if sys, err := rx.System(); err != nil {
		resp["system_error"] = "MON-SYS: " + err.Error()
	} else {
		resp["system"] = sys
	}
	groups := make([]map[string]any, 0, len(receiverGroups))
	for _, g := range receiverGroups {
		kv, e := rx.ValGet(receiver.LayerRAM, []uint32{receiver.GroupWildcard(g.id)})
		if e != nil {
			groups = append(groups, map[string]any{"id": g.id, "name": g.name, "error": e.Error()})
			continue
		}
		switch g.id {
		case 0x31:
			resp["signals"] = receiver.Constellations(kv, v.Constellations())
		case 0x91:
			resp["messages"] = receiver.MessageOutputs(kv)
			resp["ports"] = receiver.MessagePorts
		}
		groups = append(groups, map[string]any{"id": g.id, "name": g.name, "keys": receiverRows(kv)})
	}
	resp["groups"] = groups
	_, statErr := receiver.LoadStage0(s.stage0Path())
	resp["stage0_available"] = statErr == nil
	writeJSON(w, http.StatusOK, resp)
}

type receiverChange struct {
	Key   string `json:"key"`
	Value string `json:"value,omitempty"` // raw little-endian hex
	Text  string `json:"text,omitempty"`  // typed value, parsed with the key catalogue
}

// receiverApplyRequest accepts either one legacy raw key/value, or a list of
// changes applied as one transaction.
type receiverApplyRequest struct {
	Key     string           `json:"key"`
	Value   string           `json:"value"`
	Name    string           `json:"name"`
	Label   string           `json:"label"`
	Changes []receiverChange `json:"changes"`
}

func parseReceiverKey(s string) (uint32, error) {
	k := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	n, err := strconv.ParseUint(k, 16, 32)
	if err != nil || receiver.KeySize(uint32(n)) == 0 {
		return 0, fmt.Errorf("key %q must be a hexadecimal configuration key, e.g. 0x20910006", s)
	}
	return uint32(n), nil
}

func parseReceiverChange(c receiverChange) (receiver.KV, error) {
	k, err := parseReceiverKey(c.Key)
	if err != nil {
		return receiver.KV{}, err
	}
	switch {
	case c.Text != "" && c.Value != "":
		return receiver.KV{}, fmt.Errorf("key %s: give value or text, not both", c.Key)
	case c.Text != "":
		b, err := receiver.ParseValue(k, c.Text)
		if err != nil {
			return receiver.KV{}, err
		}
		return receiver.KV{Key: k, Value: b}, nil
	}
	b, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(c.Value), " ", ""))
	if err != nil {
		return receiver.KV{}, errors.New("value must be even-length hexadecimal")
	}
	if receiver.KeySize(k) != len(b) {
		return receiver.KV{}, fmt.Errorf("key %s needs a %d-byte value", c.Key, receiver.KeySize(k))
	}
	return receiver.KV{Key: k, Value: b}, nil
}

func (in receiverApplyRequest) kvs() ([]receiver.KV, error) {
	changes := in.Changes
	if len(changes) == 0 {
		if in.Key == "" {
			return nil, errors.New("no changes given")
		}
		changes = []receiverChange{{Key: in.Key, Value: in.Value}}
	}
	if len(changes) > maxReceiverChanges {
		return nil, fmt.Errorf("at most %d keys per change", maxReceiverChanges)
	}
	seen := map[uint32]bool{}
	out := make([]receiver.KV, 0, len(changes))
	for _, c := range changes {
		kv, err := parseReceiverChange(c)
		if err != nil {
			return nil, err
		}
		if seen[kv.Key] {
			return nil, fmt.Errorf("key 0x%08X appears twice", kv.Key)
		}
		seen[kv.Key] = true
		out = append(out, kv)
	}
	return out, nil
}

// handleReceiverApply makes a verified RAM-only change first. The transaction
// expires automatically; commit is a separate deliberate action which writes
// BBR and Flash only after the operator has observed the live stream.
func (s *Server) handleReceiverApply(w http.ResponseWriter, r *http.Request) {
	var in receiverApplyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	kvs, err := in.kvs()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	label := strings.TrimSpace(in.Label)
	if label == "" {
		label = strings.TrimSpace(in.Name)
	}
	s.beginReceiverTxn(w, r, kvs, label, nil)
}

// beginReceiverTxn applies kvs to RAM, verifies, and holds the result pending
// Keep. check, if set, runs on the locked session first and may refuse.
func (s *Server) beginReceiverTxn(w http.ResponseWriter, r *http.Request, kvs []receiver.KV,
	label string, check func(*receiver.Session) ([]receiver.KV, int, string)) {
	s.beginReceiverTxnWithHooks(w, r, kvs, label, check, receiverTxnHooks{})
}

type receiverTxnHooks struct {
	beforeCommit   func() error
	rollbackCommit func()
	afterCommit    func()
}

func (s *Server) beginReceiverTxnWithHooks(w http.ResponseWriter, r *http.Request, kvs []receiver.KV,
	label string, check func(*receiver.Session) ([]receiver.KV, int, string), hooks receiverTxnHooks) {
	s.receiverMu.Lock()
	if s.receiverTxn != nil {
		s.receiverMu.Unlock()
		writeErr(w, http.StatusConflict, "a receiver change is already pending")
		return
	}
	rx, pipe, err := s.receiverSession()
	if err != nil {
		s.receiverMu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	fail := func(code int, msg string) {
		pipe.Close()
		s.receiverMu.Unlock()
		writeErr(w, code, msg)
	}
	if s.Cfg != nil {
		profile, _ := boardprofile.Resolve(s.Cfg.Receiver.Profile, s.Cfg.Receiver.Model)
		v, e := rx.Version()
		if e != nil {
			fail(http.StatusBadGateway, "receiver MON-VER: "+e.Error())
			return
		}
		if v.Model() != "" && !strings.EqualFold(v.Model(), profile.Receiver) {
			fail(http.StatusConflict, "receiver reports "+v.Model()+" but board profile expects "+profile.Receiver)
			return
		}
	}
	if check != nil {
		var code int
		var msg string
		if kvs, code, msg = check(rx); code != 0 {
			fail(code, msg)
			return
		}
	}
	d := time.Duration(s.revertSeconds()) * time.Second
	txn, err := rx.Apply(kvs, d, s.Log)
	if err != nil {
		for _, kv := range kvs {
			_, _ = s.Store.LogReceiverConfig(store.ReceiverConfigLog{Actor: adminOf(r),
				KeyID: fmt.Sprintf("0x%08X", kv.Key), KeyName: auditName(kv.Key, label),
				NewValue: hex.EncodeToString(kv.Value), Reverted: true, Note: "not applied: " + err.Error()})
		}
		fail(http.StatusBadGateway, err.Error())
		return
	}
	changes := txn.Changes()
	ids := make([]int64, 0, len(changes))
	for _, c := range changes {
		id, e := s.Store.LogReceiverConfig(store.ReceiverConfigLog{Actor: adminOf(r),
			KeyID: fmt.Sprintf("0x%08X", c.Key), KeyName: auditName(c.Key, label),
			OldValue: hex.EncodeToString(c.Old), NewValue: hex.EncodeToString(c.New),
			Verified: c.Verified, Note: "awaiting explicit commit"})
		if e == nil {
			ids = append(ids, id)
		}
	}
	p := &pendingReceiverTxn{txn: txn, pipe: pipe, logIDs: ids, until: time.Now().Add(d), label: label,
		beforeCommit: hooks.beforeCommit, rollbackCommit: hooks.rollbackCommit, afterCommit: hooks.afterCommit}
	s.receiverTxn = p
	go s.watchReceiverTxn(p)
	s.receiverMu.Unlock()
	writeJSON(w, http.StatusOK, pendingView(p))
}

func (s *Server) revertSeconds() int {
	if s.Cfg != nil && s.Cfg.Receiver.RevertTimeout > 0 {
		return s.Cfg.Receiver.RevertTimeout
	}
	return 60
}

// auditName records the documented key name, keeping the operator's label.
func auditName(k uint32, label string) string {
	name := receiver.KeyName(k)
	switch {
	case name == "":
		return label
	case label == "":
		return name
	}
	return name + " — " + label
}

type changeView struct {
	receiverRow
	Old        string `json:"old"`
	OldDisplay string `json:"old_display"`
	Verified   bool   `json:"verified"`
}

func pendingView(p *pendingReceiverTxn) map[string]any {
	cs := p.txn.Changes()
	out := make([]changeView, 0, len(cs))
	for _, c := range cs {
		out = append(out, changeView{receiverRow: receiverRowFor(c.Key, c.New),
			Old: hex.EncodeToString(c.Old), OldDisplay: receiver.DisplayValue(c.Key, c.Old), Verified: c.Verified})
	}
	return map[string]any{"ok": true, "verified": true, "revert_at": p.until.UTC(),
		"label": p.label, "changes": out}
}

func (s *Server) watchReceiverTxn(p *pendingReceiverTxn) {
	<-p.txn.Done
	s.receiverMu.Lock()
	if s.receiverTxn == p {
		select {
		case <-p.txn.Reverted:
			for _, id := range p.logIDs {
				_ = s.Store.MarkReceiverConfigReverted(id)
			}
		default:
		}
		p.pipe.Close()
		s.receiverTxn = nil
	}
	s.receiverMu.Unlock()
}

func (s *Server) settleReceiver(w http.ResponseWriter, r *http.Request, commit bool) {
	s.receiverMu.Lock()
	p := s.receiverTxn
	if p == nil {
		s.receiverMu.Unlock()
		writeErr(w, http.StatusConflict, "no receiver change is pending")
		return
	}
	var err error
	if commit {
		if p.beforeCommit != nil {
			err = p.beforeCommit()
		}
		if err == nil {
			err = p.txn.Commit()
			if err != nil && p.rollbackCommit != nil {
				p.rollbackCommit()
			}
		}
	} else {
		err = p.txn.Revert()
	}
	if err != nil {
		s.receiverMu.Unlock()
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if !commit {
		for _, id := range p.logIDs {
			_ = s.Store.MarkReceiverConfigReverted(id)
		}
	}
	p.pipe.Close()
	s.receiverTxn = nil
	s.receiverMu.Unlock()
	s.Log.Info("receiver configuration transaction settled", "by", adminOf(r), "committed", commit,
		"label", p.label, "keys", len(p.txn.Changes()))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "committed": commit})
	if commit && p.afterCommit != nil {
		go p.afterCommit()
	}
}
func (s *Server) handleReceiverCommit(w http.ResponseWriter, r *http.Request) {
	s.settleReceiver(w, r, true)
}
func (s *Server) handleReceiverRevert(w http.ResponseWriter, r *http.Request) {
	s.settleReceiver(w, r, false)
}

// stage0Compare reads every group in the Stage 0 snapshot from the live
// receiver and returns the keys that differ.
func stage0Compare(rx *receiver.Session, snap *receiver.Stage0) ([]receiver.Stage0Diff, []uint32, error) {
	live := map[uint32][]byte{}
	for _, g := range snap.Groups() {
		kv, err := rx.ValGet(receiver.LayerRAM, []uint32{receiver.GroupWildcard(g)})
		if err != nil {
			return nil, nil, fmt.Errorf("read group 0x%02X: %w", g, err)
		}
		for k, v := range kv {
			live[k] = v
		}
	}
	diffs, unreadable := snap.Diff(live)
	return diffs, unreadable, nil
}

func (s *Server) loadStage0(w http.ResponseWriter) (*receiver.Stage0, bool) {
	snap, err := receiver.LoadStage0(s.stage0Path())
	if err != nil {
		writeErr(w, http.StatusNotFound, "Stage 0 snapshot is not available on this system: "+err.Error())
		return nil, false
	}
	return snap, true
}

// handleStage0Preview shows exactly what a Stage 0 restore would write.
func (s *Server) handleStage0Preview(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.loadStage0(w)
	if !ok {
		return
	}
	rx, release, ok := s.lockReceiver(w)
	if !ok {
		return
	}
	defer release()
	v, err := rx.Version()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "receiver MON-VER: "+err.Error())
		return
	}
	diffs, unreadable, err := stage0Compare(rx, snap)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	rows := make([]map[string]any, 0, len(diffs))
	for _, d := range diffs {
		rows = append(rows, map[string]any{"row": receiverRowFor(d.Key, d.Stage0),
			"current": hex.EncodeToString(d.Current), "current_display": receiver.DisplayValue(d.Key, d.Current)})
	}
	un := make([]string, 0, len(unreadable))
	for _, k := range unreadable {
		un = append(un, fmt.Sprintf("0x%08X", k))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot_model": snap.Model, "snapshot_software": snap.Software,
		"receiver_model": v.Model(), "receiver_software": v.SwVersion,
		"model_matches": snap.Model == v.Model() && snap.Software == v.SwVersion,
		"snapshot_keys": len(snap.Values), "differences": rows, "unreadable": un})
}

// handleStage0Restore applies the Stage 0 values that differ, as one ordinary
// verified RAM transaction with auto-revert. The diff is recomputed under the
// receiver lock; the client only confirms how many keys it was shown, so a
// receiver that changed after the preview is refused rather than surprised.
func (s *Server) handleStage0Restore(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Expected *int `json:"expected"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.Expected == nil {
		writeErr(w, http.StatusBadRequest, "expected: the number of differences shown in the preview is required")
		return
	}
	snap, ok := s.loadStage0(w)
	if !ok {
		return
	}
	s.beginReceiverTxn(w, r, nil, "Restore Stage 0 snapshot", func(rx *receiver.Session) ([]receiver.KV, int, string) {
		v, err := rx.Version()
		if err != nil {
			return nil, http.StatusBadGateway, "receiver MON-VER: " + err.Error()
		}
		if snap.Model != v.Model() || snap.Software != v.SwVersion {
			return nil, http.StatusConflict, fmt.Sprintf("snapshot was taken on %s %s; receiver is %s %s",
				snap.Model, snap.Software, v.Model(), v.SwVersion)
		}
		diffs, _, err := stage0Compare(rx, snap)
		if err != nil {
			return nil, http.StatusBadGateway, err.Error()
		}
		if len(diffs) != *in.Expected {
			return nil, http.StatusConflict, fmt.Sprintf("receiver now differs from Stage 0 in %d keys, not %d; preview again", len(diffs), *in.Expected)
		}
		if len(diffs) == 0 {
			return nil, http.StatusConflict, "receiver already matches the Stage 0 snapshot"
		}
		kvs := make([]receiver.KV, 0, len(diffs))
		for _, d := range diffs {
			kvs = append(kvs, receiver.KV{Key: d.Key, Value: d.Stage0})
		}
		return kvs, 0, ""
	})
}
