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
	if !content.HasUBX() {
		// RTCM carries observations but no ephemeris, so a navigation file
		// cannot be produced from it. Say so rather than emitting a silently
		// incomplete result.
		o.Log.Warn("raw file contains no UBX; navigation data will be unavailable",
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
		// Date and time must be separate arguments; see downloader.convert.
		"-ts", start.UTC().Format("2006/01/02"), start.UTC().Format("15:04:05"),
		"-te", end.UTC().Format("2006/01/02"), end.UTC().Format("15:04:05"),
		"-o", obs,
		"-n", nav,
		raw,
	}

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
