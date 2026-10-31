package hub

import (
	"errors"
	"io"
	"testing"
	"time"
)

type noSource struct{}

func (noSource) Open() (io.ReadCloser, error) { return nil, errors.New("not used") }
func (noSource) String() string               { return "none" }

// A Unicore session subscribes to text frames only, and none arrive once its
// commands are answered, so a reader is normally blocked when Close runs. Close
// once waited on that reader for good, which hung the Receiver page and held the
// control session for the life of the process.
func TestControlCloseReturnsWhileTheReaderWaitsForAFrame(t *testing.T) {
	h, err := New(Options{Source: noSource{}})
	if err != nil {
		t.Fatal(err)
	}
	h.serial, h.writer = true, io.Discard
	p, err := h.Control(ProtoNMEA, ProtoUnicoreText)
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := p.Read(make([]byte, 16))
		readDone <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the reader block on the empty queue

	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return while Read was blocked")
	}
	select {
	case err := <-readDone:
		if err != io.EOF {
			t.Fatalf("Read returned %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not end after Close")
	}

	next, err := h.Control(ProtoNMEA)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}
