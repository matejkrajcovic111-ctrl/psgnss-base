package rawdata

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/psgnss/psgnss-base/internal/hub"
	"github.com/psgnss/psgnss-base/internal/rtcm"
)

// satExpiry drops a satellite whose system stopped reporting it.
const satExpiry = 5 * time.Second

// cacheFor bounds the work per viewer: however many people watch or however
// fast one polls, each table is marshalled at most this often.
const cacheFor = 900 * time.Millisecond

var obsOrder = map[string]int{"GPS": 0, "Galileo": 1, "BeiDou": 2, "GLONASS": 3, "SBAS": 4, "QZSS": 5, "NavIC": 6}

// Monitor keeps the latest observations and ephemeris for the raw data view.
type Monitor struct {
	// Extra supplies ephemeris that is not in the receiver stream: the X20P's
	// is synthesised by the ephemeris store. It may be nil.
	Extra func(msgType int) [][]byte
	Log   *slog.Logger

	mu    sync.Mutex
	sats  map[string]Sat
	nav   map[string]map[string][]string
	lock  map[string]uint64
	cache map[string]cached
}

type cached struct {
	body []byte
	at   time.Time
}

// New returns an empty Monitor.
func New(log *slog.Logger) *Monitor {
	return &Monitor{Log: log, sats: map[string]Sat{}, nav: map[string]map[string][]string{},
		lock: map[string]uint64{}, cache: map[string]cached{}}
}

// Run reads the RTCM stream until ctx ends.
func (m *Monitor) Run(ctx context.Context, h *hub.Hub) {
	sub := h.Subscribe("rawdata", hub.NewProtoFilter(hub.ProtoRTCM3), 512)
	defer sub.Close()
	extra := time.NewTicker(10 * time.Second)
	defer extra.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-sub.C():
			if !ok {
				return
			}
			m.consume(b, time.Now())
		case <-extra.C:
			if m.Extra == nil {
				continue
			}
			for _, t := range []int{1019, 1042, 1046} {
				for _, f := range m.Extra(t) {
					m.consume(f, time.Now())
				}
			}
		}
	}
}

// consume takes one whole RTCM frame.
func (m *Monitor) consume(frame []byte, now time.Time) {
	if len(frame) < 8 || frame[0] != 0xD3 {
		return
	}
	p := bits(frame[3 : len(frame)-3])
	typ := int(p.u(0, 12))
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case rtcm.IsEphemeris(typ):
		view, sat, row, err := decodeNav(p)
		if err != nil {
			return
		}
		if m.nav[view] == nil {
			m.nav[view] = map[string][]string{}
		}
		m.nav[view][sat] = row
	case typ%10 == 7 && msmSystems[typ/10].name != "":
		sats, err := decodeMSM7(p, now, m.lock)
		if err != nil {
			if m.Log != nil {
				m.Log.Debug("raw data: MSM7 not decoded", "type", typ, "err", err)
			}
			return
		}
		for _, s := range sats {
			m.sats[s.Sat] = s
		}
	}
}

// Views lists the tables, "obs" first.
func Views() []string {
	out := []string{"obs"}
	for _, v := range navViews {
		out = append(out, v.Name)
	}
	return out
}

// JSON renders a table. It reports false for a name that is not a view.
func (m *Monitor) JSON(view string, now time.Time) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.cache[view]; ok && now.Sub(c.at) < cacheFor {
		return c.body, true
	}
	var v any
	if view == "obs" {
		v = m.obs(now)
	} else {
		var found bool
		for _, nv := range navViews {
			if nv.Name == view {
				found, v = true, m.navTable(nv)
			}
		}
		if !found {
			return nil, false
		}
	}
	body, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	m.cache[view] = cached{body, now}
	return body, true
}

func (m *Monitor) obs(now time.Time) map[string]any {
	list := make([]Sat, 0, len(m.sats))
	for k, s := range m.sats {
		if now.Sub(s.seen) > satExpiry {
			delete(m.sats, k)
			continue
		}
		list = append(list, s)
	}
	sort.Slice(list, func(a, b int) bool {
		if x, y := obsOrder[list[a].System], obsOrder[list[b].System]; x != y {
			return x < y
		}
		return list[a].Sat < list[b].Sat
	})
	return map[string]any{"view": "obs", "sats": list}
}

func (m *Monitor) navTable(v navView) map[string]any {
	rows := make([][]string, 0, len(m.nav[v.Name]))
	for _, r := range m.nav[v.Name] {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a][0] < rows[b][0] })
	return map[string]any{"view": v.Name, "columns": v.Columns, "rows": rows}
}
