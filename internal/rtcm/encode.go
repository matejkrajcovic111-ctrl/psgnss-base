// Package rtcm encodes the RTCM 3 station-description messages that a base
// must broadcast but a u-blox receiver does not emit.
//
// The ZED-X20P sends 1005 and the MSM observation messages. It never sends
// 1006 (reference point with antenna height), 1008 (antenna descriptor) or
// 1033 (receiver and antenna descriptors). Those are generated here from
// configuration, exactly as STRSVR did.
package rtcm

import (
	"fmt"
	"math"
)

// bitWriter packs big-endian bit fields, which is how RTCM 3 is laid out.
type bitWriter struct {
	buf []byte
	n   int // bits written
}

func (w *bitWriter) put(v uint64, bits int) {
	for i := bits - 1; i >= 0; i-- {
		if w.n%8 == 0 {
			w.buf = append(w.buf, 0)
		}
		if (v>>uint(i))&1 == 1 {
			w.buf[w.n/8] |= 1 << uint(7-w.n%8)
		}
		w.n++
	}
}

func (w *bitWriter) putSigned(v int64, bits int) { w.put(uint64(v)&(1<<uint(bits)-1), bits) }

func (w *bitWriter) putString(s string) {
	w.put(uint64(len(s)), 8)
	for i := 0; i < len(s); i++ {
		w.put(uint64(s[i]), 8)
	}
}

// pad completes the final byte.
func (w *bitWriter) pad() {
	for w.n%8 != 0 {
		w.put(0, 1)
	}
}

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

func crc24q(d []byte) uint32 {
	var crc uint32
	for _, b := range d {
		crc = ((crc << 8) & 0xFFFFFF) ^ crc24qTable[((crc>>16)^uint32(b))&0xFF]
	}
	return crc
}

// wrap adds the RTCM 3 transport header and CRC.
func wrap(payload []byte) ([]byte, error) {
	if len(payload) > 1023 {
		return nil, fmt.Errorf("rtcm payload %d bytes exceeds the 1023-byte frame limit", len(payload))
	}
	out := make([]byte, 0, 3+len(payload)+3)
	out = append(out, 0xD3, byte(len(payload)>>8)&0x03, byte(len(payload)))
	out = append(out, payload...)
	c := crc24q(out)
	return append(out, byte(c>>16), byte(c>>8), byte(c)), nil
}

// Station describes the base for the generated messages.
type Station struct {
	ID uint16
	// ECEF metres.
	X, Y, Z float64
	// AntennaHeight is the ARP-to-antenna offset in metres, message 1006 only.
	AntennaHeight float64
	// Descriptors. AntennaDescriptor must stay as surveyed -- changing it
	// changes what rovers assume about the phase centre.
	AntennaDescriptor string
	AntennaSerial     string
	AntennaSetupID    uint8
	ReceiverType      string
	ReceiverFirmware  string
	ReceiverSerial    string
	// Constellation indicators for 1005/1006.
	GPS, GLONASS, Galileo bool
}

// LLHToECEF converts geodetic coordinates on WGS-84 to ECEF metres.
func LLHToECEF(latDeg, lonDeg, h float64) (x, y, z float64) {
	const a = 6378137.0
	const f = 1 / 298.257223563
	e2 := f * (2 - f)
	lat := latDeg * math.Pi / 180
	lon := lonDeg * math.Pi / 180
	sl, cl := math.Sin(lat), math.Cos(lat)
	n := a / math.Sqrt(1-e2*sl*sl)
	return (n + h) * cl * math.Cos(lon), (n + h) * cl * math.Sin(lon), (n*(1-e2) + h) * sl
}

func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// encodeARP writes the fields shared by 1005 and 1006.
func (s Station) encodeARP(w *bitWriter, msgType uint16) {
	w.put(uint64(msgType), 12)                  // DF002
	w.put(uint64(s.ID), 12)                     // DF003 reference station ID
	w.put(0, 6)                                 // DF021 ITRF realization year
	w.put(b2u(s.GPS), 1)                        // DF022
	w.put(b2u(s.GLONASS), 1)                    // DF023
	w.put(b2u(s.Galileo), 1)                    // DF024
	w.put(0, 1)                                 // DF141 reference-station indicator: real
	w.putSigned(int64(math.Round(s.X*1e4)), 38) // DF025, 0.1 mm
	w.put(0, 1)                                 // DF142 single receiver oscillator
	w.put(0, 1)                                 // reserved
	w.putSigned(int64(math.Round(s.Y*1e4)), 38) // DF026
	w.put(0, 2)                                 // DF364 quarter cycle indicator
	w.putSigned(int64(math.Round(s.Z*1e4)), 38) // DF027
}

// Encode1005 builds a stationary antenna reference point message.
func (s Station) Encode1005() ([]byte, error) {
	var w bitWriter
	s.encodeARP(&w, 1005)
	w.pad()
	return wrap(w.buf)
}

// Encode1006 builds a reference point message with antenna height.
func (s Station) Encode1006() ([]byte, error) {
	var w bitWriter
	s.encodeARP(&w, 1006)
	h := int64(math.Round(s.AntennaHeight * 1e4))
	if h < 0 {
		h = 0
	}
	w.put(uint64(h), 16) // DF028 antenna height, 0.1 mm
	w.pad()
	return wrap(w.buf)
}

// Encode1008 builds an antenna descriptor message.
func (s Station) Encode1008() ([]byte, error) {
	if len(s.AntennaDescriptor) > 31 || len(s.AntennaSerial) > 31 {
		return nil, fmt.Errorf("antenna descriptor/serial exceed 31 characters")
	}
	var w bitWriter
	w.put(1008, 12)
	w.put(uint64(s.ID), 12)
	w.putString(s.AntennaDescriptor)   // DF029/DF030
	w.put(uint64(s.AntennaSetupID), 8) // DF031
	w.putString(s.AntennaSerial)       // DF032/DF033
	w.pad()
	return wrap(w.buf)
}

