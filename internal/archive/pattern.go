// Package archive writes the raw stream to disk and flushes it to the SMB
// share, keeping two independent archives: UBX for post-processing and RTCM
// for replay.
package archive

import (
	"fmt"
	"strings"
	"time"
)

// ExpandPattern renders an RTKLIB-style filename pattern.
//
// The existing archive uses "Base1_%Y%m%d%h00", which four months of history
// and the downstream tooling both depend on, so the same verbs are supported
// with the same meanings.
//
// Callers pass a UTC time: RTKLIB names these files in UTC, and mixing in a
// local hour would shift every filename whenever the offset is non-zero.
//
//	%Y  4-digit year      %y  2-digit year
//	%m  month 01-12       %d  day 01-31
//	%h  hour 00-23        %M  minute 00-59
//	%S  second 00-59      %n  day of year 001-366
//	%%  a literal percent
func ExpandPattern(pattern string, t time.Time) string {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '%' || i+1 >= len(pattern) {
			b.WriteByte(pattern[i])
			continue
		}
		i++
		switch pattern[i] {
		case 'Y':
			fmt.Fprintf(&b, "%04d", t.Year())
		case 'y':
			fmt.Fprintf(&b, "%02d", t.Year()%100)
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'h':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'M':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'S':
			fmt.Fprintf(&b, "%02d", t.Second())
		case 'n':
			fmt.Fprintf(&b, "%03d", t.YearDay())
		case '%':
			b.WriteByte('%')
		default:
			// Unknown verb: emit it literally rather than silently dropping
			// characters out of a filename.
			b.WriteByte('%')
			b.WriteByte(pattern[i])
		}
	}
	return b.String()
}

// PatternGlob turns a pattern into a shell glob for discovering existing files.
func PatternGlob(pattern string) string {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '%' || i+1 >= len(pattern) {
			b.WriteByte(pattern[i])
			continue
		}
		i++
		switch pattern[i] {
		case 'Y', 'y', 'm', 'd', 'h', 'M', 'S', 'n':
			b.WriteByte('*')
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(pattern[i])
		}
	}
	// Collapse runs of '*' so "%Y%m%d" does not become "***".
	out := b.String()
	for strings.Contains(out, "**") {
		out = strings.ReplaceAll(out, "**", "*")
	}
	return out
}

// ParseTimeFromName recovers the start time encoded in a filename, given the
// pattern that produced it. Used to index files written before PSGNSS existed.
func ParseTimeFromName(pattern, name string) (time.Time, bool) {
	var layout strings.Builder
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '%' || i+1 >= len(pattern) {
			layout.WriteByte(pattern[i])
			continue
		}
		i++
		switch pattern[i] {
		case 'Y':
			layout.WriteString("2006")
		case 'y':
			layout.WriteString("06")
		case 'm':
			layout.WriteString("01")
		case 'd':
			layout.WriteString("02")
		case 'h':
			layout.WriteString("15")
		case 'M':
			layout.WriteString("04")
		case 'S':
			layout.WriteString("05")
		case '%':
			layout.WriteByte('%')
		default:
			return time.Time{}, false
		}
	}
	// Parse as UTC to match how the names are generated.
	t, err := time.ParseInLocation(layout.String(), name, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
