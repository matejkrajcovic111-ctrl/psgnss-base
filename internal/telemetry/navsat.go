// Package telemetry decodes receiver status from UBX and records a compact
// history for the dashboard.
package telemetry

import (
	"encoding/binary"
	"fmt"
	"math"
)

// GNSS identifiers as used by u-blox.
const (
	GPS     = 0
	SBAS    = 1
	Galileo = 2
	BeiDou  = 3
	IMES    = 4
	QZSS    = 5
	GLONASS = 6
	NavIC   = 7
)

// GNSSName maps a gnssId to a display name.
func GNSSName(id uint8) string {
	switch id {
	case GPS:
		return "GPS"
	case SBAS:
		return "SBAS"
	case Galileo:
		return "Galileo"
	case BeiDou:
		return "BeiDou"
	case IMES:
		return "IMES"
	case QZSS:
		return "QZSS"
	case GLONASS:
		return "GLONASS"
	case NavIC:
		return "NavIC"
	}
	return fmt.Sprintf("GNSS%d", id)
}

// Sat is one satellite as reported by UBX-NAV-SAT.
//
// Elevation, azimuth and C/N0 all arrive together in this one message, which is
// why the skyplot is driven from UBX rather than from the outbound RTCM: the
// correction streams carry no ephemeris, so deriving positions from them would
// need a second feed.
type Sat struct {
	GNSSID uint8
	SvID   uint8
	CNO    uint8 // dB-Hz
	Elev   int8  // degrees, -90..90
	Azim   int16 // degrees, 0..360
	Used   bool  // used in the navigation solution
	Health uint8 // 0 unknown, 1 healthy, 2 unhealthy
}

// Signal is one tracked GNSS signal from UBX-NAV-SIG.  NAV-SAT deliberately
// reports one aggregate C/N0 per satellite; NAV-SIG is the companion message
// that identifies the actual band/component being tracked.
type Signal struct {
	GNSSID uint8
	SvID   uint8
	SigID  uint8
	CNO    uint8
	Used   bool
}

// Epoch is one moment of receiver state.
type Epoch struct {
	Sats    []Sat
	FixType uint8
	NumSV   uint8
}

// FixName renders a UBX fix type.
func FixName(f uint8) string {
	switch f {
	case 0:
		return "no fix"
	case 1:
		return "dead reckoning"
	case 2:
		return "2D"
	case 3:
		return "3D"
	case 4:
		return "GNSS + dead reckoning"
	case 5:
		return "time only (fixed base)"
	}
	return "unknown"
}

// ParseNavSat decodes a UBX-NAV-SAT payload.
func ParseNavSat(p []byte) ([]Sat, error) {
	if len(p) < 8 {
		return nil, fmt.Errorf("NAV-SAT payload too short: %d bytes", len(p))
	}
	n := int(p[5])
	if len(p) < 8+12*n {
		return nil, fmt.Errorf("NAV-SAT declares %d satellites but payload is %d bytes", n, len(p))
	}
	out := make([]Sat, 0, n)
	for i := 0; i < n; i++ {
		o := 8 + 12*i
		flags := binary.LittleEndian.Uint32(p[o+8 : o+12])
		out = append(out, Sat{
			GNSSID: p[o],
			SvID:   p[o+1],
			CNO:    p[o+2],
			Elev:   int8(p[o+3]),
			Azim:   int16(binary.LittleEndian.Uint16(p[o+4 : o+6])),
			Used:   flags&(1<<3) != 0,
			Health: uint8((flags >> 4) & 0x3),
		})
	}
	return out, nil
}

