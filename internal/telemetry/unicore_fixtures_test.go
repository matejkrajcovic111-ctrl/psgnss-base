package telemetry

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/psgnss/psgnss-base/internal/hub"
)

// TestGenerateUnicoreFixtures cuts one epoch out of a raw UM980 capture into
// testdata/. It only runs when PSGNSS_UM980_CAPTURE names the capture, e.g.
//
//	PSGNSS_UM980_CAPTURE=/mnt/e/psgnss/.ref/um980-capture.bin go test -run GenerateUnicoreFixtures ./internal/telemetry/
//
// The capture is never committed: it holds the station's position. SATSINFOB,
// its ASCII twin SATSINFOA and BESTSATB are satellite data and are copied as
// they are. BESTNAVB carries the position, so latitude, longitude and height
// are replaced by round fake values (48, 17, 300), the geoid undulation (which
// would give the area away) is zeroed, and the CRC is recomputed.
func TestGenerateUnicoreFixtures(t *testing.T) {
	path := os.Getenv("PSGNSS_UM980_CAPTURE")
	if path == "" {
		t.Skip("PSGNSS_UM980_CAPTURE not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	type epoch struct{ satsB, satsA, bestNav, bestSat []byte }
	epochs := map[[2]uint32]*epoch{}
	get := func(wn uint16, ms uint32) *epoch {
		k := [2]uint32{uint32(wn), ms}
		if epochs[k] == nil {
			epochs[k] = &epoch{}
		}
		return epochs[k]
	}
	sc := hub.NewScanner(f, 1<<16)
	for {
		fr, err := sc.Next()
		if err != nil {
			break
		}
		raw := append([]byte(nil), fr.Raw...)
		switch fr.Proto {
		case hub.ProtoUnicore:
			h, _, err := SplitUnicore(raw)
			if err != nil {
				continue
			}
			e := get(h.Week, h.MS)
			switch h.MsgID {
			case UnicoreSatsInfo:
				e.satsB = raw
			case UnicoreBestNav:
				e.bestNav = raw
			case UnicoreBestSat:
				e.bestSat = raw
			}
		case hub.ProtoUnicoreText:
			if !bytes.HasPrefix(raw, []byte("#SATSINFOA,")) {
				continue
			}
			var wn, ms uint32
			parts := strings.SplitN(string(raw), ",", 7)
			if len(parts) < 7 || scan(parts[4], &wn) != nil || scan(parts[5], &ms) != nil {
				continue
			}
			get(uint16(wn), ms).satsA = raw
		}
	}

	var best *epoch
	var bestKey [2]uint32
	for k, e := range epochs {
		if e.satsB != nil && e.satsA != nil && e.bestNav != nil && e.bestSat != nil &&
			(best == nil || k[1] < bestKey[1]) {
			best, bestKey = e, k
		}
	}
	if best == nil {
		t.Fatal("no epoch in the capture has SATSINFOB, SATSINFOA, BESTNAVB and BESTSATB together")
	}

	nav := append([]byte(nil), best.bestNav...)
	d := nav[unicoreHeaderLen:]
	binary.LittleEndian.PutUint64(d[8:], math.Float64bits(48.0))
	binary.LittleEndian.PutUint64(d[16:], math.Float64bits(17.0))
	binary.LittleEndian.PutUint64(d[24:], math.Float64bits(300.0))
	binary.LittleEndian.PutUint32(d[32:], 0)
	binary.LittleEndian.PutUint32(nav[len(nav)-4:], UnicoreCRC32(nav[:len(nav)-4]))

	out := "testdata"
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"um980_satsinfo.bin": best.satsB,
		"um980_satsinfo.txt": best.satsA,
		"um980_bestsat.bin":  best.bestSat,
		"um980_bestnav.bin":  nav,
	} {
		if err := os.WriteFile(filepath.Join(out, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("wrote the epoch at week %d, ms %d", bestKey[0], bestKey[1])
}

func scan(s string, v *uint32) error {
	var n uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return os.ErrInvalid
		}
		n = n*10 + uint64(c-'0')
	}
	*v = uint32(n)
	return nil
}
