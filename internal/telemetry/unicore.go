package telemetry

import (
	"encoding/binary"
	"fmt"
	"time"
)

// Unicore binary logs (UM980 and relatives), decoded into the same model the
// UBX messages feed. Layouts were established on a live UM980 (firmware
// R4.10Build20739) by decoding each binary log and comparing it field by field
// with the ASCII twin of the same epoch (same header week and ms); the manual's
// table for SATSINFO is ambiguous about the per-satellite record.

// Unicore binary message IDs.
const (
	UnicoreBestSat  = 1041
	UnicoreBestNav  = 2118
	UnicoreSatsInfo = 2124
)

const (
	unicoreHeaderLen = 24
	// unicoreTimeFine is the header time status of an ASCII "FINE" log. The
	// manual gives only the names; 160 is what the receiver sends in every
	// binary log whose ASCII twin says FINE.
	unicoreTimeFine = 160
)

// unicoreTable is NovAtel's CRC32 (reflected 0xEDB88320, zero initial value, no
// final xor), which Unicore uses unchanged. The hub frames with the same
// function; it is repeated here so the decoders can verify what they are given
// and tests can seal frames they build.
var unicoreTable = func() [256]uint32 {
	var t [256]uint32
	for i := range t {
		c := uint32(i)
		for j := 0; j < 8; j++ {
			if c&1 != 0 {
				c = c>>1 ^ 0xEDB88320
			} else {
				c >>= 1
			}
		}
		t[i] = c
	}
	return t
}()

// UnicoreCRC32 computes the CRC32 that closes a Unicore binary frame.
func UnicoreCRC32(b []byte) uint32 {
	var crc uint32
	for _, c := range b {
		crc = crc>>8 ^ unicoreTable[byte(crc)^c]
	}
	return crc
}

// IsUnicoreFrame reports whether b starts with the Unicore binary sync bytes.
func IsUnicoreFrame(b []byte) bool {
	return len(b) >= unicoreHeaderLen+4 && b[0] == 0xAA && b[1] == 0x44 && b[2] == 0xB5
}

// UnicoreHeader is the 24-byte header every binary log carries.
type UnicoreHeader struct {
	MsgID      uint16
	TimeRef    uint8  // 0 = GPST
	TimeStatus uint8  // 160 = FINE
	Week       uint16 // of TimeRef
	MS         uint32 // ms of week of the epoch the log describes
	LeapSec    uint8  // GPST - UTC
	DelayMS    uint16 // how long after the epoch the receiver emitted the log
}

// Fine reports whether the header time can be trusted as GPS time.
func (h UnicoreHeader) Fine() bool {
	return h.TimeRef == 0 && h.TimeStatus == unicoreTimeFine && h.Week != 0
}

var gpsEpoch = time.Date(1980, 1, 6, 0, 0, 0, 0, time.UTC)

// EpochUTC is the epoch time the log describes, converted to UTC with the
// header's own leap-second count. Only meaningful when Fine() and LeapSec > 0.
func (h UnicoreHeader) EpochUTC() time.Time {
	return gpsEpoch.Add(time.Duration(h.Week)*7*24*time.Hour +
		time.Duration(h.MS)*time.Millisecond - time.Duration(h.LeapSec)*time.Second)
}

// SplitUnicore validates a whole binary frame (length and CRC) and returns its
// header and data.
func SplitUnicore(f []byte) (UnicoreHeader, []byte, error) {
	if !IsUnicoreFrame(f) {
		return UnicoreHeader{}, nil, fmt.Errorf("not a Unicore binary frame")
	}
	n := int(binary.LittleEndian.Uint16(f[6:8]))
	if len(f) != unicoreHeaderLen+n+4 {
		return UnicoreHeader{}, nil, fmt.Errorf("Unicore frame is %d bytes, header says %d", len(f), unicoreHeaderLen+n+4)
	}
	if UnicoreCRC32(f[:len(f)-4]) != binary.LittleEndian.Uint32(f[len(f)-4:]) {
		return UnicoreHeader{}, nil, fmt.Errorf("Unicore frame CRC mismatch")
	}
	return UnicoreHeader{
		MsgID:      binary.LittleEndian.Uint16(f[4:6]),
		TimeRef:    f[8],
		TimeStatus: f[9],
		Week:       binary.LittleEndian.Uint16(f[10:12]),
		MS:         binary.LittleEndian.Uint32(f[12:16]),
		LeapSec:    f[21],
		DelayMS:    binary.LittleEndian.Uint16(f[22:24]),
	}, f[unicoreHeaderLen : len(f)-4], nil
}

