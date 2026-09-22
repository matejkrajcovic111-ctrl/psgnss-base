// Package downloader folds in the GNSS_Dashboard RINEX downloader: pick a UTC
// range, convert the raw archive to RINEX, and get a zip back.
//
// Behaviour is kept deliberately close to the original Flask blueprint so the
// output is the same: the same convbin flags, the same set of navigation files,
// the same GNSS_YYYYMMDD.zip naming, and the same short-lived download tokens.
package downloader

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/psgnss/psgnss-base/internal/rinex"
)

// PendingTTL matches the original: a processed zip stays available for a short
// window, then its temporary directory is removed.
const PendingTTL = 15 * time.Minute

// Source is one archive directory to search for raw day files.
type Source struct {
	Kind   string // "ubx" | "rtcm" | "nav"
	Dir    string
	Prefix string // e.g. "Base1_" or "Base1_ubx_"
	HasUBX bool   // whether files here should be inspected for UBX NAV messages
	// Spool marks the local working copy of a file still being written. It is
	// more current than the share, so it wins for the day being recorded.
	Spool bool
}

// Options configures the downloader.
type Options struct {
	Sources     []Source
	ConvbinPath string
	Version     string
	Frequencies int
	// Station metadata written into the RINEX header. A post-processing
	// service reads the marker, antenna, receiver and approximate position
	// from the file; without them a submission is anonymous and some services
	// refuse it.
	Marker    string
	Antenna   string
	Receiver  string
	Comment   string
	PositionX float64 // ECEF metres; zero means no -hp
	PositionY float64
	PositionZ float64
	WorkDir   string
	Timeout   time.Duration
	Log       *slog.Logger
}

type pending struct {
	zipPath string
	tmpDir  string
	zipName string
	expires time.Time
}

// Downloader serves the day list, conversion and download.
type Downloader struct {
	o  Options
	mu sync.Mutex
	// pending maps a one-time token to a prepared zip.
	pending map[string]*pending
	// busy guards against two conversions of the same day at once, which would
	// double the CPU cost for no benefit on a Pi.
	busy map[string]bool

	// jobs tracks conversions server-side, so navigating away from the tab or
	// reloading the page does not lose a running conversion.
	jmu  sync.Mutex
	jobs map[string]*Job

	// sniffed caches per-file protocol classification. Without it, listing the
	// calendar re-reads every file on the share over SMB, which made opening
	// the tab take seconds and grew with the archive.
	smu     sync.RWMutex
	sniffed map[string]sniffEntry
}

type sniffEntry struct {
	size   int64
	mod    int64
	hasNav bool
}

func New(o Options) *Downloader {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Version == "" {
		o.Version = "3.04"
	}
	if o.Frequencies <= 0 {
		o.Frequencies = 4
	}
	if o.Timeout <= 0 {
		o.Timeout = 20 * time.Minute
	}
	if o.WorkDir == "" {
		o.WorkDir = os.TempDir()
	}
	return &Downloader{o: o, pending: map[string]*pending{},
		busy: map[string]bool{}, jobs: map[string]*Job{},
		sniffed: map[string]sniffEntry{}}
}

// Day is one selectable date in the calendar.
type Day struct {
	Date      string  `json:"date"` // YYYY-MM-DD
	SizeMB    float64 `json:"size_mb"`
	Capturing bool    `json:"capturing"`
	Files     int     `json:"files"`
	HasNav    bool    `json:"has_nav"`
}

