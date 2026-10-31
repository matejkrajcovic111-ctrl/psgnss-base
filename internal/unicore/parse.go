package unicore

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// LogBody splits "#NAME,hdr...;data*crc" into the name, header fields and the
// data part (without the checksum). Data of multi-line logs keeps its newlines.
func LogBody(log string) (name string, header []string, data string, err error) {
	log = strings.TrimSpace(log)
	if !strings.HasPrefix(log, "#") {
		return "", nil, "", errors.New("not a Unicore log")
	}
	semi := strings.IndexByte(log, ';')
	star := strings.LastIndexByte(log, '*')
	if semi < 0 || star < semi {
		return "", nil, "", errors.New("log has no data section")
	}
	hdr := strings.Split(log[1:semi], ",")
	return hdr[0], hdr, log[semi+1 : star], nil
}

// logNamed finds the first log called name in a reply.
func logNamed(r Reply, name string) (string, []string, string, error) {
	for _, l := range r.Logs {
		n, h, d, err := LogBody(l)
		if err == nil && strings.EqualFold(n, name) {
			return n, h, d, nil
		}
	}
	return "", nil, "", fmt.Errorf("no %s log in the reply", name)
}

// Version is the VERSIONA answer.
type Version struct {
	Model    string `json:"model"`
	Firmware string `json:"firmware"`
	// Auth is the authorization type ("HRPT00-S10C-P"), PN and Serial are the
	// two halves of the product number / serial pair, EfuseID the board ID and
	// Date the firmware date the receiver reports (YYYY/MM/DD).
	Auth    string `json:"auth"`
	PN      string `json:"pn"`
	Serial  string `json:"serial"`
	EfuseID string `json:"efuse_id"`
	Date    string `json:"auth_date"`
}

var quoted = regexp.MustCompile(`"([^"]*)"`)

// ParseVersion reads a VERSIONA reply.
func ParseVersion(r Reply) (Version, error) {
	_, _, d, err := logNamed(r, "VERSIONA")
	if err != nil {
		return Version{}, err
	}
	var f []string
	for _, m := range quoted.FindAllStringSubmatch(d, -1) {
		f = append(f, m[1])
	}
	if len(f) < 4 {
		return Version{}, fmt.Errorf("VERSIONA has %d fields, want at least 4", len(f))
	}
	v := Version{Model: f[0], Firmware: f[1], Auth: f[2]}
	v.PN, v.Serial, _ = strings.Cut(f[3], "-")
	if len(f) > 4 {
		v.EfuseID = f[4]
	}
	if len(f) > 5 {
		v.Date = f[5]
	}
	return v, nil
}

// ConfigLine is one "$CONFIG,<name>,<line>*XX" answer; Line is the command
// that would recreate it ("CONFIG COM1 115200").
type ConfigLine struct {
	Name string `json:"name"`
	Line string `json:"line"`
}

func stripChecksum(s string) string {
	if i := strings.LastIndexByte(s, '*'); i >= 0 && len(s)-i-1 == 2 {
		return s[:i]
	}
	return s
}

// ParseConfig reads the $CONFIG lines of a CONFIG reply. Repeated names (a
// receiver may list one function twice) keep their order and get a #n suffix
// so snapshots can be compared by key.
func ParseConfig(r Reply) []ConfigLine {
	var out []ConfigLine
	seen := map[string]int{}
	for _, l := range r.Lines {
		if !strings.HasPrefix(l, "$CONFIG,") {
			continue
		}
		name, line, ok := strings.Cut(stripChecksum(strings.TrimPrefix(l, "$CONFIG,")), ",")
		if !ok || strings.EqualFold(name, "MASK") {
			continue
		}
		seen[name]++
		if seen[name] > 1 {
			name = fmt.Sprintf("%s#%d", name, seen[name])
		}
		out = append(out, ConfigLine{Name: name, Line: strings.TrimSpace(line)})
	}
	return out
}

// Output is one active log: UNILOGLIST row "RTCM1077 COM1 1".
type Output struct {
	Message string `json:"message"`
	Port    string `json:"port"`
	// Rate is everything after the port: a period in seconds, or a trigger
	// such as "ONCHANGED".
	Rate string `json:"rate"`
}

// Command re-enables the output.
func (o Output) Command() string {
	return strings.TrimSpace(fmt.Sprintf("%s %s %s", o.Message, o.Port, o.Rate))
}

