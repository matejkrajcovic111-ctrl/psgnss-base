package web

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/psgnss/psgnss-base/internal/archive"
	"github.com/psgnss/psgnss-base/internal/hostinfo"
	"github.com/psgnss/psgnss-base/internal/store"
)

func (s *Server) handleOperationsLogs(w http.ResponseWriter, r *http.Request) {
	if s.Logs == nil {
		writeJSON(w, http.StatusOK, map[string]any{"entries": []any{}})
		return
	}
	level := strings.ToUpper(r.URL.Query().Get("level"))
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 1000 {
		limit = 300
	}
	all := s.Logs.Entries()
	out := make([]any, 0, limit)
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		e := all[i]
		if level != "" && level != "ALL" && e.Level != level {
			continue
		}
		if query != "" {
			b, _ := json.Marshal(e.Attrs)
			if !strings.Contains(strings.ToLower(e.Message+" "+string(b)), query) {
				continue
			}
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

func mountState(path string) map[string]any {
	out := map[string]any{"path": path, "mounted": false}
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	defer f.Close()
	clean := filepath.Clean(path)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) < 3 {
			continue
		}
		mount := strings.ReplaceAll(strings.ReplaceAll(p[1], `\040`, " "), `\134`, `\`)
		if filepath.Clean(mount) == clean {
			out["mounted"], out["source"], out["type"] = true, p[0], p[2]
			break
		}
	}
	if err := sc.Err(); err != nil {
		out["error"] = err.Error()
	}
	return out
}

func writableProbe(path string) error {
	f, err := os.CreateTemp(path, ".psgnss-diagnostic-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if _, err = f.Write([]byte("PSGNSS diagnostic probe\n")); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if removeErr := os.Remove(name); err == nil {
		err = removeErr
	}
	return err
}

func (s *Server) handleOperationsHealth(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	_, epochAt := s.Collector.Latest()
	frameAt := time.Unix(0, s.Hub.Stats.LastFrameAt.Load())
	checks := []map[string]any{}
	add := func(name string, ok bool, detail string, at time.Time) {
		checks = append(checks, map[string]any{"name": name, "ok": ok, "detail": detail, "last_activity": at.UTC()})
	}
	add("Receiver feed", !frameAt.IsZero() && now.Sub(frameAt) < 15*time.Second, "Last framed receiver message", frameAt)
	add("GNSS telemetry", !epochAt.IsZero() && now.Sub(epochAt) < 15*time.Second, "Last NAV-SAT epoch", epochAt)
	if err := s.Cfg.Validate(); err != nil {
		add("Configuration", false, err.Error(), now)
	} else {
		add("Configuration", true, "Loaded configuration passes validation", now)
	}
	if version, err := s.Store.Version(); err != nil {
		add("Telemetry database", false, err.Error(), now)
	} else {
		add("Telemetry database", true, "SQLite schema "+strconv.Itoa(version)+" is readable", now)
	}
	if st, err := os.Stat(s.Cfg.RINEX.ConvbinPath); err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		detail := "Converter is missing or not executable"
		if err != nil {
			detail = err.Error()
		}
		add("RINEX converter", false, detail, now)
	} else {
		add("RINEX converter", true, s.Cfg.RINEX.ConvbinPath+" is executable", now)
	}
	if err := writableProbe(s.Cfg.Archive.SpoolDir); err != nil {
		add("Local archive spool", false, "Write probe failed: "+err.Error(), now)
	} else {
		add("Local archive spool", true, "Create, sync and remove probe succeeded", now)
	}
	if s.Caster != nil {
		for _, m := range s.Caster.Mounts() {
			at := time.UnixMilli(m.LastData.Load())
			add("Mountpoint "+m.Entry.Name, m.LastData.Load() > 0 && now.Sub(at) < 15*time.Second,
				strconv.Itoa(m.ActiveClients())+" active client(s)", at)
		}
	}
	archives := s.archiveStats()
	for _, a := range archives {
		ok, detail, at := archiveCheck(a, now, s.Cfg.Archive.SyncEverySec)
		add("Archive "+a.Name, ok, detail, at)
	}
	smb := mountState(s.Cfg.Archive.MountPoint)
	if mounted, _ := smb["mounted"].(bool); !mounted {
		add("Network archive", false, "Configured archive filesystem is not mounted", now)
	} else if err := writableProbe(s.Cfg.Archive.MountPoint); err != nil {
		add("Network archive", false, "Mounted, but write probe failed: "+err.Error(), now)
	} else {
		add("Network archive", true, "Mounted; create, sync and remove probe succeeded", now)
	}
	if settings, err := s.Store.IntegritySettings(); err == nil && settings.Enabled {
		latest, latestErr := s.Store.LatestIntegrityRun()
		switch {
		case latestErr != nil:
			add("External position integrity", false, latestErr.Error(), now)
		case s.Integrity != nil && s.Integrity.Running():
			add("External position integrity", true, "Comparison is currently running", now)
		case latest == nil:
			add("External position integrity", false, "Enabled, but no comparison has completed", now)
		default:
			at := time.Unix(latest.FinishedAt, 0)
			fresh := now.Sub(at) < 36*time.Hour
			add("External position integrity", latest.Status == "pass" && fresh, latest.Detail, at)
		}
	}
	if s.Pushout != nil {
		for _, st := range s.Pushout.Status() {
			label := "NTRIP push-out · " + st.Name
			switch st.State {
			case "connected":
				fresh := st.LastDataAt > 0 && now.Sub(time.UnixMilli(st.LastDataAt)) < 30*time.Second
				detail := fmt.Sprintf("Publishing %s to %s/%s", st.Source, st.Host, st.Mountpoint)
				if !fresh {
					detail += "; no frame sent in the last 30 s"
				}
				add(label, fresh, detail, now)
			default:
				detail := st.State
				if st.LastError != "" {
					detail = st.LastError
				}
				add(label, false, detail, now)
			}
		}
	}
	if s.RTCMOut != nil {
		for _, st := range s.RTCMOut.Status() {
			label := "RTCM output · " + st.Name + " (" + st.Kind + ")"
			detail := st.LastError
			healthy := st.State == "sending"
			if healthy {
				detail = fmt.Sprintf("%s → %s; %d bytes sent", st.Source, st.Target, st.BytesSent)
				if st.LastDataAt > 0 && now.Sub(time.UnixMilli(st.LastDataAt)) > 30*time.Second {
					healthy = false
					detail += "; nothing sent in the last 30 s"
				}
			} else if detail == "" {
				detail = st.State
			}
			add(label, healthy, detail, now)
		}
	}
	if s.TimeSync != nil {
		if st := s.TimeSync.Status(); st.Enabled {
			detail := st.State
			if st.LastError != "" {
				detail = st.LastError
			} else if st.Samples > 0 {
				detail = fmt.Sprintf("%d samples offered to chrony; last offset %+.3f ms",
					st.Samples, st.LastOffsetS*1000)
			}
			fresh := st.State == "sending" && st.LastSampleAt > 0 &&
				now.Sub(time.Unix(st.LastSampleAt, 0)) < 60*time.Second
			add("Receiver time to chrony", fresh, detail, now)
		}
	}
	allOK := true
	for _, c := range checks {
		if ok, _ := c["ok"].(bool); !ok {
			allOK = false
		}
	}
	if mounted, _ := smb["mounted"].(bool); !mounted {
		allOK = false
	}
	resp := map[string]any{"ok": allOK, "time": now.UTC(), "checks": checks, "smb": smb,
		"host": hostinfo.Read(s.Cfg.Archive.MountPoint), "archives": archives,
		"hub":     map[string]any{"bytes_in": s.Hub.Stats.BytesIn.Load(), "frames": s.Hub.Stats.FramesIn.Load(), "bytes_dropped": s.Hub.Stats.BytesDropped.Load(), "resyncs": s.Hub.Stats.Resyncs.Load(), "reconnects": s.Hub.Stats.Reconnects.Load(), "last_drop_age_s": dropAge(s.Hub.Stats.LastDropAt.Load())},
		"clients": casterConns(s.Caster), "rejected": casterRejects(s.Caster)}
	if s.Ephemeris != nil {
		resp["ephemeris"] = s.Ephemeris.Status()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleOperationsRestart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm string `json:"confirm"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in) != nil || in.Confirm != "RESTART" {
		writeErr(w, http.StatusBadRequest, "confirmation must be RESTART")
		return
	}
	if s.Restart == nil {
		writeErr(w, http.StatusServiceUnavailable, "restart is unavailable")
		return
	}
	s.Log.Warn("PSGNSS restart requested from UI", "by", adminOf(r))
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "restarting": true})
	go s.restartSoon()
}

var adminNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func (s *Server) handleAdmins(w http.ResponseWriter, r *http.Request) {
	a, err := s.Store.ListAdmins()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, a)
}
func (s *Server) handleAdminCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || !adminNameRE.MatchString(in.Username) || len(in.Password) < 8 {
		writeErr(w, 400, "valid username and password of at least 8 characters are required")
		return
	}
	if err := s.Store.CreateAdmin(in.Username, in.Password); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	s.Log.Warn("web administrator created", "user", in.Username, "by", adminOf(r))
	writeJSON(w, 201, map[string]bool{"ok": true})
}
func (s *Server) handleAdminPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil || len(in.Password) < 8 {
		writeErr(w, 400, "password must be at least 8 characters")
		return
	}
	name := r.PathValue("name")
	if err := s.Store.SetAdminPassword(name, in.Password); err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	s.Log.Warn("web administrator password changed", "user", name, "by", adminOf(r))
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == adminOf(r) {
		writeErr(w, 409, "sign in as another administrator before deleting this account")
		return
	}
	if err := s.Store.DeleteAdmin(name); err != nil {
		code := 500
		if errors.Is(err, store.ErrNotFound) {
			code = 404
		} else if strings.Contains(err.Error(), "last administrator") {
			code = 409
		}
		writeErr(w, code, err.Error())
		return
	}
	s.Log.Warn("web administrator deleted", "user", name, "by", adminOf(r))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// archiveCheck judges one archive writer for the health list.
