package receiver

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Stage0 is the receiver configuration captured in Stage 0, before PSGNSS
// wrote anything. The file holds the base position at full precision, so it
// is deployed to the Pi's state directory and never committed or embedded.
type Stage0 struct {
	Model    string
	Software string
	Values   map[uint32][]byte // RAM layer; Stage 0 recorded RAM and Flash as identical
}

type stage0File struct {
	MonVer struct {
		SwVersion  string   `json:"swVersion"`
		Extensions []string `json:"extensions"`
	} `json:"mon_ver"`
	Layers map[string]map[string][][2]string `json:"layers"`
}

// LoadStage0 reads the Stage 0 dump (snapshot/raw/ubx-config-dump.json).
func LoadStage0(path string) (*Stage0, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f stage0File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse Stage 0 snapshot: %w", err)
	}
	ram := f.Layers["RAM"]
	if len(ram) == 0 {
		return nil, errors.New("stage 0 snapshot has no RAM layer")
	}
	s := &Stage0{Software: f.MonVer.SwVersion, Values: map[uint32][]byte{}}
	s.Model = MonVer{Extensions: f.MonVer.Extensions}.Model()
	for _, rows := range ram {
		for _, kv := range rows {
			k, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(kv[0]), "0x"), 16, 32)
			if err != nil {
				return nil, fmt.Errorf("stage 0 snapshot key %q: %w", kv[0], err)
			}
			v, err := hex.DecodeString(kv[1])
			if err != nil || KeySize(uint32(k)) != len(v) {
				return nil, fmt.Errorf("stage 0 snapshot value for %s is malformed", kv[0])
			}
			s.Values[uint32(k)] = v
		}
	}
	return s, nil
}

// Groups lists the configuration groups present in the snapshot.
func (s *Stage0) Groups() []byte {
	seen := map[byte]bool{}
	for k := range s.Values {
		seen[byte(k>>16)] = true
	}
	out := make([]byte, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Stage0Diff is one key whose live value differs from Stage 0.
type Stage0Diff struct {
	Key     uint32
	Current []byte
	Stage0  []byte
}

// Diff compares the snapshot with a live read. Keys the receiver no longer
// reports are returned separately: they cannot be verified, so they are
// never written.
func (s *Stage0) Diff(live map[uint32][]byte) (diffs []Stage0Diff, unreadable []uint32) {
	for _, k := range SortedKeys(s.Values) {
		cur, ok := live[k]
		if !ok {
			unreadable = append(unreadable, k)
			continue
		}
		if !equalBytes(cur, s.Values[k]) {
			diffs = append(diffs, Stage0Diff{Key: k, Current: cur, Stage0: s.Values[k]})
		}
	}
	return diffs, unreadable
}
