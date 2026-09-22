package archive

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Writer records one filtered stream to dated files.
//
// Data is written to a local spool first and only moved to the share when a
// file is complete. An SMB outage therefore costs disk space, never data, and
// can never stall the hub -- which matters because the alternative is a stuck
// write blocking the process that owns the receiver.
type Writer struct {
	Name       string
	SpoolDir   string
	DestDir    string
	Pattern    string
	SwapHours  int
	SwapMargin time.Duration
	Log        *slog.Logger

	cur      *os.File
	buf      *bufio.Writer
	curName  string
	curStart time.Time
	nextSwap time.Time
	// curDest is the share path for the open file, chosen once at open so a
	// name collision is settled before any bytes are copied.
	curDest string
	// synced is how many bytes of the current spool file have already been
	// appended to the share.
	synced int64

	BytesWritten atomic.Int64
	FilesClosed  atomic.Int64
	FlushFailed  atomic.Int64
	SyncedBytes  atomic.Int64
	LastSyncUnix atomic.Int64
}

// SyncInterval is how often the growing file is appended to the share.
//
// Waiting for the 24-hour swap would leave up to a day of data on one disk
// with no copy, which the system being replaced did not do -- it wrote to the
// share continuously.
var SyncInterval = 60 * time.Second

// NewWriter prepares a writer. Directories are created as needed.
func NewWriter(name, spoolDir, destDir, pattern string, swapHours int,
	margin time.Duration, log *slog.Logger) (*Writer, error) {
	if log == nil {
		log = slog.Default()
	}
	if swapHours <= 0 {
		swapHours = 24
	}
	if pattern == "" {
		return nil, fmt.Errorf("archive %s: empty filename pattern", name)
	}
	if err := os.MkdirAll(spoolDir, 0o750); err != nil {
		return nil, fmt.Errorf("archive %s: create spool: %w", name, err)
	}
	return &Writer{Name: name, SpoolDir: spoolDir, DestDir: destDir, Pattern: pattern,
		SwapHours: swapHours, SwapMargin: margin, Log: log}, nil
}

// swapBoundary returns the next file-swap instant at or after t.
//
// Boundaries are aligned to the wall clock, not to process start, so restarting
// the daemon does not shift the archive's day boundaries.
func (w *Writer) swapBoundary(t time.Time) time.Time {
	d := time.Duration(w.SwapHours) * time.Hour
	day := t.Truncate(24 * time.Hour)
	for b := day; ; b = b.Add(d) {
		if b.After(t) {
			return b
		}
	}
}

// Write appends a frame, rotating the file when the boundary passes.
func (w *Writer) Write(b []byte, now time.Time) error {
	if w.cur == nil {
		if err := w.open(now); err != nil {
			return err
		}
	} else if now.After(w.nextSwap.Add(w.SwapMargin)) {
		// The margin keeps writing briefly past the boundary so a receiver
		// whose epochs straddle midnight does not lose the seconds either
		// side of the split.
		if err := w.rotate(now); err != nil {
			return err
		}
	}
	n, err := w.buf.Write(b)
	w.BytesWritten.Add(int64(n))
	return err
}

