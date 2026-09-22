package ephemeris

import (
	"errors"
	"time"
)

// Galileo I/NAV, per the OS SIS ICD. A data set spans word types 1 to 4, tied
// together by IODnav, plus word type 5 for the week number, the broadcast group
// delay and the health flags.
//
// u-blox delivers an I/NAV page as eight dwords: four for the even page part
// and four for the odd. The transmitted word is the two halves joined --- 112
// data bits from the even part and 16 from the odd --- which is why nothing
// here reads the page parts directly. Every field offset below is then the
// offset in the ICD's 128-bit word, and can be checked against the document
// without tracking u-blox's framing at the same time.
const (
	galPageBits  = 128
	galPageBytes = galPageBits / 8
)

type galileoEphemeris struct {
	sv                 int
	iodNav             uint64
	week               uint64 // from word type 5
	sisa               uint64
	toe, toc           uint64
	af0, af1, af2      int64
	m0, e, sqrtA       int64
	omega0, i0, omega  int64
	idot, omegaD       int64
	deltaN             int64
	cuc, cus, crc, crs int64
	cic, cis           int64
	bgdE1E5b           int64
	e5bHS, e1bHS       uint64
	e5bDVS, e1bDVS     uint64
	updated            time.Time
}

// iodOf records which IODnav each ephemeris word carried, so a set assembled
// across an ephemeris change is rejected rather than broadcast. Galileo changes
// IODnav on every update, and half of one data set with half of the next is a
// satellite in a place it has never been.
type galileoAssembly struct {
	eph  galileoEphemeris
	have [6]bool
	iod  [5]uint64 // word types 1..4
}

func (a *galileoAssembly) complete() bool {
	if !(a.have[1] && a.have[2] && a.have[3] && a.have[4] && a.have[5]) {
		return false
	}
	for i := 2; i <= 4; i++ {
		if a.iod[i] != a.iod[1] {
			return false
		}
	}
	return true
}

// inavWord joins the even and odd page parts into the ICD's 128-bit word.
//
// The even part carries the even/odd flag, the page type and 112 data bits; the
// odd part carries its own two flags and the remaining 16. A page whose flags
// are not even-then-odd did not arrive intact, and an alert page (page type 1)
// carries something else entirely under the same word numbering -- both are
// refused here rather than decoded into a plausible orbit.
func inavWord(words []uint32) ([]byte, error) {
	if len(words) < 8 {
		return nil, errors.New("galileo page needs eight words")
	}
	buf := make([]byte, 32)
	for i := 0; i < 8; i++ {
		buf[i*4] = byte(words[i] >> 24)
		buf[i*4+1] = byte(words[i] >> 16)
		buf[i*4+2] = byte(words[i] >> 8)
		buf[i*4+3] = byte(words[i])
	}
	r := bitReader{buf}
	if r.u(0, 1) != 0 || r.u(1, 1) != 0 {
		return nil, errors.New("not an even nominal page part")
	}
	if r.u(128, 1) != 1 || r.u(129, 1) != 0 {
		return nil, errors.New("not an odd nominal page part")
	}
	out := make([]byte, galPageBytes)
	w := bitWriterTo(out)
	for i := 0; i < 112; i++ {
		w(r.u(2+i, 1))
	}
	for i := 0; i < 16; i++ {
		w(r.u(130+i, 1))
	}
	return out, nil
}

// bitWriterTo returns a function that appends single bits to a buffer, which is
// all the page join needs.
func bitWriterTo(b []byte) func(uint64) {
	pos := 0
	return func(v uint64) {
		if pos/8 < len(b) && v != 0 {
			b[pos/8] |= 1 << (7 - uint(pos%8))
		}
		pos++
	}
}

// addPage folds one I/NAV page into the assembly and reports its word type.
func (a *galileoAssembly) addPage(words []uint32) (int, error) {
	buf, err := inavWord(words)
	if err != nil {
		return 0, err
	}
	r := bitReader{buf}
	wt := int(r.u(0, 6))
	e := &a.eph
	switch wt {
	case 1:
		a.iod[1] = r.u(6, 10)
		e.iodNav = a.iod[1]
		e.toe = r.u(16, 14)
		e.m0 = r.s(30, 32)
		e.e = int64(r.u(62, 32))
		e.sqrtA = int64(r.u(94, 32))
	case 2:
		a.iod[2] = r.u(6, 10)
		e.omega0 = r.s(16, 32)
		e.i0 = r.s(48, 32)
		e.omega = r.s(80, 32)
		e.idot = r.s(112, 14)
	case 3:
		a.iod[3] = r.u(6, 10)
		e.omegaD = r.s(16, 24)
		e.deltaN = r.s(40, 16)
		e.cuc = r.s(56, 16)
		e.cus = r.s(72, 16)
		e.crc = r.s(88, 16)
		e.crs = r.s(104, 16)
		e.sisa = r.u(120, 8)
	case 4:
		a.iod[4] = r.u(6, 10)
		// The SVID in word type 4 is the authoritative satellite number for
		// this data set; the frame's own svId is used only to file it.
		if sv := int(r.u(16, 6)); sv > 0 {
			e.sv = sv
		}
		e.cic = r.s(22, 16)
		e.cis = r.s(38, 16)
		e.toc = r.u(54, 14)
		e.af0 = r.s(68, 31)
		e.af1 = r.s(99, 21)
		e.af2 = r.s(120, 6)
	case 5:
		e.bgdE1E5b = r.s(57, 10)
		e.e5bHS = r.u(67, 2)
		e.e1bHS = r.u(69, 2)
		e.e5bDVS = r.u(71, 1)
		e.e1bDVS = r.u(72, 1)
		e.week = r.u(73, 12)
	default:
		return wt, nil // almanac and the rest
	}
	if wt >= 1 && wt <= 5 {
		a.have[wt] = true
		e.updated = time.Now()
	}
	return wt, nil
}

// encode1046 writes the Galileo I/NAV ephemeris message: 492 bits, every value
// the broadcast integer unchanged.
func (e *galileoEphemeris) encode1046(w writer) {
	w.Put(1046, 12)
	w.Put(uint64(e.sv), 6)
	w.Put(e.week, 12)
	w.Put(e.iodNav, 10)
	w.Put(e.sisa, 8)
	w.PutSigned(e.idot, 14)
	w.Put(e.toc, 14)
	w.PutSigned(e.af2, 6)
	w.PutSigned(e.af1, 21)
	w.PutSigned(e.af0, 31)
	w.PutSigned(e.crs, 16)
	w.PutSigned(e.deltaN, 16)
	w.PutSigned(e.m0, 32)
	w.PutSigned(e.cuc, 16)
	w.Put(uint64(e.e), 32)
	w.PutSigned(e.cus, 16)
	w.Put(uint64(e.sqrtA), 32)
	w.Put(e.toe, 14)
	w.PutSigned(e.cic, 16)
	w.PutSigned(e.omega0, 32)
	w.PutSigned(e.cis, 16)
	w.PutSigned(e.i0, 32)
	w.PutSigned(e.crc, 16)
	w.PutSigned(e.omega, 32)
	w.PutSigned(e.omegaD, 24)
	w.PutSigned(e.bgdE1E5b, 10)
	w.Put(e.e5bHS, 2)
	w.Put(e.e5bDVS, 1)
	w.Put(e1bBits(e.e1bHS), 2)
	w.Put(e.e1bDVS, 1)
}

func e1bBits(v uint64) uint64 { return v & 0x3 }
