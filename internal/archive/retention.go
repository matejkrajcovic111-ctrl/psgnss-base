package archive

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Prune deletes archive files older than retentionDays.
//
// The predecessor tool never implemented retention at all, so the share holds
// everything ever recorded. Enabling this will delete real data, which is why
// it refuses to run with a zero or negative retention rather than treating
// that as "delete everything".
func Prune(dir, pattern string, retentionDays int, log *slog.Logger) (removed int, freed int64, err error) {
	if log == nil {
		log = slog.Default()
	}
	if retentionDays <= 0 {
		log.Debug("retention disabled", "dir", dir)
		return 0, 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	matches, err := filepath.Glob(filepath.Join(dir, PatternGlob(pattern)))
	if err != nil {
		return 0, 0, err
	}
	for _, p := range matches {
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() {
			continue
		}
		// Prefer the time encoded in the name; fall back to mtime. An SMB
		// server can report a stale mtime for a file that was held open, so
		// the name is the more trustworthy source.
		when := fi.ModTime()
		if t, ok := ParseTimeFromName(pattern, filepath.Base(p)); ok {
			when = t
		}
		if when.After(cutoff) {
			continue
		}
		size := fi.Size()
		if err := os.Remove(p); err != nil {
			log.Warn("retention: could not remove file", "file", p, "err", err)
			continue
		}
		removed++
		freed += size
		log.Info("retention: removed expired archive file",
			"file", filepath.Base(p), "age_days", int(time.Since(when).Hours()/24))
	}
	return removed, freed, nil
}
