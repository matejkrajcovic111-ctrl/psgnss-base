package receiver_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/receiver"
	"github.com/psgnss/psgnss-base/internal/receiver/receivertest"
)

const (
	keyTMODE   = 0x20030001 // CFG-TMODE-MODE, E1
	keyLat     = 0x40030009 // CFG-TMODE-LAT, I4 1e-7 deg
	keyRate    = 0x30210001 // CFG-RATE-MEAS, U2 0.001 s
	key1077USB = 0x209102CF
	key1077U1  = 0x209102CD
	keyGPSEna  = 0x1031001F
	keyGPSL5   = 0x10310004
	keyGLOEna  = 0x10310025
)

func TestCatalogueUsesInterfaceDescriptionNames(t *testing.T) {
	for k, want := range map[uint32]string{
		key1077USB: "CFG-MSGOUT-RTCM_3X_TYPE1077_USB",
		keyTMODE:   "CFG-TMODE-MODE",
		keyGPSEna:  "CFG-SIGNAL-GPS_ENA",
		// pyubx2 calls this USE_PRN_1_TO_5; the u-blox document wins.
		0x10340014: "CFG-BDS-USE_GEO_PRN",
	} {
		if got := receiver.KeyName(k); got != want {
			t.Errorf("0x%08X: got %q want %q", k, got, want)
		}
	}
	// Present on the production X20P but in no document: never guessed.
	if name := receiver.KeyName(0x20910137); name != "" {
		t.Errorf("undocumented key was given name %q", name)
	}
}

func TestDisplayAndParseValues(t *testing.T) {
	cases := []struct {
		key     uint32
		raw     []byte
		display string
		text    string
	}{
		{keyTMODE, []byte{2}, "FIXED (2)", "FIXED"},
		{keyGPSEna, []byte{1}, "true", "true"},
		{keyRate, []byte{0xE8, 0x03}, "1000 × 0.001 s", "1000"},
		{keyLat, []byte{0xFF, 0xFF, 0xFF, 0xFF}, "-1 × 1e-7 deg", "-1"},
		{key1077USB, []byte{1}, "1", "1"},
	}
	for _, c := range cases {
		if got := receiver.DisplayValue(c.key, c.raw); got != c.display {
			t.Errorf("display 0x%08X: got %q want %q", c.key, got, c.display)
		}
		b, err := receiver.ParseValue(c.key, c.text)
		if err != nil || !bytes.Equal(b, c.raw) {
			t.Errorf("parse 0x%08X %q: got %x, %v want %x", c.key, c.text, b, err, c.raw)
		}
	}
	for _, bad := range []struct {
		key  uint32
		text string
	}{{key1077USB, "256"}, {keyGPSEna, "maybe"}, {keyTMODE, "SURVEY"}, {0x20910137, "1"}, {keyRate, "hex:01"}} {
		if _, err := receiver.ParseValue(bad.key, bad.text); err == nil {
			t.Errorf("0x%08X accepted %q", bad.key, bad.text)
		}
	}
	if b, err := receiver.ParseValue(0x20910137, "hex:01"); err != nil || !bytes.Equal(b, []byte{1}) {
		t.Errorf("hex escape for undocumented key: %x %v", b, err)
	}
}

func TestMessageOutputsAndConstellations(t *testing.T) {
	kv := map[uint32][]byte{key1077USB: {1}, key1077U1: {0}, 0x20910137: {0},
		keyGPSEna: {1}, keyGPSL5: {1}, keyGLOEna: {0}, 0x10310018: {1}}
	msgs := receiver.MessageOutputs(kv)
	if len(msgs) != 1 || msgs[0].Label != "RTCM 1077" || msgs[0].Ports["USB"].Rate != 1 ||
		msgs[0].Ports["UART1"].Key != "0x209102CD" {
		t.Fatalf("messages: %+v", msgs)
	}
	cons := receiver.Constellations(kv, []string{"GPS", "GAL", "BDS"})
	if len(cons) != 2 || cons[0].ID != "GPS" || !cons[0].Supported || !cons[0].Enabled ||
		len(cons[0].Signals) != 1 || cons[0].Signals[0].Label != "GPS L5" {
		t.Fatalf("GPS: %+v", cons)
	}
	if cons[1].ID != "GLO" || cons[1].Supported || cons[1].Label != "GLONASS" {
		t.Fatalf("GLONASS must be flagged unsupported by MON-VER: %+v", cons[1])
	}
}