// ParseOutputs reads UNILOGLIST.
func ParseOutputs(r Reply) ([]Output, error) {
	_, _, d, err := logNamed(r, "UNILOGLIST")
	if err != nil {
		return nil, err
	}
	var out []Output
	for _, row := range strings.Split(d, "\n") {
		row = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(row), "<"))
		f := strings.Fields(row)
		if len(f) < 2 { // blank, or the "<\t15" count row
			continue
		}
		o := Output{Message: f[0], Port: f[1], Rate: strings.Join(f[2:], " ")}
		out = append(out, o)
	}
	return out, nil
}

// Mode is the MODE answer.
type Mode struct {
	// Raw is the receiver's text; Command is the same with trailing junk the
	// receiver appends ("," and extra fields) removed, so it can be sent back.
	Raw     string `json:"raw"`
	Command string `json:"command"`
	// Role is BASE, ROVER, ...; Kind is "time" (self-optimizing), "fixed"
	// (known position) or "" (default or not a base).
	Role string `json:"role"`
	ID   string `json:"id,omitempty"`
	Kind string `json:"kind,omitempty"`
	// TimeSeconds and Distance are the MODE BASE TIME parameters.
	TimeSeconds string `json:"time_seconds,omitempty"`
	Distance    string `json:"distance,omitempty"`
	// Lat, Lon, Height are kept as the receiver printed them (never rounded).
	Lat    string `json:"lat,omitempty"`
	Lon    string `json:"lon,omitempty"`
	Height string `json:"height,omitempty"`
}

// ParseMode reads the #MODE log (or a "$CONFIG,MODE" line, for firmware that
// answers that way).
func ParseMode(r Reply) (Mode, error) {
	raw := ""
	if _, _, d, err := logNamed(r, "MODE"); err == nil {
		raw = d
	} else {
		for _, l := range r.Lines {
			if strings.HasPrefix(l, "$CONFIG,MODE,") {
				raw = stripChecksum(strings.TrimPrefix(l, "$CONFIG,MODE,"))
			}
		}
	}
	if raw == "" {
		return Mode{}, errors.New("no MODE answer in the reply")
	}
	return ParseModeText(raw), nil
}

func isNumber(s string) bool { _, err := strconv.ParseFloat(s, 64); return err == nil }

func ParseModeText(raw string) Mode {
	m := Mode{Raw: strings.TrimSpace(raw)}
	f := strings.Fields(strings.ReplaceAll(strings.TrimSpace(raw), ",", " "))
	if len(f) > 0 && strings.EqualFold(f[0], "MODE") {
		f = f[1:]
	}
	if len(f) == 0 {
		return m
	}
	m.Role = strings.ToUpper(f[0])
	f = f[1:]
	norm := []string{"MODE", m.Role}
	if m.Role == "BASE" {
		// Forms: [ID] TIME t [d]   |   [ID] lat lon h   |   (nothing)
		switch {
		case len(f) >= 1 && strings.EqualFold(f[0], "TIME"):
			f = append([]string{""}, f...)
			fallthrough
		case len(f) >= 2 && strings.EqualFold(f[1], "TIME"):
			m.ID = f[0]
			m.Kind = "time"
			if len(f) > 2 {
				m.TimeSeconds = f[2]
			}
			if len(f) > 3 {
				m.Distance = f[3]
			}
			if m.ID != "" {
				norm = append(norm, m.ID)
			}
			norm = append(norm, "TIME")
			if m.TimeSeconds != "" {
				norm = append(norm, m.TimeSeconds)
				if m.Distance != "" {
					norm = append(norm, m.Distance)
				}
			}
		case len(f) >= 4 && isNumber(f[1]) && isNumber(f[2]) && isNumber(f[3]):
			m.ID, m.Lat, m.Lon, m.Height = f[0], f[1], f[2], f[3]
			m.Kind = "fixed"
			norm = append(norm, f[:4]...)
		case len(f) == 3 && isNumber(f[0]) && isNumber(f[1]) && isNumber(f[2]):
			m.Lat, m.Lon, m.Height = f[0], f[1], f[2]
			m.Kind = "fixed"
			norm = append(norm, f[:3]...)
		case len(f) >= 1:
			m.ID = f[0]
			norm = append(norm, f[0])
		}
	} else {
		norm = append(norm, f...)
	}
	m.Command = strings.Join(norm, " ")
	return m
}

