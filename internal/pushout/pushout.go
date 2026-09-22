// Package pushout uploads a local mountpoint's stream to remote NTRIP casters,
// so this base can feed a public or national network without anything in front
// of it.
//
// The feature and its operational shape --- several independent targets, each
// with its own caster, credentials and remote mountpoint --- come from RTKBase's
// [ntrip_A]/[ntrip_B] services (settings.conf.default, run_cast.sh). See
// CREDITS.md. The implementation is PSGNSS's own and differs in two ways that
// matter: a target publishes a local mountpoint's exact broadcast bytes rather
// than a separately configured message list, so what a remote rover receives
// cannot drift from what a local one receives; and a stalled remote caster can
// never delay the rovers served here, because the tap drops frames instead of
// blocking the mountpoint.
package pushout

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/psgnss/psgnss-base/internal/caster"
	"github.com/psgnss/psgnss-base/internal/secrets"
	"github.com/psgnss/psgnss-base/internal/store"
	"github.com/psgnss/psgnss-base/internal/version"
)

const (
	dialTimeout      = 15 * time.Second
	handshakeTimeout = 20 * time.Second
	writeTimeout     = 30 * time.Second
	minBackoff       = 5 * time.Second
	maxBackoff       = 2 * time.Minute
	// A caster that accepts the handshake and then goes quiet still has to
	// accept bytes; a write that cannot complete in writeTimeout is a dead
	// link, not a slow one.
	tapQueue = 1024
)

// Mounts is the part of the caster this package needs: finding the local
// mountpoint a target publishes.
type Mounts interface {
	Mounts() []*caster.Mount
}

// Manager owns one runner per enabled target.
type Manager struct {
	db  *store.Store
	kr  *secrets.Keyring
	mts Mounts
	log *slog.Logger

	mu      sync.Mutex
	runners map[string]*runner
	ctx     context.Context
}

func New(db *store.Store, kr *secrets.Keyring, mts Mounts, log *slog.Logger) *Manager {
	return &Manager{db: db, kr: kr, mts: mts, log: log, runners: map[string]*runner{}}
}

// Run starts the configured targets and blocks until ctx is done, at which
// point every runner is stopped.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	if err := m.Reload(); err != nil {
		m.log.Error("cannot start NTRIP push-out", "err", err)
	}
	<-ctx.Done()
	m.mu.Lock()
	for name, r := range m.runners {
		r.stop()
		delete(m.runners, name)
	}
	m.mu.Unlock()
}

// Reload brings the running set in line with the stored configuration. It is
// called after a settings change, so editing a target does not need a restart
// of the whole daemon --- which would drop every rover.
func (m *Manager) Reload() error {
	targets, err := m.db.PushTargets()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil {
		return nil // not running yet; Run will pick these up
	}
	want := map[string]store.PushTarget{}
	for _, t := range targets {
		if t.Enabled {
			want[t.Name] = t
		}
	}
	for name, r := range m.runners {
		t, ok := want[name]
		if !ok || r.changed(t) {
			r.stop()
			delete(m.runners, name)
		}
	}
	for name, t := range want {
		if _, ok := m.runners[name]; ok {
			continue
		}
		r := &runner{t: t, m: m}
		m.runners[name] = r
		go r.run(m.ctx)
	}
	return nil
}

// Status is a target's live state, for the UI and for Diagnostics.
type Status struct {
	Name          string `json:"name"`
	Source        string `json:"source"`
	Host          string `json:"host"`
	Mountpoint    string `json:"mountpoint"`
	Protocol      string `json:"protocol"`
	State         string `json:"state"` // "connected", "connecting", "retrying", "stopped"
	ConnectedAt   int64  `json:"connected_at"`
	BytesSent     int64  `json:"bytes_sent"`
	Connects      int64  `json:"connects"`
	Dropped       int64  `json:"dropped"`
	LastDataAt    int64  `json:"last_data_at"`
	LastError     string `json:"last_error"`
	LastErrorAt   int64  `json:"last_error_at"`
	NextAttemptIn int    `json:"next_attempt_in"`
}

