// Package rawdata decodes the RTCM stream for the dashboard's live raw data
// view, the equivalent of RTKNAVI's RTK Monitor: per-satellite observations
// from MSM7 and the broadcast ephemeris from 1019, 1020, 1042, 1044, 1045 and
// 1046. It only reads; nothing here reaches the caster or the archives.
//
// Bit layouts follow RTCM 10403 and RTKLIB's rtcm3.c. The decoded values were
// compared against convbin's RINEX output for the same stream.
package rawdata

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

const clight = 299792458.0

// leapSeconds is GPS minus UTC, which is only used to pick the right day or
// week for an MSM epoch; the time of week itself comes from the message.
const leapSeconds = 18

// bits reads big-endian bit fields from an RTCM payload.
type bits []byte

func (b bits) u(pos, n int) uint64 {
	var v uint64
	for i := pos; i < pos+n; i++ {
		v = v<<1 | uint64(b[i/8]>>(7-uint(i%8))&1)
	}
	return v
}

func (b bits) s(pos, n int) int64 {
	v := b.u(pos, n)
	if v>>(n-1)&1 == 1 {
		return int64(v) - 1<<n
	}
	return int64(v)
}

// g reads a sign-magnitude field, as GLONASS 1020 uses.
func (b bits) g(pos, n int) int64 {
	v := int64(b.u(pos+1, n-1))
	if b.u(pos, 1) == 1 {
		return -v
	}
	return v
}

// Signal is one tracked signal of a satellite. Field names are short because
// this travels once a second for every signal in the sky.
type Signal struct {
	Code string  `json:"c"`
	CN0  float64 `json:"s"`
	P    float64 `json:"p"`
	L    float64 `json:"l"`
	D    float64 `json:"d"`
	// LLI is RINEX's loss-of-lock indicator: bit 0 a slip since the last
	// epoch, bit 1 half-cycle ambiguity unresolved.
	LLI uint8 `json:"i"`
	// freq sorts a satellite's signals so columns line up across satellites.
	freq float64
}

// Sat is one satellite's observations at one epoch.
type Sat struct {
	Sat     string   `json:"sat"`
	System  string   `json:"sys"`
	Time    string   `json:"t"`
	Signals []Signal `json:"sig"`
	seen    time.Time
}

type msmSystem struct {
	name   string
	letter string
	offset int // first PRN minus one
	codes  map[int]string
}

// The signal tables of RTCM 10403 table 3.5-91 onward, as RINEX codes.
var msmSystems = map[int]msmSystem{
	107: {"GPS", "G", 0, map[int]string{2: "1C", 3: "1P", 4: "1W", 8: "2C", 9: "2P", 10: "2W", 15: "2S", 16: "2L", 17: "2X",
		22: "5I", 23: "5Q", 24: "5X", 30: "1S", 31: "1L", 32: "1X"}},
	108: {"GLONASS", "R", 0, map[int]string{2: "1C", 3: "1P", 8: "2C", 9: "2P"}},
	109: {"Galileo", "E", 0, map[int]string{2: "1C", 3: "1A", 4: "1B", 5: "1X", 6: "1Z", 8: "6C", 9: "6A", 10: "6B", 11: "6X",
		12: "6Z", 14: "7I", 15: "7Q", 16: "7X", 18: "8I", 19: "8Q", 20: "8X", 22: "5I", 23: "5Q", 24: "5X"}},
	110: {"SBAS", "S", 19, map[int]string{2: "1C", 22: "5I", 23: "5Q", 24: "5X"}},
	111: {"QZSS", "J", 0, map[int]string{2: "1C", 9: "6S", 10: "6L", 11: "6X", 15: "2S", 16: "2L", 17: "2X",
		22: "5I", 23: "5Q", 24: "5X", 30: "1S", 31: "1L", 32: "1X"}},
	112: {"BeiDou", "C", 0, map[int]string{2: "2I", 3: "2Q", 4: "2X", 8: "6I", 9: "6Q", 10: "6X", 14: "7I", 15: "7Q", 16: "7X",
		22: "5D", 23: "5P", 24: "5X", 25: "7D", 30: "1D", 31: "1P", 32: "1X"}},
	113: {"NavIC", "I", 0, map[int]string{22: "5A"}},
}

// frequency is the carrier in Hz, or 0 for GLONASS without a known channel.
func frequency(system, code string, fcn int, haveFCN bool) float64 {
	band := code[0]
	switch system {
	case "GLONASS":
		if !haveFCN {
			return 0
		}
		if band == '1' {
			return 1602e6 + float64(fcn)*562.5e3
		}
		return 1246e6 + float64(fcn)*437.5e3
	case "BeiDou":
		switch band {
		case '1':
			return 1575.42e6
		case '2':
			return 1561.098e6
		case '5':
			return 1176.45e6
		case '6':
			return 1268.52e6
		case '7':
			return 1207.14e6
		}
	default:
		switch band {
		case '1':
			return 1575.42e6
		case '2':
			return 1227.60e6
		case '5':
			return 1176.45e6
		case '6':
			return 1278.75e6
		case '7':
			return 1207.14e6
		case '8':
			return 1191.795e6
		}
	}
	return 0
}

