// Package unicore drives a Unicore UM9xx receiver through its ASCII command
// interface: send a command, collect its acknowledgement and any answer, and
// parse the handful of replies the web console and Settings page need.
//
// The transport is the hub's control pipe (hub.Control with NMEA and Unicore
// text), which delivers whole framed messages: "$command,..." acknowledgements,
// "$CONFIG,..." lines and "#NAME,...*crc" logs. This package never opens the
// serial port itself.
package unicore

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Reply is everything the receiver said in answer to one command.
type Reply struct {
	Command string
	// Acked is true once the "$command,<cmd>,response:" line arrived; Status is
	// its text after "response:" ("OK" on success, the receiver's own error
	// wording otherwise).
	Acked  bool
	Status string
	// Lines are the other "$..." lines ($CONFIG answers), without CR/LF.
	// Logs are whole "#NAME,...*crc" logs, which may span several lines.
	Lines []string
	Logs  []string
}

// OK reports whether the receiver accepted the command.
func (r Reply) OK() bool { return r.Acked && strings.EqualFold(r.Status, "OK") }

// Text renders the reply the way a terminal would show it.
func (r Reply) Text() string {
	var b strings.Builder
	if r.Acked {
		fmt.Fprintf(&b, "$command,%s,response: %s\n", r.Command, r.Status)
	}
	for _, l := range r.Lines {
		b.WriteString(l + "\n")
	}
	for _, l := range r.Logs {
		b.WriteString(strings.TrimRight(l, "\r\n") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Err is nil when the command was acknowledged OK.
func (r Reply) Err() error {
	switch {
	case !r.Acked:
		return fmt.Errorf("receiver did not acknowledge %q", r.Command)
	case !r.OK():
		return fmt.Errorf("receiver rejected %q: %s", r.Command, r.Status)
	}
	return nil
}

// Session is one serialized command conversation. It is not safe for
// concurrent use; the web layer holds receiverMu around it.
type Session struct {
	// Timeout bounds the wait for the acknowledgement; Quiet is how long the
	// receiver must be silent after the last line before the reply is complete.
	Timeout time.Duration
	Quiet   time.Duration
	Log     *slog.Logger

	rw       io.ReadWriteCloser
	chunks   chan []byte
	stop     chan struct{}
	once     sync.Once
	pending  []byte
	internal bool
}

// NewSession starts reading rw. Close stops the reader and closes rw.
func NewSession(rw io.ReadWriteCloser) *Session {
	s := &Session{Timeout: 3 * time.Second, Quiet: 300 * time.Millisecond, Log: slog.Default(),
		rw: rw, chunks: make(chan []byte, 256), stop: make(chan struct{})}
	go s.pump()
	return s
}

// pump moves reads onto a channel so Do can wait with timers; the hub pipe
// blocks in Read until the next frame.
func (s *Session) pump() {
	buf := make([]byte, 4096)
	for {
		n, err := s.rw.Read(buf)
		if n > 0 {
			select {
			case s.chunks <- append([]byte(nil), buf[:n]...):
			case <-s.stop:
				return
			}
		}
		if err != nil {
			return
		}
		if n == 0 {
			select {
			case <-s.stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
}

// Close stops the reader and releases the underlying pipe.
func (s *Session) Close() error {
	var err error
	s.once.Do(func() { close(s.stop); err = s.rw.Close() })
	return err
}

var (
	// A log ends at "*" plus its checksum and a line end. Binary-framed logs
	// carry an 8-digit CRC32; short logs such as #MODE carry the 2-digit NMEA
	// style one.
	logEnd = regexp.MustCompile(`\*[0-9A-Fa-f]{2}(?:[0-9A-Fa-f]{6})?\r?\n`)
	// Streaming NMEA data sentences ($GNGGA, ...) are not replies.
	// A checksum-less log (UNILOGLIST) ends in a blank line.
	blankLine = "\r\n\r\n"
	nmeaData  = regexp.MustCompile(`^\$[A-Z]{2}[A-Z0-9]{3},`)
	ackLine  = regexp.MustCompile(`^\$command,(.*),response: (.*?)\*[0-9A-Fa-f]{2}$`)
)

// item pops one complete line or log from the buffer.
func (s *Session) item() (string, bool) {
	b := s.pending
	for len(b) > 0 && (b[0] == '\r' || b[0] == '\n' || b[0] == 0) {
		b = b[1:]
	}
	s.pending = b
	if len(b) == 0 {
		return "", false
	}
	if b[0] == '#' {
		end := -1
		if loc := logEnd.FindIndex(b); loc != nil {
			end = loc[1]
		}
		if i := strings.Index(string(b), blankLine); i >= 0 && (end < 0 || i+len(blankLine) < end) {
			end = i + len(blankLine)
		}
		if end < 0 {
			return "", false
		}
		it := string(b[:end])
		s.pending = b[end:]
		return it, true
	}
	i := strings.IndexByte(string(b), '\n')
	if i < 0 {
		return "", false
	}
	it := string(b[:i])
	s.pending = b[i+1:]
	return strings.TrimRight(it, "\r"), true
}

// nmeaChecksumOK verifies "$....*XX". The receiver's own $command and $CONFIG
// lines include the leading '$' in the XOR, unlike NMEA proper, so both forms
// are accepted.
func nmeaChecksumOK(line string) bool {
	star := strings.LastIndexByte(line, '*')
	if !strings.HasPrefix(line, "$") || star < 2 || len(line) < star+3 {
		return false
	}
	var x byte
	for i := 1; i < star; i++ {
		x ^= line[i]
	}
	got := line[star+1 : star+3]
	return strings.EqualFold(fmt.Sprintf("%02X", x), got) || strings.EqualFold(fmt.Sprintf("%02X", x^'$'), got)
}

// ackFor reports whether the acknowledged command is ours. The receiver
// echoes either the whole command or its first word.
func ackFor(cmd, ack string) bool {
	cmd, ack = strings.ToUpper(strings.TrimSpace(cmd)), strings.ToUpper(strings.TrimSpace(ack))
	if cmd == ack {
		return true
	}
	if f := strings.Fields(cmd); len(f) > 0 && f[0] == ack {
		return true
	}
	return false
}

// Do sends cmd and collects the reply: the acknowledgement for that command
// plus every line and log that follows until the receiver is quiet. It waits
// at most Timeout for the acknowledgement; without one it returns what it has
// with Acked false.
func (s *Session) Do(cmd string) (Reply, error) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" || strings.ContainsAny(cmd, "\r\n") {
		return Reply{Command: cmd}, fmt.Errorf("a command is one non-empty line")
	}
	// Whatever arrived since the last command belongs to nobody.
	for drained := false; !drained; {
		select {
		case <-s.chunks:
		default:
			drained = true
		}
	}
	s.pending = nil
	start := time.Now()
	if _, err := s.rw.Write([]byte(cmd + "\r\n")); err != nil {
		return Reply{Command: cmd}, fmt.Errorf("write %q: %w", cmd, err)
	}
	rep := Reply{Command: cmd}
	deadline := time.NewTimer(s.Timeout)
	defer deadline.Stop()
	quiet := time.NewTimer(time.Hour)
	defer quiet.Stop()
	quiet.Stop()
	for {
		select {
		case c, ok := <-s.chunks:
			if !ok {
				return rep, io.EOF
			}
			s.pending = append(s.pending, c...)
			for {
				it, ok := s.item()
				if !ok {
					break
				}
				s.take(&rep, it)
			}
			if rep.Acked {
				if !quiet.Stop() {
					select {
					case <-quiet.C:
					default:
					}
				}
				quiet.Reset(s.Quiet)
			}
		case <-quiet.C:
			s.logReply(rep, time.Since(start))
			return rep, nil
		case <-deadline.C:
			s.logReply(rep, time.Since(start))
			return rep, nil
		case <-s.stop:
			return rep, io.ErrClosedPipe
		}
	}
}

func (s *Session) take(rep *Reply, it string) {
	switch {
	case strings.HasPrefix(it, "#"):
		rep.Logs = append(rep.Logs, it)
	case strings.HasPrefix(it, "$command,"):
		if !nmeaChecksumOK(it) {
			return
		}
		m := ackLine.FindStringSubmatch(it)
		if m == nil || rep.Acked || !ackFor(rep.Command, m[1]) {
			return
		}
		rep.Acked, rep.Status = true, strings.TrimSpace(m[2])
	case nmeaData.MatchString(it):
	case strings.HasPrefix(it, "$"):
		rep.Lines = append(rep.Lines, it)
	}
}

func (s *Session) logReply(rep Reply, took time.Duration) {
	lvl := slog.LevelInfo
	if s.internal {
		lvl = slog.LevelDebug // the four snapshot reads around every change
	}
	s.Log.Log(context.Background(), lvl, "unicore command", "cmd", Redact(rep.Command), "acked", rep.Acked, "status", rep.Status,
		"lines", len(rep.Lines), "logs", len(rep.Logs), "took", took.Round(time.Millisecond),
		"reply", Redact(rep.Text()))
}
