package logbuffer

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestTeeRetainsBoundedStructuredRecords(t *testing.T) {
	b := New(100)
	l := slog.New(Tee(slog.NewTextHandler(io.Discard, nil), b))
	for i := 0; i < 120; i++ {
		l.InfoContext(context.Background(), "sample", "n", i)
	}
	got := b.Entries()
	if len(got) != 100 {
		t.Fatalf("got %d entries", len(got))
	}
	if got[0].Attrs["n"] != int64(20) && got[0].Attrs["n"] != 20 {
		t.Fatalf("oldest attrs = %#v", got[0].Attrs)
	}
}