// ParseMasks reads the "$CONFIG,MASK,..." lines of a MASK reply, e.g.
// "MASK 5.000000", "MASK GPS", "GPSMaskPrn:12,13,".
func ParseMasks(r Reply) []string {
	var out []string
	for _, l := range r.Lines {
		if rest, ok := strings.CutPrefix(l, "$CONFIG,MASK,"); ok {
			out = append(out, strings.TrimSpace(stripChecksum(rest)))
		}
	}
	return out
}

// maskCommands turns reported mask lines into the commands that recreate
// them: "MASK GPS" stays, "GPSMaskPrn:12,13," becomes MASK GPS PRN 12 / 13.
func maskCommands(masks []string) []string {
	var out []string
	for _, m := range masks {
		if i := strings.Index(m, "MaskPrn:"); i > 0 {
			sys := m[:i]
			for _, p := range strings.Split(m[i+len("MaskPrn:"):], ",") {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, fmt.Sprintf("MASK %s PRN %s", sys, p))
				}
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

// Fix is the position part of a BESTNAVA reply.
type Fix struct {
	SolStatus, PosType string
	Lat, Lon, Hgt, Und float64
}

// ParseBestNav reads BESTNAVA (also usable for BESTPOSA, same leading fields).
func ParseBestNav(r Reply) (Fix, error) {
	_, _, d, err := logNamed(r, "BESTNAVA")
	if err != nil {
		return Fix{}, err
	}
	f := strings.Split(strings.ReplaceAll(strings.TrimSpace(d), "\n", ""), ",")
	if len(f) < 6 {
		return Fix{}, errors.New("BESTNAVA is too short")
	}
	fx := Fix{SolStatus: f[0], PosType: f[1]}
	for i, dst := range []*float64{&fx.Lat, &fx.Lon, &fx.Hgt, &fx.Und} {
		v, err := strconv.ParseFloat(f[2+i], 64)
		if err != nil {
			return Fix{}, fmt.Errorf("BESTNAVA field %d: %w", 3+i, err)
		}
		*dst = v
	}
	return fx, nil
}

// ParseBasePos reads BASEPOSA: latitude, longitude, height above MSL and
// undulation as the receiver stores the base position.
func ParseBasePos(r Reply) (Fix, error) {
	_, _, d, err := logNamed(r, "BASEPOSA")
	if err != nil {
		return Fix{}, err
	}
	f := strings.Split(strings.ReplaceAll(strings.TrimSpace(d), "\n", ""), ",")
	if len(f) < 6 {
		return Fix{}, errors.New("BASEPOSA is too short")
	}
	fx := Fix{SolStatus: f[0], PosType: f[1]}
	for i, dst := range []*float64{&fx.Lat, &fx.Lon, &fx.Hgt, &fx.Und} {
		v, err := strconv.ParseFloat(f[2+i], 64)
		if err != nil {
			return Fix{}, fmt.Errorf("BASEPOSA field %d: %w", 3+i, err)
		}
		*dst = v
	}
	return fx, nil
}

var (
	modeBasePos = regexp.MustCompile(`(?i)(MODE\s+BASE\s+(?:\d+\s+)?)-?\d+\.\d+\s+-?\d+\.\d+\s+-?\d+(?:\.\d+)?`)
	posLogName  = regexp.MustCompile(`(?i)^#(BASEPOS|BESTNAV|BESTPOS|PVTSLN|PSRPOS|GPGGA|GNGGA|ADRNAV|ADRPOS|PPPNAV|SPPNAV)`)
)

// Redact hides coordinates in text bound for logs: the station's position is
// secret, and a console user can type or query it.
func Redact(s string) string {
	lines := strings.Split(s, "\n")
	skip := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case posLogName.MatchString(t):
			name, _, _ := strings.Cut(t[1:], ",")
			lines[i] = "#" + name + ",<position log omitted>"
			skip = !logEnd.MatchString(l + "\n")
		case skip:
			lines[i] = ""
			skip = !logEnd.MatchString(l + "\n")
		default:
			lines[i] = modeBasePos.ReplaceAllString(l, "${1}<position redacted>")
		}
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}
