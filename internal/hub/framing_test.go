package hub

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// capturePath locates the Stage 0 reference capture. It lives in the gitignored
// snapshot/raw, so tests that need it skip when it is absent.
func capturePath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "snapshot", "raw", "5003-capture.bin")
	if _, err := os.Stat(p); err != nil {
		t.Skip("Stage 0 capture not present; skipping replay test")
	}
	return p
}

// TestFramerAgainstStage0Capture replays 180 s of the real receiver stream and
// asserts the frame counts independently measured during Stage 0.
//
// The decisive assertion is BytesDropped == 0: every byte the receiver emitted
// must land inside a checksum-valid frame. If the framer ever regresses, this
// catches it against real hardware output rather than a synthetic fixture.
func TestFramerAgainstStage0Capture(t *testing.T) {
	f, err := os.Open(capturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()

	sc := NewScanner(f, 32768)
	rtcm := map[int]int{}
	ubx := map[int]int{}
	var total int
	for {
		fr, err := sc.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		total++
		switch fr.Proto {
		case ProtoRTCM3:
			rtcm[fr.Type]++
		case ProtoUBX:
			ubx[fr.Type]++
		}
	}

	if sc.BytesDropped != 0 {
		t.Errorf("BytesDropped = %d, want 0 (every byte must be inside a valid frame)", sc.BytesDropped)
	}
	if sc.Resyncs != 0 {
		t.Errorf("Resyncs = %d, want 0", sc.Resyncs)
	}
	if sc.BytesFramed != fi.Size() {
		t.Errorf("BytesFramed = %d, want %d (whole file)", sc.BytesFramed, fi.Size())
	}

	// Measured independently in Stage 0: all seven RTCM types at exactly 1 Hz
	// over 180 s. The receiver emits MSM4 and MSM7 concurrently, which is what
	// makes transcoding unnecessary.
	wantRTCM := map[int]int{1005: 180, 1074: 180, 1077: 180, 1094: 180, 1097: 180, 1124: 180, 1127: 180}
	for typ, want := range wantRTCM {
		if got := rtcm[typ]; got != want {
			t.Errorf("RTCM %d count = %d, want %d", typ, got, want)
		}
	}
	for typ, got := range rtcm {
		if _, ok := wantRTCM[typ]; !ok {
			t.Errorf("unexpected RTCM type %d (x%d)", typ, got)
		}
	}

	// UBX: NAV-PVT, NAV-SAT, NAV-SIG, RXM-RAWX at 1 Hz; RXM-SFRBX ~28 Hz.
	for _, c := range []struct {
		name string
		typ  int
		want int
	}{
		{"NAV-PVT", 0x0107, 180}, {"NAV-SAT", 0x0135, 180},
		{"NAV-SIG", 0x0143, 180}, {"RXM-RAWX", 0x0215, 180},
	} {
		if got := ubx[c.typ]; got != c.want {
			t.Errorf("UBX %s count = %d, want %d", c.name, got, c.want)
		}
	}
	if got := ubx[0x0213]; got < 4000 || got > 6000 {
		t.Errorf("UBX RXM-SFRBX count = %d, want ~5049", got)
	}
}

// TestFramerReassemblesAcrossReadBoundaries feeds the capture one byte at a
// time. A framer that assumed frames arrive whole would fail this.
func TestFramerReassemblesAcrossReadBoundaries(t *testing.T) {
	data, err := os.ReadFile(capturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	data = data[:min(len(data), 200000)]

	whole := frameCount(t, bytes.NewReader(data))
	dribbled := frameCount(t, &iotest1{data: data})
	if whole != dribbled {
		t.Errorf("frame count differs by read size: whole=%d one-byte-reads=%d", whole, dribbled)
	}
}

type iotest1 struct {
	data []byte
	pos  int
}

func (r *iotest1) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

func frameCount(t *testing.T, r io.Reader) int {
	t.Helper()
	sc := NewScanner(r, 8192)
	n := 0
	for {
		_, err := sc.Next()
		if err == io.EOF {
			return n
		}
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
	}
}

// TestFramerResyncsAfterGarbage confirms corruption costs only the damaged
// frame, not the rest of the stream.
func TestFramerResyncsAfterGarbage(t *testing.T) {
	data, err := os.ReadFile(capturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	clean := data[:100000]
	want := frameCount(t, bytes.NewReader(clean))

	// Splice 64 bytes of junk into the middle.
	var buf bytes.Buffer
	buf.Write(clean[:50000])
	buf.Write(bytes.Repeat([]byte{0xD3, 0xFF, 0xAA}, 21))
	buf.Write(clean[50000:])
	got := frameCount(t, bytes.NewReader(buf.Bytes()))

	// We lose at most the frame straddling the splice.
	if got < want-2 {
		t.Errorf("after garbage got %d frames, want >= %d (resync lost too much)", got, want-2)
	}
}

func TestFilterDecimation(t *testing.T) {
	f, err := NewFilter(ProtoRTCM3, []FilterSpec{
		{Type: 1006, Interval: 10},
		{Type: 1077, Interval: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	f1006 := Frame{Proto: ProtoRTCM3, Type: 1006}
	f1077 := Frame{Proto: ProtoRTCM3, Type: 1077}
	f1074 := Frame{Proto: ProtoRTCM3, Type: 1074}

	if !f.Pass(f1006, base) {
		t.Error("first 1006 should pass")
	}
	if f.Pass(f1006, base.Add(5*time.Second)) {
		t.Error("1006 at +5s should be decimated away")
	}
	if !f.Pass(f1006, base.Add(10*time.Second)) {
		t.Error("1006 at +10s should pass")
	}
	for i := 0; i < 5; i++ {
		if !f.Pass(f1077, base.Add(time.Duration(i)*time.Second)) {
			t.Errorf("1077 at +%ds should always pass", i)
		}
	}
	if f.Pass(f1074, base) {
		t.Error("1074 is not selected and must not pass")
	}
	if f.Pass(Frame{Proto: ProtoUBX, Type: 0x0107}, base) {
		t.Error("UBX must not pass an RTCM filter")
	}
}

func TestPassthroughPassesEverything(t *testing.T) {
	f := NewPassthrough()
	now := time.Now()
	for _, fr := range []Frame{
		{Proto: ProtoUBX, Type: 0x0107}, {Proto: ProtoRTCM3, Type: 1005},
		{Proto: ProtoNMEA},
	} {
		if !f.Pass(fr, now) {
			t.Errorf("passthrough dropped %v", fr.Proto)
		}
	}
}
