package caster

import (
	"sync/atomic"
)

// Tap receives a copy of every frame a mountpoint broadcasts, including the
// synthesised 1006/1008/1033 and the end-of-epoch marker repair. It exists so
// that an outbound NTRIP push sends rovers on a remote caster exactly the bytes
// a rover connected here would receive, rather than a second stream rebuilt
// from the same filter definition and free to drift from it.
type Tap struct {
	Name string

	ch      chan []byte
	mount   *Mount
	closed  atomic.Bool
	Dropped atomic.Int64
}

// C is the frame channel. It is closed when the tap is closed.
func (t *Tap) C() <-chan []byte { return t.ch }

// Close detaches the tap. It is safe to call more than once.
func (t *Tap) Close() {
	if t.closed.Swap(true) {
		return
	}
	t.mount.mu.Lock()
	delete(t.mount.taps, t)
	t.mount.mu.Unlock()
	close(t.ch)
}

// Tap attaches a named tap with a bounded queue. A tap that cannot keep up
// drops frames and counts them; it never stalls the mountpoint, because a slow
// remote caster must not be able to delay the rovers served here.
func (m *Mount) Tap(name string, queue int) *Tap {
	if queue <= 0 {
		queue = 512
	}
	t := &Tap{Name: name, ch: make(chan []byte, queue), mount: m}
	m.mu.Lock()
	if m.taps == nil {
		m.taps = make(map[*Tap]struct{})
	}
	m.taps[t] = struct{}{}
	m.mu.Unlock()
	return t
}

// Replay returns the remembered station-description frames, so a push that
// connects mid-epoch does not wait up to 10 s for 1006/1008/1033.
func (m *Mount) Replay() [][]byte {
	m.lastMu.Lock()
	defer m.lastMu.Unlock()
	out := make([][]byte, 0, len(m.last))
	for _, b := range m.last {
		out = append(out, b)
	}
	return out
}

// Publish broadcasts one frame to this mountpoint's clients and taps. The hub
// feed and the push-in handler reach broadcast directly; this exists for feeds
// assembled elsewhere and for tests that need a mountpoint without a receiver.
func (m *Mount) Publish(b []byte) { m.broadcast(b) }
