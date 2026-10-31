package rinex

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Options configures a conversion.
type Options struct {
	ConvbinPath string
	Version     string // RINEX version, e.g. "3.04"
	// Frequencies is convbin's -f. It MUST match the receiver.
	//
	// The predecessor tool hardcoded 2, an F9P-era default. On the
	// triple-frequency ZED-X20P that silently dropped L5, E6 and B2a from
	// every file it produced. Defaults to 3 here, and the config validator
	// rejects 2 on a triple-frequency board.
	Frequencies int
	Timeout     time.Duration
	Log         *slog.Logger
}

// Result describes a completed conversion.
type Result struct {
	Files    []string
	Format   string
	Content  Content
	Duration time.Duration
	Stderr   string
}

// WindowArgs returns convbin's time-window arguments.
//
// Date and time must be separate arguments: RTKLIB reads them as two. Passing
// "2026/09/14 09:00:00" as one makes convbin consume the next flag as the time,
// and the range is silently ignored -- the whole file is converted instead.
//
// RTCM3 messages carry only a week-less time of week (or a GPS week modulo
// 1024), so convbin needs an approximate time to resolve the week. The window
// start is within a day of every message in the file, which is all it asks.
// The same two-argument rule applies to -tr.
func WindowArgs(format string, start, end time.Time) []string {
	const dateF, timeF = "2006/01/02", "15:04:05"
	s, e := start.UTC(), end.UTC()
	args := []string{
		"-ts", s.Format(dateF), s.Format(timeF),
		"-te", e.Format(dateF), e.Format(timeF),
	}
	if format == "rtcm3" {
		args = append(args, "-tr", s.Format(dateF), s.Format(timeF))
	}
	return args
}

// Convert turns one raw archive file into RINEX observation and navigation
// files covering [start, end].
func Convert(ctx context.Context, raw string, start, end time.Time, outDir, stem string,
	o Options) (*Result, error) {

	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.ConvbinPath == "" {
		return nil, fmt.Errorf("convbin path not configured")
	}
	if _, err := os.Stat(o.ConvbinPath); err != nil {
		return nil, fmt.Errorf("convbin not found at %s: %w "+
			"(build RTKLIB convbin for this architecture)", o.ConvbinPath, err)
	}
	if o.Frequencies <= 0 {
		o.Frequencies = 3
	}
	if o.Version == "" {
		o.Version = "3.04"
	}
	if o.Timeout <= 0 {
		o.Timeout = 20 * time.Minute
	}

	content, err := Sniff(raw, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("sniff %s: %w", raw, err)
	}
	if !content.HasNavigation() {
		// An RTCM stream without ephemeris messages carries observations only,
		// so a navigation file cannot be produced from it. Say so rather than
		// emitting a silently incomplete result.
		o.Log.Warn("raw file contains no ephemeris; navigation data will be unavailable",
			"file", filepath.Base(raw), "content", content.String())
	}
	format := content.Format()

	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return nil, err
	}
	obs := filepath.Join(outDir, stem+".obs")
	nav := filepath.Join(outDir, stem+".nav")

	args := []string{
		"-r", format,
		"-v", o.Version,
		"-f", fmt.Sprint(o.Frequencies),
	}
	args = append(args, WindowArgs(format, start, end)...)
	args = append(args, "-o", obs, "-n", nav, raw)

	cctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, o.ConvbinPath, args...)
	var errb strings.Builder
	cmd.Stderr = &errb
	t0 := time.Now()
	o.Log.Info("converting to RINEX", "file", filepath.Base(raw),
		"format", format, "freq", o.Frequencies, "version", o.Version)
	runErr := cmd.Run()
	dur := time.Since(t0)

	if cctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("convbin timed out after %s on %s", o.Timeout, filepath.Base(raw))
	}

	var produced []string
	for _, p := range []string{obs, nav} {
		if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
			produced = append(produced, p)
		} else {
			os.Remove(p) // drop empty outputs rather than offering a 0-byte file
		}
	}
	if len(produced) == 0 {
		return nil, fmt.Errorf("convbin produced no output for %s (exit: %v): %s",
			filepath.Base(raw), runErr, strings.TrimSpace(errb.String()))
	}
	return &Result{Files: produced, Format: format, Content: content,
		Duration: dur, Stderr: strings.TrimSpace(errb.String())}, nil
}
