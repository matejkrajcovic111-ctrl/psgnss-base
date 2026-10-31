package rinex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// rtcmFrame builds a minimal valid RTCM3 frame of the given message type. The
// payload is zeros past the 12-bit type: enough to frame, and carrying no
// coordinates.
func rtcmFrame(msg int) []byte {
	b := []byte{0xD3, 0x00, 0x04, byte(msg >> 4), byte(msg&0xF) << 4, 0, 0}
	c := crc24q(b)
	return append(b, byte(c>>16), byte(c>>8), byte(c))
}

func TestRTCMEphemerisCountsAsNavigation(t *testing.T) {
	obs := classify(append(rtcmFrame(1077), rtcmFrame(1127)...))
	if obs.HasNavigation() || obs.Ephemeris != 0 {
		t.Fatalf("MSM-only RTCM must not promise navigation: %+v", obs)
	}
	um980 := classify(append(rtcmFrame(1077), rtcmFrame(1046)...))
	if !um980.HasNavigation() || um980.Ephemeris != 1 {
		t.Fatalf("RTCM with ephemeris must offer navigation: %+v", um980)
	}
	if um980.Format() != "rtcm3" {
		t.Fatalf("format = %s, want rtcm3", um980.Format())
	}
}

func TestWindowArgsKeepDateAndTimeSeparate(t *testing.T) {
	start := time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	got := strings.Join(WindowArgs("rtcm3", start, end), "|")
	want := "-ts|2026/10/31|16:00:00|-te|2026/10/31|17:00:00|-tr|2026/10/31|16:00:00"
	if got != want {
		t.Errorf("rtcm3 args = %s, want %s", got, want)
	}
	// UBX carries receiver time itself, so its arguments stay as they were.
	got = strings.Join(WindowArgs("ubx", start, end), "|")
	if strings.Contains(got, "-tr") {
		t.Errorf("ubx args gained -tr: %s", got)
	}
}
