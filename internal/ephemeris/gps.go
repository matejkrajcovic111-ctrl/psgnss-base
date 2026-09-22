package ephemeris

import (
	"errors"
	"time"
)

// GPS LNAV, IS-GPS-200. A data set spans subframes 1, 2 and 3; each arrives as
// ten 30-bit words, of which u-blox hands over 24 data bits per word once the
// six parity bits are stripped. The three subframes are packed back into a
// 240-bit buffer so the bit positions below are the ones in the interface
// control document, with the 48-bit telemetry and handover words ahead of them.
const (
	gpsSubframeBits  = 240
	gpsSubframeBytes = gpsSubframeBits / 8
	gpsDataStart     = 48 // after TLM (24) and HOW (24)
)

// gpsEphemeris holds the broadcast fields as the integers they are transmitted
// as. Nothing is scaled: RTCM 1019 uses the same units.
type gpsEphemeris struct {
	sv               int
	week             uint64 // 10-bit, modulo 1024, exactly as broadcast
	ura, health      uint64
	codeL2, l2PFlag  uint64
	iodc, iode2, sv3 uint64
	toc, toe         uint64
	af0, af1, af2    int64
	tgd              int64
	crs, crc         int64
	cuc, cus         int64
	cic, cis         int64
	deltaN           int64
	m0, e, sqrtA     int64
	omega0, omega    int64
	i0, idot, omegaD int64
	fitInterval      uint64
	updated          time.Time
}

// complete reports whether all three subframes of one data set have arrived.
// The issue-of-data fields tie them together: subframes 2 and 3 each carry
// IODE, and the low eight bits of subframe 1's IODC must match both. A set
// assembled across an ephemeris change would be a plausible-looking satellite
// in the wrong place, which is worse than none.
func (e *gpsEphemeris) complete(have [4]bool) bool {
	return have[1] && have[2] && have[3] &&
		e.iode2 == e.sv3 && e.iodc&0xFF == e.iode2
}

type gpsAssembly struct {
	eph  gpsEphemeris
	have [4]bool
}

// addSubframe folds one subframe into the assembly and reports its number.
func (a *gpsAssembly) addSubframe(words []uint32) (int, error) {
	if len(words) < 10 {
		return 0, errors.New("gps subframe needs ten words")
	}
	buf := make([]byte, gpsSubframeBytes)
	for i := 0; i < 10; i++ {
		v := words[i] & 0xFFFFFF
		buf[i*3], buf[i*3+1], buf[i*3+2] = byte(v>>16), byte(v>>8), byte(v)
	}
	r := bitReader{buf}
	id := int(r.u(24+19, 3)) // subframe ID, in the handover word
	e := &a.eph
	switch id {
	case 1:
		i := gpsDataStart
		e.week = r.u(i, 10)
		e.codeL2 = r.u(i+10, 2)
		e.ura = r.u(i+12, 4)
		e.health = r.u(i+16, 6)
		iodcHigh := r.u(i+22, 2)
		e.l2PFlag = r.u(i+24, 1)
		// 87 reserved bits follow the L2 P data flag.
		j := i + 25 + 87
		e.tgd = r.s(j, 8)
		e.iodc = iodcHigh<<8 | r.u(j+8, 8)
		e.toc = r.u(j+16, 16)
		e.af2 = r.s(j+32, 8)
		e.af1 = r.s(j+40, 16)
		e.af0 = r.s(j+56, 22)
	case 2:
		i := gpsDataStart
		e.iode2 = r.u(i, 8)
		e.crs = r.s(i+8, 16)
		e.deltaN = r.s(i+24, 16)
		e.m0 = r.s(i+40, 32)
		e.cuc = r.s(i+72, 16)
		e.e = int64(r.u(i+88, 32))
		e.cus = r.s(i+120, 16)
		e.sqrtA = int64(r.u(i+136, 32))
		e.toe = r.u(i+168, 16)
		e.fitInterval = r.u(i+184, 1)
	case 3:
		i := gpsDataStart
		e.cic = r.s(i, 16)
		e.omega0 = r.s(i+16, 32)
		e.cis = r.s(i+48, 16)
		e.i0 = r.s(i+64, 32)
		e.crc = r.s(i+96, 16)
		e.omega = r.s(i+112, 32)
		e.omegaD = r.s(i+144, 24)
		e.sv3 = r.u(i+168, 8)
		e.idot = r.s(i+176, 14)
	default:
		return id, nil // 4 and 5 are almanac and ionosphere: not ephemeris
	}
	if id >= 1 && id <= 3 {
		a.have[id] = true
		e.updated = time.Now()
	}
	return id, nil
}

// writer is the subset of rtcm.Writer this package needs.
type writer interface {
	Put(v uint64, bits int)
	PutSigned(v int64, bits int)
}

// encode1019 writes the GPS ephemeris message. The field order and widths are
// RTCM 10403, and every value is the broadcast integer unchanged.
func (e *gpsEphemeris) encode1019(w writer) {
	w.Put(1019, 12)
	w.Put(uint64(e.sv), 6)
	w.Put(e.week, 10)
	w.Put(e.ura, 4)
	w.Put(e.codeL2, 2)
	w.PutSigned(e.idot, 14)
	w.Put(e.iode2, 8)
	w.Put(e.toc, 16)
	w.PutSigned(e.af2, 8)
	w.PutSigned(e.af1, 16)
	w.PutSigned(e.af0, 22)
	w.Put(e.iodc, 10)
	w.PutSigned(e.crs, 16)
	w.PutSigned(e.deltaN, 16)
	w.PutSigned(e.m0, 32)
	w.PutSigned(e.cuc, 16)
	w.Put(uint64(e.e), 32)
	w.PutSigned(e.cus, 16)
	w.Put(uint64(e.sqrtA), 32)
	w.Put(e.toe, 16)
	w.PutSigned(e.cic, 16)
	w.PutSigned(e.omega0, 32)
	w.PutSigned(e.cis, 16)
	w.PutSigned(e.i0, 32)
	w.PutSigned(e.crc, 16)
	w.PutSigned(e.omega, 32)
	w.PutSigned(e.omegaD, 24)
	w.PutSigned(e.tgd, 8)
	w.Put(e.health, 6)
	w.Put(e.l2PFlag, 1)
	w.Put(e.fitInterval, 1)
}
