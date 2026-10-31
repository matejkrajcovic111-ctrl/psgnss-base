package hub

import (
	"bytes"
	"testing"
	"time"
)

// Replies copied byte for byte from the live UM980 (R4.10Build20739).
const (
	liveAck  = "$command,MODE,response: OK*5D\r\n"
	liveMode = "#MODE,95,GPS,FINE,2442,582350000,20739,0,18,759;MODE BASE 1 TIME 60 0 0 1,*76\r\n"
	liveList = "#UNILOGLIST,95,GPS,FINE,2442,582500000,20739,0,18,510;\r\n<\t2\r\n<\tRTCM1019 COM1 1 \r\n<\tBESTSATB COM1 1 \r\n\r\n"
)

func TestLiveReceiverRepliesAllFrameBetweenBinaryLogs(t *testing.T) {
	bin := unicoreBinary(2124, []byte{1, 2, 3, 4})
	var s bytes.Buffer
	s.Write(bin)
	s.WriteString(liveAck)
	s.WriteString(liveMode)
	s.Write(bin)
	s.WriteString(liveList)
	s.Write(bin)

	frames, sc := scanAll(t, &s)
	want := []Proto{ProtoUnicore, ProtoNMEA, ProtoUnicoreText, ProtoUnicore, ProtoUnicoreText, ProtoUnicore}
	if len(frames) != len(want) {
		t.Fatalf("got %d frames, want %d", len(frames), len(want))
	}
	for i, p := range want {
		if frames[i].Proto != p {
			t.Errorf("frame %d is %s, want %s", i, frames[i].Proto, p)
		}
	}
	if string(frames[2].Raw) != liveMode || string(frames[4].Raw) != liveList {
		t.Errorf("text frames were not whole: %q %q", frames[2].Raw, frames[4].Raw)
	}
	if sc.BytesDropped != 0 {
		t.Errorf("dropped %d bytes", sc.BytesDropped)
	}
}

func TestShortChecksumLogWithWrongChecksumIsDropped(t *testing.T) {
	var s bytes.Buffer
	s.WriteString("#MODE,95,GPS,FINE,2442,582350000,20739,0,18,759;MODE BASE 1 TIME 60 0 0 1,*77\r\n")
	s.Write(unicoreBinary(2124, []byte{1, 2, 3, 4}))
	frames, _ := scanAll(t, &s)
	if len(frames) != 1 || frames[0].Proto != ProtoUnicore {
		t.Fatalf("frames %+v", frames)
	}
}

func TestDecimationKeepsASourceAlreadyAtTheInterval(t *testing.T) {
	f, err := NewFilter(ProtoRTCM3, []FilterSpec{{Type: 1230, Interval: 10}})
	if err != nil {
		t.Fatal(err)
	}
	fr := Frame{Proto: ProtoRTCM3, Type: 1230}
	now := time.Unix(1000, 0)
	passed := 0
	for i := 0; i < 30; i++ {
		// A few milliseconds early or late, as the receiver delivers it.
		jitter := time.Duration(i%3-1) * 5 * time.Millisecond
		if f.Pass(fr, now.Add(time.Duration(i)*10*time.Second+jitter)) {
			passed++
		}
	}
	if passed != 30 {
		t.Fatalf("passed %d of 30", passed)
	}
}

func TestDecimationOfOneHertzStaysOnTheInterval(t *testing.T) {
	f, err := NewFilter(ProtoRTCM3, []FilterSpec{{Type: 1077, Interval: 10}})
	if err != nil {
		t.Fatal(err)
	}
	fr := Frame{Proto: ProtoRTCM3, Type: 1077}
	now := time.Unix(1000, 0)
	passed := 0
	for i := 0; i < 100; i++ {
		if f.Pass(fr, now.Add(time.Duration(i)*time.Second)) {
			passed++
		}
	}
	if passed != 10 {
		t.Fatalf("passed %d of 100, want 10", passed)
	}
}