// Available lists days that have raw data, collapsing multiple files per day.
//
// Today is reported as "capturing" rather than offered for conversion, because
// its file is still being written.
func (d *Downloader) Available() ([]Day, error) {
	// Archive files are named in UTC, so "today" is the UTC day.
	today := time.Now().UTC().Format("20060102")
	type agg struct {
		size   int64
		files  int
		hasNav bool
		// spoolKinds records which archive kinds were counted from the spool,
		// so the share copy of the same day is not added on top of it.
		spoolKinds map[string]bool
	}
	byDay := map[string]*agg{}
	var lastErr error

	// Spool first, so the share copy of a day still being written is skipped.
	srcs := append([]Source{}, d.o.Sources...)
	sort.SliceStable(srcs, func(i, j int) bool { return srcs[i].Spool && !srcs[j].Spool })

	for _, src := range srcs {
		if src.Dir == "" {
			continue
		}
		entries, err := os.ReadDir(src.Dir)
		if err != nil {
			lastErr = err
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), src.Prefix) {
				continue
			}
			rest := e.Name()[len(src.Prefix):]
			if len(rest) < 8 {
				continue
			}
			date := rest[:8]
			if !allDigits(date) {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			a := byDay[date]
			if a == nil {
				a = &agg{spoolKinds: map[string]bool{}}
				byDay[date] = a
			}
			if src.Spool {
				a.spoolKinds[src.Kind] = true
			} else if a.spoolKinds[src.Kind] {
				continue // already counted the more complete spool copy
			}
			a.size += fi.Size()
			a.files++
			if src.HasUBX && d.fileHasNavigation(filepath.Join(src.Dir, e.Name()), fi) {
				a.hasNav = true
			}
		}
	}

	out := make([]Day, 0, len(byDay)+1)
	for date, a := range byDay {
		t, err := time.Parse("20060102", date)
		if err != nil {
			continue
		}
		out = append(out, Day{
			Date: t.Format("2006-01-02"), SizeMB: float64(a.size) / 1048576,
			Files: a.files, HasNav: a.hasNav,
			// The day being recorded is offered, but only up to now: its file
			// is still growing.
			Capturing: date == today,
		})
	}
	if _, ok := byDay[today]; !ok {
		out = append(out, Day{Date: time.Now().UTC().Format("2006-01-02"), Capturing: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	if len(byDay) == 0 && lastErr != nil {
		return out, fmt.Errorf("cannot reach the raw-data archive: %w", lastErr)
	}
	return out, nil
}

// fileHasNavigation reports whether a file carries both UBX messages required
// for RINEX navigation output, caching by size and mtime.
//
// A completed archive file never changes, so this is read once per file for
// the life of the process instead of on every calendar load.
func (d *Downloader) fileHasNavigation(path string, fi os.FileInfo) bool {
	key := path
	size, mod := fi.Size(), fi.ModTime().Unix()

	d.smu.RLock()
	e, ok := d.sniffed[key]
	d.smu.RUnlock()
	if ok && e.size == size && e.mod == mod {
		return e.hasNav
	}

	// Read both ends so a growing file is recognized immediately after an
	// archive-filter upgrade, without rescanning the whole file over SMB.
	c, err := rinex.SniffEdges(path, 64<<10)
	has := true // on error, assume usable rather than hiding a day
	if err == nil {
		has = c.HasNavigation()
	}
	d.smu.Lock()
	d.sniffed[key] = sniffEntry{size: size, mod: mod, hasNav: has}
	d.smu.Unlock()
	return has
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// filesForKind returns the files covering one UTC date for one archive kind.
//
// The spool copy wins when present: it is the file currently being written, so
// it is more complete than whatever has reached the share.
func (d *Downloader) filesForKind(date, kind string) []string {
	var spool, share []string
	for _, src := range d.o.Sources {
		if src.Dir == "" || src.Kind != kind {
			continue
		}
		entries, err := os.ReadDir(src.Dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), src.Prefix) {
				continue
			}
			rest := e.Name()[len(src.Prefix):]
			if len(rest) < 8 || rest[:8] != date {
				continue
			}
			p := filepath.Join(src.Dir, e.Name())
			if src.Spool {
				spool = append(spool, p)
			} else {
				share = append(share, p)
			}
		}
	}
	if len(spool) > 0 {
		sort.Strings(spool)
		return spool
	}
	sort.Strings(share)
	return share
}

// kindFilesForWindow returns one archive kind for every UTC date touched by a
// window. EndMin is exclusive, so a range ending exactly at midnight does not
// read the following day's file.
func (d *Downloader) kindFilesForWindow(date string, win Window, kind string) []string {
	base, err := time.Parse("20060102", date)
	if err != nil {
		return nil
	}
	lastDay := (win.EndMin - 1) / 1440
	var out []string
	for n := 0; n <= lastDay; n++ {
		out = append(out, d.filesForKind(base.AddDate(0, 0, n).Format("20060102"), kind)...)
	}
	return out
}

type conversionInput struct {
	files []string
	want  Outputs
}

// inputsForWindow routes each requested product to its authoritative source.
// Asking every source for every output made "Both" noisy and could silently
// return observations only when an incomplete NAV source was tried first.
func (d *Downloader) inputsForWindow(date string, win Window, out Outputs) []conversionInput {
	var inputs []conversionInput
	if out.Obs {
		files := d.kindFilesForWindow(date, win, "rtcm")
		if len(files) == 0 {
			files = d.kindFilesForWindow(date, win, "ubx")
		}
		if len(files) > 0 {
			inputs = append(inputs, conversionInput{files: files, want: Outputs{Obs: true}})
		}
	}
	if out.Nav {
		files := d.kindFilesForWindow(date, win, "nav")
		if len(files) == 0 {
			files = d.kindFilesForWindow(date, win, "ubx")
		}
		if len(files) > 0 {
			inputs = append(inputs, conversionInput{files: files, want: Outputs{Nav: true}})
		}
	}
	return inputs
}

// Result describes a completed conversion.
type Result struct {
	Token   string   `json:"token"`
	ZipName string   `json:"zip_name"`
	Files   []string `json:"files"`
	SizeMB  float64  `json:"size_mb"`
	Took    string   `json:"took"`
	Warn    string   `json:"warn,omitempty"`
}

// Outputs selects which RINEX products to produce.
type Outputs struct {
	Obs bool
	Nav bool
}

// ParseOutputs maps the UI's choice onto the flags.
func ParseOutputs(s string) Outputs {
	switch s {
	case "obs":
		return Outputs{Obs: true}
	case "nav":
		return Outputs{Nav: true}
	default:
		return Outputs{Obs: true, Nav: true}
	}
}

func (o Outputs) String() string {
	switch {
	case o.Obs && o.Nav:
		return "both"
	case o.Nav:
		return "nav"
	default:
		return "obs"
	}
}

// Window is the span to convert, in UTC minutes from the selected day's
// midnight. EndMin may extend across multiple UTC dates.
type Window struct {
	StartMin int
	EndMin   int
}

// FullDay is the whole of the selected day.
func FullDay() Window { return Window{0, 1440} }

// MaxWindow is deliberately bounded so one browser request cannot ask a small
// Raspberry Pi to concatenate and convert the entire archive in one process.
const MaxWindow = 31 * 24 * time.Hour

// Valid reports whether the window makes sense. The start belongs to the
// selected date and the exclusive end may be up to 31 days later.
func (w Window) Valid() bool {
	return w.StartMin >= 0 && w.StartMin < 1440 &&
		w.EndMin > w.StartMin &&
		time.Duration(w.EndMin-w.StartMin)*time.Minute <= MaxWindow
}

// IsFullDay reports whether this covers exactly the selected day.
func (w Window) IsFullDay() bool { return w.StartMin == 0 && w.EndMin == 1440 }

// CrossesMidnight reports whether the span runs into the following day.
func (w Window) CrossesMidnight() bool { return w.EndMin > 1440 }

// Label renders the span for filenames and the UI, e.g. "2300-0500+1" or
// "2300-0500+3". The suffix is the end date's offset from the start date.
func (w Window) Label() string {
	l := fmt.Sprintf("%02d%02d-%02d%02d",
		w.StartMin/60, w.StartMin%60, (w.EndMin/60)%24, w.EndMin%60)
	if w.CrossesMidnight() {
		l += fmt.Sprintf("+%d", w.EndMin/1440)
	}
	return l
}

// Process converts one day and stages a zip for download. job may be nil; when
// present its progress fields are updated as convbin works.
func (d *Downloader) Process(ctx context.Context, dateStr string, job *Job,
	out Outputs, win Window, preset Preset) (*Result, error) {
	day, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return nil, fmt.Errorf("invalid date %q, expected YYYY-MM-DD", dateStr)
	}
	if !win.Valid() {
		return nil, fmt.Errorf("invalid time range")
	}
	key := day.Format("20060102")

	busyKey := key + out.String() + win.Label() + preset.ID
	d.mu.Lock()
	if d.busy[busyKey] {
		d.mu.Unlock()
		return nil, fmt.Errorf("that conversion is already running; try again in a moment")
	}
	d.busy[busyKey] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.busy, busyKey)
		d.mu.Unlock()
	}()

	d.expire()

	inputs := d.inputsForWindow(key, win, out)
	if len(inputs) == 0 {
		return nil, fmt.Errorf("no raw data found for %s in the archive", dateStr)
	}

	tmp, err := os.MkdirTemp(d.o.WorkDir, "gnss_dl_")
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(tmp)
		}
	}()

	// The archive files are named and rotated on UTC boundaries, so the window
	// is interpreted in UTC too.
	start := day.Add(time.Duration(win.StartMin) * time.Minute)
	end := day.Add(time.Duration(win.EndMin)*time.Minute - time.Second)
	// The day being recorded has no data past the present moment; asking for
	// it makes convbin scan to the end of a growing file for nothing.
	if now := time.Now().UTC(); end.After(now) {
		end = now
	}
	if !end.After(start) {
		return nil, fmt.Errorf("the selected range has not been recorded yet")
	}
	t0 := time.Now()
	var produced []string
	var errs []string

	stemBase := "GNSS_" + key
	if !win.IsFullDay() {
		stemBase += "_" + win.Label()
	}

	// Join multi-day files separately per product. Mixing RTCM and UBX
	// into one byte stream is both slower and invalid input to convbin.
	for i := range inputs {
		if win.CrossesMidnight() && len(inputs[i].files) > 1 {
			joined := filepath.Join(tmp, fmt.Sprintf("joined-%02d.bin", i))
			if err := concat(joined, inputs[i].files); err != nil {
				return nil, fmt.Errorf("joining archive files for the selected range: %w", err)
			}
			inputs[i].files = []string{joined}
		}
	}
	var tasks []conversionInput
	for _, input := range inputs {
		for _, raw := range input.files {
			tasks = append(tasks, conversionInput{files: []string{raw}, want: input.want})
		}
	}

	for i, task := range tasks {
		raw := task.files[0]
		// Convert each source into its own directory under the base name, so
		// the final naming can depend on what actually came out rather than on
		// how many files were searched. A source that contributes nothing --
		// an ephemeris file when only observations were asked for -- should
		// not push the others into "_partNN".
		sub := filepath.Join(tmp, fmt.Sprintf("s%02d", i))
		if err := os.MkdirAll(sub, 0o750); err != nil {
			return nil, err
		}
		files, stderr, err := d.convert(ctx, raw, start, end, sub, stemBase, task.want, preset,
			jobProgress(d, job, i, len(tasks)))
		produced = append(produced, files...)
		if errors.Is(err, context.Canceled) {
			return nil, context.Canceled
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("[%s] %v", filepath.Base(raw), err))
		} else if stderr != "" {
			// convbin writes ordinary progress to stderr, so only note it when
			// nothing came out; otherwise it reads as a failure when it is not.
			if len(files) == 0 {
				errs = append(errs, fmt.Sprintf("[%s] %s", filepath.Base(raw), stderr))
			} else {
				d.o.Log.Debug("convbin output", "file", filepath.Base(raw), "stderr", stderr)
			}
		}
	}
	// Assemble the final names: one contributor for an extension keeps the base
	// name, several get numbered.
	produced, err = assemble(tmp, stemBase, produced)
	if err != nil {
		return nil, err
	}

	if len(produced) == 0 {
		// convbin writes its normal banner to stderr, so surfacing that as the
		// error tells the operator nothing. The common real cause is a window
		// the file does not cover -- a day whose recording started late or
		// stopped early.
		if !win.IsFullDay() {
			return nil, fmt.Errorf("no data between %s UTC on %s; "+
				"that day's recording may not cover the whole window",
				win.Label(), dateStr)
		}
		if out.Nav && !out.Obs {
			return nil, fmt.Errorf("no ephemeris found for %s; "+
				"that day may predate the navigation archive", dateStr)
		}
		return nil, fmt.Errorf("conversion produced no output for %s", dateStr)
	}
	if out.Obs && !hasOutput(produced, ".obs") {
		return nil, fmt.Errorf("observation conversion produced no data for %s", dateStr)
	}
	if out.Nav && !hasNavigationOutput(produced) {
		return nil, fmt.Errorf("navigation conversion produced no data for %s; the recorded NAV source may lack RXM-RAWX timing data", dateStr)
	}

	zipName := stemBase + ".zip"
	zipPath := filepath.Join(tmp, zipName)
	if err := writeZip(zipPath, produced); err != nil {
		return nil, err
	}
	// The RINEX files are inside the zip now; drop the loose copies.
	for _, f := range produced {
		os.Remove(f)
	}

	tok := newToken()
	d.mu.Lock()
	d.pending[tok] = &pending{zipPath: zipPath, tmpDir: tmp, zipName: zipName,
		expires: time.Now().Add(PendingTTL)}
	d.mu.Unlock()
	ok = true

	names := make([]string, 0, len(produced))
	for _, f := range produced {
		names = append(names, filepath.Base(f))
	}
	var zipMB float64
	if fi, err := os.Stat(zipPath); err == nil {
		zipMB = float64(fi.Size()) / 1048576
	}
	res := &Result{Token: tok, ZipName: zipName, Files: names,
		SizeMB: zipMB, Took: time.Since(t0).Round(time.Second).String()}
	if len(errs) > 0 {
		res.Warn = strings.Join(errs, "; ")
	}
	d.o.Log.Info("RINEX conversion complete", "date", dateStr,
		"sources", len(tasks), "outputs", len(produced), "took", res.Took)
	return res, nil
}

