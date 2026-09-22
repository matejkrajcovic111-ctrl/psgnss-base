package ephemeris

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/psgnss/psgnss-base/internal/hub"
	"github.com/psgnss/psgnss-base/internal/rtcm"
)

// MessageTypes lists the RTCM ephemeris messages this package can synthesise:
// GPS 1019, BeiDou 1042 and Galileo 1046, which are the three constellations
// this station tracks. QZSS 1044 and NavIC 1041 are not implemented -- neither
// is visible from here, so there would be no way to check a decoder against
// real sky, and an unverified one produces a plausible satellite in the wrong
// place rather than failing visibly.
var MessageTypes = []int{1019, 1042, 1046}

// Supports reports whether a message type can be synthesised.
func Supports(msgType int) bool {
	for _, t := range MessageTypes {
		if t == msgType {
			return true
		}
	}
	return false
}

// maxAge drops an ephemeris the receiver has stopped refreshing. GPS repeats
// each data set every 30 seconds and a set is nominally valid for four hours;
// two hours without a refresh means the satellite has set or the receiver has
// lost it, and a stale orbit is worse than a missing one.
const maxAge = 2 * time.Hour

type Store struct {
	log *slog.Logger
	mu  sync.Mutex
	gps map[int]*gpsAssembly
	gal map[int]*galileoAssembly
	bds map[int]*beidouAssembly

	framesSeen, framesBad, encoded uint64
	announced                      map[string]bool
}

func New(log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{log: log, gps: map[int]*gpsAssembly{}, gal: map[int]*galileoAssembly{},
		bds: map[int]*beidouAssembly{}, announced: map[string]bool{}}
}

// Run collects navigation subframes until the context ends.
func (s *Store) Run(ctx context.Context, h *hub.Hub) {
	if h == nil {
		return
	}
	filter, err := hub.NewFilter(hub.ProtoUBX, []hub.FilterSpec{{Type: 0x0213}}) // RXM-SFRBX
	if err != nil {
		s.log.Error("cannot build the ephemeris filter", "err", err)
		return
	}
	sub := h.Subscribe("ephemeris", filter, 512)
	defer sub.Close()
	// Without this the collector is invisible: it has no output of its own
	// until a mountpoint asks for 1019, so there would be no way to tell a
	// working decoder from a silent one.
	report := time.NewTicker(15 * time.Minute)
	defer report.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-report.C:
			s.log.Info("ephemeris collector", "status", s.Status())
		case frame, ok := <-sub.C():
			if !ok {
				return
			}
			s.consume(frame)
			s.announceFirst()
		}
	}
}

// consume decodes one UBX-RXM-SFRBX frame.
//
// Payload: gnssId, svId, reserved, freqId, numWords, chn, version, reserved,
// then numWords little-endian dwords.
//
// Which signal a frame came from decides everything. The receiver forwards the
// modern data signals in exactly the same frame shape as the legacy ones, and
// they carry entirely different message structures: GPS L2 CM and L5 carry
// CNAV, Galileo E5b carries I/NAV pages this decoder cannot read, BeiDou B1C
// and B2a carry B-CNAV. Feeding any of them to the wrong parser yields a
// plausible ephemeris built from the wrong bits -- a satellite in a place it
// has never been -- so each constellation accepts one signal and one word
// count, and everything else is dropped.
func (s *Store) consume(frame []byte) {
	const header = 6 + 8 // UBX header and the fixed part of the payload
	if len(frame) < header+2 {
		return
	}
	s.mu.Lock()
	s.framesSeen++
	s.mu.Unlock()
	p := frame[6:]
	gnssID, svID, sigID, numWords, version := p[0], p[1], p[2], int(p[4]), p[6]
	if len(p) < 8+4*numWords {
		s.mu.Lock()
		s.framesBad++
		s.mu.Unlock()
		return
	}
	words := make([]uint32, numWords)
	for i := 0; i < numWords; i++ {
		b := p[8+4*i:]
		words[i] = uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
	}
	// sigId is only defined from payload version 2, so an older receiver is
	// skipped rather than guessed at.
	if version < 2 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case gnssID == 0 && sigID == 0 && numWords >= 10: // GPS L1 C/A, LNAV
		for i := range words {
			words[i] >>= 6 // drop the parity bits
		}
		a := s.gps[int(svID)]
		if a == nil {
			a = &gpsAssembly{}
			a.eph.sv = int(svID)
			s.gps[int(svID)] = a
		}
		if _, err := a.addSubframe(words); err != nil {
			s.framesBad++
		}
	case gnssID == 2 && sigID == galileoE1B && numWords == 8: // Galileo E1-B, I/NAV
		a := s.gal[int(svID)]
		if a == nil {
			a = &galileoAssembly{}
			a.eph.sv = int(svID)
			s.gal[int(svID)] = a
		}
		if _, err := a.addPage(words); err != nil {
			s.framesBad++
		}
	case gnssID == 3 && numWords == 10 && !beidouGEO(int(svID)): // BeiDou B1I, D1 NAV
		a := s.bds[int(svID)]
		if a == nil {
			a = &beidouAssembly{}
			a.eph.sv = int(svID)
			s.bds[int(svID)] = a
		}
		if _, err := a.addSubframe(words); err != nil {
			s.framesBad++
		}
	}
}