// Unicore satellite-system identifiers as they appear in SATSINFO.
const (
	ucGPS     = 0
	ucGLONASS = 1
	ucSBAS    = 2
	ucGalileo = 3
	ucBeiDou  = 4
	ucQZSS    = 5
	ucNavIC   = 6
)

// satID maps a Unicore system id and PRN to the u-blox (gnssId, svId) that
// UBX-NAV-SAT would use for the same satellite, so history recorded under the
// X20P and under the UM980 lines up. Unicore numbers GLONASS 38-61 (slot + 37;
// checked against the GLONASS frequency channels BESTSAT reports, which match
// the almanac for slots 3, 4, 11, 12, 13, 21, 22, 23) and QZSS 193-202, where
// u-blox uses slots 1-24 and 1-10. The rest are the same numbers.
func satID(sys, prn uint8) (gnss, sv uint8, ok bool) {
	switch sys {
	case ucGPS:
		return GPS, prn, prn >= 1 && prn <= 32
	case ucGLONASS:
		return GLONASS, prn - 37, prn >= 38 && prn <= 61
	case ucSBAS:
		return SBAS, prn, prn >= 120 && prn <= 158
	case ucGalileo:
		return Galileo, prn, prn >= 1 && prn <= 36
	case ucBeiDou:
		return BeiDou, prn, prn >= 1 && prn <= 63
	case ucQZSS:
		return QZSS, prn - 192, prn >= 193 && prn <= 202
	case ucNavIC:
		return NavIC, prn, prn >= 1 && prn <= 15
	}
	return 0, 0, false
}

// bdsGEO reports the BeiDou geostationary satellites, which broadcast the D2
// message on B1I/B2I/B3I where MEO and IGSO satellites broadcast D1.
func bdsGEO(sv uint8) bool { return sv <= 5 || sv >= 59 }

// sigID maps a Unicore frequency identifier (manual table "Frequency
// Identifier", which is per system) to the u-blox sigId that UBX-NAV-SIG uses,
// because the web UI labels bands from the u-blox numbers. Signals u-blox has no
// id for (GPS L2P(Y) and L1C, GLONASS L3, QZSS L6, BeiDou B2b, SBAS L5) are
// not mapped; they still count toward the satellite's aggregate C/N0.
func sigID(gnss, sv, freq uint8) (uint8, bool) {
	switch gnss {
	case GPS:
		switch freq {
		case 0: // L1 C/A
			return 0, true
		case 17: // L2C(L)
			return 3, true
		case 6: // L5 data (I)
			return 6, true
		case 14: // L5 pilot (Q)
			return 7, true
		}
	case SBAS:
		if freq == 0 {
			return 0, true
		}
	case Galileo:
		switch freq {
		case 2: // E1C
			return 0, true
		case 1: // E1B
			return 1, true
		case 12: // E5a pilot (Q)
			return 4, true
		case 17: // E5b pilot (Q)
			return 6, true
		case 22: // E6C
			return 7, true
		case 18: // E6B
			return 8, true
		}
	case BeiDou:
		d2 := uint8(0)
		if bdsGEO(sv) {
			d2 = 1
		}
		switch freq {
		case 0: // B1I
			return 0 + d2, true
		case 17: // B2I
			return 2 + d2, true
		case 21: // B3I
			return 4 + d2, true
		case 8, 23: // B1C pilot, data
			return 5, true
		case 12, 28: // B2a pilot, data
			return 7, true
		}
	case QZSS:
		switch freq {
		case 0: // L1 C/A
			return 0, true
		case 17: // L2C(L)
			return 5, true
		case 6: // L5 data (I)
			return 8, true
		case 14: // L5 pilot (Q)
			return 9, true
		}
	case GLONASS:
		switch freq {
		case 0: // L1 OF
			return 0, true
		case 5: // L2 OF
			return 2, true
		}
	case NavIC:
		if freq == 6 { // L5 data
			return 0, true
		}
	}
	return 0, false
}

