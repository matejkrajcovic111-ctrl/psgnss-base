// Package receiver talks UBX to a u-blox receiver: reading configuration,
// writing it safely, and verifying every change.
//
// Firmware updates are deliberately not implemented. Nothing in this package
// can flash the device.
package receiver

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// UBX message identifiers used here.
const (
	clsNAV = 0x01
	clsACK = 0x05
	clsCFG = 0x06
	clsMON = 0x0A

	idACKNak    = 0x00
	idACKAck    = 0x01
	idCFGValSet = 0x8A
	idCFGValGet = 0x8B
	idMONVer    = 0x04
)

// Config layers. VALGET takes one; VALSET takes a bitmask.
const (
	LayerRAM   = 0
	LayerBBR   = 1
	LayerFlash = 2
	LayerDflt  = 7

	SetRAM   = 0x01
	SetBBR   = 0x02
	SetFlash = 0x04
)

var (
	ErrNAK     = errors.New("receiver rejected the request (ACK-NAK)")
	ErrTimeout = errors.New("timed out waiting for receiver response")
)

// KeySize returns the value width in bytes encoded in a config key.
func KeySize(key uint32) int {
	switch (key >> 28) & 0x7 {
	case 1, 2:
		return 1
	case 3:
		return 2
	case 4:
		return 4
	case 5:
		return 8
	}
	return 0
}

// KV is one configuration key and its raw little-endian value.
type KV struct {
	Key   uint32
	Value []byte
}

func (k KV) String() string { return fmt.Sprintf("0x%08X=%x", k.Key, k.Value) }

// Frame builds a UBX message with its checksum.
func Frame(cls, id byte, payload []byte) []byte {
	out := make([]byte, 0, 8+len(payload))
	out = append(out, 0xB5, 0x62, cls, id, byte(len(payload)), byte(len(payload)>>8))
	out = append(out, payload...)
	var a, b byte
	for _, c := range out[2:] {
		a += c
		b += a
	}
	return append(out, a, b)
}

// msg is a decoded UBX message.
type msg struct {
	cls, id byte
	payload []byte
}

// Session is a request/response conversation with the receiver over an
// already-open port. It does not own the port's lifetime.
type Session struct {
	rw      io.ReadWriter
	Timeout time.Duration
	buf     []byte
}

func NewSession(rw io.ReadWriter) *Session {
	return &Session{rw: rw, Timeout: 3 * time.Second, buf: make([]byte, 0, 65536)}
}

