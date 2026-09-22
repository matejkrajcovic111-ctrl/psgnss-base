package web

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The unit is fixed rather than taken from the request: an operator of the base
// has no reason to read another unit's journal through this endpoint, and a
// parameter would be an invitation.
func TestJournalUnitIsNotUserControlled(t *testing.T) {
	src := readAsset(t, "assets/app.js")
	if strings.Contains(src, "/api/logs?unit=") {
		t.Error("the UI passes a unit to the journal endpoint")
	}
	if journalUnit != "psgnss.service" {
		t.Errorf("journalUnit = %q", journalUnit)
	}
}

// A window is an argument to journalctl, so only known values may reach it.
func TestLogWindowIsAnAllowlist(t *testing.T) {
	for _, bad := range []string{"; reboot", "-1 year", "", "10y"} {
		if _, ok := logWindows[bad]; ok {
			t.Errorf("%q is accepted as a window", bad)
		}
	}
	if _, ok := logWindows["24h"]; !ok {
		t.Error("24h is missing from the allowed windows")
	}
}

func TestJournalLineParsing(t *testing.T) {
	cases := map[string]struct{ level, msg string }{
		`{"MESSAGE":"time=2026-09-21T10:00:00Z level=WARN msg=\"push-in rejected\"","__REALTIME_TIMESTAMP":"1758448800000000","PRIORITY":"6"}`: {"WARN", "push-in rejected"},
		`{"MESSAGE":"time=2026-09-21T10:00:00Z level=ERROR msg=boom","__REALTIME_TIMESTAMP":"1758448800000000","PRIORITY":"6"}`:                {"ERROR", "boom"},
		// No level in the message: fall back to the journal's own priority.
		`{"MESSAGE":"kernel said something","__REALTIME_TIMESTAMP":"1758448800000000","PRIORITY":"3"}`: {"ERROR", "kernel said"},
	}
	for raw, want := range cases {
		got, ok := parseJournalLine([]byte(raw))
		if !ok {
			t.Fatalf("did not parse: %s", raw)
		}
		if got.Level != want.level {
			t.Errorf("level = %q, want %q", got.Level, want.level)
		}
		if !strings.Contains(got.Message, want.msg) {
			t.Errorf("message %q does not contain %q", got.Message, want.msg)
		}
		if got.Time == "" {
			t.Error("timestamp was not decoded")
		}
	}
	// A message journald split into an array must be skipped, not guessed at.
	if _, ok := parseJournalLine([]byte(`{"MESSAGE":[104,105]}`)); ok {
		t.Error("a non-string message was accepted")
	}
	if _, ok := parseJournalLine([]byte("not json")); ok {
		t.Error("malformed JSON was accepted")
	}
}

// Each power action needs its own typed confirmation, and nothing reaches a
// shell: the action names a fixed argument list.
func TestPowerActionsRequireTheirOwnConfirmation(t *testing.T) {
	s := &Server{Log: testLogger()}
	for _, body := range []string{
		`{"action":"reboot","confirm":"SHUTDOWN"}`,
		`{"action":"shutdown","confirm":"REBOOT"}`,
		`{"action":"reboot","confirm":"yes"}`,
		`{"action":"rm -rf /","confirm":"REBOOT"}`,
		`{"action":"","confirm":""}`,
	} {
		rr := httptest.NewRecorder()
		s.handleSystemPower(rr, httptest.NewRequest("POST", "/api/system/power", strings.NewReader(body)))
		if rr.Code != 400 {
			t.Errorf("%s: status %d, want 400", body, rr.Code)
		}
	}
	for name, spec := range powerActions {
		if len(spec.args) != 1 || strings.ContainsAny(spec.args[0], " ;&|$") {
			t.Errorf("%s: argument %q is not a bare systemctl verb", name, spec.args)
		}
	}
}
