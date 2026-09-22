// Package logbuffer retains a bounded, structured copy of recent daemon logs.
package logbuffer

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type Entry struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

type Buffer struct {
	mu      sync.RWMutex
	entries []Entry
	limit   int
}

func New(limit int) *Buffer {
	if limit < 100 {
		limit = 100
	}
	return &Buffer{limit: limit}
}

func (b *Buffer) add(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) == b.limit {
		copy(b.entries, b.entries[1:])
		b.entries = b.entries[:b.limit-1]
	}
	b.entries = append(b.entries, e)
}

func (b *Buffer) Entries() []Entry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Entry, len(b.entries))
	copy(out, b.entries)
	return out
}

// Handler tees records to the normal system handler and the in-memory ring.
type Handler struct {
	next   slog.Handler
	buf    *Buffer
	attrs  []slog.Attr
	groups []string
}

func Tee(next slog.Handler, b *Buffer) slog.Handler               { return &Handler{next: next, buf: b} }
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	attrs := make(map[string]any)
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.Any(); return true })
	h.buf.add(Entry{Time: r.Time.UTC(), Level: r.Level.String(), Message: r.Message, Attrs: attrs})
	return h.next.Handle(ctx, r)
}
func (h *Handler) WithAttrs(a []slog.Attr) slog.Handler {
	n := *h
	n.next = h.next.WithAttrs(a)
	n.attrs = append(append([]slog.Attr(nil), h.attrs...), a...)
	return &n
}
func (h *Handler) WithGroup(g string) slog.Handler {
	n := *h
	n.next = h.next.WithGroup(g)
	n.groups = append(append([]string(nil), h.groups...), g)
	return &n
}