func (w *Writer) open(now time.Time) error {
	start := w.swapBoundary(now).Add(-time.Duration(w.SwapHours) * time.Hour)
	// Format in UTC. The existing archive was written by STRSVR, which swaps
	// on UTC midnight and names files with UTC time -- Base1_202609140000 was
	// closed at 01:00 BST. Using local time here would name the same file
	// ...0100 during BST and break continuity with four months of history.
	name := ExpandPattern(w.Pattern, start.UTC())
	path := filepath.Join(w.SpoolDir, name)
	// Append rather than truncate: a restart mid-period must not discard what
	// was already recorded for that file.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("archive %s: open %s: %w", w.Name, path, err)
	}
	w.cur, w.buf = f, bufio.NewWriterSize(f, 64*1024)
	w.curName, w.curStart = name, start
	w.nextSwap = w.swapBoundary(now)

	// Resolve the destination now, so a collision is settled before any bytes
	// are copied and the same path is appended to for the file's whole life.
	//
	// The chosen path and sync offset are recorded in a sidecar file, so a
	// restart resumes the same destination instead of guessing and creating a
	// fresh fragment each time.
	w.curDest, w.synced = "", 0
	if w.DestDir != "" {
		if err := os.MkdirAll(w.DestDir, 0o750); err != nil {
			w.FlushFailed.Add(1)
			w.Log.Error("archive destination unavailable; data stays in the spool",
				"archive", w.Name, "dest", w.DestDir, "err", err)
		} else if dest, off, ok := w.loadState(name); ok {
			w.curDest, w.synced = dest, off
		} else {
			w.curDest = w.resolveDest(name)
			w.saveState(name)
		}
		// Trust the share's actual length over our record: a partial append
		// must not be counted as fully synced, and a truncated file must not
		// be skipped over.
		if w.curDest != "" {
			if fi, err := os.Stat(w.curDest); err == nil && fi.Size() < w.synced {
				w.synced = fi.Size()
			} else if os.IsNotExist(err) {
				w.synced = 0
			}
		}
	}
	w.Log.Info("archive file opened", "archive", w.Name, "file", name,
		"dest", w.curDest, "resume_at", w.synced,
		"swap_at", w.nextSwap.Format(time.RFC3339))
	return nil
}

// statePath is the sidecar recording which share file this spool file feeds.
func (w *Writer) statePath(name string) string {
	return filepath.Join(w.SpoolDir, "."+name+".dest")
}

// loadState recovers the destination and sync offset chosen on a previous run.
func (w *Writer) loadState(name string) (dest string, synced int64, ok bool) {
	b, err := os.ReadFile(w.statePath(name))
	if err != nil {
		return "", 0, false
	}
	parts := strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)
	if len(parts) < 1 || parts[0] == "" {
		return "", 0, false
	}
	dest = parts[0]
	// The destination must still be inside the configured directory; if the
	// mount point changed, start fresh rather than writing somewhere stale.
	if filepath.Dir(dest) != filepath.Clean(w.DestDir) {
		return "", 0, false
	}
	if len(parts) > 1 {
		synced, _ = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	}
	return dest, synced, true
}

func (w *Writer) saveState(name string) {
	if w.curDest == "" {
		return
	}
	body := fmt.Sprintf("%s\n%d\n", w.curDest, w.synced)
	if err := os.WriteFile(w.statePath(name), []byte(body), 0o640); err != nil {
		w.Log.Debug("could not record archive destination state",
			"archive", w.Name, "file", name, "err", err)
	}
}

func (w *Writer) clearState(name string) { os.Remove(w.statePath(name)) }

// resolveDest picks the share path for a new file, stepping aside from any
// name already taken by something else.
func (w *Writer) resolveDest(name string) string {
	dst := filepath.Join(w.DestDir, name)
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return dst
	}
	for i := 1; i < 100; i++ {
		cand := fmt.Sprintf("%s.psgnss%s", dst, suffixNum(i))
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			w.Log.Warn("archive name already taken; writing alongside it",
				"archive", w.Name, "file", name, "written_as", filepath.Base(cand))
			return cand
		}
	}
	w.Log.Error("no free archive name on the share", "archive", w.Name, "file", name)
	return ""
}

// syncToDest appends bytes written since the last sync to the share file.
//
// Appending the delta rather than recopying keeps the cost proportional to new
// data, so a 700 MB day file does not get rewritten every minute.
func (w *Writer) syncToDest() {
	if w.cur == nil || w.curDest == "" {
		return
	}
	if w.buf != nil {
		if err := w.buf.Flush(); err != nil {
			w.Log.Warn("archive buffer flush failed", "archive", w.Name, "err", err)
			return
		}
	}
	src := filepath.Join(w.SpoolDir, w.curName)
	fi, err := os.Stat(src)
	if err != nil || fi.Size() <= w.synced {
		return
	}
	n, err := appendRange(src, w.curDest, w.synced, fi.Size())
	if err != nil {
		w.FlushFailed.Add(1)
		w.Log.Warn("archive sync to share failed; data is still in the spool",
			"archive", w.Name, "file", w.curName, "err", err)
		return
	}
	w.synced += n
	w.SyncedBytes.Add(n)
	w.LastSyncUnix.Store(time.Now().Unix())
	w.saveState(w.curName)
}

