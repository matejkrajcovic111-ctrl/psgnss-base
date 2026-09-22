// Package hub owns the receiver stream: it reads one input, splits it into
// protocol frames, and fans those out to many independent outputs.
package hub

import (
	"encoding/binary"

	"io"
)

// Proto identifies the wire protocol of a frame.
type Proto uint8

const (
	ProtoUnknown Proto = iota
	ProtoUBX
	ProtoRTCM3
	ProtoNMEA
)

func (p Proto) String() string {
	switch p {
	case ProtoUBX:
		return "ubx"
	case ProtoRTCM3:
		return "rtcm3"
	case ProtoNMEA:
		return "nmea"
	}
	return "unknown"
}

// Frame is one complete, checksum-valid protocol message.
//
// Raw is the entire frame including sync bytes and checksum, so an output can
// forward it byte-for-byte without re-encoding. That is what makes the fan-out
// lossless.
type Frame struct {
	Proto Proto
	// Type is the RTCM3 message number, or (class<<8 | id) for UBX.
	// Zero for NMEA.
	Type int
	Raw  []byte
}

// UBX class/id helpers.
func (f Frame) UBXClass() byte { return byte(f.Type >> 8) }
func (f Frame) UBXID() byte    { return byte(f.Type) }

const (
	maxRTCMLen = 1023 + 6  // 10-bit length field plus header and CRC
	maxUBXLen  = 8 + 65535 // 16-bit length field plus header and checksum
	maxNMEALen = 128
	// maxFrame bounds the resync buffer.
	maxFrame = maxUBXLen
)

// Scanner splits a byte stream into frames, discarding anything that is not a
// valid frame. It tolerates arbitrary read boundaries and resynchronises after
// corruption without losing the following frames.
type Scanner struct {
	r   io.Reader
	buf []byte
	// start..end is the unconsumed window within buf.
	start, end int
	err        error
	// Stats, cumulative.
	BytesRead    int64
	BytesFramed  int64
	BytesDropped int64
	Resyncs      int64
}

// NewScanner reads frames from r. bufSize should comfortably exceed the largest
// expected frame; it grows automatically if not.
func NewScanner(r io.Reader, bufSize int) *Scanner {
	if bufSize < 8192 {
		bufSize = 8192
	}
	return &Scanner{r: r, buf: make([]byte, bufSize)}
}

// Next returns the next complete frame. The returned Frame.Raw aliases the
// scanner's internal buffer and is only valid until the following call to Next;
// copy it if it must outlive that.
//
// Returns io.EOF when the underlying reader is exhausted.
func (s *Scanner) Next() (Frame, error) {
	for {
		if f, ok := s.scan(); ok {
			return f, nil
		}
		if s.err != nil {
			return Frame{}, s.err
		}
		if err := s.fill(); err != nil {
			// Surface buffered frames before reporting the error.
			if f, ok := s.scan(); ok {
				return f, nil
			}
			return Frame{}, err
		}
	}
}

// fill compacts the buffer and reads more data.
func (s *Scanner) fill() error {
	if s.start > 0 {
		copy(s.buf, s.buf[s.start:s.end])
		s.end -= s.start
		s.start = 0
	}
	if s.end == len(s.buf) {
		if len(s.buf) >= maxFrame {
			// A full buffer with no valid frame means the window is garbage.
			s.BytesDropped += int64(s.end)
			s.Resyncs++
			s.end = 0
		} else {
			grow := make([]byte, min(len(s.buf)*2, maxFrame))
			copy(grow, s.buf[:s.end])
			s.buf = grow
		}
	}
	n, err := s.r.Read(s.buf[s.end:])
	s.end += n
	s.BytesRead += int64(n)
	if err != nil {
		s.err = err
		if n > 0 {
			return nil
		}
		return err
	}
	return nil
}