func hasOutput(files []string, suffix string) bool {
	for _, file := range files {
		if strings.HasSuffix(file, suffix) {
			return true
		}
	}
	return false
}

func hasNavigationOutput(files []string) bool {
	for _, suffix := range []string{".nav", ".gnav", ".lnav", ".cnav", ".hnav", ".qnav", ".inav"} {
		if hasOutput(files, suffix) {
			return true
		}
	}
	return false
}

// headerArgs writes the station identification a post-processing service reads
// out of the RINEX header. convbin takes the approximate position as ECEF
// metres.
func (d *Downloader) headerArgs() []string {
	var args []string
	if d.o.Comment != "" {
		args = append(args, "-hc", d.o.Comment)
	}
	if d.o.Marker != "" {
		args = append(args, "-hm", d.o.Marker)
	}
	if d.o.Antenna != "" {
		args = append(args, "-ha", "0000/"+d.o.Antenna)
	}
	if d.o.Receiver != "" {
		args = append(args, "-hr", "0000/"+d.o.Receiver)
	}
	if d.o.PositionX != 0 || d.o.PositionY != 0 || d.o.PositionZ != 0 {
		args = append(args, "-hp", fmt.Sprintf("%.4f/%.4f/%.4f", d.o.PositionX, d.o.PositionY, d.o.PositionZ))
	}
	return args
}