// appendRange copies src[from:to] onto the end of dst.
func appendRange(src, dst string, from, to int64) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if _, err := in.Seek(from, io.SeekStart); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	n, err := io.CopyN(out, in, to-from)
	if err != nil && err != io.EOF {
		return n, err
	}
	return n, nil
}

func (w *Writer) rotate(now time.Time) error {
	// The period really has ended, so this file is finished.
	if err := w.closeCurrent(true); err != nil {
		return err
	}
	return w.open(now)
}

// closeCurrent finishes the open file. complete says whether the file's period
// has actually ended.
//
// A shutdown is not a completion: if it were treated as one, the spool copy and
// the resume state would be discarded and the next start would open a fresh
// destination, fragmenting the day across a new file on every restart.
func (w *Writer) closeCurrent(complete bool) error {
	if w.cur == nil {
		return nil
	}
	name := w.curName
	// Final sync while the file is still open, so nothing written since the
	// last interval is lost.
	w.syncToDest()
	if err := w.buf.Flush(); err != nil {
		w.Log.Error("archive flush failed", "archive", w.Name, "file", name, "err", err)
	}
	if err := w.cur.Close(); err != nil {
		w.Log.Error("archive close failed", "archive", w.Name, "file", name, "err", err)
	}
	dest, synced := w.curDest, w.synced
	w.cur, w.buf, w.curName, w.curDest, w.synced = nil, nil, "", "", 0
	w.FilesClosed.Add(1)

	if !complete {
		w.Log.Info("archive file suspended; state kept so the next start resumes it",
			"archive", w.Name, "file", name, "synced", synced)
		return nil
	}

	src := filepath.Join(w.SpoolDir, name)
	if dest == "" {
		w.Log.Warn("archive file complete but never reached the share; kept in spool",
			"archive", w.Name, "file", name)
		return nil
	}
	// Only drop the spool copy once the share copy is the same size.
	sfi, err1 := os.Stat(src)
	dfi, err2 := os.Stat(dest)
	if err1 == nil && err2 == nil && dfi.Size() == sfi.Size() && sfi.Size() == synced {
		w.clearState(name)
		if err := os.Remove(src); err != nil {
			w.Log.Warn("could not remove spooled file after sync", "archive", w.Name, "file", name, "err", err)
		}
		w.Log.Info("archive file complete on the share",
			"archive", w.Name, "file", filepath.Base(dest), "bytes", dfi.Size())
	} else {
		w.Log.Warn("share copy does not match the spool; keeping the spool copy",
			"archive", w.Name, "file", name)
	}
	return nil
}

// flushToDest moves a completed spool file to the share. Failure leaves the
// file in the spool for a later attempt rather than losing it.
func (w *Writer) flushToDest(name string) {
	if w.DestDir == "" {
		return
	}
	src := filepath.Join(w.SpoolDir, name)
	dst := filepath.Join(w.DestDir, name)
	if err := os.MkdirAll(w.DestDir, 0o750); err != nil {
		w.FlushFailed.Add(1)
		w.Log.Error("archive destination unavailable, file kept in spool",
			"archive", w.Name, "file", name, "err", err)
		return
	}
	// Never overwrite an existing archive file. Another writer may own the
	// same name -- during the changeover, STRSVR's file for the same day is
	// the full multiplexed stream, and clobbering it would destroy data that
	// cannot be recovered.
	//
	// Land alongside it under a suffixed name rather than refusing outright:
	// refusing would leave the file in the spool and retry forever, so the
	// data would survive but never arrive.
	if _, err := os.Stat(dst); err == nil {
		alt := ""
		for i := 1; i < 100; i++ {
			cand := fmt.Sprintf("%s.psgnss%s", dst, suffixNum(i))
			if _, err := os.Stat(cand); os.IsNotExist(err) {
				alt = cand
				break
			}
		}
		if alt == "" {
			w.FlushFailed.Add(1)
			w.Log.Error("archive destination exists and no free alternate name; file kept in spool",
				"archive", w.Name, "file", name)
			return
		}
		w.Log.Warn("archive destination already exists; writing alongside it instead of overwriting",
			"archive", w.Name, "file", name, "written_as", filepath.Base(alt))
		dst = alt
	}
	if err := copyFile(src, dst); err != nil {
		w.FlushFailed.Add(1)
		w.Log.Error("archive flush to share failed, file kept in spool",
			"archive", w.Name, "file", name, "err", err)
		return
	}
	if err := os.Remove(src); err != nil {
		w.Log.Warn("could not remove spooled file after flush",
			"archive", w.Name, "file", name, "err", err)
	}
	w.Log.Info("archive file flushed to share", "archive", w.Name, "file", name)
}

