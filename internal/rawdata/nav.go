package rawdata

import (
	"errors"
	"fmt"
	"math"
)

// field is one entry of a table-driven ephemeris layout: its name, width, and
// how to read it.
type field struct {
	name   string
	bits   int
	signed bool
	scale  float64
}

func p2(n int) float64 { return math.Pow(2, float64(n)) }

var sc = math.Pi // semicircles to radians

func u(name string, n int, scale float64) field { return field{name, n, false, scale} }
func i(name string, n int, scale float64) field { return field{name, n, true, scale} }

// Every Keplerian message shares the orbit block; only scales differ.
func orbit(c, crScale float64, cBits, crBits int, toeBits int, toeScale float64) []field {
	return []field{
		i("crs", crBits, crScale), i("deln", 16, p2(-43)*sc), i("M0", 32, p2(-31)*sc),
		i("cuc", cBits, c), u("e", 32, p2(-33)), i("cus", cBits, c), u("sqrtA", 32, p2(-19)),
		u("toe", toeBits, toeScale), i("cic", cBits, c), i("OMG0", 32, p2(-31)*sc),
		i("cis", cBits, c), i("i0", 32, p2(-31)*sc), i("crc", crBits, crScale),
		i("omg", 32, p2(-31)*sc), i("OMGd", 24, p2(-43)*sc),
	}
}

