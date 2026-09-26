package archive

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExpandPatternMatchesExistingConvention(t *testing.T) {
	// The live archive contains Base1_202609140000 for 2026-09-14 00:00.
	ts := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	if got := ExpandPattern("Base1_%Y%m%d%h00", ts); got != "Base1_202609140000" {
		t.Errorf("got %q, want Base1_202609140000", got)
	}
	// And Base1_202609150800 for a file started in hour 08.
	ts = time.Date(2026, 9, 15, 8, 30, 0, 0, time.UTC)
	if got := ExpandPattern("Base1_%Y%m%d%h00", ts); got != "Base1_202609150800" {
		t.Errorf("got %q, want Base1_202609150800", got)
	}
}

func TestExpandPatternVerbs(t *testing.T) {
	ts := time.Date(2026, 3, 7, 5, 9, 3, 0, time.UTC)
	for _, c := range []struct{ pat, want string }{
		{"%Y", "2026"}, {"%y", "26"}, {"%m", "03"}, {"%d", "07"},
		{"%h", "05"}, {"%M", "09"}, {"%S", "03"}, {"%n", "066"},
		{"100%%", "100%"}, {"%Q", "%Q"},
	} {
		if got := ExpandPattern(c.pat, ts); got != c.want {
			t.Errorf("ExpandPattern(%q) = %q, want %q", c.pat, got, c.want)
		}
	}
}

func TestParseTimeFromName(t *testing.T) {
	got, ok := ParseTimeFromName("Base1_%Y%m%d%h00", "Base1_202609140000")
	if !ok {
		t.Fatal("failed to parse a name the same pattern produced")
	}
	want := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if _, ok := ParseTimeFromName("Base1_%Y%m%d%h00", "not-an-archive-file"); ok {
		t.Error("parsed a name that does not match the pattern")
	}
}

func TestPatternGlob(t *testing.T) {
	if got := PatternGlob("Base1_%Y%m%d%h00"); got != "Base1_*00" {
		t.Errorf("PatternGlob = %q, want Base1_*00", got)
	}
}

// TestSwapBoundaryIsWallClockAligned guards the property that matters after a
// restart: boundaries must follow the clock, not process start time.
func TestSwapBoundaryIsWallClockAligned(t *testing.T) {
	w := &Writer{SwapHours: 24}
	for _, at := range []time.Time{
		time.Date(2026, 9, 14, 0, 0, 1, 0, time.UTC),
		time.Date(2026, 9, 14, 13, 47, 0, 0, time.UTC),
		time.Date(2026, 9, 14, 23, 59, 59, 0, time.UTC),
	} {
		got := w.swapBoundary(at)
		want := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("swapBoundary(%v) = %v, want %v", at, got, want)
		}
	}
}

func TestSwapBoundaryTwelveHour(t *testing.T) {
	w := &Writer{SwapHours: 12}
	got := w.swapBoundary(time.Date(2026, 9, 14, 5, 0, 0, 0, time.UTC))
	want := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestWriterRotatesAndFlushes exercises the whole spool-to-share path.
func TestWriterRotatesAndFlushes(t *testing.T) {
	spool, dest := t.TempDir(), t.TempDir()
	w, err := NewWriter("test", spool, dest, "F_%Y%m%d%h00", 24, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	day1 := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	if err := w.Write([]byte("first-day"), day1); err != nil {
		t.Fatal(err)
	}
	if err := w.Write([]byte("second-day"), day2); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish(); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{
		"F_202609140000": "first-day",
		"F_202609150000": "second-day",
	} {
		b, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil {
			t.Errorf("expected %s on the share: %v", name, err)
			continue
		}
		if string(b) != want {
			t.Errorf("%s contains %q, want %q", name, b, want)
		}
	}
	if e, _ := os.ReadDir(spool); len(e) != 0 {
		t.Errorf("spool should be empty after flush, has %d entries", len(e))
	}
}

// TestWriterKeepsDataWhenShareUnavailable is the resilience property: an SMB
// outage must cost disk, never data.
func TestWriterKeepsDataWhenShareUnavailable(t *testing.T) {
	spool := t.TempDir()
	// A destination under a file (not a directory) cannot be created.
	blocker := filepath.Join(t.TempDir(), "blocker")
	os.WriteFile(blocker, []byte("x"), 0o600)
	dest := filepath.Join(blocker, "sub")

	w, _ := NewWriter("test", spool, dest, "F_%Y%m%d%h00", 24, 0, nil)
	w.Write([]byte("precious"), time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC))
	w.Finish()

	entries, _ := os.ReadDir(spool)
	if len(entries) != 1 {
		t.Fatalf("spool has %d files, want 1 retained after a failed flush", len(entries))
	}
	b, _ := os.ReadFile(filepath.Join(spool, entries[0].Name()))
	if string(b) != "precious" {
		t.Errorf("spooled data = %q, want %q", b, "precious")
	}
	if w.FlushFailed.Load() == 0 {
		t.Error("flush failure was not counted")
	}
}

func TestWriterAppendsOnReopen(t *testing.T) {
	spool, dest := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	w1, _ := NewWriter("t", spool, dest, "F_%Y%m%d%h00", 24, 0, nil)
	w1.Write([]byte("part1"), at)
	w1.Sync()
	// Simulate a crash: no Close, so no flush to the share.
	w1.cur.Close()

	w2, _ := NewWriter("t", spool, dest, "F_%Y%m%d%h00", 24, 0, nil)
	if err := w2.Write([]byte("part2"), at); err != nil {
		t.Fatal(err)
	}
	w2.Sync()
	b, _ := os.ReadFile(filepath.Join(spool, "F_202609140000"))
	if string(b) != "part1part2" {
		t.Errorf("after reopen file is %q, want part1part2 (data was truncated)", b)
	}
}

func TestPruneRespectsRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	mk := func(t2 time.Time) string {
		n := ExpandPattern("Base1_%Y%m%d%h00", t2)
		os.WriteFile(filepath.Join(dir, n), []byte("data"), 0o600)
		return n
	}
	old := mk(now.AddDate(0, 0, -400))
	recent := mk(now.AddDate(0, 0, -10))

	removed, _, err := Prune(dir, "Base1_%Y%m%d%h00", 365, nil)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed %d files, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, old)); !os.IsNotExist(err) {
		t.Error("400-day-old file should have been pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, recent)); err != nil {
		t.Error("10-day-old file must be kept")
	}
}

