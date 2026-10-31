package hub

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"testing"
	"time"
)

// unicoreBinary builds a CRC-valid Unicore binary frame with the given
// message ID and data.
func unicoreBinary(id uint16, data []byte) []byte {
	h := make([]byte, unicoreHeaderLen)
	h[0], h[1], h[2] = 0xAA, 0x44, 0xB5
	binary.LittleEndian.PutUint16(h[4:6], id)
	binary.LittleEndian.PutUint16(h[6:8], uint16(len(data)))
	b := append(h, data...)
	return binary.LittleEndian.AppendUint32(b, crc32Unicore(b))
}

// unicoreText builds a CRC-valid Unicore ASCII log.
func unicoreText(body string) []byte {
	return []byte(fmt.Sprintf("#%s*%08x\r\n", body, crc32Unicore([]byte(body))))
}

func scanAll(t *testing.T, r io.Reader) ([]Frame, *Scanner) {
	t.Helper()
	sc := NewScanner(r, 8192)
	var out []Frame
	for {
		f, err := sc.Next()
		if err == io.EOF {
			return out, sc
		}
		if err != nil {
			t.Fatal(err)
		}
		f.Raw = append([]byte(nil), f.Raw...)
		out = append(out, f)
	}
}

// The CRC is NovAtel's CRC32, checked against a VERSIONA reply captured from
// the station's own UM980 on 2026-10-31.
func TestUnicoreCRCMatchesTheReceiver(t *testing.T) {
	body := `VERSIONA,95,GPS,FINE,2442,577931000,20739,0,18,510;"UM980","R4.10Build20739","HRPT00-S10C-P","2310415000001-MD22C5260607530","ff3a80e655b8f36c","2025/08/12"`
	if got := crc32Unicore([]byte(body)); got != 0xa34d9369 {
		t.Fatalf("crc = %08x, receiver sent a34d9369", got)
	}
}

func TestUnicoreFramesInterleaveWithRTCMAndNMEA(t *testing.T) {
	rtcm := []byte{0xD3, 0x00, 0x02, 0x3E, 0xD0}
	rtcm = append(rtcm, byte(crc24q(rtcm)>>16), byte(crc24q(rtcm)>>8), byte(crc24q(rtcm)))
	var s bytes.Buffer
	s.Write(rtcm)
	s.Write(unicoreBinary(2124, []byte{1, 2, 3, 4}))
	s.WriteString("$command,MODE,response: OK*5D\r\n")
	// A multi-line ASCII log, as UNILOGLIST answers.
	s.Write(unicoreText("UNILOGLIST,94,GPS,FINE,2442,1,20739,0,18,1;\r\n<\t1\r\n<\tRTCM1077 COM1 1 \r\n"))
	s.Write(rtcm)

	frames, sc := scanAll(t, &s)
	want := []Proto{ProtoRTCM3, ProtoUnicore, ProtoNMEA, ProtoUnicoreText, ProtoRTCM3}
	if len(frames) != len(want) {
		t.Fatalf("got %d frames, want %d", len(frames), len(want))
	}
	for i, p := range want {
		if frames[i].Proto != p {
			t.Errorf("frame %d is %s, want %s", i, frames[i].Proto, p)
		}
	}
	if frames[1].Type != 2124 {
		t.Errorf("binary frame type = %d, want the message ID 2124", frames[1].Type)
	}
	if sc.BytesDropped != 0 {
		t.Errorf("dropped %d bytes from a clean stream", sc.BytesDropped)
	}
}

func TestUnicoreBadCRCIsDroppedAndTheNextFrameSurvives(t *testing.T) {
	bad := unicoreBinary(2124, []byte{9, 9, 9})
	bad[len(bad)-1] ^= 0xFF
	var s bytes.Buffer
	s.Write(bad)
	s.Write(unicoreText("BESTNAVA,1;x"))
	frames, _ := scanAll(t, &s)
	if len(frames) != 1 || frames[0].Proto != ProtoUnicoreText {
		t.Fatalf("frames = %+v, want only the valid ASCII log", frames)
	}
}

// A '#' inside binary data must not stall the stream waiting for a '*' that
// will never come: the receiver's RTCM is full of arbitrary bytes.
func TestStrayHashDoesNotStallTheStream(t *testing.T) {
	rtcm := []byte{0xD3, 0x00, 0x02, 0x3E, 0xD0}
	rtcm = append(rtcm, byte(crc24q(rtcm)>>16), byte(crc24q(rtcm)>>8), byte(crc24q(rtcm)))
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte{'#', 'G', 0x00, 0xFF})
		pw.Write(rtcm)
		// Keep the writer open: a stalled scanner would block here forever.
	}()
	sc := NewScanner(pr, 8192)
	got := make(chan Frame, 1)
	go func() {
		f, err := sc.Next()
		if err == nil {
			got <- f
		}
	}()
	select {
	case f := <-got:
		if f.Proto != ProtoRTCM3 {
			t.Fatalf("first frame is %s, want the RTCM after the stray '#'", f.Proto)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scanner stalled on a stray '#'")
	}
	pw.Close()
}

// Replays the capture taken from the station's UM980 on 2026-10-31, when it
// was emitting RTCM plus SATSINFO, BESTNAV and BESTSAT in binary and ASCII.
// The capture stays out of the repository (it carries the antenna position),
// so the test skips without it.
func TestFramerAgainstUM980Capture(t *testing.T) {
	path := os.Getenv("PSGNSS_UM980_CAPTURE")
	if path == "" {
		t.Skip("PSGNSS_UM980_CAPTURE not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	frames, sc := scanAll(t, f)
	count := map[string]int{}
	for _, fr := range frames {
		count[fmt.Sprintf("%s/%d", fr.Proto, fr.Type)]++
		if fr.Proto == ProtoUnicoreText {
			count["text"]++
		}
	}
	t.Logf("frames by kind: %v", count)
	t.Logf("read %d framed %d dropped %d resyncs %d", sc.BytesRead, sc.BytesFramed, sc.BytesDropped, sc.Resyncs)
	for _, k := range []string{"unicore/2124", "rtcm3/1077", "rtcm3/1087"} {
		if count[k] < 15 {
			t.Errorf("%s: %d frames in a 20 s capture", k, count[k])
		}
	}
	if count["text"] < 30 {
		t.Errorf("only %d ASCII logs framed", count["text"])
	}
	// Only the partial frame at the start of the capture may be lost.
	if sc.BytesDropped > 4096 {
		t.Errorf("dropped %d bytes", sc.BytesDropped)
	}
}
