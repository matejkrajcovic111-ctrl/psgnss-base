package unicore

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Snapshot is the part of the receiver's state an operator can change from the
// console: the CONFIG lines, the active outputs, the mode and the masks.
type Snapshot struct {
	Config  []ConfigLine `json:"config"`
	Outputs []Output     `json:"outputs"`
	Mode    Mode         `json:"mode"`
	Masks   []string     `json:"masks"`
}

// Snapshot reads CONFIG, UNILOGLIST, MODE and MASK.
func (s *Session) Snapshot() (*Snapshot, error) {
	s.internal = true
	defer func() { s.internal = false }()
	snap := &Snapshot{}
	r, err := s.Do("CONFIG")
	if err == nil {
		err = r.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("read CONFIG: %w", err)
	}
	snap.Config = ParseConfig(r)
	if len(snap.Config) == 0 {
		return nil, fmt.Errorf("read CONFIG: the receiver listed no configuration")
	}
	if r, err = s.Do("UNILOGLIST"); err == nil {
		if err = r.Err(); err == nil {
			snap.Outputs, err = ParseOutputs(r)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("read UNILOGLIST: %w", err)
	}
	if r, err = s.Do("MODE"); err == nil {
		if err = r.Err(); err == nil {
			snap.Mode, err = ParseMode(r)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("read MODE: %w", err)
	}
	if r, err = s.Do("MASK"); err == nil {
		err = r.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("read MASK: %w", err)
	}
	snap.Masks = ParseMasks(r)
	return snap, nil
}

// Change is one observed difference between two snapshots; Old or New is empty
// when the item appeared or disappeared.
type Change struct {
	Kind string `json:"kind"` // config, output, mode, mask
	Name string `json:"name"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

var elevationMask = regexp.MustCompile(`^MASK\s+-?\d+(\.\d+)?$`)

// Diff lists what differs from old to cur, in a stable order.
func Diff(old, cur *Snapshot) []Change {
	var out []Change
	// Masks: compared as the commands that recreate them. The elevation mask
	// is a single replaceable value; the others are a set.
	oldM, curM := map[string]bool{}, map[string]bool{}
	oldElev, curElev := "", ""
	for _, c := range maskCommands(old.Masks) {
		if elevationMask.MatchString(c) {
			oldElev = c
		} else {
			oldM[c] = true
		}
	}
	for _, c := range maskCommands(cur.Masks) {
		if elevationMask.MatchString(c) {
			curElev = c
		} else {
			curM[c] = true
		}
	}
	if oldElev != curElev {
		out = append(out, Change{Kind: "mask", Name: "elevation", Old: oldElev, New: curElev})
	}
	for _, c := range maskCommands(old.Masks) {
		if oldM[c] && !curM[c] {
			out = append(out, Change{Kind: "mask", Name: c, Old: c})
		}
	}
	for _, c := range maskCommands(cur.Masks) {
		if curM[c] && !oldM[c] {
			out = append(out, Change{Kind: "mask", Name: c, New: c})
		}
	}
	oc := map[string]string{}
	for _, c := range old.Config {
		oc[c.Name] = c.Line
	}
	cc := map[string]string{}
	for _, c := range cur.Config {
		cc[c.Name] = c.Line
	}
	for _, c := range old.Config {
		if cc[c.Name] != c.Line {
			out = append(out, Change{Kind: "config", Name: c.Name, Old: c.Line, New: cc[c.Name]})
		}
	}
	for _, c := range cur.Config {
		if _, ok := oc[c.Name]; !ok {
			out = append(out, Change{Kind: "config", Name: c.Name, New: c.Line})
		}
	}
	if old.Mode.Command != cur.Mode.Command {
		out = append(out, Change{Kind: "mode", Name: "MODE", Old: old.Mode.Command, New: cur.Mode.Command})
	}
	key := func(o Output) string { return o.Message + " " + o.Port }
	oo, co := map[string]Output{}, map[string]Output{}
	for _, o := range old.Outputs {
		oo[key(o)] = o
	}
	for _, o := range cur.Outputs {
		co[key(o)] = o
	}
	for _, o := range old.Outputs {
		if n, ok := co[key(o)]; !ok {
			out = append(out, Change{Kind: "output", Name: key(o), Old: o.Command()})
		} else if n.Rate != o.Rate {
			out = append(out, Change{Kind: "output", Name: key(o), Old: o.Command(), New: n.Command()})
		}
	}
	for _, o := range cur.Outputs {
		if _, ok := oo[key(o)]; !ok {
			out = append(out, Change{Kind: "output", Name: key(o), New: o.Command()})
		}
	}
	return out
}

// RestoreCommands are the commands that turn the receiver's state back into
// the one the changes started from. Items that appeared and have no "off"
// command (a new CONFIG line, a new elevation mask) cannot be restored and are
// returned in unrestorable.
func RestoreCommands(changes []Change) (cmds, unrestorable []string) {
	for _, c := range changes {
		switch c.Kind {
		case "config":
			if c.Old == "" {
				unrestorable = append(unrestorable, c.New)
			} else {
				cmds = append(cmds, c.Old)
			}
		case "mode":
			if c.Old == "" {
				unrestorable = append(unrestorable, c.New)
			} else {
				cmds = append(cmds, c.Old)
			}
		case "mask":
			switch {
			case c.Old != "":
				cmds = append(cmds, c.Old)
			case c.Name == "elevation":
				unrestorable = append(unrestorable, c.New)
			default:
				cmds = append(cmds, "UN"+c.New)
			}
		case "output":
			if c.Old == "" {
				f := strings.Fields(c.New)
				cmds = append(cmds, fmt.Sprintf("UNLOG %s %s", f[1], f[0]))
			} else {
				cmds = append(cmds, c.Old)
			}
		}
	}
	return cmds, unrestorable
}

// Txn is a set of commands applied to the running configuration but not yet
// saved. Like the UBX transaction it reverts by itself unless Commit arrives.
type Txn struct {
	sess    *Session
	log     *slog.Logger
	Before  *Snapshot
	After   *Snapshot
	Cmds    []string
	Replies []Reply

	timer *time.Timer
	mu    sync.Mutex
	done  bool
	// Reverted is closed if the transaction ended by reverting; Done is closed
	// when it ended either way.
	Reverted chan struct{}
	Done     chan struct{}
}

// Changes reports what the commands changed, as read back from the receiver.
func (t *Txn) Changes() []Change { return Diff(t.Before, t.After) }

// Apply snapshots the receiver, sends cmds in order (each must be acknowledged
// OK), re-reads the snapshot and arms an auto-revert. post, if set, runs after
// the commands and may return corrective commands (it can also veto by
// returning an error, which reverts). On any failure the snapshot difference
// is restored before returning, so a refused or half-applied change does not
// stay behind. The replies are returned even on failure so the console can
// show the receiver's own words.
func (s *Session) Apply(cmds []string, revertAfter time.Duration, log *slog.Logger,
	post func(*Session) ([]string, error)) (*Txn, []Reply, error) {
	if log == nil {
		log = slog.Default()
	}
	before, err := s.Snapshot()
	if err != nil {
		return nil, nil, err
	}
	t := &Txn{sess: s, log: log, Before: before, Reverted: make(chan struct{}), Done: make(chan struct{})}
	var replies []Reply
	fail := func(cause error) (*Txn, []Reply, error) {
		if rerr := s.restore(before); rerr != nil {
			cause = fmt.Errorf("%w; AND the restore failed, receiver may be in an unintended state: %v", cause, rerr)
		}
		return nil, replies, cause
	}
	send := func(list []string) error {
		for _, c := range list {
			r, err := s.Do(c)
			replies = append(replies, r)
			t.Cmds = append(t.Cmds, c)
			if err != nil {
				return err
			}
			if err := r.Err(); err != nil {
				return err
			}
		}
		return nil
	}
	if err := send(cmds); err != nil {
		return fail(err)
	}
	if t.After, err = s.Snapshot(); err != nil {
		return fail(fmt.Errorf("read back: %w", err))
	}
	if post != nil {
		extra, err := post(s)
		if err != nil {
			return fail(err)
		}
		if len(extra) > 0 {
			if err := send(extra); err != nil {
				return fail(err)
			}
			if t.After, err = s.Snapshot(); err != nil {
				return fail(fmt.Errorf("read back: %w", err))
			}
		}
	}
	t.Replies = replies
	if revertAfter <= 0 {
		revertAfter = 60 * time.Second
	}
	t.timer = time.AfterFunc(revertAfter, func() { _ = t.revertNow("auto-revert timer expired") })
	log.Info("unicore change applied, awaiting confirmation", "commands", len(t.Cmds),
		"changes", len(t.Changes()), "auto_revert_in", revertAfter)
	return t, replies, nil
}

// restore re-issues what differs between the receiver now and want, then
// checks that nothing differs any more.
func (s *Session) restore(want *Snapshot) error {
	cur, err := s.Snapshot()
	if err != nil {
		return fmt.Errorf("read current state: %w", err)
	}
	cmds, unrestorable := RestoreCommands(Diff(want, cur))
	for _, c := range cmds {
		r, err := s.Do(c)
		if err == nil {
			err = r.Err()
		}
		if err != nil {
			return fmt.Errorf("restore with %q: %w", Redact(c), err)
		}
	}
	if len(cmds) == 0 && len(unrestorable) == 0 {
		return nil
	}
	after, err := s.Snapshot()
	if err != nil {
		return fmt.Errorf("verify restore: %w", err)
	}
	skip := map[string]bool{}
	for _, u := range unrestorable {
		skip[u] = true
	}
	var left []string
	for _, c := range Diff(want, after) {
		if c.Old == "" && skip[c.New] {
			continue // appeared and has no "off" command; nothing to verify
		}
		left = append(left, c.Kind+" "+c.Name)
	}
	if len(left) > 0 {
		return fmt.Errorf("still different after restore: %s", strings.Join(left, ", "))
	}
	return nil
}

// Commit saves the running configuration (SAVECONFIG) and cancels the revert.
func (t *Txn) Commit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return fmt.Errorf("transaction already finished")
	}
	r, err := t.sess.Do("SAVECONFIG")
	if err == nil {
		err = r.Err()
	}
	if err != nil {
		return fmt.Errorf("SAVECONFIG: %w", err)
	}
	if t.timer != nil {
		t.timer.Stop()
	}
	t.done = true
	close(t.Done)
	t.log.Info("unicore configuration saved (SAVECONFIG)", "commands", len(t.Cmds))
	return nil
}

// Revert restores the snapshot now.
func (t *Txn) Revert() error { return t.revertNow("explicit revert") }

func (t *Txn) revertNow(reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil
	}
	if t.timer != nil {
		t.timer.Stop()
	}
	t.done = true
	defer close(t.Reverted)
	defer close(t.Done)
	if err := t.sess.restore(t.Before); err != nil {
		t.log.Error("UNICORE REVERT FAILED -- receiver may be in an unintended state", "reason", reason, "err", err)
		return err
	}
	t.log.Warn("unicore configuration reverted", "reason", reason, "commands", len(t.Cmds))
	return nil
}
