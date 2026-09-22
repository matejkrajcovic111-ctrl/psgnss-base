package receiver

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// ValGet reads configuration keys from one layer.
//
// An item ID of 0xFFFF acts as a group wildcard: 0x0091FFFF reads every key in
// the CFG-MSGOUT group. Responses are paged; this follows the paging.
func (s *Session) ValGet(layer byte, keys []uint32) (map[uint32][]byte, error) {
	out := make(map[uint32][]byte)
	for _, key := range keys {
		pos := uint16(0)
		for page := 0; page < 64; page++ {
			req := make([]byte, 4+4)
			req[0] = 0 // version: request
			req[1] = layer
			binary.LittleEndian.PutUint16(req[2:4], pos)
			binary.LittleEndian.PutUint32(req[4:8], key)

			ms, err := s.poll(clsCFG, idCFGValGet, req, clsCFG, idCFGValGet, false)
			if err == ErrNAK {
				break // unknown group on this receiver; not fatal
			}
			if err != nil {
				if page == 0 {
					break
				}
				return out, err
			}
			n := 0
			for _, m := range ms {
				if m.cls != clsCFG || m.id != idCFGValGet || len(m.payload) < 4 {
					continue
				}
				kvs := parseCfgData(m.payload[4:])
				for _, kv := range kvs {
					out[kv.Key] = kv.Value
				}
				n += len(kvs)
			}
			if n < 64 {
				break
			}
			pos += uint16(n)
		}
	}
	return out, nil
}

func parseCfgData(d []byte) []KV {
	var out []KV
	for i := 0; i+4 <= len(d); {
		k := binary.LittleEndian.Uint32(d[i:])
		i += 4
		n := KeySize(k)
		if n == 0 || i+n > len(d) {
			break
		}
		v := make([]byte, n)
		copy(v, d[i:i+n])
		out = append(out, KV{Key: k, Value: v})
		i += n
	}
	return out
}

// valSet writes keys to the given layer bitmask, returning on ACK or NAK.
func (s *Session) valSet(layers byte, kvs []KV) error {
	payload := make([]byte, 4, 4+len(kvs)*12)
	payload[0] = 0 // version 0
	payload[1] = layers
	for _, kv := range kvs {
		if n := KeySize(kv.Key); n != len(kv.Value) {
			return fmt.Errorf("key 0x%08X declares %d value bytes, got %d", kv.Key, n, len(kv.Value))
		}
		var k [4]byte
		binary.LittleEndian.PutUint32(k[:], kv.Key)
		payload = append(payload, k[:]...)
		payload = append(payload, kv.Value...)
	}
	if _, err := s.rw.Write(Frame(clsCFG, idCFGValSet, payload)); err != nil {
		return fmt.Errorf("write VALSET: %w", err)
	}
	// Wait for the ACK that belongs to this request.
	deadline := time.Now().Add(s.Timeout)
	rd := make([]byte, 4096)
	for time.Now().Before(deadline) {
		n, err := s.rw.Read(rd)
		if n > 0 {
			s.buf = append(s.buf, rd[:n]...)
			var got []msg
			s.buf, _ = s.drain(&got, clsCFG, idCFGValSet)
			for _, m := range got {
				if m.cls == clsACK && len(m.payload) >= 2 &&
					m.payload[0] == clsCFG && m.payload[1] == idCFGValSet {
					if m.id == idACKAck {
						return nil
					}
					return ErrNAK
				}
			}
		}
		if err != nil {
			return err
		}
	}
	return ErrTimeout
}

// ChangeResult records what a single key write did.
type ChangeResult struct {
	Key      uint32
	Old      []byte
	New      []byte
	Verified bool
}

// Txn is a configuration change that has been applied to RAM but not yet made
// permanent.
//
// The safety model: writes land in RAM first, are read back and verified, and
// are then held by a revert timer. If Commit is not called before the timer
// expires, the previous values are restored. A configuration that kills the
// stream therefore cannot survive -- the operator does nothing and the receiver
// heals itself.
type Txn struct {
	sess    *Session
	log     *slog.Logger
	changes []ChangeResult
	timer   *time.Timer
	mu      sync.Mutex
	done    bool
	// Reverted is closed if the auto-revert fired.
	Reverted chan struct{}
	// Done is closed after either Commit or Revert. It lets the owner of a
	// multiplexed transport release it once the transaction is settled.
	Done chan struct{}
}

