package hub

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"

	"golang.org/x/sys/unix"
	"time"
)

// Source is the hub's input. Exactly one process may hold the serial device, so
// during parallel testing the hub reads from the existing str2str TCP server
// instead; the rest of the pipeline is identical either way.
type Source interface {
	Open() (io.ReadCloser, error)
	String() string
}

// SerialSource reads directly from the receiver. This is the production input.
type SerialSource struct {
	Device string
	Baud   int
}

func (s SerialSource) String() string { return fmt.Sprintf("serial:%s@%d", s.Device, s.Baud) }

func (s SerialSource) Open() (io.ReadCloser, error) {
	f, err := os.OpenFile(s.Device, unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", s.Device, err)
	}
	if err := configureTTY(f, s.Baud); err != nil {
		f.Close()
		return nil, fmt.Errorf("configure %s: %w", s.Device, err)
	}
	return f, nil
}

// TCPSource reads from a TCP server. Used to run alongside the existing
// str2str during Stage 2 verification, and for replay against a recorded feed.
type TCPSource struct {
	Addr    string
	Timeout time.Duration
}

func (t TCPSource) String() string { return "tcp:" + t.Addr }

func (t TCPSource) Open() (io.ReadCloser, error) {
	to := t.Timeout
	if to == 0 {
		to = 15 * time.Second
	}
	c, err := net.DialTimeout("tcp", t.Addr, to)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", t.Addr, err)
	}
	return c, nil
}

// FileSource replays a capture. This is what lets the Stage 0 recordings act as
// regression fixtures for the whole pipeline.
type FileSource struct{ Path string }

func (f FileSource) String() string { return "file:" + f.Path }

func (f FileSource) Open() (io.ReadCloser, error) { return os.Open(f.Path) }

// NewSource builds a Source from config.
func NewSource(kind, addr string, baud int) (Source, error) {
	switch kind {
	case "serial":
		if baud <= 0 {
			return nil, fmt.Errorf("serial source needs a positive baud, got %d", baud)
		}
		return SerialSource{Device: addr, Baud: baud}, nil
	case "tcp":
		return TCPSource{Addr: addr}, nil
	case "file":
		return FileSource{Path: addr}, nil
	}
	return nil, fmt.Errorf("unknown hub input kind %q (want serial, tcp or file)", kind)
}

// reconnect wraps a Source with retry, so an unplugged USB cable or a dropped
// TCP peer recovers on its own rather than requiring a restart.
type reconnecting struct {
	src   Source
	delay time.Duration
	log   *slog.Logger
}

func (r *reconnecting) open(stop <-chan struct{}) (io.ReadCloser, bool) {
	for attempt := 1; ; attempt++ {
		rc, err := r.src.Open()
		if err == nil {
			if attempt > 1 {
				r.log.Info("input reconnected", "source", r.src.String(), "attempts", attempt)
			}
			return rc, true
		}
		r.log.Warn("input open failed, retrying",
			"source", r.src.String(), "attempt", attempt, "err", err, "retry_in", r.delay)
		select {
		case <-stop:
			return nil, false
		case <-time.After(r.delay):
		}
	}
}