// FlushPending copies completed spool files that never reached the share.
//
// It deliberately skips any file that still has a destination sidecar: that
// marks an in-progress file the writer will resume. Without this check, a
// restart would see the file it is about to reopen, treat it as a stranded
// orphan, and copy it to a fresh name -- fragmenting the day on every restart.
func (w *Writer) FlushPending() {
	if w.DestDir == "" {
		return
	}
	entries, err := os.ReadDir(w.SpoolDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == w.curName || strings.HasPrefix(name, ".") {
			continue
		}
		if _, err := os.Stat(w.statePath(name)); err == nil {
			continue // in progress; the writer owns it
		}
		w.flushToDest(name)
	}
}

// Stats is a live snapshot of one archive, for the dashboard.
type Stats struct {
	Name      string `json:"name"`
	File      string `json:"file"`
	Bytes     int64  `json:"bytes"`
	Synced    int64  `json:"synced"`
	Dest      string `json:"dest"`
	SwapAtSec int64  `json:"swap_at"`
	StartSec  int64  `json:"start"`
	LastSync  int64  `json:"last_sync"`
	Failures  int64  `json:"failures"`
}

// Snapshot reports the current file's progress.
func (w *Writer) Snapshot() Stats {
	st := Stats{
		Name:     w.Name,
		File:     w.curName,
		Bytes:    w.BytesWritten.Load(),
		Synced:   w.SyncedBytes.Load(),
		LastSync: w.LastSyncUnix.Load(),
		Failures: w.FlushFailed.Load(),
	}
	if w.curDest != "" {
		st.Dest = filepath.Base(w.curDest)
	}
	if !w.nextSwap.IsZero() {
		st.SwapAtSec = w.nextSwap.Unix()
		st.StartSec = w.nextSwap.Add(-time.Duration(w.SwapHours) * time.Hour).Unix()
	}
	return st
}

// Close stops writing and syncs, but does not mark the file complete: the
// period has not ended, only the process.
func (w *Writer) Close() error { return w.closeCurrent(false) }

// Finish marks the current file complete, as a period rotation would.
func (w *Writer) Finish() error { return w.closeCurrent(true) }

// Sync flushes buffered bytes to the spool file without rotating, so a crash
// loses at most the buffer rather than the period.
func (w *Writer) Sync() {
	if w.buf != nil {
		if err := w.buf.Flush(); err != nil {
			w.Log.Warn("archive periodic flush failed", "archive", w.Name, "err", err)
		}
	}
}

// Run consumes frames until ctx is cancelled.
func (w *Writer) Run(ctx context.Context, frames <-chan []byte) error {
	defer w.Close()
	// Open the current file first so FlushPending knows which one is live.
	if err := w.open(time.Now()); err != nil {
		w.Log.Error("archive open failed", "archive", w.Name, "err", err)
	}
	w.FlushPending()
	sync := time.NewTicker(SyncInterval)
	defer sync.Stop()
	retry := time.NewTicker(5 * time.Minute)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sync.C:
			w.syncToDest()
		case <-retry.C:
			w.FlushPending()
		case b, ok := <-frames:
			if !ok {
				return nil
			}
			if err := w.Write(b, time.Now()); err != nil {
				w.Log.Error("archive write failed", "archive", w.Name, "err", err)
			}
		}
	}
}

// suffixNum renders "" for the first alternate and "-2", "-3" thereafter, so
// the common case is a clean ".psgnss" suffix.
func suffixNum(i int) string {
	if i <= 1 {
		return ""
	}
	return fmt.Sprintf("-%d", i)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// Rename last so a partial copy is never visible under the real name.
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