// TestPruneRefusesZeroRetention guards against "0 days" meaning "delete it all".
func TestPruneRefusesZeroRetention(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ExpandPattern("Base1_%Y%m%d%h00",
		time.Now().AddDate(0, 0, -500))), []byte("x"), 0o600)
	for _, days := range []int{0, -1} {
		removed, _, err := Prune(dir, "Base1_%Y%m%d%h00", days, nil)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 0 {
			t.Errorf("retention=%d removed %d files; must be a no-op", days, removed)
		}
	}
}

// TestNamingIsUTCNotLocal is the regression guard for the bug found at
// cutover: the swap boundary was correct but the filename was formatted in
// local time, producing Base1_..0100 during BST instead of ..0000.
func TestNamingIsUTCNotLocal(t *testing.T) {
	// A zone with a non-zero offset, so local and UTC naming differ.
	bst := time.FixedZone("BST", 3600)
	spool, dest := t.TempDir(), t.TempDir()
	w, err := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 2026-09-15 16:22 BST == 15:22 UTC. The enclosing UTC day starts
	// 2026-09-15 00:00 UTC, so the file must be Base1_202609150000.
	now := time.Date(2026, 9, 15, 16, 22, 0, 0, bst)
	if err := w.Write([]byte("x"), now); err != nil {
		t.Fatal(err)
	}
	got := w.curName
	if got != "Base1_202609150000" {
		t.Errorf("filename = %q, want Base1_202609150000 (UTC-named, not local)", got)
	}
	w.Close()
}