// ParseNavSig decodes UBX-NAV-SIG. Version 0 has an eight-byte header followed
// by one 16-byte record per signal. The u-blox-defined sigFlags bit 3 means the
// receiver is using that signal in its navigation solution.
func ParseNavSig(p []byte) ([]Signal, error) {
	if len(p) < 8 {
		return nil, fmt.Errorf("NAV-SIG payload too short: %d bytes", len(p))
	}
	n := int(p[5])
	if len(p) < 8+16*n {
		return nil, fmt.Errorf("NAV-SIG declares %d signals but payload is %d bytes", n, len(p))
	}
	out := make([]Signal, 0, n)
	for i := 0; i < n; i++ {
		o := 8 + 16*i
		flags := binary.LittleEndian.Uint16(p[o+10 : o+12])
		out = append(out, Signal{GNSSID: p[o], SvID: p[o+1], SigID: p[o+2],
			CNO: p[o+6], Used: flags&(1<<3) != 0})
	}
	return out, nil
}

// PVT is the subset of UBX-NAV-PVT the dashboard shows.
type PVT struct {
	FixType uint8
	NumSV   uint8
	HAcc    float64 // metres
	VAcc    float64
}

// ParseNavPVT decodes a UBX-NAV-PVT payload.
func ParseNavPVT(p []byte) (PVT, error) {
	if len(p) < 48 {
		return PVT{}, fmt.Errorf("NAV-PVT payload too short: %d bytes", len(p))
	}
	return PVT{
		FixType: p[20],
		NumSV:   p[23],
		HAcc:    float64(binary.LittleEndian.Uint32(p[40:44])) / 1000,
		VAcc:    float64(binary.LittleEndian.Uint32(p[44:48])) / 1000,
	}, nil
}

// ParseRawxLeapSeconds pulls the GPS-UTC offset out of a UBX-RXM-RAWX payload.
//
// The UI shows GPS time alongside UTC, and the offset changes when a leap
// second is introduced, so it is read from the receiver rather than hardcoded.
func ParseRawxLeapSeconds(p []byte) (int8, uint16, float64, bool) {
	if len(p) < 16 {
		return 0, 0, 0, false
	}
	tow := math.Float64frombits(binary.LittleEndian.Uint64(p[0:8]))
	week := binary.LittleEndian.Uint16(p[8:10])
	leap := int8(p[10])
	if week == 0 {
		return 0, 0, 0, false
	}
	return leap, week, tow, true
}

// Pack serialises satellites into a compact blob.
//
// Six bytes per satellite instead of a database row each. At ~30 satellites
// that is ~180 bytes per epoch, so a week at the configured decimation costs
// tens of megabytes rather than tens of millions of rows.
func Pack(sats []Sat) []byte {
	b := make([]byte, 0, len(sats)*6)
	for _, s := range sats {
		flags := s.Health & 0x3
		if s.Used {
			flags |= 1 << 2
		}
		// Azimuth fits in one byte at 2-degree resolution, which is finer
		// than a skyplot can display.
		az := uint8((int(s.Azim) % 360) / 2)
		b = append(b, s.GNSSID, s.SvID, s.CNO, uint8(s.Elev), az, flags)
	}
	return b
}

// Unpack reverses Pack.
func Unpack(b []byte) []Sat {
	out := make([]Sat, 0, len(b)/6)
	for i := 0; i+6 <= len(b); i += 6 {
		out = append(out, Sat{
			GNSSID: b[i],
			SvID:   b[i+1],
			CNO:    b[i+2],
			Elev:   int8(b[i+3]),
			Azim:   int16(b[i+4]) * 2,
			Used:   b[i+5]&(1<<2) != 0,
			Health: b[i+5] & 0x3,
		})
	}
	return out
}

// PackSignals serialises NAV-SIG observations at five bytes each. It is kept
// separate from the satellite blob so existing history remains readable.
func PackSignals(signals []Signal) []byte {
	b := make([]byte, 0, len(signals)*5)
	for _, s := range signals {
		flags := byte(0)
		if s.Used {
			flags = 1
		}
		b = append(b, s.GNSSID, s.SvID, s.SigID, s.CNO, flags)
	}
	return b
}

func UnpackSignals(b []byte) []Signal {
	out := make([]Signal, 0, len(b)/5)
	for i := 0; i+5 <= len(b); i += 5 {
		out = append(out, Signal{GNSSID: b[i], SvID: b[i+1], SigID: b[i+2], CNO: b[i+3], Used: b[i+4]&1 != 0})
	}
	return out
}
