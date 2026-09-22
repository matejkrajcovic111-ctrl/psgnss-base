package rinex

import (
	"os"
	"path/filepath"
	"testing"
)

func ubxFrame(class, id byte, payload []byte) []byte {
	b := []byte{0xb5, 0x62, class, id, byte(len(payload)), byte(len(payload) >> 8)}
	b = append(b, payload...)
	var a, k byte
	for _, x := range b[2:] {
		a += x
		k += a
	}
	return append(b, a, k)
}

func TestNavigationRequiresRawxAndSfrbx(t *testing.T) {
	sfrbx := classify(ubxFrame(0x02, 0x13, []byte{1, 2, 3}))
	if sfrbx.HasNavigation() {
		t.Fatal("SFRBX-only content must not be advertised as convertible navigation")
	}
	both := classify(append(ubxFrame(0x02, 0x13, []byte{1}), ubxFrame(0x02, 0x15, []byte{2})...))
	if !both.HasNavigation() {
		t.Fatal("SFRBX + RAWX should be advertised as convertible navigation")
	}
}

func TestSniffEdgesSeesUpgradedLiveTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nav.ubx")
	b := append(ubxFrame(0x02, 0x13, []byte{1}), make([]byte, 256)...)
	b = append(b, ubxFrame(0x02, 0x15, []byte{2})...)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := SniffEdges(p, 32)
	if err != nil {
		t.Fatal(err)
	}
	if !c.HasNavigation() {
		t.Fatalf("edge scan missed message pair: %+v", c)
	}
}
