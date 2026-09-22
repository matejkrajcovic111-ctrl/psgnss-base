package caster

import (
	"fmt"
	"sort"
	"strings"
)

// MountEntry describes one mountpoint for the sourcetable.
type MountEntry struct {
	Name      string
	SourceID  int
	Format    string // e.g. "RTCM 3.2"
	Messages  string // e.g. "1006(10),1077(1)"
	Carrier   int
	NavSystem string
	Network   string
	Country   string
	Latitude  float64
	Longitude float64
	NMEA      int
	Solution  int
	Generator string
	Compress  string
	Auth      string // "N" none, "B" basic
	Fee       string // "N" or "Y"
	Bitrate   int
	MSMDetail string // optional format-details prefix
}

// STR renders one STR record.
//
// Field order is fixed by the NTRIP spec; SNIP and BNC both parse positionally,
// so nothing here may be reordered or omitted.
func (m MountEntry) STR() string {
	details := m.Messages
	if m.MSMDetail != "" {
		details = m.MSMDetail + " " + m.Messages
	}
	compress := m.Compress
	if compress == "" {
		compress = "none"
	}
	// Coordinates are published to 2 dp only. That is the convention every
	// caster follows and it avoids putting a precise private location in a
	// document served to anonymous clients.
	return fmt.Sprintf("STR;%s;%d;%s;%s;%d;%s;%s;%s;%.2f;%.2f;%d;%d;%s;%s;%s;%s;%d;",
		m.Name, m.SourceID, m.Format, details, m.Carrier, m.NavSystem,
		m.Network, m.Country, m.Latitude, m.Longitude, m.NMEA, m.Solution,
		m.Generator, compress, m.Auth, m.Fee, m.Bitrate)
}

// NetEntry is a NET record.
type NetEntry struct {
	Network  string
	Operator string
	Auth     string
	Fee      string
	WebNet   string
	WebStr   string
	WebReg   string
}

func (n NetEntry) NET() string {
	// Deliberately no internal address here. The system being replaced
	// published its caster's private tailnet address to anyone who asked for
	// the sourcetable.
	return fmt.Sprintf("NET;%s;%s;%s;%s;%s;%s;%s;",
		n.Network, n.Operator, n.Auth, n.Fee, dash(n.WebNet), dash(n.WebStr), dash(n.WebReg))
}

func dash(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Sourcetable renders the full table body.
func Sourcetable(mounts []MountEntry, net *NetEntry) string {
	sorted := append([]MountEntry(nil), mounts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var b strings.Builder
	for _, m := range sorted {
		b.WriteString(m.STR())
		b.WriteString("\r\n")
	}
	if net != nil {
		b.WriteString(net.NET())
		b.WriteString("\r\n")
	}
	b.WriteString("ENDSOURCETABLE\r\n")
	return b.String()
}

// FormatMessages renders a message list as sourcetable "type(interval)" text.
func FormatMessages(types []int, intervals map[int]int) string {
	sort.Ints(types)
	parts := make([]string, 0, len(types))
	for _, t := range types {
		iv := intervals[t]
		if iv <= 0 {
			iv = 1
		}
		parts = append(parts, fmt.Sprintf("%d(%d)", t, iv))
	}
	return strings.Join(parts, ",")
}
