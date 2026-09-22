package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/psgnss/psgnss-base/internal/archive"
	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/hub"
	"github.com/psgnss/psgnss-base/internal/rinex"
)

// startArchives wires the archives to the hub and starts retention.
func startArchives(ctx context.Context, cfg *config.Config, h *hub.Hub,
	wg *sync.WaitGroup, log *slog.Logger) ([]*archive.Writer, error) {
	if cfg.Archive.SyncEverySec > 0 {
		archive.SyncInterval = time.Duration(cfg.Archive.SyncEverySec) * time.Second
	}

	var writers []*archive.Writer
	type spec struct {
		name string
		set  config.ArchiveSet
		kind string
	}
	for _, s := range []spec{
		{"rtcm", cfg.Archive.RTCM, "rtcm"},
		{"ubx", cfg.Archive.UBX, "ubx"},
		{"nav", cfg.Archive.Nav, "nav"},
	} {
		if !s.set.Enabled {
			log.Info("archive disabled", "archive", s.name)
			continue
		}
		proto, err := hub.ParseProto(s.set.Filter)
		if err != nil {
			return nil, fmt.Errorf("archive %s: %w", s.name, err)
		}
		dest := ""
		if cfg.Archive.MountPoint != "" {
			dest = filepath.Join(cfg.Archive.MountPoint, s.set.Subdir)
		}
		w, err := archive.NewWriter(s.name,
			filepath.Join(cfg.Archive.SpoolDir, s.name), dest,
			s.set.Pattern, s.set.SwapHours,
			time.Duration(s.set.SwapMargin)*time.Second, log)
		if err != nil {
			return nil, err
		}
		// A message list narrows the archive further, which is how the nav
		// archive records only the ephemeris source instead of all of UBX.
		var f *hub.Filter
		if len(s.set.Messages) > 0 {
			specs := make([]hub.FilterSpec, 0, len(s.set.Messages))
			for _, name := range s.set.Messages {
				t, err := hub.ParseUBXMessage(name)
				if err != nil {
					return nil, fmt.Errorf("archive %s: %w (known: %v)",
						s.name, err, hub.UBXMessageNames())
				}
				specs = append(specs, hub.FilterSpec{Type: t, Interval: 1})
			}
			f, err = hub.NewFilter(proto, specs)
			if err != nil {
				return nil, fmt.Errorf("archive %s: %w", s.name, err)
			}
		} else {
			f = hub.NewProtoFilter(proto)
		}
		writers = append(writers, w)
		sub := h.Subscribe("archive:"+s.name, f, 1024)
		wg.Add(1)
		go func(w *archive.Writer, sub *hub.Sub) {
			defer wg.Done()
			defer sub.Close()
			w.Run(ctx, sub.C())
		}(w, sub)
		log.Info("archive running", "archive", s.name, "filter", s.set.Filter,
			"messages", s.set.Messages, "pattern", s.set.Pattern, "dest", dest)

		// Retention, daily. Deleting real data, so it only runs with a
		// positive retention configured.
		if cfg.Archive.RetentionDays > 0 && dest != "" {
			wg.Add(1)
			go func(dest, pattern, name string) {
				defer wg.Done()
				t := time.NewTicker(24 * time.Hour)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						n, freed, err := archive.Prune(dest, pattern,
							cfg.Archive.RetentionDays, log)
						if err != nil {
							log.Error("retention failed", "archive", name, "err", err)
						} else if n > 0 {
							log.Info("retention complete", "archive", name,
								"removed", n, "freed_mb", freed/(1<<20))
						}
					}
				}
			}(dest, s.set.Pattern, s.name)
		}
	}

	return writers, nil
}

// rinexConvert converts one archive file on demand.
func rinexConvert(cfg *config.Config, rawPath, outDir string) error {
	if outDir == "" {
		outDir = cfg.RINEX.OutputDir
	}
	content, err := rinex.Sniff(rawPath, 1<<20)
	if err != nil {
		return err
	}
	fmt.Printf("source   : %s\n", filepath.Base(rawPath))
	fmt.Printf("content  : %s\n", content.String())
	fmt.Printf("format   : %s (detected, not assumed from the filename)\n", content.Format())
	if !content.HasUBX() {
		fmt.Println("warning  : no UBX in this file; no navigation data can be produced")
	}

	base := filepath.Base(rawPath)
	start, ok := archive.ParseTimeFromName(cfg.Archive.RTCM.Pattern, base)
	if !ok {
		start = time.Now().AddDate(0, 0, -1).Truncate(24 * time.Hour)
	}
	end := start.Add(24 * time.Hour)

	res, err := rinex.Convert(context.Background(), rawPath, start, end, outDir, base,
		rinex.Options{
			ConvbinPath: cfg.RINEX.ConvbinPath,
			Version:     cfg.RINEX.Version,
			Frequencies: cfg.RINEX.Frequencies,
			Log:         slog.Default(),
		})
	if err != nil {
		return err
	}
	fmt.Printf("converted in %s using -f %d\n", res.Duration.Round(time.Millisecond),
		cfg.RINEX.Frequencies)
	for _, f := range res.Files {
		fi, _ := os.Stat(f)
		fmt.Printf("  %s  %.1f MB\n", f, float64(fi.Size())/(1<<20))
	}
	return nil
}