// Apply writes kvs to RAM, verifies by read-back, and arms an auto-revert.
//
// It never writes BBR or Flash; only Commit does that. That is what makes a bad
// configuration survivable across a power cycle: until Commit, Flash still
// holds the known-good values.
func (s *Session) Apply(kvs []KV, revertAfter time.Duration, log *slog.Logger) (*Txn, error) {
	if log == nil {
		log = slog.Default()
	}
	if len(kvs) == 0 {
		return nil, fmt.Errorf("no keys to apply")
	}
	keys := make([]uint32, 0, len(kvs))
	for _, kv := range kvs {
		keys = append(keys, kv.Key)
	}

	// Snapshot the current values so a revert is possible.
	before, err := s.ReadKeys(LayerRAM, keys)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	for _, k := range keys {
		if _, ok := before[k]; !ok {
			return nil, fmt.Errorf("key 0x%08X not readable on this receiver; refusing to write it", k)
		}
	}

	t := &Txn{sess: s, log: log, Reverted: make(chan struct{}), Done: make(chan struct{})}
	if err := s.setChunked(SetRAM, kvs); err != nil {
		// A later chunk may have failed after earlier ones landed; restore
		// whatever was read before the write rather than guessing.
		for _, kv := range kvs {
			t.changes = append(t.changes, ChangeResult{Key: kv.Key, Old: before[kv.Key], New: kv.Value})
		}
		t.revertNow("write failed")
		return nil, fmt.Errorf("VALSET to RAM: %w", err)
	}

	// Verify by read-back. A silent no-op is treated as a failure. After a
	// CFG-SIGNAL change the GNSS subsystem resets and polls can briefly go
	// unanswered, so the read-back is retried before being called a failure.
	var after map[uint32][]byte
	for attempt := 0; ; attempt++ {
		after, err = s.ReadKeys(LayerRAM, keys)
		// ValGet reports an unanswered first page as an empty result, so a
		// short read is retried as well as an error.
		if (err == nil && len(after) == len(keys)) || attempt == 2 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		for _, kv := range kvs {
			t.changes = append(t.changes, ChangeResult{Key: kv.Key, Old: before[kv.Key], New: kv.Value})
		}
		t.revertNow("read-back failed")
		return nil, fmt.Errorf("read back config: %w", err)
	}
	var unverified []string
	for _, kv := range kvs {
		got := after[kv.Key]
		ok := equalBytes(got, kv.Value)
		t.changes = append(t.changes, ChangeResult{
			Key: kv.Key, Old: before[kv.Key], New: kv.Value, Verified: ok,
		})
		if !ok {
			unverified = append(unverified, fmt.Sprintf("0x%08X (wrote %x, read %x)", kv.Key, kv.Value, got))
		}
	}
	if len(unverified) > 0 {
		// Roll back immediately rather than leaving a half-applied state.
		t.revertNow("verification failed")
		return nil, fmt.Errorf("config write not verified: %v", unverified)
	}

	if revertAfter <= 0 {
		revertAfter = 60 * time.Second
	}
	t.timer = time.AfterFunc(revertAfter, func() { t.revertNow("auto-revert timer expired") })
	log.Info("receiver config applied to RAM, awaiting confirmation",
		"keys", len(kvs), "auto_revert_in", revertAfter)
	return t, nil
}

// Changes reports what was applied.
func (t *Txn) Changes() []ChangeResult { return t.changes }

// Commit makes the change permanent (RAM already holds it; write BBR + Flash)
// and cancels the auto-revert.
func (t *Txn) Commit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return fmt.Errorf("transaction already finished")
	}
	kvs := make([]KV, 0, len(t.changes))
	for _, c := range t.changes {
		kvs = append(kvs, KV{Key: c.Key, Value: c.New})
	}
	if err := t.sess.setChunked(SetBBR|SetFlash, kvs); err != nil {
		return fmt.Errorf("persist to BBR+Flash: %w", err)
	}
	if t.timer != nil {
		t.timer.Stop()
	}
	t.done = true
	close(t.Done)
	t.log.Info("receiver config committed to BBR+Flash", "keys", len(kvs))
	return nil
}

// Revert restores the previous values explicitly.
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
	kvs := make([]KV, 0, len(t.changes))
	for _, c := range t.changes {
		if c.Old != nil {
			kvs = append(kvs, KV{Key: c.Key, Value: c.Old})
		}
	}
	t.done = true
	defer close(t.Reverted)
	defer close(t.Done)
	if len(kvs) == 0 {
		return nil
	}
	if err := t.sess.setChunked(SetRAM, kvs); err != nil {
		t.log.Error("REVERT FAILED -- receiver may be in an unintended state",
			"reason", reason, "err", err)
		return err
	}
	t.log.Warn("receiver config reverted", "reason", reason, "keys", len(kvs))
	return nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// maxValSetKeys is the UBX-CFG-VALSET limit on key/value pairs per message.
const maxValSetKeys = 64

// signalSettle is how long to wait after a CFG-SIGNAL write. u-blox: such
// changes reset the GNSS subsystem; wait for the ACK and then 0.5 s before
// sending the next command. This allows double that.
const signalSettle = time.Second

// setChunked writes kvs in VALSET-sized batches, pausing after any batch that
// touches CFG-SIGNAL.
func (s *Session) setChunked(layers byte, kvs []KV) error {
	for len(kvs) > 0 {
		n := min(len(kvs), maxValSetKeys)
		if err := s.valSet(layers, kvs[:n]); err != nil {
			return err
		}
		for _, kv := range kvs[:n] {
			if kv.Key>>16&0xFF == groupSignal {
				time.Sleep(signalSettle)
				break
			}
		}
		kvs = kvs[n:]
	}
	return nil
}

const groupSignal = 0x31

// ReadKeys reads exactly the requested keys. Groups with several requested
// keys are read with one wildcard poll rather than one poll per key.
func (s *Session) ReadKeys(layer byte, keys []uint32) (map[uint32][]byte, error) {
	byGroup := map[byte][]uint32{}
	for _, k := range keys {
		g := byte(k >> 16)
		byGroup[g] = append(byGroup[g], k)
	}
	groups := make([]int, 0, len(byGroup))
	for g := range byGroup {
		groups = append(groups, int(g))
	}
	sort.Ints(groups)
	out := make(map[uint32][]byte, len(keys))
	for _, gi := range groups {
		g := byte(gi)
		want := byGroup[g]
		var got map[uint32][]byte
		var err error
		if len(want) > 4 {
			got, err = s.ValGet(layer, []uint32{GroupWildcard(g)})
		} else {
			got, err = s.ValGet(layer, want)
		}
		if err != nil {
			return out, err
		}
		for _, k := range want {
			if v, ok := got[k]; ok {
				out[k] = v
			}
		}
	}
	return out, nil
}

// SortedKeys returns map keys in ascending order, for stable output.
func SortedKeys(m map[uint32][]byte) []uint32 {
	out := make([]uint32, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// GroupWildcard builds the VALGET key that reads every item in a group.
func GroupWildcard(group byte) uint32 { return uint32(group)<<16 | 0xFFFF }