// convert runs convbin with the same flags the original downloader used.
func (d *Downloader) convert(ctx context.Context, raw string, start, end time.Time,
	outDir, stem string, want Outputs, preset Preset, report func(frac float64, stage string)) ([]string, string, error) {

	if _, err := os.Stat(d.o.ConvbinPath); err != nil {
		return nil, "", fmt.Errorf("convbin not found at %s", d.o.ConvbinPath)
	}
	p := func(ext string) string { return filepath.Join(outDir, stem+ext) }

	// Detect rather than assume: files written before cutover are the full
	// multiplexed stream, files written after are RTCM only.
	format := "ubx"
	if c, err := rinex.Sniff(raw, 1<<20); err == nil {
		format = c.Format()
		if !c.HasUBX() && want.Nav {
			d.o.Log.Warn("navigation data requested but this file has no ephemeris source",
				"file", filepath.Base(raw), "content", c.String())
		}
	}
	version, frequencies := d.o.Version, d.o.Frequencies
	if preset.Version != "" {
		version = preset.Version
	}
	if preset.Frequencies > 0 {
		frequencies = preset.Frequencies
	}
	args := []string{
		"-r", format,
		"-v", version,
		"-f", fmt.Sprint(frequencies),
		// RTKLIB reads the date and the time as two separate arguments. Passing
		// "2026/09/14 09:00:00" as one argument makes convbin consume the next
		// flag as the time, and the range is silently ignored -- the whole file
		// is converted instead. The original downloader had this bug; it was
		// invisible there because it only ever requested full days.
		"-ts", start.Format("2006/01/02"), start.Format("15:04:05"),
		"-te", end.Format("2006/01/02"), end.Format("15:04:05"),
		// Doppler, SNR, and iono/time/leap-second records in the nav header,
		// exactly as the original invoked it.
		"-od", "-os", "-oi", "-ot", "-ol",
	}
	// Only ask for what was requested: convbin writes every output it is given
	// a path for, so omitting the flag is how a product is excluded.
	if want.Obs {
		args = append(args, "-o", p(".obs"))
	}
	if want.Nav {
		args = append(args,
			"-n", p(".nav"),
			"-g", p(".gnav"),
			"-l", p(".lnav"),
			"-b", p(".cnav"),
			"-h", p(".hnav"),
			"-q", p(".qnav"),
			"-i", p(".inav"))
	}
	// Decimation and constellation selection come from the chosen preset; the
	// station header fields are always written when they are configured.
	if preset.IntervalSec > 0 {
		args = append(args, "-ti", strconv.FormatFloat(preset.IntervalSec, 'f', -1, 64))
		args = append(args, "-tt", strconv.FormatFloat(preset.ToleranceSec, 'f', -1, 64))
	}
	for _, sys := range preset.Exclude {
		args = append(args, "-y", sys)
	}
	args = append(args, d.headerArgs()...)
	args = append(args, raw)
	// A multi-day input takes roughly proportionally longer than one day on the
	// Pi. Scale the per-conversion ceiling, while retaining a finite upper bound.
	timeout := d.o.Timeout
	days := int(math.Ceil(end.Sub(start).Hours() / 24))
	if days > 1 {
		timeout *= time.Duration(days)
		if timeout > 6*time.Hour {
			timeout = 6 * time.Hour
		}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, d.o.ConvbinPath, args...)
	var errb strings.Builder
	pr := newProgress(start, end)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, "", err
	}
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pr.watch(stderr, &errb, func() {
			if report != nil {
				f, st := pr.read()
				report(f, st)
			}
		})
	}()
	runErr := cmd.Wait()
	<-done
	if cctx.Err() == context.Canceled || ctx.Err() == context.Canceled {
		return nil, "", context.Canceled
	}
	if cctx.Err() == context.DeadlineExceeded {
		return nil, "", fmt.Errorf("convbin timed out after %s", timeout)
	}

	// Collect whatever was actually produced and is non-empty, as the original
	// did: which navigation files appear depends on the constellations present.
	matches, _ := filepath.Glob(filepath.Join(outDir, stem+".*"))
	var out []string
	for _, m := range matches {
		if strings.HasSuffix(m, ".zip") {
			continue
		}
		if fi, err := os.Stat(m); err == nil && fi.Size() > 0 {
			out = append(out, m)
		} else {
			os.Remove(m)
		}
	}
	sort.Strings(out)
	if len(out) == 0 && runErr != nil {
		return nil, strings.TrimSpace(errb.String()), runErr
	}
	return out, strings.TrimSpace(errb.String()), nil
}