// poll writes a request and collects messages until want is seen, an ACK-NAK
// arrives, or the timeout expires. The receiver is streaming continuously, so
// responses are interleaved with navigation data and must be filtered out.
func (s *Session) poll(cls, id byte, payload []byte, wantCls, wantID byte, multi bool) ([]msg, error) {
	if _, err := s.rw.Write(Frame(cls, id, payload)); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	deadline := time.Now().Add(s.Timeout)
	var got []msg
	rd := make([]byte, 4096)
	for time.Now().Before(deadline) {
		n, err := s.rw.Read(rd)
		if n > 0 {
			s.buf = append(s.buf, rd[:n]...)
			var found bool
			s.buf, found = s.drain(&got, wantCls, wantID)
			if found && !multi {
				return got, nil
			}
			for _, m := range got {
				if m.cls == clsACK && m.id == idACKNak {
					return nil, ErrNAK
				}
			}
			if found && multi {
				return got, nil
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return got, err
		}
	}
	if len(got) > 0 {
		return got, nil
	}
	return nil, ErrTimeout
}

// drain extracts complete UBX frames, appending wanted ones (and any ACK) to out.
func (s *Session) drain(out *[]msg, wantCls, wantID byte) ([]byte, bool) {
	b := s.buf
	i, found := 0, false
	for i+8 <= len(b) {
		if b[i] != 0xB5 || b[i+1] != 0x62 {
			i++
			continue
		}
		ln := int(binary.LittleEndian.Uint16(b[i+4 : i+6]))
		if i+8+ln > len(b) {
			break
		}
		var a, c byte
		for _, x := range b[i+2 : i+6+ln] {
			a += x
			c += a
		}
		if a == b[i+6+ln] && c == b[i+7+ln] {
			cls, id := b[i+2], b[i+3]
			if (cls == wantCls && id == wantID) || cls == clsACK {
				p := make([]byte, ln)
				copy(p, b[i+6:i+6+ln])
				*out = append(*out, msg{cls, id, p})
				if cls == wantCls && id == wantID {
					found = true
				}
			}
			i += 8 + ln
			continue
		}
		i++
	}
	return b[i:], found
}

// MonVer is the receiver identification from UBX-MON-VER.
type MonVer struct {
	SwVersion  string
	HwVersion  string
	Extensions []string
}

// Model returns the MOD= extension, e.g. "ZED-X20P".
func (m MonVer) Model() string { return m.ext("MOD=") }

// Protver returns the PROTVER= extension.
func (m MonVer) Protver() string { return m.ext("PROTVER=") }

// Firmware returns the FWVER= extension.
func (m MonVer) Firmware() string { return m.ext("FWVER=") }

// Constellations reports the GNSS systems this firmware build supports.
// On the ZED-X20P this is how GLONASS support is determined authoritatively:
// it is simply absent from the list.
func (m MonVer) Constellations() []string {
	var out []string
	known := map[string]bool{"GPS": true, "GAL": true, "BDS": true, "GLO": true,
		"QZSS": true, "SBAS": true, "NAVIC": true, "IMES": true}
	for _, e := range m.Extensions {
		ok := true
		parts := splitSemi(e)
		if len(parts) == 0 {
			continue
		}
		for _, p := range parts {
			if !known[p] {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, parts...)
		}
	}
	return out
}

func (m MonVer) ext(prefix string) string {
	for _, e := range m.Extensions {
		if len(e) > len(prefix) && e[:len(prefix)] == prefix {
			return e[len(prefix):]
		}
	}
	return ""
}

func splitSemi(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ';' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// Version polls UBX-MON-VER.
func (s *Session) Version() (MonVer, error) {
	ms, err := s.poll(clsMON, idMONVer, nil, clsMON, idMONVer, false)
	if err != nil {
		return MonVer{}, err
	}
	for _, m := range ms {
		if m.cls != clsMON || m.id != idMONVer || len(m.payload) < 40 {
			continue
		}
		v := MonVer{
			SwVersion: cstr(m.payload[0:30]),
			HwVersion: cstr(m.payload[30:40]),
		}
		for o := 40; o+30 <= len(m.payload); o += 30 {
			if e := cstr(m.payload[o : o+30]); e != "" {
				v.Extensions = append(v.Extensions, e)
			}
		}
		return v, nil
	}
	return MonVer{}, ErrTimeout
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

const idMONSys = 0x39

// MonSys is the receiver's system report from UBX-MON-SYS. Its temperature
// field is documented as unsupported (always 0) on HPG 2.00 and is omitted.
type MonSys struct {
	BootType   int    `json:"boot_type"`
	BootReason string `json:"boot_reason"`
	CPULoad    int    `json:"cpu_load_pct"`
	CPULoadMax int    `json:"cpu_load_max_pct"`
	MemUsage   int    `json:"mem_usage_pct"`
	RunTime    int64  `json:"run_time_s"` // seconds since the receiver last restarted
	Notices    int    `json:"notices"`
	Warnings   int    `json:"warnings"`
	Errors     int    `json:"errors"`
}

var bootTypes = []string{"Unknown", "Cold start", "Watchdog", "Hardware reset",
	"Hardware backup", "Software backup", "Software reset", "VIO fail",
	"VDD_X fail", "VDD_RF fail", "V_CORE_HIGH fail", "System reset"}

// ParseMonSys decodes a UBX-MON-SYS payload (24 bytes, message version 1).
func ParseMonSys(p []byte) (MonSys, error) {
	if len(p) != 24 || p[0] != 1 {
		return MonSys{}, fmt.Errorf("unexpected MON-SYS payload (%d bytes)", len(p))
	}
	m := MonSys{
		BootType: int(p[1]), CPULoad: int(p[2]), CPULoadMax: int(p[3]), MemUsage: int(p[4]),
		RunTime:  int64(binary.LittleEndian.Uint32(p[8:12])),
		Notices:  int(binary.LittleEndian.Uint16(p[12:14])),
		Warnings: int(binary.LittleEndian.Uint16(p[14:16])),
		Errors:   int(binary.LittleEndian.Uint16(p[16:18])),
	}
	if m.BootType < len(bootTypes) {
		m.BootReason = bootTypes[m.BootType]
	}
	return m, nil
}

// System polls UBX-MON-SYS, which carries the receiver's uptime.
func (s *Session) System() (MonSys, error) {
	ms, err := s.poll(clsMON, idMONSys, nil, clsMON, idMONSys, false)
	if err != nil {
		return MonSys{}, err
	}
	for _, m := range ms {
		if m.cls == clsMON && m.id == idMONSys {
			return ParseMonSys(m.payload)
		}
	}
	return MonSys{}, ErrTimeout
}
