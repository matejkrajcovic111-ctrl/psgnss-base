// Package timesync offers the receiver's own time to chrony, so a headless base
// with no RTC and no internet still has a correct clock.
//
// RTKBase reaches the same goal with gpsd feeding chrony (tools/install.sh
// --gpsd-chrony); see CREDITS.md. PSGNSS does it without gpsd, which the owner
// removed as unused: gpsd would need the serial port the hub already owns, and
// the hub is already decoding the messages that carry time. Samples go to
// chrony's SOCK refclock, a 40-byte struct on a Unix datagram socket, so there
// is no shared memory, no cgo and no second process on the port.
//
// Accuracy: this is USB-timestamped NAV-PVT, not PPS. The receiver's own time is
// excellent, but it reaches us after USB buffering and scheduling, so the offset
// carries a few milliseconds of jitter and an unknown small bias. That is worth
// several orders of magnitude more than a Pi with no clock at all, and far less
// than a wire from the receiver's PPS pin to a GPIO would give. Do not present
// it as PPS-grade.
package timesync

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"sync"
	"time"

	"github.com/psgnss/psgnss-base/internal/hub"
)

// chronySampleMagic identifies a SOCK refclock sample ("SOCK" little-endian).
const chronySampleMagic = 0x534f434b

// leapNormal is chrony's LEAP_Normal: no leap second pending.
const leapNormal = 0

// sampleSize is sizeof(struct sock_sample) on 64-bit Linux: struct timeval
// (two 64-bit words), a double offset, then four 32-bit ints.
const sampleSize = 40

// Options configures the sender.
type Options struct {
	// Socket is chrony's SOCK refclock path. chronyd creates and listens on it,
	// so a missing socket means chrony is not configured, not that we failed.
	Socket string
	// MinInterval rate-limits samples. chrony does its own filtering; one
	// sample a second is plenty and matches the receiver's navigation rate.
	MinInterval time.Duration
	// MaxAccuracy rejects a sample whose reported time accuracy is worse than
	// this. A receiver that has just started reports metres of tAcc and times
	// to match.
	MaxAccuracy time.Duration
}

// Sender reads NAV-PVT from the hub and offers each fix to chrony.
type Sender struct {
	opt Options
	h   *hub.Hub
	log *slog.Logger

	mu       sync.Mutex
	state    string
	lastErr  string
	lastAt   int64
	lastOff  float64
	sent     int64
	rejected int64
}

func New(h *hub.Hub, opt Options, log *slog.Logger) *Sender {
	if opt.MinInterval <= 0 {
		opt.MinInterval = time.Second
	}
	if opt.MaxAccuracy <= 0 {
		opt.MaxAccuracy = 100 * time.Millisecond
	}
	return &Sender{opt: opt, h: h, log: log, state: "stopped"}
}

// Status is what Diagnostics and the UI show.
type Status struct {
	Enabled      bool    `json:"enabled"`
	Socket       string  `json:"socket"`
	State        string  `json:"state"` // "sending", "waiting for chrony", "waiting for a fix", "stopped"
	Samples      int64   `json:"samples"`
	Rejected     int64   `json:"rejected"`
	LastOffsetS  float64 `json:"last_offset_s"`
	LastSampleAt int64   `json:"last_sample_at"`
	LastError    string  `json:"last_error"`
}

func (s *Sender) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Enabled: s.opt.Socket != "", Socket: s.opt.Socket, State: s.state,
		Samples: s.sent, Rejected: s.rejected, LastOffsetS: s.lastOff,
		LastSampleAt: s.lastAt, LastError: s.lastErr}
}

func (s *Sender) set(state, errText string) {
	s.mu.Lock()
	s.state = state
	s.lastErr = errText
	s.mu.Unlock()
}

