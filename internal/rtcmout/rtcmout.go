// Package rtcmout sends a mountpoint's stream somewhere that is not a caster:
// to UDP listeners, or out of a serial port to a radio link.
//
// These outputs come from RTKBase, which exposes them as separate str2str
// services ([rtcm_udp_svr], [rtcm_udp_client], [rtcm_serial] in
// settings.conf.default). See CREDITS.md. As with push-out, a sink names a
// local mountpoint and sends its exact broadcast bytes rather than re-deriving
// a message list, and a sink that cannot keep up drops frames instead of
// delaying the rovers served here.
//
// UNVERIFIED AGAINST HARDWARE: the owner has no radio attached, so the serial
// sink has been tested against a pty and its unit tests, never against a real
// transmitter. Baud, flow control and framing on an actual radio are untested.
// Say so before claiming this works in the field.
package rtcmout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/psgnss/psgnss-base/internal/caster"
	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/hub"
)

const (
	writeTimeout = 10 * time.Second
	retryDelay   = 15 * time.Second
	tapQueue     = 512
)

// Mounts is the part of the caster this package needs.
type Mounts interface {
	Mounts() []*caster.Mount
}

// Sink is one configured output.
type Sink struct {
	cfg config.RTCMOut
	mts Mounts
	log *slog.Logger

	mu      sync.Mutex
	state   string
	lastErr string

	bytes    atomic.Int64
	dropped  atomic.Int64
	lastData atomic.Int64
}

// Status is what Diagnostics shows.
type Status struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"` // "udp" or "serial"
	Target     string `json:"target"`
	Source     string `json:"source"`
	State      string `json:"state"`
	BytesSent  int64  `json:"bytes_sent"`
	Dropped    int64  `json:"dropped"`
	LastDataAt int64  `json:"last_data_at"`
	LastError  string `json:"last_error"`
}

func (s *Sink) Status() Status {
	s.mu.Lock()
	st := Status{Name: s.cfg.Name, Kind: s.cfg.Kind, Source: s.cfg.Source,
		State: s.state, LastError: s.lastErr}
	s.mu.Unlock()
	if s.cfg.Kind == "serial" {
		st.Target = fmt.Sprintf("%s at %d baud", s.cfg.Device, s.cfg.Baud)
	} else {
		st.Target = s.cfg.Target
	}
	st.BytesSent = s.bytes.Load()
	st.Dropped = s.dropped.Load()
	st.LastDataAt = s.lastData.Load()
	return st
}

func (s *Sink) set(state, errText string) {
	s.mu.Lock()
	s.state, s.lastErr = state, errText
	s.mu.Unlock()
}

// Manager owns every configured sink.
type Manager struct {
	sinks []*Sink
}

func New(cfgs []config.RTCMOut, mts Mounts, log *slog.Logger) *Manager {
	m := &Manager{}
	for _, c := range cfgs {
		if c.Disabled {
			continue
		}
		m.sinks = append(m.sinks, &Sink{cfg: c, mts: mts, log: log, state: "stopped"})
	}
	return m
}

func (m *Manager) Status() []Status {
	out := make([]Status, 0, len(m.sinks))
	for _, s := range m.sinks {
		out = append(out, s.Status())
	}
	return out
}

// Run drives every sink until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, s := range m.sinks {
		wg.Add(1)
		go func(s *Sink) { defer wg.Done(); s.run(ctx) }(s)
	}
	wg.Wait()
}

func (s *Sink) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			s.set("stopped", "")
			return
		}
		err := s.session(ctx)
		if ctx.Err() != nil {
			s.set("stopped", "")
			return
		}
		if err == nil {
			err = errors.New("output closed")
		}
		s.set("retrying", err.Error())
		s.log.Warn("RTCM output stopped", "sink", s.cfg.Name, "kind", s.cfg.Kind,
			"retry_in", retryDelay.String(), "err", err)
		select {
		case <-ctx.Done():
			s.set("stopped", "")
			return
		case <-time.After(retryDelay):
		}
	}
}

func (s *Sink) session(ctx context.Context) error {
	mount := s.mount()
	if mount == nil {
		return fmt.Errorf("local mountpoint %q does not exist or is disabled", s.cfg.Source)
	}
	w, target, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer w.Close()
	s.set("sending", "")
	s.log.Info("RTCM output running", "sink", s.cfg.Name, "kind", s.cfg.Kind,
		"target", target, "source", s.cfg.Source)

	tap := mount.Tap("rtcmout:"+s.cfg.Name, tapQueue)
	defer tap.Close()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case b, ok := <-tap.C():
			if !ok {
				return errors.New("mountpoint closed")
			}
			s.dropped.Store(tap.Dropped.Load())
			if err := w.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
				return err
			}
			if _, err := w.Write(b); err != nil {
				return err
			}
			s.bytes.Add(int64(len(b)))
			s.lastData.Store(time.Now().UnixMilli())
		}
	}
}

// writer is the small part of a connection or file this package uses; a serial
// port supports write deadlines too, once it is opened non-blocking.
type writer interface {
	Write([]byte) (int, error)
	SetWriteDeadline(time.Time) error
	Close() error
}

func (s *Sink) open(ctx context.Context) (writer, string, error) {
	switch s.cfg.Kind {
	case "udp":
		var d net.Dialer
		conn, err := d.DialContext(ctx, "udp", s.cfg.Target)
		if err != nil {
			return nil, "", err
		}
		// UDP has no delivery guarantee and no back pressure. A datagram per
		// RTCM frame keeps message boundaries intact for a receiver that reads
		// datagrams, which is what makes this usable at all.
		return conn.(*net.UDPConn), s.cfg.Target, nil
	case "serial":
		f, err := os.OpenFile(s.cfg.Device, os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, "", err
		}
		if err := hub.ConfigureTTY(f, s.cfg.Baud); err != nil {
			f.Close()
			return nil, "", err
		}
		return f, fmt.Sprintf("%s at %d baud", s.cfg.Device, s.cfg.Baud), nil
	default:
		return nil, "", fmt.Errorf("unknown output kind %q", s.cfg.Kind)
	}
}

func (s *Sink) mount() *caster.Mount {
	for _, m := range s.mts.Mounts() {
		if strings.EqualFold(m.Entry.Name, s.cfg.Source) {
			return m
		}
	}
	return nil
}
