package hub

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Filter decides which frames an output receives.
//
// It selects by message type and optionally decimates: an interval of 10 means
// "pass at most one of these per 10 seconds". Frames are never modified, only
// passed or dropped, which is what keeps the fan-out lossless.
type Filter struct {
	// proto restricts by protocol; ProtoUnknown means any.
	proto Proto
	// also widens proto to further protocols, for consumers that read both a
	// receiver's binary and its text output.
	also []Proto
	// allow maps message type to its minimum spacing. A nil map means pass all.
	allow map[int]time.Duration
	last  map[int]time.Time
}

// FilterSpec is one message-type selection.
type FilterSpec struct {
	Type     int
	Interval int // seconds; 0 or 1 means every occurrence
}

// NewPassthrough builds a filter that passes every frame untouched.
func NewPassthrough() *Filter { return &Filter{} }

// NewProtoFilter passes every frame of the given protocols.
func NewProtoFilter(p Proto, more ...Proto) *Filter { return &Filter{proto: p, also: more} }

// NewFilter builds a message-type filter with per-type decimation.
func NewFilter(p Proto, specs []FilterSpec) (*Filter, error) {
	f := &Filter{proto: p, allow: make(map[int]time.Duration, len(specs)),
		last: make(map[int]time.Time, len(specs))}
	for _, s := range specs {
		if s.Type == 0 {
			return nil, fmt.Errorf("filter: message type must be non-zero")
		}
		if s.Interval < 0 {
			return nil, fmt.Errorf("filter: type %d has negative interval %d", s.Type, s.Interval)
		}
		iv := time.Duration(s.Interval) * time.Second
		if s.Interval <= 1 {
			iv = 0
		}
		f.allow[s.Type] = iv
	}
	return f, nil
}

// ubxNames maps the message names usable in config to class/id, encoded the
// same way Frame.Type is for UBX.
var ubxNames = map[string]int{
	"RXM-SFRBX": 0x0213, "RXM-RAWX": 0x0215,
	"NAV-PVT": 0x0107, "NAV-SAT": 0x0135, "NAV-SIG": 0x0143,
	"NAV-TIMEGPS": 0x0120, "NAV-CLOCK": 0x0122, "NAV-SVIN": 0x013B,
	"MON-VER": 0x0A04, "MON-HW": 0x0A09, "MON-RF": 0x0A38,
}

// ParseUBXMessage resolves a UBX message name to its filter type.
func ParseUBXMessage(name string) (int, error) {
	if t, ok := ubxNames[strings.ToUpper(strings.TrimSpace(name))]; ok {
		return t, nil
	}
	return 0, fmt.Errorf("unknown UBX message %q", name)
}

// UBXMessageNames lists the names config may use.
func UBXMessageNames() []string {
	out := make([]string, 0, len(ubxNames))
	for k := range ubxNames {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ParseProto maps a config string to a Proto.
func ParseProto(s string) (Proto, error) {
	switch s {
	case "", "none", "any", "all":
		return ProtoUnknown, nil
	case "ubx":
		return ProtoUBX, nil
	case "rtcm3":
		return ProtoRTCM3, nil
	case "nmea":
		return ProtoNMEA, nil
	case "unicore":
		return ProtoUnicore, nil
	}
	return ProtoUnknown, fmt.Errorf("unknown filter protocol %q", s)
}

// decimationTolerance stays well below the shortest interval (2 s) and below a
// 1 Hz source's period, so it cannot change which epoch a 1 Hz stream emits.
const decimationTolerance = 250 * time.Millisecond

// Pass reports whether the frame should be forwarded. now is passed in rather
// than read from the clock so decimation is testable.
func (f *Filter) Pass(fr Frame, now time.Time) bool {
	if f.proto != ProtoUnknown && fr.Proto != f.proto && !f.alsoPasses(fr.Proto) {
		return false
	}
	if f.allow == nil {
		return true
	}
	iv, ok := f.allow[fr.Type]
	if !ok {
		return false
	}
	if iv == 0 {
		return true
	}
	prev, seen := f.last[fr.Type]
	// Decimate on a fixed grid relative to the last emission, so a 10 s
	// interval stays on cadence rather than drifting by one epoch each time.
	// The tolerance is for a source already at the interval: the UM980's 1230
	// every 10 s arrives a few milliseconds either side, and a strict compare
	// dropped every other one, measured as gaps of 10 and 20 s on the mount.
	if seen && now.Sub(prev) < iv-decimationTolerance {
		return false
	}
	f.last[fr.Type] = now
	return true
}

func (f *Filter) alsoPasses(p Proto) bool {
	for _, q := range f.also {
		if p == q {
			return true
		}
	}
	return false
}

// Types returns the selected message types, for sourcetable generation.
func (f *Filter) Types() []int {
	if f.allow == nil {
		return nil
	}
	out := make([]int, 0, len(f.allow))
	for t := range f.allow {
		out = append(out, t)
	}
	return out
}