func archiveCheck(a archive.Stats, now time.Time, syncEverySec int) (bool, string, time.Time) {
	at := time.Unix(a.LastSync, 0)
	recent := a.LastSync > 0 && now.Sub(at) < 3*time.Duration(max(1, syncEverySec))*time.Second
	// A writer begins recording immediately, while its first periodic share
	// sync has not happened yet. The separate network-archive write probe
	// verifies destination access during this short post-start window.
	resumed := a.LastSync == 0 && a.Bytes > 0
	// Failures is a count since start, so it cannot say whether anything
	// is wrong now: one soft-mount timeout used to hold this check red for
	// days after every sync since had succeeded. It fails while the latest
	// failure is newer than the latest sync, or a finished file is still
	// waiting in the spool.
	recovered := a.LastFailure == 0 || a.LastSync > a.LastFailure
	ok := recovered && a.Pending == 0 && (recent || resumed)
	detail := a.File + " · " + strconv.FormatInt(a.Failures, 10) + " sync failure(s) since start"
	if a.LastFailure > 0 {
		detail += " · last " + time.Unix(a.LastFailure, 0).UTC().Format("2006-01-02 15:04Z")
		if recovered {
			detail += ", recovered"
		} else if a.LastError != "" {
			detail += ": " + a.LastError
		}
	}
	if a.Pending > 0 {
		detail += " · " + strconv.FormatInt(a.Pending, 10) + " finished file(s) waiting in the spool"
	}
	if resumed {
		detail += " · first post-restart sync pending"
		at = now
	}
	return ok, detail, at
}