// ParseSatsInfo decodes a whole SATSINFOB frame (ID 2124).
//
// Data layout, version 2: count u8, version u8, three reserved bytes, frequency
// flag u8 (all verified), then per satellite PRN u8, azimuth u16, elevation i8,
// followed by one 4-byte record per tracked frequency: system u8, C/N0 u8,
// frequency id u8, number of frequencies u8 (repeated in each record). The
// manual's "next frequency info" description hides that the first record is
// the same shape as the others.
//
// Sat.CNO is the best of the satellite's signals. SATSINFO does not say whether
// a satellite is used in the solution; Sat.Used and Signal.Used stay false
// until MarkUsed applies a BESTSAT. Health is left 0 (unknown).
func ParseSatsInfo(frame []byte) ([]Sat, []Signal, error) {
	h, p, err := SplitUnicore(frame)
	if err != nil {
		return nil, nil, err
	}
	if h.MsgID != UnicoreSatsInfo {
		return nil, nil, fmt.Errorf("message %d is not SATSINFO", h.MsgID)
	}
	if len(p) < 6 {
		return nil, nil, fmt.Errorf("SATSINFO data too short: %d bytes", len(p))
	}
	if p[1] != 2 {
		return nil, nil, fmt.Errorf("SATSINFO version %d, only 2 is understood", p[1])
	}
	n := int(p[0])
	sats := make([]Sat, 0, n)
	var sigs []Signal
	o := 6
	for i := 0; i < n; i++ {
		if o+4 > len(p) {
			return nil, nil, fmt.Errorf("SATSINFO truncated at satellite %d of %d", i, n)
		}
		prn := p[o]
		azim := binary.LittleEndian.Uint16(p[o+1 : o+3])
		elev := int8(p[o+3])
		nf := 0
		if o+8 <= len(p) {
			nf = int(p[o+7])
		}
		end := o + 4 + 4*nf
		if end > len(p) {
			return nil, nil, fmt.Errorf("SATSINFO truncated in the frequencies of satellite %d", i)
		}
		sys := uint8(0)
		if nf > 0 {
			sys = p[o+4]
		}
		gnss, sv, ok := satID(sys, prn)
		if ok {
			var best uint8
			for q := 0; q < nf; q++ {
				r := p[o+4+4*q : o+8+4*q]
				if r[1] > best {
					best = r[1]
				}
				if id, ok := sigID(gnss, sv, r[2]); ok {
					sigs = addSignal(sigs, Signal{GNSSID: gnss, SvID: sv, SigID: id, CNO: r[1]})
				}
			}
			sats = append(sats, Sat{GNSSID: gnss, SvID: sv, CNO: best, Elev: elev, Azim: int16(azim)})
		}
		o = end
	}
	return sats, sigs, nil
}

// addSignal appends s, keeping the stronger of two records that map to one
// u-blox sigId (BeiDou B1C data and pilot, for example).
func addSignal(sigs []Signal, s Signal) []Signal {
	for i := range sigs {
		if sigs[i].GNSSID == s.GNSSID && sigs[i].SvID == s.SvID && sigs[i].SigID == s.SigID {
			if s.CNO > sigs[i].CNO {
				sigs[i].CNO = s.CNO
			}
			return sigs
		}
	}
	return append(sigs, s)
}

// UsedSet is the satellites BESTSAT lists as used, keyed by u-blox (gnssId, svId).
type UsedSet map[[2]uint8]bool

// BESTSAT satellite-system enum, which differs from SATSINFO's.
const (
	bsGPS     = 0
	bsGLONASS = 1
	bsSBAS    = 2
	bsGalileo = 5
	bsBeiDou  = 6
	bsQZSS    = 7
	bsNavIC   = 9
)