// Status reports every running target. A configured but disabled target is not
// listed here; the API merges those from storage.
func (m *Manager) Status() []Status {
	m.mu.Lock()
	rs := make([]*runner, 0, len(m.runners))
	for _, r := range m.runners {
		rs = append(rs, r)
	}
	m.mu.Unlock()
	out := make([]Status, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.status())
	}
	return out
}

type runner struct {
	t store.PushTarget
	m *Manager

	cancel context.CancelFunc

	mu          sync.Mutex
	state       string
	connectedAt int64
	lastErr     string
	lastErrAt   int64
	nextAttempt time.Time

	bytes    atomic.Int64
	connects atomic.Int64
	dropped  atomic.Int64
	lastData atomic.Int64
}

// changed reports whether a stored target differs from the one this runner was
// started with, in any way that needs a reconnect.
func (r *runner) changed(t store.PushTarget) bool {
	return r.t.Source != t.Source || r.t.Host != t.Host || r.t.Mountpoint != t.Mountpoint ||
		r.t.Protocol != t.Protocol || r.t.Username != t.Username ||
		r.t.UpdatedAt != t.UpdatedAt
}

func (r *runner) stop() {
	if r.cancel != nil {
		r.cancel()
	}
	r.setState("stopped")
}

func (r *runner) setState(s string) {
	r.mu.Lock()
	r.state = s
	r.mu.Unlock()
}

func (r *runner) fail(err error) {
	r.mu.Lock()
	r.state = "retrying"
	r.lastErr = err.Error()
	r.lastErrAt = time.Now().Unix()
	r.connectedAt = 0
	r.mu.Unlock()
}

func (r *runner) status() Status {
	r.mu.Lock()
	st := Status{Name: r.t.Name, Source: r.t.Source, Host: r.t.Host, Mountpoint: r.t.Mountpoint,
		Protocol: r.t.Protocol, State: r.state, ConnectedAt: r.connectedAt,
		LastError: r.lastErr, LastErrorAt: r.lastErrAt}
	if !r.nextAttempt.IsZero() {
		if d := time.Until(r.nextAttempt); d > 0 {
			st.NextAttemptIn = int(d.Seconds())
		}
	}
	r.mu.Unlock()
	st.BytesSent = r.bytes.Load()
	st.Connects = r.connects.Load()
	st.Dropped = r.dropped.Load()
	st.LastDataAt = r.lastData.Load()
	return st
}