// epochTime turns a message's time of week into GPS time, taking the week or
// day from the clock.
func epochTime(system string, tow uint64, now time.Time) time.Time {
	gpst := now.UTC().Add(leapSeconds * time.Second)
	if system == "GLONASS" { // tow here is the time of day in Moscow
		ms := tow & (1<<27 - 1) // the top three bits are the day of week
		day := gpst.Truncate(24 * time.Hour)
		t := day.Add(time.Duration(ms)*time.Millisecond - 3*time.Hour + leapSeconds*time.Second)
		for t.Sub(gpst) > 12*time.Hour {
			t = t.Add(-24 * time.Hour)
		}
		for gpst.Sub(t) > 12*time.Hour {
			t = t.Add(24 * time.Hour)
		}
		return t
	}
	if system == "BeiDou" {
		tow += 14000
	}
	// GPS weeks start on Sunday. Truncate counts from year 1, which began on a
	// Monday, so a week boundary is a Monday and the shift moves it back a day.
	const week = 7 * 24 * time.Hour
	sunday := gpst.Add(24 * time.Hour).Truncate(week).Add(-24 * time.Hour)
	t := sunday.Add(time.Duration(tow) * time.Millisecond)
	if t.Sub(gpst) > week/2 {
		t = t.Add(-week)
	} else if gpst.Sub(t) > week/2 {
		t = t.Add(week)
	}
	return t
}

// decodeMSM7 decodes an RTCM payload (no frame) of an MSM7 message. lock holds
// the previous lock indicator per satellite and signal, for slip detection.
func decodeMSM7(p bits, now time.Time, lock map[string]uint64) ([]Sat, error) {
	if len(p) < 24 {
		return nil, errors.New("MSM7 too short")
	}
	typ := int(p.u(0, 12))
	if typ%10 != 7 {
		return nil, fmt.Errorf("message %d is not MSM7", typ)
	}
	sys, ok := msmSystems[typ/10]
	if !ok {
		return nil, fmt.Errorf("message %d has no known constellation", typ)
	}
	tow := p.u(24, 30)
	pos := 73
	var sats, sigs []int
	for i := 0; i < 64; i++ {
		if p.u(pos+i, 1) == 1 {
			sats = append(sats, i+1)
		}
	}
	pos += 64
	for i := 0; i < 32; i++ {
		if p.u(pos+i, 1) == 1 {
			sigs = append(sigs, i+1)
		}
	}
	pos += 32
	ncell := len(sats) * len(sigs)
	if ncell > 64 {
		return nil, errors.New("MSM7 cell mask over 64")
	}
	if len(p)*8 < pos+ncell {
		return nil, errors.New("MSM7 truncated")
	}
	cells := make([]bool, ncell)
	n := 0
	for i := range cells {
		cells[i] = p.u(pos+i, 1) == 1
		if cells[i] {
			n++
		}
	}
	pos += ncell
	if need := pos + len(sats)*36 + n*80; len(p)*8 < need {
		return nil, errors.New("MSM7 truncated")
	}
	ns := len(sats)
	rough := make([]float64, ns)
	fcn := make([]int, ns)
	haveFCN := make([]bool, ns)
	rate := make([]float64, ns)
	rateOK := make([]bool, ns)
	for i := 0; i < ns; i++ {
		v := p.u(pos+i*8, 8)
		if v == 255 {
			rough[i] = -1
		} else {
			rough[i] = float64(v)
		}
	}
	pos += ns * 8
	for i := 0; i < ns; i++ {
		if v := p.u(pos+i*4, 4); v <= 13 {
			fcn[i], haveFCN[i] = int(v)-7, sys.name == "GLONASS"
		}
	}
	pos += ns * 4
	for i := 0; i < ns; i++ {
		if rough[i] >= 0 {
			rough[i] += float64(p.u(pos+i*10, 10)) / 1024
		}
	}
	pos += ns * 10
	for i := 0; i < ns; i++ {
		v := p.s(pos+i*14, 14)
		rate[i], rateOK[i] = float64(v), v != -1<<13
	}
	pos += ns * 14
	field := func(start, width, index int) int { return start + width*index }
	prPos, cpPos := pos, pos+n*20
	lockPos, halfPos := cpPos+n*24, cpPos+n*34
	cnrPos, ratePos := cpPos+n*35, cpPos+n*45

	label := epochTime(sys.name, tow, now).Format("2006/01/02 15:04:05.000")
	out := make([]Sat, 0, ns)
	cell, idx := 0, 0
	for i, prn := range sats {
		sat := Sat{Sat: fmt.Sprintf("%s%02d", sys.letter, prn+sys.offset), System: sys.name, Time: label, seen: now}
		for _, sg := range sigs {
			on := cells[cell]
			cell++
			if !on {
				continue
			}
			j := idx
			idx++
			code, known := sys.codes[sg]
			if !known {
				continue
			}
			f := frequency(sys.name, code, fcn[i], haveFCN[i])
			s := Signal{Code: code, freq: f}
			s.CN0 = float64(p.u(field(cnrPos, 10, j), 10)) / 16
			if rough[i] >= 0 {
				if pr := p.s(field(prPos, 20, j), 20); pr != -1<<19 {
					s.P = (rough[i] + float64(pr)*math.Pow(2, -29)) * clight / 1000
				}
				if cp := p.s(field(cpPos, 24, j), 24); cp != -1<<23 && f > 0 {
					s.L = (rough[i] + float64(cp)*math.Pow(2, -31)) * clight / 1000 * f / clight
				}
			}
			if rr := p.s(field(ratePos, 15, j), 15); rr != -1<<14 && rateOK[i] && f > 0 {
				s.D = -(rate[i] + float64(rr)*1e-4) * f / clight
			}
			lk := p.u(field(lockPos, 10, j), 10)
			key := sat.Sat + code
			if prev, seen := lock[key]; seen && lk < prev {
				s.LLI |= 1
			}
			lock[key] = lk
			if p.u(field(halfPos, 1, j), 1) == 1 {
				s.LLI |= 2
			}
			sat.Signals = append(sat.Signals, s)
		}
		sort.SliceStable(sat.Signals, func(a, b int) bool { return sat.Signals[a].freq > sat.Signals[b].freq })
		if len(sat.Signals) > 0 {
			out = append(out, sat)
		}
	}
	return out, nil
}