// TestFlushRefusesToOverwrite guards the parallel-run case: while the system
// being replaced still writes the same filename, its file is the full
// multiplexed stream and must never be clobbered by ours.
func TestFlushRefusesToOverwrite(t *testing.T) {
	spool, dest := t.TempDir(), t.TempDir()
	existing := filepath.Join(dest, "Base1_202609150000")
	if err := os.WriteFile(existing, []byte("SOMEONE-ELSES-FULL-MUX-DATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _ := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
	w.Write([]byte("ours"), time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	w.Finish()

	b, _ := os.ReadFile(existing)
	if string(b) != "SOMEONE-ELSES-FULL-MUX-DATA" {
		t.Errorf("existing archive file was overwritten: now %q", b)
	}
	// Ours must land alongside rather than be dropped or left in the spool.
	alt := existing + ".psgnss"
	ours, err := os.ReadFile(alt)
	if err != nil {
		t.Fatalf("our file was not written alongside: %v", err)
	}
	if string(ours) != "ours" {
		t.Errorf("alternate file contains %q, want %q", ours, "ours")
	}
	if e, _ := os.ReadDir(spool); len(e) != 0 {
		t.Errorf("spool should be empty after landing alongside, found %d entries", len(e))
	}
}

// TestIncrementalSyncReachesShareBeforeSwap is the durability property that
// was missing: data must arrive on the share continuously, not only when the
// 24-hour file rotates. Otherwise a day of data lives on one disk.
func TestIncrementalSyncReachesShareBeforeSwap(t *testing.T) {
	spool, dest := t.TempDir(), t.TempDir()
	w, err := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	w.Write([]byte("first-chunk-"), at)
	w.syncToDest()

	onShare := filepath.Join(dest, "Base1_202609150000")
	b, err := os.ReadFile(onShare)
	if err != nil {
		t.Fatalf("nothing on the share before the swap: %v", err)
	}
	if string(b) != "first-chunk-" {
		t.Errorf("share has %q, want %q", b, "first-chunk-")
	}

	// A second sync must append the delta, not rewrite the file.
	w.Write([]byte("second-chunk"), at)
	w.syncToDest()
	b, _ = os.ReadFile(onShare)
	if string(b) != "first-chunk-second-chunk" {
		t.Errorf("after second sync share has %q, want the two chunks appended", b)
	}

	w.Finish()
	// Once complete and matching, the spool copy is released.
	if e, _ := os.ReadDir(spool); len(e) != 0 {
		t.Errorf("spool should be empty after a complete sync, has %d", len(e))
	}
	b, _ = os.ReadFile(onShare)
	if string(b) != "first-chunk-second-chunk" {
		t.Errorf("final share content %q", b)
	}
}

// TestSyncResumesAfterRestart covers the daemon restarting mid-file: it must
// continue appending where the share copy ended, not duplicate or truncate.
func TestSyncResumesAfterRestart(t *testing.T) {
	spool, dest := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	w1, _ := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
	w1.Write([]byte("AAAA"), at)
	w1.syncToDest()
	w1.buf.Flush()
	w1.cur.Close() // simulate a crash: no Close(), no final sync

	w2, _ := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
	w2.Write([]byte("BBBB"), at)
	w2.syncToDest()
	w2.Finish()

	b, err := os.ReadFile(filepath.Join(dest, "Base1_202609150000"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "AAAABBBB" {
		t.Errorf("after restart share has %q, want AAAABBBB (no duplication or gap)", b)
	}
}

// TestRestartResumesSameDestination is the fragmentation guard. Restarting
// mid-file must continue appending to the same share file, not start a new
// ".psgnss-N" fragment each time.
func TestRestartResumesSameDestination(t *testing.T) {
	spool, dest := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	// Something else already owns the canonical name, forcing an alternate.
	os.WriteFile(filepath.Join(dest, "Base1_202609150000"), []byte("theirs"), 0o600)

	var chosen string
	for i, chunk := range []string{"AAA", "BBB", "CCC"} {
		w, err := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(chunk), at)
		w.syncToDest()
		if i == 0 {
			chosen = w.curDest
		} else if w.curDest != chosen {
			t.Fatalf("restart %d chose %q, want the original %q (fragmenting)",
				i, filepath.Base(w.curDest), filepath.Base(chosen))
		}
		w.buf.Flush()
		w.cur.Close() // simulate restart without a clean Close
	}

	b, err := os.ReadFile(chosen)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "AAABBBCCC" {
		t.Errorf("share file has %q, want AAABBBCCC", b)
	}
	// Exactly two files: theirs and ours. No fragments.
	entries, _ := os.ReadDir(dest)
	if len(entries) != 2 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("share has %d files %v, want 2 (no fragments)", len(entries), names)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "Base1_202609150000")); string(b) != "theirs" {
		t.Error("the other writer's file was modified")
	}
}

// TestShutdownIsNotCompletion: stopping the service mid-period must keep the
// spool copy and resume state, so the next start continues the same file
// rather than opening a new fragment.
func TestShutdownIsNotCompletion(t *testing.T) {
	spool, dest := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	os.WriteFile(filepath.Join(dest, "Base1_202609150000"), []byte("theirs"), 0o600)

	w1, _ := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
	w1.Write([]byte("AAA"), at)
	w1.syncToDest()
	first := w1.curDest
	w1.Close() // shutdown, not completion

	if e, _ := os.ReadDir(spool); len(e) == 0 {
		t.Error("spool was emptied on shutdown; the next start cannot resume")
	}

	w2, _ := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
	w2.Write([]byte("BBB"), at)
	w2.syncToDest()
	if w2.curDest != first {
		t.Fatalf("after restart wrote to %q, want %q", filepath.Base(w2.curDest), filepath.Base(first))
	}
	w2.Finish() // now the period really ends

	b, _ := os.ReadFile(first)
	if string(b) != "AAABBB" {
		t.Errorf("share file has %q, want AAABBB", b)
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 2 {
		t.Errorf("share has %d files, want 2 (theirs + ours, no fragments)", len(entries))
	}
	if e, _ := os.ReadDir(spool); len(e) != 0 {
		t.Errorf("after Finish the spool should be empty, has %d", len(e))
	}
}

// TestFlushPendingSkipsInProgressFile: a restart must not treat the file it is
// about to resume as a stranded orphan and copy it to a new name.
func TestFlushPendingSkipsInProgressFile(t *testing.T) {
	spool, dest := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	os.WriteFile(filepath.Join(dest, "Base1_202609150000"), []byte("theirs"), 0o600)

	w1, _ := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
	w1.Write([]byte("AAA"), at)
	w1.syncToDest()
	first := w1.curDest
	w1.Close()

	// Restart: open, then sweep, exactly as Run does.
	w2, _ := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
	if err := w2.open(at); err != nil {
		t.Fatal(err)
	}
	w2.FlushPending()
	if w2.curDest != first {
		t.Errorf("resumed into %q, want %q", filepath.Base(w2.curDest), filepath.Base(first))
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 2 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("share has %d files %v, want 2 -- FlushPending duplicated the live file", len(entries), names)
	}
	w2.Finish()
}

// A soft SMB mount can time out after part of an append has landed. Resending
// the range from the recorded offset put those bytes on the share twice; the
// next sync has to continue from what the share really holds.
func TestSyncAfterPartialAppendDoesNotDuplicate(t *testing.T) {
	spool, dest := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	w, _ := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
	w.Write([]byte("AAAA"), at)
	w.syncToDest()

	// Half of the next append reaches the share, and the writer never hears.
	w.Write([]byte("BBBB"), at)
	f, _ := os.OpenFile(w.curDest, os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte("BB"))
	f.Close()

	w.syncToDest()
	w.Write([]byte("CC"), at)
	w.Finish()

	b, _ := os.ReadFile(filepath.Join(dest, "Base1_202609150000"))
	if string(b) != "AAAABBBBCC" {
		t.Errorf("share has %q, want AAAABBBBCC", b)
	}
	if e, _ := os.ReadDir(spool); len(e) != 0 {
		t.Errorf("spool should be empty after a clean finish, has %d entries", len(e))
	}
}

// A file whose period ended without being finished on the share -- the final
// sync failed, or the station was down over the swap -- still has its sidecar.
// It used to be skipped as "in progress" forever. The next sweep finishes it
// at the destination it was already feeding.
func TestFlushPendingFinishesAnEndedFile(t *testing.T) {
	spool, dest := t.TempDir(), t.TempDir()
	day1 := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	w1, _ := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
	w1.Write([]byte("AAAA"), day1)
	w1.syncToDest()
	w1.Write([]byte("BBBB"), day1)
	w1.Close() // shutdown; it stays down past midnight
	first := filepath.Join(dest, "Base1_202609150000")
	os.Truncate(first, 2) // and the share lost the tail of what it had

	w2, _ := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
	if err := w2.open(day1.Add(24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	w2.FlushPending()

	if b, _ := os.ReadFile(first); string(b) != "AAAABBBB" {
		t.Errorf("yesterday's share file has %q, want AAAABBBB", b)
	}
	if _, err := os.Stat(filepath.Join(spool, "Base1_202609150000")); !os.IsNotExist(err) {
		t.Error("yesterday's spool copy is still there after it reached the share")
	}
	if w2.Pending.Load() != 0 {
		t.Errorf("Pending = %d, want 0", w2.Pending.Load())
	}
	w2.Close()
}

// While a finished file cannot reach the share, it is counted as pending and
// the failure is recorded with its reason.
func TestPendingAndLastErrorWhileShareIsGone(t *testing.T) {
	spool, parent := t.TempDir(), t.TempDir()
	dest := filepath.Join(parent, "share")
	day1 := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	w1, _ := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
	w1.Write([]byte("AAAA"), day1)
	w1.Close()
	os.RemoveAll(dest)
	os.WriteFile(dest, nil, 0o600) // a file where the directory was: MkdirAll fails

	w2, _ := NewWriter("t", spool, dest, "Base1_%Y%m%d%h00", 24, 0, nil)
	w2.open(day1.Add(24 * time.Hour))
	w2.FlushPending()
	st := w2.Snapshot()
	if st.Pending != 1 || st.LastFailure == 0 || st.LastError == "" {
		t.Errorf("snapshot = pending %d, last failure %d, error %q; want 1, set, set",
			st.Pending, st.LastFailure, st.LastError)
	}
	w2.Close()
}
