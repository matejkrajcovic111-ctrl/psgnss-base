package web

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/archive"
)

func TestWritableProbeLeavesDirectoryClean(t *testing.T) {
	dir := t.TempDir()
	if err := writableProbe(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe left %d file(s) behind", len(entries))
	}
}

// One share failure days ago, with every sync since succeeding, used to hold
// the archive check red until the service restarted. The count is history;
// the check is about now.
func TestArchiveCheckRecoversAfterAFailure(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	base := archive.Stats{Name: "rtcm", File: "Base1_x", Bytes: 1, LastSync: now.Add(-30 * time.Second).Unix()}

	cases := []struct {
		name string
		mod  func(*archive.Stats)
		ok   bool
		want string
	}{
		{"never failed", func(*archive.Stats) {}, true, "0 sync failure(s)"},
		{"failed, then synced", func(a *archive.Stats) {
			a.Failures, a.LastFailure = 3, now.Add(-72*time.Hour).Unix()
		}, true, "recovered"},
		{"failing now", func(a *archive.Stats) {
			a.Failures, a.LastFailure, a.LastError = 1, now.Add(-5*time.Second).Unix(), "host is down"
		}, false, "host is down"},
		{"finished file stranded", func(a *archive.Stats) { a.Pending = 1 }, false, "waiting in the spool"},
		{"syncs stopped", func(a *archive.Stats) { a.LastSync = now.Add(-10 * time.Minute).Unix() }, false, ""},
	}
	for _, c := range cases {
		a := base
		c.mod(&a)
		ok, detail, _ := archiveCheck(a, now, 60)
		if ok != c.ok || !strings.Contains(detail, c.want) {
			t.Errorf("%s: ok=%v detail=%q, want ok=%v containing %q", c.name, ok, detail, c.ok, c.want)
		}
	}
}
