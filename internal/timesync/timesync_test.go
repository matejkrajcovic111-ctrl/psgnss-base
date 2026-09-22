package timesync

import (
	"context"
	"encoding/binary"
	"math"
	"net"
	"testing"
	"time"
)

// navPVT builds a framed NAV-PVT message with the fields this package reads.
func navPVT(t time.Time, tAccNS uint32, valid byte) []byte {
	p := make([]byte, 92)
	binary.LittleEndian.PutUint16(p[4:6], uint16(t.Year()))
	p[6] = byte(t.Month())
	p[7] = byte(t.Day())
	p[8] = byte(t.Hour())
	p[9] = byte(t.Minute())
	p[10] = byte(t.Second())
	p[11] = valid
	binary.LittleEndian.PutUint32(p[12:16], tAccNS)
	binary.LittleEndian.PutUint32(p[16:20], uint32(int32(t.Nanosecond())))
	frame := append([]byte{0xB5, 0x62, 0x01, 0x07, 92, 0}, p...)
	return append(frame, 0, 0) // checksum is not read here
}

func TestParseNavPVTTime(t *testing.T) {
	want := time.Date(2026, 9, 20, 18, 30, 45, 123456789, time.UTC)
	got, ok := parseNavPVTTime(navPVT(want, 25_000, 0x07))
	if !ok {
		t.Fatal("a valid NAV-PVT was rejected")
	}
	if !got.utc.Equal(want) {
		t.Errorf("time = %s, want %s", got.utc, want)
	}
	if got.accuracy != 25*time.Microsecond {
		t.Errorf("accuracy = %s", got.accuracy)
	}
	if !got.resolved {
		t.Error("fullyResolved was not read")
	}
}

func TestParseNavPVTRejectsUnresolvedAndForeignFrames(t *testing.T) {
	now := time.Now().UTC()
	if got, ok := parseNavPVTTime(navPVT(now, 10, 0x03)); !ok || got.resolved {
		t.Error("a fix without fullyResolved must not be reported as resolved")
	}
	other := navPVT(now, 10, 0x07)
	other[3] = 0x35 // NAV-SAT, not NAV-PVT
	if _, ok := parseNavPVTTime(other); ok {
		t.Error("a different UBX message was accepted as NAV-PVT")
	}
	if _, ok := parseNavPVTTime([]byte{0xB5, 0x62, 0x01, 0x07, 1, 0, 0, 0, 0}); ok {
		t.Error("a truncated frame was accepted")
	}
	rtcm := []byte{0xD3, 0x00, 0x10}
	if _, ok := parseNavPVTTime(rtcm); ok {
		t.Error("an RTCM frame was accepted")
	}
}

// The sample layout is an ABI, not a choice: chrony rejects anything else.
func TestEncodeSampleMatchesChronyABI(t *testing.T) {
	tv := time.Unix(1789930000, 123_456_000).UTC()
	b := encodeSample(tv, -0.0042)
	if len(b) != 40 {
		t.Fatalf("sample is %d bytes, chrony expects 40", len(b))
	}
	if got := binary.LittleEndian.Uint64(b[0:8]); got != 1789930000 {
		t.Errorf("tv_sec = %d", got)
	}
	if got := binary.LittleEndian.Uint64(b[8:16]); got != 123456 {
		t.Errorf("tv_usec = %d, want microseconds not nanoseconds", got)
	}
	if got := math.Float64frombits(binary.LittleEndian.Uint64(b[16:24])); math.Abs(got+0.0042) > 1e-12 {
		t.Errorf("offset = %v", got)
	}
	if got := binary.LittleEndian.Uint32(b[24:28]); got != 0 {
		t.Errorf("pulse = %d; this is not a PPS edge", got)
	}
	if got := binary.LittleEndian.Uint32(b[28:32]); got != leapNormal {
		t.Errorf("leap = %d", got)
	}
	if got := binary.LittleEndian.Uint32(b[36:40]); got != chronySampleMagic {
		t.Errorf("magic = %#x, want %#x", got, chronySampleMagic)
	}
}

func TestOffsetSignIsReferenceMinusLocal(t *testing.T) {
	// A system clock two seconds behind the receiver must produce a positive
	// offset: that is the correction chrony has to apply.
	local := time.Unix(1789930000, 0)
	reference := local.Add(2 * time.Second)
	b := encodeSample(local, reference.Sub(local).Seconds())
	if got := math.Float64frombits(binary.LittleEndian.Uint64(b[16:24])); got != 2 {
		t.Errorf("offset = %v, want +2", got)
	}
}

func TestDisabledSenderDoesNothing(t *testing.T) {
	s := New(nil, Options{}, nil)
	if st := s.Status(); st.Enabled || st.State != "stopped" {
		t.Errorf("a sender with no socket looks enabled: %+v", st)
	}
	// Run must return immediately rather than dereferencing a nil hub.
	done := make(chan struct{})
	go func() { s.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return with no socket configured")
	}
}

// The socket type is the thing that broke in production: chrony's refclock
// socket is SOCK_DGRAM, and a stream dial against it fails.
func TestDialChronyUsesADatagramSocket(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/chrony.sock"
	ln, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	conn, err := dialChrony(path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	sent := time.Unix(1789930000, 500_000_000)
	if _, err := conn.Write(encodeSample(sent, 0.0125)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 64)
	if err := ln.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := ln.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != sampleSize {
		t.Fatalf("chrony received %d bytes, expected %d", n, sampleSize)
	}
	if got := binary.LittleEndian.Uint32(buf[36:40]); got != chronySampleMagic {
		t.Errorf("magic = %#x", got)
	}
	if got := math.Float64frombits(binary.LittleEndian.Uint64(buf[16:24])); math.Abs(got-0.0125) > 1e-12 {
		t.Errorf("offset = %v", got)
	}

	// A stream dial must fail, which is what the old code did every time.
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		t.Error("a stream dial against a datagram socket succeeded; the test proves nothing")
	}
}
