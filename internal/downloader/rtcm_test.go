package downloader

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var crcTable = func() (t [256]uint32) {
	for i := range t {
		v := uint32(i) << 16
		for j := 0; j < 8; j++ {
			v <<= 1
			if v&0x1000000 != 0 {
				v ^= 0x1864CFB
			}
		}
		t[i] = v & 0xFFFFFF
	}
	return
}()

// rtcmFrame is a valid RTCM3 frame of one message type with a zero payload; it
// carries no coordinates.
func rtcmFrame(msg int) []byte {
	b := []byte{0xD3, 0x00, 0x04, byte(msg >> 4), byte(msg&0xF) << 4, 0, 0}
	var c uint32
	for _, x := range b {
		c = ((c << 8) & 0xFFFFFF) ^ crcTable[((c>>16)^uint32(x))&0xFF]
	}
	return append(b, byte(c>>16), byte(c>>8), byte(c))
}

// ubxFrame is an empty UBX message with a valid Fletcher checksum.
func ubxFrame(class, id byte) []byte {
	f := []byte{0xb5, 0x62, class, id, 0, 0}
	var a, k byte
	for _, x := range f[2:] {
		a += x
		k += a
	}
	return append(f, a, k)
}

func write(t *testing.T, path string, frames ...[]byte) {
	t.Helper()
	var b []byte
	for _, f := range frames {
		b = append(b, f...)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeConvbin records its arguments, so a test can see exactly what the
// downloader asked for.
func fakeConvbin(t *testing.T) (path, log string) {
	t.Helper()
	dir := t.TempDir()
	path, log = filepath.Join(dir, "convbin"), filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" > " + log + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return
}

func convertArgs(t *testing.T, raw string, start time.Time) string {
	t.Helper()
	bin, argLog := fakeConvbin(t)
	d := New(Options{ConvbinPath: bin, WorkDir: t.TempDir()})
	_, _, _ = d.convert(context.Background(), raw, start, start.Add(time.Hour),
		t.TempDir(), "x", Outputs{Obs: true}, Preset{}, nil)
	got, _ := os.ReadFile(argLog)
	return string(got)
}

// RTCM3 gives convbin only a time of week, so it needs -tr to find the week.
// Date and time are two arguments there as well, exactly like -ts and -te.
func TestRTCM3ConversionPassesAnApproximateTimeAsTwoArguments(t *testing.T) {
	raw := filepath.Join(t.TempDir(), "Base1_202610310000")
	write(t, raw, rtcmFrame(1077), rtcmFrame(1087))
	line := convertArgs(t, raw, time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC))
	for _, want := range []string{"-r rtcm3 ", "-tr 2026/10/31 16:00:00 ", "-ts 2026/10/31 16:00:00 "} {
		if !strings.Contains(line, want) {
			t.Errorf("convbin arguments lack %q: %s", want, line)
		}
	}
}

func TestUBXConversionKeepsItsArguments(t *testing.T) {
	raw := filepath.Join(t.TempDir(), "Base1_202609010000")
	write(t, raw, ubxFrame(0x02, 0x15))
	line := convertArgs(t, raw, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if strings.Contains(line, "-tr") || !strings.Contains(line, "-r ubx ") {
		t.Errorf("ubx arguments changed: %s", line)
	}
}

// A UM980 day has its ephemeris in the RTCM file, so navigation must come from
// there and not require a file in the UBX navigation archive.
func TestNavigationComesFromRTCMEphemerisWhenPresent(t *testing.T) {
	rtcmDir, navDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(rtcmDir, "Base1_202611010000"), rtcmFrame(1077), rtcmFrame(1046))
	d := New(Options{Sources: []Source{
		{Kind: "rtcm", Dir: rtcmDir, Prefix: "Base1_"},
		{Kind: "nav", Dir: navDir, Prefix: "Base1_nav_"},
	}})
	in := d.inputsForWindow("20261101", FullDay(), Outputs{Nav: true})
	if len(in) != 1 || len(in[0].files) != 1 || in[0].optional {
		t.Fatalf("inputs = %+v, want the RTCM file as the one required nav source", in)
	}

	// Observation-only RTCM (the X20P era) is not a navigation source.
	write(t, filepath.Join(rtcmDir, "Base1_202610010000"), rtcmFrame(1077))
	if in := d.inputsForWindow("20261001", FullDay(), Outputs{Nav: true}); len(in) != 0 {
		t.Errorf("MSM-only RTCM was offered as navigation: %+v", in)
	}
}

// On the swap day the UBX navigation archive holds the morning and the RTCM
// file the evening, so both are used, and the RTCM one may legitimately be
// empty for a window that ends before the swap.
func TestNavigationOnTheSwapDayUsesBothSources(t *testing.T) {
	rtcmDir, navDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(rtcmDir, "Base1_202610310000"), rtcmFrame(1077), rtcmFrame(1019))
	write(t, filepath.Join(navDir, "Base1_nav_202610310000"), ubxFrame(0x02, 0x13), ubxFrame(0x02, 0x15))
	d := New(Options{Sources: []Source{
		{Kind: "rtcm", Dir: rtcmDir, Prefix: "Base1_"},
		{Kind: "nav", Dir: navDir, Prefix: "Base1_nav_"},
	}})
	in := d.inputsForWindow("20261031", FullDay(), Outputs{Nav: true})
	if len(in) != 2 || in[0].optional || !in[1].optional {
		t.Fatalf("inputs = %+v, want the UBX nav file then an optional RTCM one", in)
	}
}