func (r *runner) run(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel
	defer cancel()

	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		r.setState("connecting")
		err := r.session(ctx)
		if ctx.Err() != nil {
			r.setState("stopped")
			return
		}
		if err == nil {
			// A clean end without cancellation still means the remote closed.
			err = errors.New("remote caster closed the connection")
		}
		r.fail(err)
		r.m.log.Warn("NTRIP push-out disconnected", "target", r.t.Name, "host", r.t.Host,
			"mount", r.t.Mountpoint, "retry_in", backoff.String(), "err", err)
		r.mu.Lock()
		r.nextAttempt = time.Now().Add(backoff)
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			r.setState("stopped")
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// session connects, handshakes and pumps until the link or the context ends.
func (r *runner) session(ctx context.Context) error {
	mount := r.mount()
	if mount == nil {
		return fmt.Errorf("local mountpoint %q does not exist or is disabled", r.t.Source)
	}
	password := ""
	if r.t.HasPassword {
		p, err := r.m.kr.Open(r.t.PasswordEnc, r.t.PasswordNonce)
		if err != nil {
			return fmt.Errorf("cannot decrypt the caster password: %w", err)
		}
		password = p
	}

	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", r.t.Host)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Cancellation has to interrupt a blocked write, not wait for it.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return err
	}
	chunked, err := r.handshake(conn, password)
	if err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return err
	}

	// Attach the tap before reporting the target connected. The other order
	// silently loses every frame the mountpoint broadcasts between the
	// handshake completing and the tap existing.
	tap := mount.Tap("push:"+r.t.Name, tapQueue)
	defer tap.Close()

	r.connects.Add(1)
	r.mu.Lock()
	r.state = "connected"
	r.connectedAt = time.Now().Unix()
	r.lastErr = ""
	r.nextAttempt = time.Time{}
	r.mu.Unlock()
	r.m.log.Info("NTRIP push-out connected", "target", r.t.Name, "host", r.t.Host,
		"mount", r.t.Mountpoint, "protocol", r.t.Protocol, "source", r.t.Source)

	write := func(b []byte) error {
		if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return err
		}
		var err error
		if chunked {
			_, err = fmt.Fprintf(conn, "%x\r\n%s\r\n", len(b), b)
		} else {
			_, err = conn.Write(b)
		}
		if err != nil {
			return err
		}
		r.bytes.Add(int64(len(b)))
		r.lastData.Store(time.Now().UnixMilli())
		return nil
	}

	// Send the remembered station description immediately, so a remote rover
	// that is already connected does not wait up to 10 s for 1006/1008/1033.
	for _, b := range mount.Replay() {
		if err := write(b); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case b, ok := <-tap.C():
			if !ok {
				return errors.New("mountpoint closed")
			}
			r.dropped.Store(tap.Dropped.Load())
			if err := write(b); err != nil {
				return err
			}
		}
	}
}

func (r *runner) mount() *caster.Mount {
	for _, m := range r.m.mts.Mounts() {
		if strings.EqualFold(m.Entry.Name, r.t.Source) {
			return m
		}
	}
	return nil
}

// handshake performs the NTRIP v1 SOURCE or v2 POST upload handshake and
// reports whether the body must be chunked (v2).
func (r *runner) handshake(conn net.Conn, password string) (chunked bool, err error) {
	agent := "NTRIP PSGNSS/" + version.Version
	br := bufio.NewReader(conn)
	if r.t.Protocol == store.ProtocolV1 {
		if _, err := fmt.Fprintf(conn, "SOURCE %s /%s\r\nSource-Agent: %s\r\n\r\n",
			password, r.t.Mountpoint, agent); err != nil {
			return false, err
		}
		line, err := br.ReadString('\n')
		if err != nil {
			return false, fmt.Errorf("no reply to SOURCE: %w", err)
		}
		line = strings.TrimSpace(line)
		// Casters answer "ICY 200 OK"; a few answer bare "OK".
		if !strings.Contains(line, "200 OK") && line != "OK" {
			return false, fmt.Errorf("caster refused the upload: %s", sanitise(line))
		}
		return false, nil
	}

	auth := base64.StdEncoding.EncodeToString([]byte(r.t.Username + ":" + password))
	if _, err := fmt.Fprintf(conn, "POST /%s HTTP/1.1\r\nHost: %s\r\nNtrip-Version: Ntrip/2.0\r\n"+
		"User-Agent: %s\r\nAuthorization: Basic %s\r\nConnection: close\r\n"+
		"Transfer-Encoding: chunked\r\n\r\n",
		r.t.Mountpoint, r.t.Host, agent, auth); err != nil {
		return false, err
	}
	line, err := br.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("no reply to POST: %w", err)
	}
	line = strings.TrimSpace(line)
	if !strings.Contains(line, "200") {
		return false, fmt.Errorf("caster refused the upload: %s", sanitise(line))
	}
	// Drain the remaining response headers so the body starts clean.
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return false, fmt.Errorf("truncated reply to POST: %w", err)
		}
		if strings.TrimSpace(h) == "" {
			break
		}
	}
	return true, nil
}

// sanitise keeps a caster's refusal readable in a log line and in the UI
// without letting it carry control characters or a password echo.
func sanitise(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