// galileoE1B is the signal that carries the I/NAV pages this decoder reads.
// E5b carries I/NAV too, but the pages the receiver forwards from it do not
// have the same layout: decoded as E1-B they produce week numbers centuries
// out, which is how they were caught.
const galileoE1B = 1

// beidouGEO reports whether a satellite is geostationary, and so broadcasts D2
// rather than D1. D2 has a different frame layout, and RTCM 1042 carries D1.
func beidouGEO(sv int) bool { return sv <= 5 || (sv >= 59 && sv <= 63) }

// Messages returns one RTCM frame per satellite with a complete, fresh data
// set. An empty result is normal for the first minute after start: a full GPS
// ephemeris takes three subframes, which arrive over 18 seconds.
func (s *Store) Messages(msgType int) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]byte
	emit := func(sv int, fresh bool, encode func(writer)) {
		if !fresh {
			return
		}
		w := rtcm.NewWriter()
		encode(w)
		frame, err := w.Frame()
		if err != nil {
			s.log.Error("cannot encode ephemeris", "type", msgType, "sv", sv, "err", err)
			return
		}
		out = append(out, frame)
		s.encoded++
	}
	switch msgType {
	case 1019:
		for sv, a := range s.gps {
			emit(sv, a.eph.complete(a.have) && time.Since(a.eph.updated) <= maxAge, a.eph.encode1019)
		}
	case 1042:
		for sv, a := range s.bds {
			emit(sv, a.complete() && time.Since(a.eph.updated) <= maxAge, a.eph.encode1042)
		}
	case 1046:
		for sv, a := range s.gal {
			emit(sv, a.complete() && time.Since(a.eph.updated) <= maxAge, a.eph.encode1046)
		}
	}
	return out
}

// announceFirst logs the moment the first complete data set exists, which is
// the only externally visible sign that decoding works.
func (s *Store) announceFirst() {
	type found struct {
		system string
		sv     int
	}
	var news []found
	s.mu.Lock()
	for id, a := range s.gps {
		if a.eph.complete(a.have) && !s.announced["GPS"] {
			s.announced["GPS"] = true
			news = append(news, found{"GPS", id})
		}
	}
	for id, a := range s.gal {
		if a.complete() && !s.announced["Galileo"] {
			s.announced["Galileo"] = true
			news = append(news, found{"Galileo", id})
		}
	}
	for id, a := range s.bds {
		if a.complete() && !s.announced["BeiDou"] {
			s.announced["BeiDou"] = true
			news = append(news, found{"BeiDou", id})
		}
	}
	seen := s.framesSeen
	s.mu.Unlock()
	for _, n := range news {
		s.log.Info("first ephemeris assembled", "system", n.system, "sv", n.sv, "subframes_seen", seen)
	}
}

// Supports reports whether this store can synthesise a message type, so the
// caster can tell an ephemeris type from a station-description one.
func (s *Store) Supports(msgType int) bool { return Supports(msgType) }

// Status reports what the store holds, for the diagnostics page.
func (s *Store) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	ready, satellites := 0, len(s.gps)+len(s.gal)+len(s.bds)
	per := map[string]any{}
	count := func(name string, seen, done int) {
		ready += done
		per[name] = map[string]int{"satellites": seen, "ready": done}
	}
	done := 0
	for _, a := range s.gps {
		if a.eph.complete(a.have) && time.Since(a.eph.updated) <= maxAge {
			done++
		}
	}
	count("GPS", len(s.gps), done)
	done = 0
	for _, a := range s.gal {
		if a.complete() && time.Since(a.eph.updated) <= maxAge {
			done++
		}
	}
	count("Galileo", len(s.gal), done)
	done = 0
	for _, a := range s.bds {
		if a.complete() && time.Since(a.eph.updated) <= maxAge {
			done++
		}
	}
	count("BeiDou", len(s.bds), done)
	return map[string]any{
		"subframes": s.framesSeen, "malformed": s.framesBad,
		"satellites": satellites, "ready": ready, "encoded": s.encoded,
		"types": MessageTypes, "systems": per,
	}
}