// ParseBestSat decodes a whole BESTSATB frame (ID 1041): count u32, then 16
// bytes per satellite: system u32, satellite id u32, status u32, signal mask
// u32. Verified: the PRN sits in the upper 16 bits of the id (the manual says
// lower) and for GLONASS the lower 16 bits are the frequency channel + 7. The
// PRN uses the same numbering as SATSINFO. A satellite counts as used when its
// signal mask is non-zero; the manual's two mask tables disagree on bit
// positions, but bit 4 alone (common view) is never seen without a signal bit.
func ParseBestSat(frame []byte) (UsedSet, error) {
	h, p, err := SplitUnicore(frame)
	if err != nil {
		return nil, err
	}
	if h.MsgID != UnicoreBestSat {
		return nil, fmt.Errorf("message %d is not BESTSAT", h.MsgID)
	}
	if len(p) < 4 {
		return nil, fmt.Errorf("BESTSAT data too short: %d bytes", len(p))
	}
	n := int(binary.LittleEndian.Uint32(p[0:4]))
	if n < 0 || len(p) < 4+16*n {
		return nil, fmt.Errorf("BESTSAT declares %d satellites but data is %d bytes", n, len(p))
	}
	out := make(UsedSet, n)
	for i := 0; i < n; i++ {
		r := p[4+16*i : 20+16*i]
		sys := binary.LittleEndian.Uint32(r[0:4])
		prn := binary.LittleEndian.Uint16(r[6:8])
		if binary.LittleEndian.Uint32(r[12:16]) == 0 || prn > 255 {
			continue
		}
		var us uint8
		switch sys {
		case bsGPS:
			us = ucGPS
		case bsGLONASS:
			us = ucGLONASS
		case bsSBAS:
			us = ucSBAS
		case bsGalileo:
			us = ucGalileo
		case bsBeiDou:
			us = ucBeiDou
		case bsQZSS:
			us = ucQZSS
		case bsNavIC:
			us = ucNavIC
		default:
			continue
		}
		if g, s, ok := satID(us, uint8(prn)); ok {
			out[[2]uint8{g, s}] = true
		}
	}
	return out, nil
}

// MarkUsed sets Used on the satellites BESTSAT lists, and on all their signals:
// BESTSAT's mask is per band family (L1/L2/L5, B1/B2/B3) and not worth
// splitting across the u-blox signal ids.
func MarkUsed(sats []Sat, sigs []Signal, used UsedSet) {
	for i := range sats {
		sats[i].Used = used[[2]uint8{sats[i].GNSSID, sats[i].SvID}]
	}
	for i := range sigs {
		sigs[i].Used = used[[2]uint8{sigs[i].GNSSID, sigs[i].SvID}]
	}
}

// BestNav is the part of BESTNAVB the dashboard and time sync use.
type BestNav struct {
	Header  UnicoreHeader
	FixType uint8 // u-blox fix-type code for FixName
	NumSV   uint8 // satellites used in the solution
}

// Position types (manual table "Position or Velocity Type").
const (
	posNone       = 0
	posFixedPos   = 1
	posFixedAlt   = 2
	solComputed   = 0
	bestNavMinLen = 66
)

// ParseBestNav decodes a whole BESTNAVB frame (ID 2118): solution status u32 @0,
// position type u32 @4, satellites tracked u8 @64, satellites used u8 @65
// (verified against the ASCII twin; the station's coordinates are not read).
//
// The position type becomes a u-blox fix type: a solution that is not
// SOL_COMPUTED or has type NONE is 0 (no fix); FIXEDPOS is 5, "time only (fixed
// base)", which is what a surveyed base reports on the X20P; FIXEDHEIGHT is 2
// (2D); every other type (SINGLE, PSRDIFF, SBAS, float, integer, INS, PPP) is
// 3, a 3D fix. UBX has no finer grading and the dashboard only prints FixName.
func ParseBestNav(frame []byte) (BestNav, error) {
	h, p, err := SplitUnicore(frame)
	if err != nil {
		return BestNav{}, err
	}
	if h.MsgID != UnicoreBestNav {
		return BestNav{}, fmt.Errorf("message %d is not BESTNAV", h.MsgID)
	}
	if len(p) < bestNavMinLen {
		return BestNav{}, fmt.Errorf("BESTNAV data too short: %d bytes", len(p))
	}
	status := binary.LittleEndian.Uint32(p[0:4])
	pos := binary.LittleEndian.Uint32(p[4:8])
	fix := uint8(3)
	switch {
	case status != solComputed || pos == posNone:
		fix = 0
	case pos == posFixedPos:
		fix = 5
	case pos == posFixedAlt:
		fix = 2
	}
	return BestNav{Header: h, FixType: fix, NumSV: p[65]}, nil
}
