package unicore_test

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/unicore"
	"github.com/psgnss/psgnss-base/internal/unicore/unicoretest"
)

func session(t *testing.T, f *unicoretest.Fake) *unicore.Session {
	t.Helper()
	p, _ := f.Open()
	s := unicore.NewSession(p)
	s.Timeout, s.Quiet, s.Log = time.Second, 60*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSessionReturnsAckAndTheVersionLog(t *testing.T) {
	s := session(t, unicoretest.New())
	r, err := s.Do("VERSIONA")
	if err != nil || !r.OK() {
		t.Fatalf("Do: %v %+v", err, r)
	}
	v, err := unicore.ParseVersion(r)
	if err != nil {
		t.Fatal(err)
	}
	want := unicore.Version{Model: "UM980", Firmware: "R4.10Build20739", Auth: "HRPT00-S10C-P",
		PN: "2310415000001", Serial: "MD22C5260607530", EfuseID: "ff3a80e655b8f36c", Date: "2025/08/12"}
	if v != want {
		t.Fatalf("version %+v, want %+v", v, want)
	}
}

func TestSessionCollectsConfigLinesAfterTheAck(t *testing.T) {
	s := session(t, unicoretest.New())
	r, err := s.Do("config")
	if err != nil || !r.OK() {
		t.Fatalf("Do: %v %+v", err, r)
	}
	cfg := unicore.ParseConfig(r)
	if len(cfg) != 6 || cfg[3].Name != "COM1" || cfg[3].Line != "CONFIG COM1 115200" {
		t.Fatalf("config %+v", cfg)
	}
}

func TestSessionParsesModeUnilogAndMask(t *testing.T) {
	s := session(t, unicoretest.New())
	r, _ := s.Do("MODE")
	m, err := unicore.ParseMode(r)
	if err != nil || m.Role != "BASE" || m.ID != "1" || m.Kind != "time" || m.TimeSeconds != "60" ||
		m.Command != "MODE BASE 1 TIME 60 0" {
		t.Fatalf("mode %+v %v", m, err)
	}
	r, _ = s.Do("UNILOGLIST")
	outs, err := unicore.ParseOutputs(r)
	if err != nil || len(outs) != 5 || outs[0].Message != "RTCM1019" || outs[0].Port != "COM1" || outs[0].Rate != "1" {
		t.Fatalf("outputs %+v %v", outs, err)
	}
	r, _ = s.Do("MASK")
	if masks := unicore.ParseMasks(r); len(masks) != 1 || masks[0] != "MASK 5.000000" {
		t.Fatalf("masks %v", masks)
	}
}

func TestParseModeFixedKeepsPrintedDigits(t *testing.T) {
	m := unicore.ParseModeText("MODE BASE 7 48.12345678901 17.10987654321 301.2345,")
	if m.Kind != "fixed" || m.ID != "7" || m.Lat != "48.12345678901" || m.Height != "301.2345" ||
		m.Command != "MODE BASE 7 48.12345678901 17.10987654321 301.2345" {
		t.Fatalf("%+v", m)
	}
}

func TestSessionReportsARejectedCommand(t *testing.T) {
	f := unicoretest.New()
	f.Reject["BOGUS"] = true
	s := session(t, f)
	r, err := s.Do("bogus 1")
	if err != nil || r.OK() || !strings.Contains(r.Status, "PARSING") || r.Err() == nil {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestSessionTimesOutWithoutAnAck(t *testing.T) {
	f := unicoretest.New()
	f.Silent = true
	s := session(t, f)
	s.Timeout = 150 * time.Millisecond
	r, err := s.Do("VERSIONA")
	if err != nil || r.Acked || r.Err() == nil {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestSessionRejectsMultiLineCommands(t *testing.T) {
	s := session(t, unicoretest.New())
	if _, err := s.Do("MODE\r\nFRESET"); err == nil {
		t.Fatal("a command with a line break was sent")
	}
}

// scripted hands over fixed chunks, like a pipe delivering a reply in pieces.
type scripted struct {
	mu     sync.Mutex
	chunks []string
}

func (p *scripted) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.chunks) == 0 {
		time.Sleep(10 * time.Millisecond)
		return 0, nil
	}
	n := copy(b, p.chunks[0])
	p.chunks = p.chunks[1:]
	return n, nil
}
func (p *scripted) Write(b []byte) (int, error) { return len(b), nil }
func (p *scripted) Close() error                { return nil }

func TestSessionHandlesLinesBeforeTheAckAndSplitReads(t *testing.T) {
	cfg := "CONFIG,PPS,CONFIG PPS ENABLE GPS POSITIVE 500000 1000 0 0"
	ack := "command,CONFIG,response: OK"
	cs := func(s string) string {
		var x byte
		for i := 0; i < len(s); i++ {
			x ^= s[i]
		}
		return "*" + strings.ToUpper(string("0123456789abcdef"[x>>4])+string("0123456789abcdef"[x&15]))
	}
	full := "$" + cfg + cs(cfg) + "\r\n$GNGGA,1,2,3,4*00\r\n$" + ack + cs(ack) + "\r\n"
	p := &scripted{}
	s := unicore.NewSession(p)
	s.Timeout, s.Quiet = time.Second, 50*time.Millisecond
	defer s.Close()
	// The reply has to arrive after Do has written, so feed it from the write.
	go func() {
		time.Sleep(30 * time.Millisecond)
		p.mu.Lock()
		for i := 0; i < len(full); i += 7 {
			p.chunks = append(p.chunks, full[i:min(i+7, len(full))])
		}
		p.mu.Unlock()
	}()
	r, err := s.Do("CONFIG")
	if err != nil || !r.OK() || len(r.Lines) != 1 || len(unicore.ParseConfig(r)) != 1 {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestDiffAndRestoreCommands(t *testing.T) {
	f := unicoretest.New()
	s := session(t, f)
	before, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"CONFIG PPS DISABLE", "UNLOG COM1 RTCM1019", "RTCM1077 COM1 5",
		"RTCM1124 COM1 1", "MASK 15", "MASK GPS", "MODE BASE 2 TIME 30"} {
		if r, _ := s.Do(c); !r.OK() {
			t.Fatalf("%s: %+v", c, r)
		}
	}
	after, _ := s.Snapshot()
	cmds, _ := unicore.RestoreCommands(unicore.Diff(before, after))
	got := strings.Join(cmds, "|")
	for _, w := range []string{"MASK 5.000000", "UNMASK GPS", "CONFIG PPS ENABLE GPS POSITIVE 500000 1000 0 0",
		"MODE BASE 1 TIME 60 0", "RTCM1019 COM1 1", "RTCM1077 COM1 1", "UNLOG COM1 RTCM1124"} {
		if !strings.Contains(got, w) {
			t.Errorf("restore commands lack %q: %v", w, cmds)
		}
	}
}

func TestTxnRevertRestoresEverythingAndCommitSaves(t *testing.T) {
	f := unicoretest.New()
	s := session(t, f)
	mode0, out0 := f.State()
	txn, replies, err := s.Apply([]string{"UNLOG COM1 RTCM1019", "RTCM1077 COM1 5"}, time.Minute, nil, nil)
	if err != nil || len(replies) != 2 || len(txn.Changes()) != 2 {
		t.Fatalf("apply: %v %v %+v", err, replies, txn)
	}
	if err := txn.Revert(); err != nil {
		t.Fatal(err)
	}
	mode1, out1 := f.State()
	if mode0 != mode1 || strings.Join(out0, ",") != strings.Join(out1, ",") {
		t.Fatalf("not restored: %v vs %v", out0, out1)
	}
	if f.Saved {
		t.Fatal("revert saved the configuration")
	}
	txn, _, err = s.Apply([]string{"CONFIG PPS DISABLE"}, time.Minute, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := txn.Commit(); err != nil || !f.Saved {
		t.Fatalf("commit: %v saved=%v", err, f.Saved)
	}
}

func TestTxnAutoRevertsWhenNotKept(t *testing.T) {
	f := unicoretest.New()
	s := session(t, f)
	txn, _, err := s.Apply([]string{"CONFIG PPS DISABLE"}, 150*time.Millisecond, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-txn.Reverted:
	case <-time.After(5 * time.Second):
		t.Fatal("no auto-revert")
	}
	if f.Config["PPS"] != "CONFIG PPS ENABLE GPS POSITIVE 500000 1000 0 0" {
		t.Fatalf("PPS = %q", f.Config["PPS"])
	}
}

func TestTxnRefusedCommandLeavesNothingBehind(t *testing.T) {
	f := unicoretest.New()
	f.Reject["MASK 99"] = true
	s := session(t, f)
	_, replies, err := s.Apply([]string{"CONFIG PPS DISABLE", "MASK 99"}, time.Minute, nil, nil)
	if err == nil || len(replies) != 2 || replies[1].Status == "" {
		t.Fatalf("err %v replies %+v", err, replies)
	}
	if f.Config["PPS"] != "CONFIG PPS ENABLE GPS POSITIVE 500000 1000 0 0" {
		t.Fatalf("the accepted first command was not undone: %q", f.Config["PPS"])
	}
}

func TestRedactHidesPositions(t *testing.T) {
	for _, s := range []string{"MODE BASE 1 48.12345678901 17.1234567 301.23",
		"#BESTNAVA,97,GPS;SOL_COMPUTED,SINGLE,48.123,17.123,300.0,43.0*aa"} {
		got := unicore.Redact(s)
		if strings.Contains(got, "48.1") || strings.Contains(got, "17.1") {
			t.Errorf("Redact(%q) = %q", s, got)
		}
	}
	if got := unicore.Redact("MODE BASE 1 TIME 60"); got != "MODE BASE 1 TIME 60" {
		t.Errorf("redacted a non-position: %q", got)
	}
}