// scan attempts to extract one frame from the buffered window.
func (s *Scanner) scan() (Frame, bool) {
	for s.start < s.end {
		b := s.buf[s.start]
		var (
			f    Frame
			n    int
			need bool
		)
		switch b {
		case 0xD3:
			f, n, need = s.tryRTCM3()
		case 0xB5:
			f, n, need = s.tryUBX()
		case '$':
			f, n, need = s.tryNMEA()
		default:
			s.start++
			s.BytesDropped++
			continue
		}
		if need {
			// Incomplete: wait for more bytes rather than discarding.
			return Frame{}, false
		}
		if n > 0 {
			s.start += n
			s.BytesFramed += int64(n)
			return f, true
		}
		// Sync byte present but the frame did not validate: skip one byte and
		// resync. Skipping exactly one avoids swallowing a real frame that
		// starts inside the rejected window.
		s.start++
		s.BytesDropped++
		s.Resyncs++
	}
	return Frame{}, false
}

// tryRTCM3 reports (frame, consumed, needMore).
func (s *Scanner) tryRTCM3() (Frame, int, bool) {
	w := s.buf[s.start:s.end]
	if len(w) < 3 {
		return Frame{}, 0, true
	}
	if w[1]&0xFC != 0 { // upper 6 bits of the length field are reserved zero
		return Frame{}, 0, false
	}
	length := int(w[1]&0x03)<<8 | int(w[2])
	total := 3 + length + 3
	if len(w) < total {
		if total > maxRTCMLen {
			return Frame{}, 0, false
		}
		return Frame{}, 0, true
	}
	got := uint32(w[3+length])<<16 | uint32(w[4+length])<<8 | uint32(w[5+length])
	if crc24q(w[:3+length]) != got {
		return Frame{}, 0, false
	}
	typ := 0
	if length >= 2 {
		typ = int(w[3])<<4 | int(w[4])>>4
	}
	return Frame{Proto: ProtoRTCM3, Type: typ, Raw: w[:total]}, total, false
}
func (s *Scanner) tryUBX() (Frame, int, bool) {
	w := s.buf[s.start:s.end]
	if len(w) < 2 {
		return Frame{}, 0, true
	}
	if w[1] != 0x62 {
		return Frame{}, 0, false
	}
	if len(w) < 6 {
		return Frame{}, 0, true
	}
	length := int(binary.LittleEndian.Uint16(w[4:6]))
	total := 8 + length
	if len(w) < total {
		return Frame{}, 0, true
	}
	var a, b byte
	for _, c := range w[2 : 6+length] {
		a += c
		b += a
	}
	if a != w[6+length] || b != w[7+length] {
		return Frame{}, 0, false
	}
	return Frame{Proto: ProtoUBX, Type: int(w[2])<<8 | int(w[3]), Raw: w[:total]}, total, false
}
func (s *Scanner) tryNMEA() (Frame, int, bool) {
	w := s.buf[s.start:s.end]
	for i := 1; i < len(w) && i < maxNMEALen; i++ {
		if w[i] == '\n' {
			return Frame{Proto: ProtoNMEA, Raw: w[:i+1]}, i + 1, false
		}
		if w[i] == '$' { // a new sentence started: the previous one was truncated
			return Frame{}, 0, false
		}
	}
	if len(w) >= maxNMEALen {
		return Frame{}, 0, false
	}
	return Frame{}, 0, true
}

// crc24qTable is the RTCM3 CRC-24Q table (polynomial 0x1864CFB).
var crc24qTable = func() [256]uint32 {
	var t [256]uint32
	for i := range t {
		c := uint32(i) << 16
		for j := 0; j < 8; j++ {
			c <<= 1
			if c&0x1000000 != 0 {
				c ^= 0x1864CFB
			}
		}
		t[i] = c & 0xFFFFFF
	}
	return t
}()

func crc24q(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc = ((crc << 8) & 0xFFFFFF) ^ crc24qTable[((crc>>16)^uint32(b))&0xFF]
	}
	return crc
}
