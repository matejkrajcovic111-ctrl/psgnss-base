package hub

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Hub owns the single input stream and fans frames out to many outputs.
//
// The central rule is that a slow or stuck output must never stall the input.
// Each output has a bounded queue; when it fills, frames for that output are
// dropped and counted. One wedged rover cannot back-pressure the receiver.
type Hub struct {
	src        Source
	bufSize    int
	stallWarn  time.Duration
	retryDelay time.Duration
	log        *slog.Logger

	mu   sync.RWMutex
	subs map[int]*Sub
	next int

	// The receiver is physically one serial device.  Control sessions borrow
	// the hub's already-open descriptor and receive replies from the framed
	// input, rather than ever opening the device a second time.
	controlMu sync.Mutex
	writeMu   sync.Mutex
	writer    io.Writer
	serial    bool

	Stats Stats
}

// Stats are cumulative hub counters, safe for concurrent reads.
type Stats struct {
	BytesIn      atomic.Int64
	FramesIn     atomic.Int64
	BytesDropped atomic.Int64
	Resyncs      atomic.Int64
	Reconnects   atomic.Int64
	UBXFrames    atomic.Int64
	RTCMFrames   atomic.Int64
	NMEAFrames   atomic.Int64
	LastFrameAt  atomic.Int64 // unix nanos
	// LastDropAt marks when unframed bytes were last discarded. Attaching to a
	// live stream always costs a partial frame at startup, so the total alone
	// cannot distinguish a healthy stream from a degrading one -- when it last
	// happened is the part that matters.
	LastDropAt atomic.Int64 // unix seconds
}

// Sub is one output's subscription.
type Sub struct {
	id     int
	Name   string
	filter *Filter
	ch     chan []byte
	hub    *Hub

	Sent    atomic.Int64
	Dropped atomic.Int64
}

// C is the frame channel. Each value is a copy safe to retain.
func (s *Sub) C() <-chan []byte { return s.ch }

// Close removes the subscription.
func (s *Sub) Close() {
	s.hub.mu.Lock()
	if _, ok := s.hub.subs[s.id]; ok {
		delete(s.hub.subs, s.id)
		close(s.ch)
	}
	s.hub.mu.Unlock()
}

// Options configures a Hub.
type Options struct {
	Source     Source
	BufSize    int
	StallWarn  time.Duration
	RetryDelay time.Duration
	Logger     *slog.Logger
}

func New(o Options) (*Hub, error) {
	if o.Source == nil {
		return nil, errors.New("hub: no source")
	}
	if o.BufSize <= 0 {
		o.BufSize = 32768
	}
	if o.StallWarn <= 0 {
		o.StallWarn = 10 * time.Second
	}
	if o.RetryDelay <= 0 {
		o.RetryDelay = 5 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	_, serial := o.Source.(SerialSource)
	return &Hub{
		src: o.Source, bufSize: o.BufSize, stallWarn: o.StallWarn,
		retryDelay: o.RetryDelay, log: o.Logger,
		subs: make(map[int]*Sub), serial: serial,
	}, nil
}

// Control borrows the production serial stream for one serialized UBX
// request/response conversation.  The returned closer must be closed.  It is
// deliberately unavailable for TCP/file inputs: those are verification feeds,
// never hardware control paths.
func (h *Hub) Control() (io.ReadWriteCloser, error) {
	if !h.serial {
		return nil, errors.New("receiver control is only available with a serial hub input")
	}
	h.controlMu.Lock()
	h.writeMu.Lock()
	ready := h.writer != nil
	h.writeMu.Unlock()
	if !ready {
		h.controlMu.Unlock()
		return nil, errors.New("receiver serial stream is not connected")
	}
	return &controlPipe{hub: h, sub: h.Subscribe("receiver-control", NewProtoFilter(ProtoUBX), 512)}, nil
}

type controlPipe struct {
	hub    *Hub
	sub    *Sub
	buf    []byte
	mu     sync.Mutex
	closed bool
}

func (p *controlPipe) Read(dst []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.buf) == 0 {
		b, ok := <-p.sub.C()
		if !ok {
			return 0, io.EOF
		}
		p.buf = append(p.buf, b...)
	}
	n := copy(dst, p.buf)
	p.buf = p.buf[n:]
	return n, nil
}
func (p *controlPipe) Write(b []byte) (int, error) {
	p.hub.writeMu.Lock()
	defer p.hub.writeMu.Unlock()
	if p.hub.writer == nil {
		return 0, errors.New("receiver serial stream disconnected")
	}
	return p.hub.writer.Write(b)
}
func (p *controlPipe) Close() error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		p.sub.Close()
		p.hub.controlMu.Unlock()
	}
	p.mu.Unlock()
	return nil
}

