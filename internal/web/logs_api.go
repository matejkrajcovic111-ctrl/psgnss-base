package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The journal of this daemon, read through journalctl.
//
// Until now every diagnosis in this project ended in an SSH session. The unit
// is fixed, not taken from the request: there is no reason for an operator of
// the base to read another unit's journal through this endpoint, and a
// parameter here would be an invitation.
const journalUnit = "psgnss.service"

// Windows an operator may ask for. An allowlist rather than a free string,
// because the value is an argument to journalctl.
var logWindows = map[string]string{
	"15m": "-15 min", "1h": "-1 hour", "6h": "-6 hours",
	"24h": "-24 hours", "7d": "-7 days",
}

const (
	logLimitDefault = 500
	logLimitMax     = 5000
)

type logLine struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	lines, window, limit, err := s.readJournal(r)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSONCompressed(w, r, http.StatusOK, map[string]any{
		"unit": journalUnit, "window": window, "limit": limit,
		"lines": lines, "levels": []string{"ERROR", "WARN", "INFO", "DEBUG"},
	})
}

// handleLogsDownload returns the same selection as plain text, for attaching to
// a bug report.
func (s *Server) handleLogsDownload(w http.ResponseWriter, r *http.Request) {
	lines, window, _, err := s.readJournal(r)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s · last %s · %d lines · generated %s\n",
		journalUnit, window, len(lines), time.Now().UTC().Format(time.RFC3339))
	for _, l := range lines {
		fmt.Fprintf(&b, "%s\n", l.Message)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q",
		"psgnss-log-"+time.Now().UTC().Format("20060102-150405")+".txt"))
	_, _ = w.Write(b.Bytes())
}

func (s *Server) readJournal(r *http.Request) ([]logLine, string, int, error) {
	q := r.URL.Query()
	window := q.Get("window")
	since, ok := logWindows[window]
	if !ok {
		window, since = "1h", logWindows["1h"]
	}
	limit := logLimitDefault
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = min(v, logLimitMax)
	}
	path, err := exec.LookPath("journalctl")
	if err != nil {
		return nil, window, limit, fmt.Errorf("journalctl is not available on this host")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-u", journalUnit, "--no-pager",
		"-o", "json", "--since", since, "-n", strconv.Itoa(limit))
	out, err := cmd.Output()
	if err != nil {
		return nil, window, limit, fmt.Errorf("could not read the journal: %w", err)
	}
	level, needle := strings.ToUpper(q.Get("level")), strings.ToLower(q.Get("q"))
	lines := make([]logLine, 0, 64)
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		l, ok := parseJournalLine(sc.Bytes())
		if !ok {
			continue
		}
		if level != "" && level != "ALL" && l.Level != level {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(l.Message), needle) {
			continue
		}
		lines = append(lines, l)
	}
	return lines, window, limit, sc.Err()
}

// parseJournalLine turns one journalctl JSON record into a display line. The
// daemon logs key=value text, so the level comes from the message itself;
// journal priority only reflects the stream, not what slog decided.
func parseJournalLine(b []byte) (logLine, bool) {
	var rec struct {
		Message   any    `json:"MESSAGE"`
		Timestamp string `json:"__REALTIME_TIMESTAMP"`
		Priority  string `json:"PRIORITY"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return logLine{}, false
	}
	msg, ok := rec.Message.(string)
	if !ok { // a binary or split message: skip rather than guess
		return logLine{}, false
	}
	l := logLine{Message: msg, Level: levelOf(msg, rec.Priority)}
	if us, err := strconv.ParseInt(rec.Timestamp, 10, 64); err == nil {
		l.Time = time.UnixMicro(us).UTC().Format(time.RFC3339)
	}
	return l, true
}

func levelOf(msg, priority string) string {
	if i := strings.Index(msg, "level="); i >= 0 {
		rest := msg[i+len("level="):]
		if j := strings.IndexByte(rest, ' '); j > 0 {
			return strings.ToUpper(rest[:j])
		}
	}
	switch priority {
	case "0", "1", "2", "3":
		return "ERROR"
	case "4":
		return "WARN"
	case "7":
		return "DEBUG"
	}
	return "INFO"
}