// Encode1033 builds the receiver and antenna descriptor message.
func (s Station) Encode1033() ([]byte, error) {
	for _, f := range []string{s.AntennaDescriptor, s.AntennaSerial,
		s.ReceiverType, s.ReceiverFirmware, s.ReceiverSerial} {
		if len(f) > 31 {
			return nil, fmt.Errorf("descriptor %q exceeds 31 characters", f)
		}
	}
	var w bitWriter
	w.put(1033, 12)
	w.put(uint64(s.ID), 12)
	w.putString(s.AntennaDescriptor)
	w.put(uint64(s.AntennaSetupID), 8)
	w.putString(s.AntennaSerial)
	w.putString(s.ReceiverType)
	w.putString(s.ReceiverFirmware)
	w.putString(s.ReceiverSerial)
	w.pad()
	return wrap(w.buf)
}

// Generate builds the message of the given type, or reports that this package
// does not synthesise it.
func (s Station) Generate(msgType int) ([]byte, error) {
	switch msgType {
	case 1005:
		return s.Encode1005()
	case 1006:
		return s.Encode1006()
	case 1008:
		return s.Encode1008()
	case 1033:
		return s.Encode1033()
	}
	return nil, fmt.Errorf("message type %d is not generated here", msgType)
}

// CanGenerate reports whether Generate handles this type. The caster uses it to
// decide which mountpoint messages come from the receiver and which must be
// synthesised.
func CanGenerate(msgType int) bool {
	switch msgType {
	case 1005, 1006, 1008, 1033:
		return true
	}
	return false
}

// MSM header bit offsets, from the RTCM 10403.x MSM header layout:
// DF002 message number (12) | DF003 reference station ID (12) |
// GNSS epoch time (30) | DF393 multiple message bit (1) | ...
const (
	msmTypeBits  = 12
	msmStaIDBits = 12
	msmEpochBits = 30
	// df393Offset is the bit position of the multiple message bit.
	df393Offset = msmTypeBits + msmStaIDBits + msmEpochBits
)

// IsMSM reports whether a message type is a Multiple Signal Message.
func IsMSM(msgType int) bool {
	if msgType < 1071 || msgType > 1137 {
		return false
	}
	switch msgType % 10 {
	case 1, 2, 3, 4, 5, 6, 7:
		return true
	}
	return false
}

// MSMType extracts the message type from a complete RTCM frame.
func MSMType(frame []byte) int {
	if len(frame) < 9 || frame[0] != 0xD3 {
		return 0
	}
	return int(frame[3])<<4 | int(frame[4])>>4
}

// ClearMultipleMessageBit sets DF393 to 0 on an MSM frame, marking it the last
// message of its epoch, and repairs the CRC.
//
// This is needed because a mountpoint serves a *subset* of the receiver's
// messages. The receiver terminates each epoch once, on the very last message
// it emits (1127 here). A mountpoint that filters that message away leaves the
// rover with no end-of-epoch marker, so it cannot process the epoch until the
// next one begins -- one full second of extra correction age.
//
// It returns a new slice; the input is not modified, so the raw passthrough
// outputs and the archives keep the receiver's bytes exactly.
//
// This is the only place PSGNSS alters a receiver message. It is a single bit
// and a CRC, not a decode/re-encode: no observable is touched.
func ClearMultipleMessageBit(frame []byte) ([]byte, bool) {
	if len(frame) < 6 || frame[0] != 0xD3 {
		return frame, false
	}
	length := int(frame[1]&0x03)<<8 | int(frame[2])
	if len(frame) != 3+length+3 {
		return frame, false
	}
	if !IsMSM(MSMType(frame)) {
		return frame, false
	}
	byteIdx := 3 + df393Offset/8
	mask := byte(1 << (7 - df393Offset%8))
	if byteIdx >= 3+length {
		return frame, false
	}
	if frame[byteIdx]&mask == 0 {
		return frame, false // already terminated
	}
	out := make([]byte, len(frame))
	copy(out, frame)
	out[byteIdx] &^= mask
	c := crc24q(out[:3+length])
	out[3+length] = byte(c >> 16)
	out[4+length] = byte(c >> 8)
	out[5+length] = byte(c)
	return out, true
}

// EpochTerminator picks which of a mountpoint's MSM types should carry the
// end-of-epoch marker: the last one the receiver emits within an epoch.
//
// The receiver emits ascending by constellation (GPS 107x, Galileo 109x,
// BeiDou 112x) and MSM4 before MSM7 within each, so the highest selected type
// is the last to arrive. Returns 0 when the set contains no MSM messages.
func EpochTerminator(types []int) int {
	best := 0
	for _, t := range types {
		if IsMSM(t) && t > best {
			best = t
		}
	}
	return best
}

// Writer builds one RTCM 3 message. It exists so other packages can encode
// message types this one does not know about -- ephemeris, for instance --
// without reimplementing the bit packing, the padding or the CRC.
type Writer struct{ w bitWriter }

func NewWriter() *Writer { return &Writer{} }

// Put writes an unsigned field of the given width, most significant bit first.
func (o *Writer) Put(v uint64, bits int) { o.w.put(v, bits) }

// PutSigned writes a two's-complement field of the given width.
func (o *Writer) PutSigned(v int64, bits int) { o.w.putSigned(v, bits) }

// Frame pads the payload to a byte boundary and wraps it with the D3 preamble,
// the length and the CRC-24Q.
func (o *Writer) Frame() ([]byte, error) {
	o.w.pad()
	return wrap(o.w.buf)
}
