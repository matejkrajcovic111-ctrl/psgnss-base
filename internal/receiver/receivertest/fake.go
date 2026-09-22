// Package receivertest simulates a u-blox receiver's UBX configuration
// interface closely enough to exercise PSGNSS's control path in tests.
package receivertest

import (
	"encoding/binary"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/psgnss/psgnss-base/internal/receiver"
)

// Fake holds receiver state shared by every session opened on it.
type Fake struct {
	mu      sync.Mutex
	RAM     map[uint32][]byte
	Flash   map[uint32][]byte
	Model   string
	Sw      string
	RunTime uint32
	// Ignore lists keys the receiver ACKs but silently does not change.
	Ignore map[uint32]bool
	// ValSets counts VALSET messages and the largest key count seen in one.
	ValSets, MaxKeysPerSet int
	out                    []byte
	wake                   chan struct{}
}

func New(values map[uint32][]byte) *Fake {
	f := &Fake{RAM: map[uint32][]byte{}, Flash: map[uint32][]byte{}, Model: "ZED-X20P",
		Sw: "EXT HPG 2.00 (test)", RunTime: 3725, Ignore: map[uint32]bool{}, wake: make(chan struct{}, 1)}
	for k, v := range values {
		f.RAM[k] = append([]byte(nil), v...)
		f.Flash[k] = append([]byte(nil), v...)
	}
	return f
}

// Open returns a control pipe, matching the hub's Control signature.
func (f *Fake) Open() (io.ReadWriteCloser, error) { return pipe{f}, nil }

// Get returns a RAM value.
func (f *Fake) Get(k uint32) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.RAM[k]...)
}

type pipe struct{ f *Fake }

func (p pipe) Close() error { return nil }

func (p pipe) Read(b []byte) (int, error) {
	f := p.f
	for i := 0; i < 2; i++ {
		f.mu.Lock()
		if len(f.out) > 0 {
			n := copy(b, f.out)
			f.out = f.out[n:]
			f.mu.Unlock()
			return n, nil
		}
		f.mu.Unlock()
		select {
		case <-f.wake:
		case <-time.After(20 * time.Millisecond):
		}
	}
	return 0, nil
}

func (p pipe) Write(b []byte) (int, error) {
	f := p.f
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := 0; i+8 <= len(b); {
		if b[i] != 0xB5 || b[i+1] != 0x62 {
			i++
			continue
		}
		ln := int(binary.LittleEndian.Uint16(b[i+4:]))
		if i+8+ln > len(b) {
			break
		}
		f.handle(b[i+2], b[i+3], b[i+6:i+6+ln])
		i += 8 + ln
	}
	select {
	case f.wake <- struct{}{}:
	default:
	}
	return len(b), nil
}

func (f *Fake) reply(cls, id byte, payload []byte) {
	f.out = append(f.out, receiver.Frame(cls, id, payload)...)
}

func (f *Fake) handle(cls, id byte, p []byte) {
	switch {
	case cls == 0x0A && id == 0x04:
		pl := make([]byte, 40)
		copy(pl, f.Sw)
		for _, e := range []string{"PROTVER=50.02", "MOD=" + f.Model, "GPS;GAL;BDS", "SBAS;QZSS", "NAVIC"} {
			x := make([]byte, 30)
			copy(x, e)
			pl = append(pl, x...)
		}
		f.reply(cls, id, pl)
	case cls == 0x0A && id == 0x39:
		pl := make([]byte, 24)
		pl[0], pl[1], pl[2] = 1, 1, 12
		binary.LittleEndian.PutUint32(pl[8:], f.RunTime)
		f.reply(cls, id, pl)
	case cls == 0x06 && id == 0x8B:
		f.valget(p)
	case cls == 0x06 && id == 0x8A:
		f.valset(p)
	}
}

func (f *Fake) valget(p []byte) {
	layer, pos := p[1], int(binary.LittleEndian.Uint16(p[2:]))
	src := f.RAM
	if layer == receiver.LayerFlash {
		src = f.Flash
	}
	var keys []uint32
	for i := 4; i+4 <= len(p); i += 4 {
		k := binary.LittleEndian.Uint32(p[i:])
		if k&0xFFFF == 0xFFFF {
			for have := range src {
				if have>>16&0xFF == k>>16&0xFF {
					keys = append(keys, have)
				}
			}
			continue
		}
		if _, ok := src[k]; ok {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		f.reply(0x05, 0x00, []byte{0x06, 0x8B})
		return
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	if pos >= len(keys) {
		f.reply(0x05, 0x00, []byte{0x06, 0x8B})
		return
	}
	keys = keys[pos:]
	if len(keys) > 64 {
		keys = keys[:64]
	}
	pl := []byte{1, layer, p[2], p[3]}
	for _, k := range keys {
		pl = binary.LittleEndian.AppendUint32(pl, k)
		pl = append(pl, src[k]...)
	}
	f.reply(0x06, 0x8B, pl)
	f.reply(0x05, 0x01, []byte{0x06, 0x8B})
}

func (f *Fake) valset(p []byte) {
	layers := p[1]
	type kv struct {
		k uint32
		v []byte
	}
	var kvs []kv
	for i := 4; i+4 <= len(p); {
		k := binary.LittleEndian.Uint32(p[i:])
		n := receiver.KeySize(k)
		i += 4
		if _, ok := f.RAM[k]; !ok || i+n > len(p) {
			f.reply(0x05, 0x00, []byte{0x06, 0x8A})
			return
		}
		kvs = append(kvs, kv{k, append([]byte(nil), p[i:i+n]...)})
		i += n
	}
	f.ValSets++
	f.MaxKeysPerSet = max(f.MaxKeysPerSet, len(kvs))
	for _, x := range kvs {
		if f.Ignore[x.k] {
			continue
		}
		if layers&receiver.SetRAM != 0 {
			f.RAM[x.k] = x.v
		}
		if layers&receiver.SetFlash != 0 {
			f.Flash[x.k] = x.v
		}
	}
	f.reply(0x05, 0x01, []byte{0x06, 0x8A})
}
