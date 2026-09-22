package ephemeris

import (
	"errors"
	"time"
)

// BeiDou D1 NAV, per BDS-SIS-ICD-B1I. A data set spans subframes 1, 2 and 3,
// tied together by AODE/AODC.
//
// The information bits are recovered first and every field is then read from
// that stream, which makes the parameters the ICD describes as split across
// words -- M0 as 20 bits then 12, sqrt(A) as 12 then 20 -- simply contiguous.
// Only t_oe is genuinely split, across two subframes.
//
// Two things here were settled by measurement rather than by reading, because
// getting either wrong produces a plausible orbit instead of an error:
//
//   - The receiver hands over each 30-bit word already de-interleaved, with the
//     22 information bits at the top and the BCH parity below. Decoding the
//     interleaved BCH blocks by hand, which the ICD's wording suggests, yields
//     orbits of the wrong size.
//   - In subframe 2 the order is eccentricity, Cus, Crc, Crs -- not the
//     Cus, Crc, Crs, eccentricity that the word-by-word table reads like.
//
// Both were found by decoding this station's own capture and comparing every
// field with RTKLIB's convbin over the same bytes.
//
// D2 NAV, which the geostationary satellites broadcast, has a different frame
// layout and is not decoded: RTCM 1042 carries D1.
const (
	bdsInfoBits = 224 // 26 bits from word 1, 22 from each of words 2..10
	bdsPreamble = 0x712
)

type beidouEphemeris struct {
	sv                 int
	week               uint64 // BDT week, 13 bits
	urai               uint64
	satH1              uint64
	aode, aodc         uint64
	toc, toe           uint64
	a0, a1, a2         int64
	tgd1, tgd2         int64
	deltaN             int64
	m0, e, sqrtA       int64
	cuc, cus, crc, crs int64
	cic, cis           int64
	i0, idot           int64
	omega0, omega      int64
	omegaD             int64
	updated            time.Time
}

type beidouAssembly struct {
	eph  beidouEphemeris
	have [4]bool
	// toe arrives in three pieces across subframes 2 and 3; keeping the parts
	// separate means a set assembled across an update cannot silently mix them.
	toeHi uint64
}

// complete requires all three subframes. AODE and AODC are the issue-of-data
// fields, and a set whose clock and orbit come from different updates is a
// satellite in the wrong place rather than a missing one.
func (a *beidouAssembly) complete() bool {
	return a.have[1] && a.have[2] && a.have[3]
}

// bdsWordBits recovers the information bits of one D1 subframe: 26 from word 1
// and 22 from each of words 2 to 10, concatenated.
//
// Word 1 carries 11 preamble bits, 4 reserved and then the frame ID and the
// first part of the seconds-of-week, with 4 parity bits at the end. Words 2 to
// 10 carry 22 information bits followed by 8 of parity. The receiver has
// already undone the BCH interleaving, so nothing here needs to.
func bdsWordBits(words []uint32) ([]byte, error) {
	if len(words) < 10 {
		return nil, errors.New("beidou subframe needs ten words")
	}
	out := make([]byte, (bdsInfoBits+7)/8)
	put := bitWriterTo(out)
	first := words[0] & 0x3FFFFFFF
	for i := 29; i >= 4; i-- {
		put(uint64(first>>uint(i)) & 1)
	}
	for w := 1; w < 10; w++ {
		v := words[w] & 0x3FFFFFFF
		for i := 29; i >= 8; i-- {
			put(uint64(v>>uint(i)) & 1)
		}
	}
	return out, nil
}

// join puts a split parameter back together.
func join(hi uint64, loBits int, lo uint64) uint64 { return hi<<uint(loBits) | lo }

// addSubframe folds one D1 subframe into the assembly and reports its frame ID.
func (a *beidouAssembly) addSubframe(words []uint32) (int, error) {
	buf, err := bdsWordBits(words)
	if err != nil {
		return 0, err
	}
	r := bitReader{buf}
	if r.u(0, 11) != bdsPreamble {
		return 0, errors.New("beidou subframe preamble missing")
	}
	id := int(r.u(15, 3))
	e := &a.eph
	switch id {
	case 1:
		e.satH1 = r.u(38, 1)
		e.aodc = r.u(39, 5)
		e.urai = r.u(44, 4)
		e.week = r.u(48, 13)
		e.toc = r.u(61, 17)
		e.tgd1 = r.s(78, 10)
		e.tgd2 = r.s(88, 10)
		// 64 bits of ionospheric coefficients sit between the group delays and
		// the clock terms; they belong to RTCM 1021, not to an ephemeris.
		e.a2 = r.s(162, 11)
		e.a0 = r.s(173, 24)
		e.a1 = r.s(197, 22)
		e.aode = r.u(219, 5)
	case 2:
		e.deltaN = r.s(38, 16)
		e.cuc = r.s(54, 18)
		e.m0 = r.s(72, 32)
		e.e = int64(r.u(104, 32))
		e.cus = r.s(136, 18)
		e.crc = r.s(154, 18)
		e.crs = r.s(172, 18)
		e.sqrtA = int64(r.u(190, 32))
		a.toeHi = r.u(222, 2)
	case 3:
		// t_oe is the one parameter that really is split: two bits at the end
		// of subframe 2 and fifteen at the start of subframe 3.
		e.toe = join(a.toeHi, 15, r.u(38, 15))
		e.i0 = r.s(53, 32)
		e.cic = r.s(85, 18)
		e.omegaD = r.s(103, 24)
		e.cis = r.s(127, 18)
		e.idot = r.s(145, 14)
		e.omega0 = r.s(159, 32)
		e.omega = r.s(191, 32)
	default:
		return id, nil // 4 and 5 are almanac
	}
	if id >= 1 && id <= 3 {
		a.have[id] = true
		e.updated = time.Now()
	}
	return id, nil
}

// encode1042 writes the BeiDou ephemeris message: 511 bits, every value the
// broadcast integer unchanged.
func (e *beidouEphemeris) encode1042(w writer) {
	w.Put(1042, 12)
	w.Put(uint64(e.sv), 6)
	w.Put(e.week, 13)
	w.Put(e.urai, 4)
	w.PutSigned(e.idot, 14)
	w.Put(e.aode, 5)
	w.Put(e.toc, 17)
	w.PutSigned(e.a2, 11)
	w.PutSigned(e.a1, 22)
	w.PutSigned(e.a0, 24)
	w.Put(e.aodc, 5)
	w.PutSigned(e.crs, 18)
	w.PutSigned(e.deltaN, 16)
	w.PutSigned(e.m0, 32)
	w.PutSigned(e.cuc, 18)
	w.Put(uint64(e.e), 32)
	w.PutSigned(e.cus, 18)
	w.Put(uint64(e.sqrtA), 32)
	w.Put(e.toe, 17)
	w.PutSigned(e.cic, 18)
	w.PutSigned(e.omega0, 32)
	w.PutSigned(e.cis, 18)
	w.PutSigned(e.i0, 32)
	w.PutSigned(e.crc, 18)
	w.PutSigned(e.omega, 32)
	w.PutSigned(e.omegaD, 24)
	w.PutSigned(e.tgd1, 10)
	w.PutSigned(e.tgd2, 10)
	w.Put(e.satH1, 1)
}
