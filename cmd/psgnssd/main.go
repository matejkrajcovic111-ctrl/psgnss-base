// Command psgnssd is the PSGNSS_base daemon: stream hub, NTRIP caster,
// archiver and web UI in a single process.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/downloader"
	"github.com/psgnss/psgnss-base/internal/ephemeris"
	"github.com/psgnss/psgnss-base/internal/hostinfo"
	"github.com/psgnss/psgnss-base/internal/hub"
	"github.com/psgnss/psgnss-base/internal/integrity"
	"github.com/psgnss/psgnss-base/internal/logbuffer"
	"github.com/psgnss/psgnss-base/internal/pushout"
	"github.com/psgnss/psgnss-base/internal/rtcm"
	"github.com/psgnss/psgnss-base/internal/rtcmout"
	"github.com/psgnss/psgnss-base/internal/secrets"
	"github.com/psgnss/psgnss-base/internal/store"
	"github.com/psgnss/psgnss-base/internal/telemetry"
	"github.com/psgnss/psgnss-base/internal/timesync"
	"github.com/psgnss/psgnss-base/internal/version"
	"github.com/psgnss/psgnss-base/internal/web"
)

func main() {
	var (
		cfgPath     = flag.String("config", "/etc/psgnss/psgnss.toml", "path to config file")
		checkOnly   = flag.Bool("check", false, "validate config and exit")
		showVersion = flag.Bool("version", false, "print version and exit")
		rxInfo      = flag.Bool("receiver-info", false, "poll the receiver for identification and config, then exit (needs exclusive serial access)")
		rxRevert    = flag.String("receiver-revert-test", "", "prove the auto-revert path against hardware: key,hexvalue,seconds (never commits)")
		userAddF    = flag.String("user-add", "", "create an NTRIP user: username:password[:limit]")
		userListF   = flag.Bool("user-list", false, "list NTRIP users")
		showPass    = flag.Bool("show-passwords", false, "with --user-list, decrypt and show passwords")
		connListF   = flag.Int("connections", 0, "show the N most recent connections")
		rinexConv   = flag.String("rinex", "", "convert one raw archive file to RINEX")
		rinexOut    = flag.String("rinex-out", "", "output directory for --rinex")
		adminAdd    = flag.String("admin-add", "", "create the web admin account: username:password")
		doSetup     = flag.Bool("setup", false, "first-run interview: build this station's config, key, units and admin account")
		setupRoot   = flag.String("setup-root", "", "with --setup, write the install under this directory instead of / (for inspection)")
		setupDry    = flag.Bool("setup-dry-run", false, "with --setup, show the plan and write nothing")
		setupForce  = flag.Bool("setup-force", false, "with --setup, replace an existing installation")
		setupYes    = flag.Bool("setup-yes", false, "with --setup, do not ask for confirmation before writing")
		updCheck    = flag.Bool("update-check", false, "check for a signed release and report, changing nothing")
		updApply    = flag.Bool("update", false, "install the available signed release, with rollback if it does not serve")
		updYes      = flag.Bool("update-yes", false, "with --update, do not ask for confirmation")
		updInv      = flag.Bool("dependencies", false, "list this build's third-party dependencies and their versions")
		updInvJSON  = flag.Bool("dependencies-json", false, "with --dependencies, print JSON for the release tool")
	)
	flag.Parse()

	if *updInv || *updInvJSON {
		// Runs without a configuration: a build should be able to say what it
		// is made of even on a machine that has no station on it.
		cfg, _ := config.Load(*cfgPath)
		if err := inventory(cfg, *updInvJSON); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if *updCheck || *updApply {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if *updCheck {
			err = updateCheck(cfg, *cfgPath)
		} else {
			err = updateApply(cfg, *cfgPath, *updYes)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "\nupdate: "+err.Error())
			os.Exit(1)
		}
		return
	}

	if *doSetup {
		if err := runSetup(*setupRoot, *setupDry, *setupForce, *setupYes); err != nil {
			fmt.Fprintln(os.Stderr, "\nsetup: "+err.Error())
			os.Exit(1)
		}
		return
	}

	if *showVersion {
		fmt.Println("psgnssd", version.String())
		return
	}

	if *adminAdd != "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		parts := strings.SplitN(*adminAdd, ":", 2)
		if len(parts) != 2 {
			fmt.Fprintln(os.Stderr, "error: want username:password")
			os.Exit(1)
		}
		if err := adminCreate(cfg, parts[0], parts[1]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if *rinexConv != "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
		err = rinexConvert(cfg, *rinexConv, *rinexOut)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if *userAddF != "" || *userListF || *connListF > 0 {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		switch {
		case *userAddF != "":
			parts := strings.SplitN(*userAddF, ":", 3)
			if len(parts) < 2 {
				fmt.Fprintln(os.Stderr, "error: want username:password[:limit]")
				os.Exit(1)
			}
			limit := 5
			if len(parts) == 3 {
				if n, e := strconv.Atoi(parts[2]); e == nil {
					limit = n
				}
			}
			err = userAdd(cfg, parts[0], parts[1], limit)
		case *userListF:
			err = userList(cfg, *showPass)
		default:
			err = connList(cfg, *connListF)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if *rxInfo || *rxRevert != "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
		if *rxInfo {
			err = receiverInfo(cfg)
		} else {
			err = runRevertTest(cfg, *rxRevert)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(*cfgPath, *checkOnly); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, checkOnly bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	lvl := slog.LevelInfo
	switch cfg.Logging.Level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	logs := logbuffer.New(1000)
	textLog := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(logbuffer.Tee(textLog, logs)))

	if checkOnly {
		fmt.Printf("config OK: %s\n", cfgPath)
		fmt.Printf("  station      %s (id %d)\n", cfg.Station.Name, cfg.Station.StationID)
		fmt.Printf("  receiver     %s @ %d\n", cfg.Receiver.Model, cfg.Receiver.Baud)
		fmt.Printf("  mountpoints  %d\n", len(cfg.Caster.Mountpoint))
		fmt.Printf("  hub listeners %d\n", len(cfg.Hub.Listener))
		return nil
	}

	slog.Info("psgnssd starting", "version", version.String(), "station", cfg.Station.Name)

	db, err := store.Open(cfg.Telemetry.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	v, err := db.Version()
	if err != nil {
		return err
	}
	slog.Info("database ready", "path", cfg.Telemetry.DBPath, "schema", v)

	// ---- stream hub ----
	inputKind := cfg.Hub.Input
	if inputKind == "" {
		inputKind = "serial"
	}
	inputAddr := cfg.Hub.InputAddr
	if inputKind == "serial" && inputAddr == "" {
		inputAddr = cfg.Receiver.Device
	}
	src, err := hub.NewSource(inputKind, inputAddr, cfg.Receiver.Baud)
	if err != nil {
		return err
	}

	h, err := hub.New(hub.Options{
		Source:     src,
		BufSize:    cfg.Hub.ReadBuffer,
		StallWarn:  time.Duration(cfg.Hub.StallWarn) * time.Second,
		RetryDelay: time.Duration(cfg.Hub.RetryDelay) * time.Second,
		Logger:     slog.Default(),
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup

	// Raw passthrough listeners, replacing the str2str/STRSVR TCP outputs.
	var servers []*hub.TCPServer
	for _, l := range cfg.Hub.Listener {
		p, err := hub.ParseProto(l.Filter)
		if err != nil {
			return fmt.Errorf("listener %q: %w", l.Name, err)
		}
		var f *hub.Filter
		if p == hub.ProtoUnknown {
			f = hub.NewPassthrough()
		} else {
			f = hub.NewProtoFilter(p)
		}
		srv := hub.NewTCPServer(h, l.Name, l.Listen, f, 512, slog.Default())
		servers = append(servers, srv)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("listener failed", "name", srv.Name, "addr", srv.Addr, "err", err)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := h.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("hub stopped", "err", err)
		}
	}()

	// ---- NTRIP caster ----
	kr, created, err := secrets.LoadOrGenerate(cfg.Security.KeyFile)
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	if created {
		slog.Warn("generated a new master key", "path", cfg.Security.KeyFile,
			"note", "back this up separately from the database")
	}
	if n, err := db.CloseStaleConnections(); err == nil && n > 0 {
		slog.Info("closed stale connection rows from a previous run", "rows", n)
	}

	// Navigation messages synthesised from the receiver's broadcast subframes.
	// The receiver emits no ephemeris RTCM and cannot be configured to; see
	// internal/ephemeris.
	eph := ephemeris.New(slog.Default())
	wg.Add(1)
	go func() { defer wg.Done(); eph.Run(ctx, h) }()

	cst, err := buildCaster(cfg, h, db, kr, eph, slog.Default())
	if err != nil {
		return err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := cst.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("caster stopped", "err", err)
		}
	}()

	// Periodic stats, so operation is observable before the web UI exists.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				args := []any{
					"bytes_in", h.Stats.BytesIn.Load(),
					"frames", h.Stats.FramesIn.Load(),
					"rtcm", h.Stats.RTCMFrames.Load(),
					"ubx", h.Stats.UBXFrames.Load(),
					"dropped", h.Stats.BytesDropped.Load(),
					"resyncs", h.Stats.Resyncs.Load(),
				}
				for _, srv := range servers {
					args = append(args, srv.Name+"_clients", srv.Active.Load())
				}
				args = append(args, "ntrip_clients", cst.Connections.Load(),
					"ntrip_rejected", cst.Rejected.Load())
				slog.Info("hub stats", args...)
			}
		}
	}()

	archives, err := startArchives(ctx, cfg, h, &wg, slog.Default())
	if err != nil {
		return err
	}

	// ---- telemetry ----
	coll := telemetry.NewCollector(db,
		time.Duration(cfg.Telemetry.FineWindowMin)*time.Minute,
		cfg.Telemetry.CoarseInterval, cfg.Telemetry.RetentionDays, slog.Default())
	tsub := h.Subscribe("telemetry", hub.NewProtoFilter(hub.ProtoUBX), 512)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer tsub.Close()
		coll.Run(ctx, tsub.C())
	}()

	// Host and stream health, on the coarse interval.
	wg.Add(1)
	go func() {
		defer wg.Done()
		iv := time.Duration(cfg.Telemetry.CoarseInterval) * time.Second
		if iv <= 0 {
			iv = 30 * time.Second
		}
		t := time.NewTicker(iv)
		defer t.Stop()
		var lastBytes int64
		last := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				hi := hostinfo.Read(cfg.Archive.MountPoint)
				cur := h.Stats.BytesIn.Load()
				el := now.Sub(last).Seconds()
				var bps int64
				if el > 0 {
					bps = int64(float64(cur-lastBytes) * 8 / el)
				}
				lastBytes, last = cur, now
				rtcm := h.Stats.RTCMFrames.Load()
				ubx := h.Stats.UBXFrames.Load()
				var rb, ub int64
				if tot := rtcm + ubx; tot > 0 {
					rb = bps * rtcm / tot
					ub = bps * ubx / tot
				}
				if err := db.PutHealth(now, cur, rb, ub, int(cst.Connections.Load()),
					hi.CPUPercent, hi.TempC, hi.DiskFreeMB, hi.MemFreeMB); err != nil {
					slog.Debug("health sample failed", "err", err)
				}
			}
		}
	}()

	// ---- RINEX downloader ----
	ecefX, ecefY, ecefZ := rtcm.LLHToECEF(cfg.Station.Position.Latitude,
		cfg.Station.Position.Longitude, cfg.Station.Position.Height)
	dl := downloader.New(downloader.Options{
		Sources: []downloader.Source{
			// Spool first: the file currently being written lives here, so the
			// day being recorded is downloadable up to the present moment.
			{Kind: "rtcm", Dir: filepath.Join(cfg.Archive.SpoolDir, "rtcm"),
				Prefix: archivePrefix(cfg.Archive.RTCM.Pattern), HasUBX: true, Spool: true},
			{Kind: "nav", Dir: filepath.Join(cfg.Archive.SpoolDir, "nav"),
				Prefix: archivePrefix(cfg.Archive.Nav.Pattern), HasUBX: true, Spool: true},
			{Kind: "ubx", Dir: filepath.Join(cfg.Archive.SpoolDir, "ubx"),
				Prefix: archivePrefix(cfg.Archive.UBX.Pattern), HasUBX: true, Spool: true},
			// Observations come from the RTCM archive; historical files there
			// are the full multiplexed stream and carry UBX too.
			{Kind: "rtcm", Dir: filepath.Join(cfg.Archive.MountPoint, cfg.Archive.RTCM.Subdir),
				Prefix: archivePrefix(cfg.Archive.RTCM.Pattern), HasUBX: true},
			// Ephemeris comes from the nav archive.
			{Kind: "nav", Dir: filepath.Join(cfg.Archive.MountPoint, cfg.Archive.Nav.Subdir),
				Prefix: archivePrefix(cfg.Archive.Nav.Pattern), HasUBX: true},
			{Kind: "ubx", Dir: filepath.Join(cfg.Archive.MountPoint, cfg.Archive.UBX.Subdir),
				Prefix: archivePrefix(cfg.Archive.UBX.Pattern), HasUBX: true},
		},
		ConvbinPath: cfg.RINEX.ConvbinPath,
		Version:     cfg.RINEX.Version,
		Frequencies: cfg.RINEX.Frequencies,
		// Station identification for the RINEX header. A post-processing
		// service reads these out of the file; without them a submission is
		// anonymous. The position is the broadcast base coordinate in ECEF,
		// the same one RTCM 1005 carries.
		Marker:    cfg.Station.Name,
		Antenna:   cfg.Station.Antenna,
		Receiver:  cfg.Station.Receiver,
		Comment:   "PSGNSS " + version.Version,
		PositionX: ecefX, PositionY: ecefY, PositionZ: ecefZ,
		WorkDir: cfg.RINEX.OutputDir,
		Log:     slog.Default(),
	})
	wg.Add(1)
	go func() { defer wg.Done(); dl.Sweep(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); dl.Warm(ctx) }()

	// ---- non-caster RTCM outputs: UDP and serial ----
	// From RTKBase's rtcm_udp_* and rtcm_serial services; see CREDITS.md.
	// UNVERIFIED on a real radio: no transmitter has been attached.
	rtcmOut := rtcmout.New(cfg.RTCMOut, cst, slog.Default())
	wg.Add(1)
	go func() { defer wg.Done(); rtcmOut.Run(ctx) }()

	// ---- receiver time offered to chrony ----
	// The goal comes from RTKBase's gpsd+chrony arrangement; this reaches it
	// without gpsd. See CREDITS.md and internal/timesync.
	timeSender := timesync.New(h, timesync.Options{
		Socket:      cfg.TimeSync.ChronySocket,
		MinInterval: time.Duration(cfg.TimeSync.MinIntervalSec) * time.Second,
		MaxAccuracy: time.Duration(cfg.TimeSync.MaxAccuracyMS) * time.Millisecond,
	}, slog.Default())
	wg.Add(1)
	go func() { defer wg.Done(); timeSender.Run(ctx) }()

	// ---- NTRIP push-out to remote casters ----
	// Modelled on RTKBase's ntrip_A/ntrip_B services; see CREDITS.md.
	pushMgr := pushout.New(db, kr, cst, slog.Default())
	wg.Add(1)
	go func() { defer wg.Done(); pushMgr.Run(ctx) }()

	// ---- independent external base-position integrity monitor ----
	integrityMonitor := integrity.New(db, kr, cfg, h, slog.Default())
	wg.Add(1)
	go func() { defer wg.Done(); integrityMonitor.StartScheduler(ctx) }()

	// ---- web UI ----
	if cfg.Web.Listen != "" {
		ok, err := db.HasAdmin()
		if err != nil {
			return err
		}
		if !ok {
			slog.Warn("no web admin account exists; the UI cannot be signed into",
				"fix", "psgnssd --admin-add <username>:<password>")
		}
		ws := web.New(&web.Server{
			Cfg: cfg, ConfigPath: cfgPath, Store: db, Keyring: kr, Hub: h, Caster: cst,
			Collector: coll, Downloader: dl, Integrity: integrityMonitor, Ephemeris: eph, Pushout: pushMgr,
			TimeSync: timeSender, RTCMOut: rtcmOut,
			Archives: archives, Log: slog.Default(), Logs: logs,
			Restart: func() error { return syscall.Kill(os.Getpid(), syscall.SIGHUP) },
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ws.Run(ctx); err != nil {
				slog.Error("web server stopped", "err", err)
			}
		}()
	}

	slog.Info("hub running", "input", src.String(), "listeners", len(servers))
	slog.Info("caster running", "addr", cfg.Caster.Listen, "mountpoints", len(cst.Mounts()))
	slog.Info("web UI", "addr", cfg.Web.Listen)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	s := <-sig
	slog.Info("shutting down", "signal", s.String())
	cancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		slog.Warn("shutdown timed out, exiting anyway")
	}
	return nil
}

// runRevertTest parses "key,hexvalue,seconds" and runs the hardware revert test.
func runRevertTest(cfg *config.Config, spec string) error {
	parts := strings.Split(spec, ",")
	if len(parts) != 3 {
		return fmt.Errorf("want key,hexvalue,seconds (e.g. 0x40530001,00c20100,15)")
	}
	k, err := strconv.ParseUint(strings.TrimPrefix(parts[0], "0x"), 16, 32)
	if err != nil {
		return fmt.Errorf("bad key %q: %w", parts[0], err)
	}
	val, err := hex.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("bad hex value %q: %w", parts[1], err)
	}
	secs, err := strconv.Atoi(parts[2])
	if err != nil {
		return fmt.Errorf("bad seconds %q: %w", parts[2], err)
	}
	return receiverRevertTest(cfg, uint32(k), val, time.Duration(secs)*time.Second)
}

// archivePrefix is the literal part of a filename pattern before the first
// date verb, e.g. "Base1_%Y%m%d%h00" -> "Base1_".
func archivePrefix(pattern string) string {
	if i := strings.IndexByte(pattern, '%'); i >= 0 {
		return pattern[:i]
	}
	return pattern
}