// Subscribe registers an output. queue bounds how many frames may be pending
// before this output starts dropping; it does not affect any other output.
func (h *Hub) Subscribe(name string, f *Filter, queue int) *Sub {
	if queue <= 0 {
		queue = 256
	}
	if f == nil {
		f = NewPassthrough()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	s := &Sub{id: h.next, Name: name, filter: f, ch: make(chan []byte, queue), hub: h}
	h.subs[s.id] = s
	return s
}

// Subs returns a snapshot of current subscriptions.
func (h *Hub) Subs() []*Sub {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Sub, 0, len(h.subs))
	for _, s := range h.subs {
		out = append(out, s)
	}
	return out
}

// Run reads the source until ctx is cancelled, reconnecting as needed.
func (h *Hub) Run(ctx context.Context) error {
	stop := make(chan struct{})
	go func() { <-ctx.Done(); close(stop) }()

	rc := &reconnecting{src: h.src, delay: h.retryDelay, log: h.log}
	first := true
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		conn, ok := rc.open(stop)
		if !ok {
			return ctx.Err()
		}
		if !first {
			h.Stats.Reconnects.Add(1)
		}
		first = false
		h.log.Info("hub input open", "source", h.src.String())
		if w, ok := conn.(io.Writer); ok && h.serial {
			h.writeMu.Lock()
			h.writer = w
			h.writeMu.Unlock()
		}
		err := h.pump(ctx, conn)
		h.writeMu.Lock()
		h.writer = nil
		h.writeMu.Unlock()
		conn.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, io.EOF) {
			h.log.Warn("hub input ended", "source", h.src.String(), "err", err)
		} else {
			h.log.Warn("hub input closed", "source", h.src.String())
		}
		select {
		case <-stop:
			return ctx.Err()
		case <-time.After(h.retryDelay):
		}
	}
}

func (h *Hub) pump(ctx context.Context, r io.Reader) error {
	sc := NewScanner(r, h.bufSize)
	var lastBytes, lastFramed, lastDropped, lastResync int64
	stallT := time.NewTicker(h.stallWarn)
	defer stallT.Stop()
	lastData := time.Now()

	done := make(chan error, 1)
	frames := make(chan Frame, 64)
	go func() {
		for {
			f, err := sc.Next()
			if err != nil {
				done <- err
				close(frames)
				return
			}
			// Copy: Raw aliases the scanner buffer.
			cp := make([]byte, len(f.Raw))
			copy(cp, f.Raw)
			f.Raw = cp
			select {
			case frames <- f:
			case <-ctx.Done():
				done <- ctx.Err()
				close(frames)
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stallT.C:
			if d := sc.BytesRead - lastBytes; d == 0 {
				h.log.Warn("hub input stalled", "no_data_for", time.Since(lastData).Round(time.Second))
			}
			lastBytes = sc.BytesRead
			if dd := sc.BytesDropped - lastDropped; dd > 0 {
				h.log.Warn("hub dropped unframed bytes",
					"bytes", dd, "resyncs", sc.Resyncs-lastResync)
			}
			lastDropped, lastResync, lastFramed = sc.BytesDropped, sc.Resyncs, sc.BytesFramed
			_ = lastFramed
		case f, ok := <-frames:
			if !ok {
				return <-done
			}
			lastData = time.Now()
			h.dispatch(f)
			h.Stats.BytesIn.Store(sc.BytesRead)
			if sc.BytesDropped > h.Stats.BytesDropped.Load() {
				h.Stats.LastDropAt.Store(time.Now().Unix())
			}
			h.Stats.BytesDropped.Store(sc.BytesDropped)
			h.Stats.Resyncs.Store(sc.Resyncs)
		}
	}
}

func (h *Hub) dispatch(f Frame) {
	now := time.Now()
	h.Stats.FramesIn.Add(1)
	h.Stats.LastFrameAt.Store(now.UnixNano())
	switch f.Proto {
	case ProtoUBX:
		h.Stats.UBXFrames.Add(1)
	case ProtoRTCM3:
		h.Stats.RTCMFrames.Add(1)
	case ProtoNMEA:
		h.Stats.NMEAFrames.Add(1)
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.subs {
		if !s.filter.Pass(f, now) {
			continue
		}
		select {
		case s.ch <- f.Raw:
			s.Sent.Add(1)
		default:
			// Bounded queue full: drop for this output only. Never block the
			// input, and never let one stuck consumer affect another.
			s.Dropped.Add(1)
		}
	}
}