func join(parts ...[]field) []field {
	var out []field
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var layouts = map[int][]field{
	1019: join([]field{u("prn", 6, 1), u("week", 10, 1), u("ura", 4, 1), u("code", 2, 1), i("idot", 14, p2(-43)*sc),
		u("iode", 8, 1), u("toc", 16, 16), i("af2", 8, p2(-55)), i("af1", 16, p2(-43)), i("af0", 22, p2(-31)), u("iodc", 10, 1)},
		orbit(p2(-29), p2(-5), 16, 16, 16, 16),
		[]field{i("tgd", 8, p2(-31)), u("health", 6, 1)}),
	1042: join([]field{u("prn", 6, 1), u("week", 13, 1), u("ura", 4, 1), i("idot", 14, p2(-43)*sc), u("iode", 5, 1),
		u("toc", 17, 8), i("af2", 11, p2(-66)), i("af1", 22, p2(-50)), i("af0", 24, p2(-33)), u("iodc", 5, 1)},
		orbit(p2(-31), p2(-6), 18, 18, 17, 8),
		[]field{i("tgd", 10, 1e-10), i("tgd2", 10, 1e-10), u("health", 1, 1)}),
	1044: join([]field{u("prn", 4, 1), u("toc", 16, 16), i("af2", 8, p2(-55)), i("af1", 16, p2(-43)), i("af0", 22, p2(-31)), u("iode", 8, 1)},
		orbit(p2(-29), p2(-5), 16, 16, 16, 16),
		[]field{i("idot", 14, p2(-43)*sc), u("code", 2, 1), u("week", 10, 1), u("ura", 4, 1), u("health", 6, 1), i("tgd", 8, p2(-31)), u("iodc", 10, 1)}),
	1045: join([]field{u("prn", 6, 1), u("week", 12, 1), u("iode", 10, 1), u("ura", 8, 1), i("idot", 14, p2(-43)*sc),
		u("toc", 14, 60), i("af2", 6, p2(-59)), i("af1", 21, p2(-46)), i("af0", 31, p2(-34))},
		orbit(p2(-29), p2(-5), 16, 16, 14, 60),
		[]field{i("tgd", 10, p2(-32)), u("health", 2, 1), u("dvs", 1, 1)}),
	1046: join([]field{u("prn", 6, 1), u("week", 12, 1), u("iode", 10, 1), u("ura", 8, 1), i("idot", 14, p2(-43)*sc),
		u("toc", 14, 60), i("af2", 6, p2(-59)), i("af1", 21, p2(-46)), i("af0", 31, p2(-34))},
		orbit(p2(-29), p2(-5), 16, 16, 14, 60),
		[]field{i("tgd", 10, p2(-32)), i("tgd2", 10, p2(-32)), u("h5b", 2, 1), u("d5b", 1, 1), u("health", 2, 1), u("dvs", 1, 1)}),
}

func read(p bits, layout []field) (map[string]float64, error) {
	pos, width := 12, 0
	for _, f := range layout {
		width += f.bits
	}
	if len(p)*8 < pos+width {
		return nil, errors.New("ephemeris message truncated")
	}
	out := make(map[string]float64, len(layout))
	for _, f := range layout {
		var v float64
		if f.signed {
			v = float64(p.s(pos, f.bits))
		} else {
			v = float64(p.u(pos, f.bits))
		}
		out[f.name] = v * f.scale
		pos += f.bits
	}
	return out, nil
}

// gpsRollover restores the epoch 1019 and 1044 drop: their week is modulo 1024,
// and weeks 2048 to 3071 run from 2019 to 2038.
const gpsRollover = 2048

// navView says which messages fill a table and how to print a row.
type navView struct {
	Name    string
	Columns []string
}

var keplerColumns = []string{"SAT", "Health", "IODE", "IODC", "URA", "Week", "Toe (s)", "Toc (s)", "A (m)", "e", "i0 (rad)",
	"OMEGA0 (rad)", "omega (rad)", "M0 (rad)", "deltaN (rad/s)", "OMEGAdot (rad/s)", "IDOT (rad/s)",
	"Crc (m)", "Crs (m)", "Cuc (rad)", "Cus (rad)", "Cic (rad)", "Cis (rad)", "af0 (s)", "af1 (s/s)", "af2 (s/s2)", "TGD (s)"}

// navViews is the Nav menu, in dashboard order.
var navViews = []navView{
	{"GPS", keplerColumns}, {"Galileo", keplerColumns}, {"BeiDou", keplerColumns},
	{"GLONASS", []string{"SAT", "Health", "Channel", "tb (min)", "X (m)", "Y (m)", "Z (m)", "VX (m/s)", "VY (m/s)", "VZ (m/s)",
		"AX (m/s2)", "AY (m/s2)", "AZ (m/s2)", "TauN (s)", "GammaN"}},
	{"QZSS", keplerColumns},
}

// decodeNav returns the table, satellite and row an ephemeris payload belongs in.
func decodeNav(p bits) (view, sat string, row []string, err error) {
	typ := int(p.u(0, 12))
	if typ == 1020 {
		return decodeGLONASS(p)
	}
	layout, ok := layouts[typ]
	if !ok {
		return "", "", nil, fmt.Errorf("message %d is not an ephemeris", typ)
	}
	m, err := read(p, layout)
	if err != nil {
		return "", "", nil, err
	}
	week := m["week"]
	switch typ {
	case 1019:
		view, sat = "GPS", fmt.Sprintf("G%02.0f", m["prn"])
		week += gpsRollover
	case 1042:
		view, sat = "BeiDou", fmt.Sprintf("C%02.0f", m["prn"])
	case 1044:
		view, sat = "QZSS", fmt.Sprintf("J%02.0f", m["prn"])
		week += gpsRollover
	default:
		view, sat = "Galileo", fmt.Sprintf("E%02.0f", m["prn"])
		if typ == 1045 {
			sat += " F"
		}
	}
	f := func(k string) string { return fmt.Sprintf("%.6e", m[k]) }
	n := func(k string) string { return fmt.Sprintf("%.0f", m[k]) }
	a := m["sqrtA"] * m["sqrtA"]
	return view, sat, []string{sat, n("health"), n("iode"), n("iodc"), n("ura"), fmt.Sprintf("%.0f", week), n("toe"), n("toc"),
		fmt.Sprintf("%.3f", a), fmt.Sprintf("%.9f", m["e"]), fmt.Sprintf("%.9f", m["i0"]), fmt.Sprintf("%.9f", m["OMG0"]),
		fmt.Sprintf("%.9f", m["omg"]), fmt.Sprintf("%.9f", m["M0"]), f("deln"), f("OMGd"), f("idot"), fmt.Sprintf("%.6f", m["crc"]), fmt.Sprintf("%.6f", m["crs"]),
		f("cuc"), f("cus"), f("cic"), f("cis"), f("af0"), f("af1"), f("af2"), f("tgd")}, nil
}

func decodeGLONASS(p bits) (view, sat string, row []string, err error) {
	if len(p)*8 < 12+6+5+4+12+1+1+7+3*(24+27+5)+1+11+3+22 {
		return "", "", nil, errors.New("1020 truncated")
	}
	pos := 12
	prn := p.u(pos, 6)
	pos += 6
	channel := int(p.u(pos, 5)) - 7
	pos += 5 + 1 + 1 + 2 + 5 + 6 + 1
	health := p.u(pos, 1)
	pos += 2
	tb := p.u(pos, 7) * 15
	pos += 7
	var vel, xyz, acc [3]float64
	for k := 0; k < 3; k++ {
		vel[k] = float64(p.g(pos, 24)) * p2(-20) * 1e3
		pos += 24
		xyz[k] = float64(p.g(pos, 27)) * p2(-11) * 1e3
		pos += 27
		acc[k] = float64(p.g(pos, 5)) * p2(-30) * 1e3
		pos += 5
	}
	pos++
	gamma := float64(p.g(pos, 11)) * p2(-40)
	pos += 11 + 3
	tau := float64(p.g(pos, 22)) * p2(-30)
	sat = fmt.Sprintf("R%02d", prn)
	return "GLONASS", sat, []string{sat, fmt.Sprint(health), fmt.Sprint(channel), fmt.Sprint(tb),
		fmt.Sprintf("%.3f", xyz[0]), fmt.Sprintf("%.3f", xyz[1]), fmt.Sprintf("%.3f", xyz[2]),
		fmt.Sprintf("%.6f", vel[0]), fmt.Sprintf("%.6f", vel[1]), fmt.Sprintf("%.6f", vel[2]),
		fmt.Sprintf("%.3e", acc[0]), fmt.Sprintf("%.3e", acc[1]), fmt.Sprintf("%.3e", acc[2]),
		fmt.Sprintf("%.6e", tau), fmt.Sprintf("%.6e", gamma)}, nil
}