// assemble moves per-source outputs into the working directory, numbering only
// the extensions that genuinely have more than one contributor.
func assemble(dir, stem string, produced []string) ([]string, error) {
	byExt := map[string][]string{}
	for _, p := range produced {
		byExt[filepath.Ext(p)] = append(byExt[filepath.Ext(p)], p)
	}
	out := make([]string, 0, len(produced))
	for ext, list := range byExt {
		sort.Strings(list)
		for i, src := range list {
			name := stem + ext
			if len(list) > 1 {
				name = fmt.Sprintf("%s_part%02d%s", stem, i+1, ext)
			}
			dst := filepath.Join(dir, name)
			if err := os.Rename(src, dst); err != nil {
				return nil, err
			}
			out = append(out, dst)
		}
	}
	sort.Strings(out)
	return out, nil
}

// concat joins raw archive files end to end.
func concat(dst string, srcs []string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	for _, s := range srcs {
		in, err := os.Open(s)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeZip(path string, files []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, src := range files {
		in, err := os.Open(src)
		if err != nil {
			continue
		}
		w, err := zw.Create(filepath.Base(src))
		if err != nil {
			in.Close()
			zw.Close()
			return err
		}
		if _, err := io.Copy(w, in); err != nil {
			in.Close()
			zw.Close()
			return err
		}
		in.Close()
	}
	return zw.Close()
}

// Take resolves a download token, returning the zip path and filename.
func (d *Downloader) Take(token string) (path, name string, ok bool) {
	d.expire()
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.pending[token]
	if p == nil || time.Now().After(p.expires) {
		return "", "", false
	}
	return p.zipPath, p.zipName, true
}

// expire removes prepared downloads past their TTL.
func (d *Downloader) expire() {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for tok, p := range d.pending {
		if now.After(p.expires) {
			os.RemoveAll(p.tmpDir)
			delete(d.pending, tok)
		}
	}
}

// Warm populates the classification cache in the background.
//
// The first calendar load otherwise has to read every file on the share, which
// took 25 seconds against a four-month archive. Doing it at startup means the
// UI is responsive by the time anyone opens the tab, and the work happens once.
func (d *Downloader) Warm(ctx context.Context) {
	// Let the SMB mount settle first; a scan against an unmounted share
	// caches nothing useful.
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
	}
	t0 := time.Now()
	n := 0
	for _, src := range d.o.Sources {
		if src.Dir == "" || !src.HasUBX {
			continue
		}
		entries, err := os.ReadDir(src.Dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if ctx.Err() != nil {
				return
			}
			if e.IsDir() || !strings.HasPrefix(e.Name(), src.Prefix) {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			d.fileHasNavigation(filepath.Join(src.Dir, e.Name()), fi)
			n++
		}
	}
	d.o.Log.Info("archive classification cache warmed",
		"files", n, "took", time.Since(t0).Round(time.Millisecond))
}

// Sweep expires stale downloads until ctx ends.
func (d *Downloader) Sweep(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			d.mu.Lock()
			for tok, p := range d.pending {
				os.RemoveAll(p.tmpDir)
				delete(d.pending, tok)
			}
			d.mu.Unlock()
			return
		case <-t.C:
			d.expire()
			d.expireJobs()
		}
	}
}

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// jobProgress maps one file's convbin progress onto the whole job, so a day
// split across several raw files still reports a single 0-100% bar.
func jobProgress(d *Downloader, job *Job, idx, total int) func(float64, string) {
	if job == nil || total <= 0 {
		return nil
	}
	return func(frac float64, stage string) {
		d.jmu.Lock()
		job.Percent = (float64(idx) + frac) / float64(total)
		job.Stage = stage
		if total > 1 {
			job.Stage = fmt.Sprintf("%s (file %d of %d)", stage, idx+1, total)
		}
		d.jmu.Unlock()
	}
}
