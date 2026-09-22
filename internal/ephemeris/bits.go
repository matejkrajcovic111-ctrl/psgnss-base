// Package ephemeris turns the receiver's broadcast navigation subframes into
// RTCM ephemeris messages.
//
// Why this exists: this receiver family emits no ephemeris RTCM at all -- its
// configuration key set has no 1019, 1020, 1042, 1045 or 1046 -- so the caster's
// stream carries observations and a station position and nothing a rover could
// use to place a satellite in orbit. Most rovers decode their own ephemeris from
// the sky and never notice. One that cannot, because it is indoors on a repeater
// or has just been switched on, is stuck. RTKBase avoids the problem by running
// the raw stream through str2str, which re-encodes everything; this project does
// not transcode observations, by a decision recorded in CLAUDE.md, so only the
// navigation data is synthesised here and the observations still pass through
// untouched.
//
// The broadcast subframes and the RTCM message carry the *same* integer fields
// with the *same* scale factors, so nothing here converts to floating point and
// back. The values are moved as raw integers, which removes an entire class of
// scaling error.
package ephemeris

// bitReader reads big-endian bit fields, the way every GNSS interface control
// document numbers them.
type bitReader struct{ b []byte }

// u reads an unsigned field of n bits starting at bit pos.
func (r bitReader) u(pos, n int) uint64 {
	var v uint64
	for i := pos; i < pos+n; i++ {
		if i/8 >= len(r.b) {
			return v << uint(pos+n-i) // past the end: zero-fill rather than panic
		}
		v = v<<1 | uint64(r.b[i/8]>>(7-uint(i%8))&1)
	}
	return v
}

// s reads a two's-complement signed field of n bits.
func (r bitReader) s(pos, n int) int64 {
	v := r.u(pos, n)
	if n < 64 && v&(1<<uint(n-1)) != 0 {
		return int64(v) - 1<<uint(n)
	}
	return int64(v)
}