// Run pumps samples until ctx is done.
func (s *Sender) Run(ctx context.Context) {
	if s.opt.Socket == "" {
		return
	}
	sub := s.h.Subscribe("timesync", hub.NewProtoFilter(hub.ProtoUBX), 64)
	defer sub.Close()

	var conn net.Conn
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	var last time.Time
	s.set("waiting for a fix", "")
	for {
		select {
		case <-ctx.Done():
			s.set("stopped", "")
			return
		case frame, ok := <-sub.C():
			if !ok {
				s.set("stopped", "")
				return
			}
			// Local time is read first: it must be as close as possible to the
			// moment the frame arrived, not to the moment parsing finished.
			now := time.Now()
			fix, ok := parseNavPVTTime(frame)
			if !ok {
				continue
			}
			if !fix.resolved {
				s.bump(&s.rejected)
				s.set("waiting for a fix", "receiver time is not fully resolved")
				continue
			}
			if fix.accuracy > s.opt.MaxAccuracy {
				s.bump(&s.rejected)
				s.set("waiting for a fix",
					fmt.Sprintf("receiver reports %s time accuracy, worse than the %s limit",
						fix.accuracy.Round(time.Microsecond), s.opt.MaxAccuracy))
				continue
			}
			if !last.IsZero() && now.Sub(last) < s.opt.MinInterval {
				continue
			}
			if conn == nil {
				c, err := dialChrony(s.opt.Socket)
				if err != nil {
					reason := err.Error()
					if errors.Is(err, os.ErrNotExist) {
						reason = "chrony is not listening on " + s.opt.Socket +
							" (add: refclock SOCK " + s.opt.Socket + " refid UBX)"
					}
					// Log the first failure and any change of reason. Silence
					// here cost real debugging time: the status field said
					// "waiting for chrony" while the journal said nothing at
					// all, and the socket looked healthy from chrony's side.
					s.mu.Lock()
					changed := s.lastErr != reason
					s.mu.Unlock()
					if changed && s.log != nil {
						s.log.Warn("cannot offer receiver time to chrony",
							"socket", s.opt.Socket, "err", reason)
					}
					s.set("waiting for chrony", reason)
					// Do not spin: the socket appears when chrony starts.
					select {
					case <-ctx.Done():
						return
					case <-time.After(10 * time.Second):
					}
					continue
				}
				conn = c
				s.log.Info("offering receiver time to chrony", "socket", s.opt.Socket)
			}
			offset := fix.utc.Sub(now).Seconds()
			if _, err := conn.Write(encodeSample(now, offset)); err != nil {
				conn.Close()
				conn = nil
				s.set("waiting for chrony", err.Error())
				continue
			}
			last = now
			s.mu.Lock()
			s.sent++
			s.lastOff = offset
			s.lastAt = now.Unix()
			s.state = "sending"
			s.lastErr = ""
			s.mu.Unlock()
		}
	}
}

func (s *Sender) bump(p *int64) {
	s.mu.Lock()
	*p++
	s.mu.Unlock()
}

// dialChrony connects to chrony's SOCK refclock.
//
// The socket is SOCK_DGRAM, so this must be "unixgram". Dialing "unix" gets a
// stream socket and the connect fails with "protocol wrong type for socket" ---
// which is exactly what happened on the first deployment: chrony sat there with
// a healthy-looking socket and never received a sample.
func dialChrony(path string) (net.Conn, error) {
	return net.DialTimeout("unixgram", path, 2*time.Second)
}

// encodeSample builds chrony's struct sock_sample. tv is when the local clock
// read the event; offset is reference time minus local time, in seconds, which
// is the correction chrony must apply.
func encodeSample(tv time.Time, offset float64) []byte {
	b := make([]byte, sampleSize)
	binary.LittleEndian.PutUint64(b[0:8], uint64(tv.Unix()))
	binary.LittleEndian.PutUint64(b[8:16], uint64(tv.Nanosecond()/1000))
	binary.LittleEndian.PutUint64(b[16:24], math.Float64bits(offset))
	binary.LittleEndian.PutUint32(b[24:28], 0) // pulse: this is not a PPS edge
	binary.LittleEndian.PutUint32(b[28:32], leapNormal)
	binary.LittleEndian.PutUint32(b[32:36], 0) // _pad
	binary.LittleEndian.PutUint32(b[36:40], chronySampleMagic)
	return b
}

type navTime struct {
	utc      time.Time
	accuracy time.Duration
	resolved bool
}

// parseNavPVTTime pulls UTC time out of a framed UBX NAV-PVT message.
//
// Layout (UBX-13003221, NAV-PVT): iTOW 0..4, year 4..6, month 6, day 7, hour 8,
// min 9, sec 10, valid 11, tAcc 12..16, nano 16..20 (signed, can be negative).
// The valid bitfield is validDate 0, validTime 1, fullyResolved 2.
func parseNavPVTTime(frame []byte) (navTime, bool) {
	// 0xB5 0x62, class, id, length(2), payload, checksum(2)
	if len(frame) < 6+92+2 || frame[0] != 0xB5 || frame[1] != 0x62 ||
		frame[2] != 0x01 || frame[3] != 0x07 {
		return navTime{}, false
	}
	p := frame[6 : len(frame)-2]
	if len(p) < 92 {
		return navTime{}, false
	}
	valid := p[11]
	nano := int32(binary.LittleEndian.Uint32(p[16:20]))
	sec := int(p[10])
	// A negative nano means the second field has been rounded up: Go's Date
	// normalises the result either way.
	t := time.Date(int(binary.LittleEndian.Uint16(p[4:6])), time.Month(p[6]), int(p[7]),
		int(p[8]), int(p[9]), sec, int(nano), time.UTC)
	return navTime{
		utc:      t,
		accuracy: time.Duration(binary.LittleEndian.Uint32(p[12:16])) * time.Nanosecond,
		resolved: valid&0x07 == 0x07,
	}, true
}