func TestParseMonSys(t *testing.T) {
	p := make([]byte, 24)
	p[0], p[1], p[2] = 1, 6, 17
	p[8], p[9] = 0x10, 0x0E // 3600 s
	m, err := receiver.ParseMonSys(p)
	if err != nil || m.RunTime != 3600 || m.BootReason != "Software reset" || m.CPULoad != 17 {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := receiver.ParseMonSys(p[:20]); err == nil {
		t.Fatal("short payload accepted")
	}
}

func TestApplyChunksLargeTransactionsAndReverts(t *testing.T) {
	vals := map[uint32][]byte{}
	var kvs []receiver.KV
	for i := uint32(0); i < 130; i++ {
		k := 0x20910000 + 0x200 + i
		vals[k] = []byte{0}
		kvs = append(kvs, receiver.KV{Key: k, Value: []byte{1}})
	}
	f := receivertest.New(vals)
	p, _ := f.Open()
	s := receiver.NewSession(p)
	s.Timeout = time.Second
	txn, err := s.Apply(kvs, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.MaxKeysPerSet > 64 {
		t.Fatalf("VALSET carried %d keys; the protocol limit is 64", f.MaxKeysPerSet)
	}
	if !bytes.Equal(f.Get(0x20910200+129), []byte{1}) {
		t.Fatal("last chunk not applied")
	}
	if err := txn.Revert(); err != nil {
		t.Fatal(err)
	}
	for _, kv := range kvs {
		if !bytes.Equal(f.Get(kv.Key), []byte{0}) {
			t.Fatalf("0x%08X not reverted", kv.Key)
		}
	}
}

func TestApplyRevertsWhenReadBackDisagrees(t *testing.T) {
	f := receivertest.New(map[uint32][]byte{key1077USB: {1}, key1077U1: {0}})
	f.Ignore[key1077U1] = true
	p, _ := f.Open()
	s := receiver.NewSession(p)
	s.Timeout = 500 * time.Millisecond
	_, err := s.Apply([]receiver.KV{{Key: key1077USB, Value: []byte{0}}, {Key: key1077U1, Value: []byte{1}}}, time.Hour, nil)
	if err == nil {
		t.Fatal("unverified write accepted")
	}
	if !bytes.Equal(f.Get(key1077USB), []byte{1}) {
		t.Fatal("verified half of a failed transaction was left applied")
	}
}

func TestStage0DiffSkipsUnreadableKeys(t *testing.T) {
	dump := map[string]any{
		"mon_ver": map[string]any{"swVersion": "EXT HPG 2.00 (test)", "extensions": []string{"MOD=ZED-X20P"}},
		"layers": map[string]any{
			"RAM":   map[string][][2]string{"0x91": {{"0x209102CF", "01"}, {"0x209102CD", "00"}}, "0x99": {{"0x20990001", "05"}}},
			"Flash": map[string][][2]string{},
		},
	}
	b, _ := json.Marshal(dump)
	path := filepath.Join(t.TempDir(), "stage0.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := receiver.LoadStage0(path)
	if err != nil || snap.Model != "ZED-X20P" || len(snap.Values) != 3 {
		t.Fatalf("%+v %v", snap, err)
	}
	diffs, unreadable := snap.Diff(map[uint32][]byte{key1077USB: {0}, key1077U1: {0}})
	if len(diffs) != 1 || diffs[0].Key != key1077USB || !bytes.Equal(diffs[0].Stage0, []byte{1}) {
		t.Fatalf("diffs %+v", diffs)
	}
	if len(unreadable) != 1 || unreadable[0] != 0x20990001 {
		t.Fatalf("unreadable %x", unreadable)
	}
}
