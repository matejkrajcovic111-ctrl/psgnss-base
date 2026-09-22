package web

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/psgnss/psgnss-base/internal/boardprofile"
	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/receiver"
)

const (
	keyRTCMStationID = 0x30090001
	keyTModeMode     = 0x20030001
	keyTModePosType  = 0x20030002
	keyTModeLat      = 0x40030009
	keyTModeLon      = 0x4003000A
	keyTModeHeight   = 0x4003000B
	keyTModeLatHP    = 0x2003000C
	keyTModeLonHP    = 0x2003000D
	keyTModeHeightHP = 0x2003000E
)

func displayHiddenConstellations(c *config.Config) []string {
	if c.Web.DisplayHiddenConstellations == nil {
		return []string{"SBAS", "QZSS", "NavIC"}
	}
	return append([]string(nil), c.Web.DisplayHiddenConstellations...)
}

func diagnosticsIntervalMinutes(c *config.Config) int {
	if c.Web.DiagnosticsIntervalMinutes < 1 {
		return 60
	}
	return c.Web.DiagnosticsIntervalMinutes
}

func effectiveMountpoint(c *config.Config, m config.Mountpoint) map[string]any {
	format := m.Format
	if format == "" {
		format = c.Caster.FormatString
	}
	carrier := c.Caster.Carrier
	if m.Carrier != nil {
		carrier = *m.Carrier
	}
	network := m.Network
	if network == "" {
		network = c.Caster.Operator
	}
	country := m.Country
	if country == "" {
		country = c.Caster.Country
	}
	generator := m.Generator
	if generator == "" {
		generator = "PSGNSS"
	}
	compress := m.Compress
	if compress == "" {
		compress = "none"
	}
	auth := m.Auth
	if auth == "" {
		auth = "B"
	}
	fee := m.Fee
	if fee == "" {
		fee = "N"
	}
	return map[string]any{
		"name": m.Name, "enabled": !m.Disabled, "source_id": m.SourceID,
		"format": format, "carrier": carrier, "nav_system": m.NavSystem,
		"network": network, "country": country, "nmea": m.NMEA, "solution": m.Solution,
		"generator": generator, "compress": compress, "auth": auth, "fee": fee,
		"bitrate": m.Bitrate, "msm_detail": m.MSMDetail, "messages": m.Messages,
	}
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	profile, _ := boardprofile.Resolve(s.Cfg.Receiver.Profile, s.Cfg.Receiver.Model)
	mounts := make([]map[string]any, 0, len(s.Cfg.Caster.Mountpoint))
	for _, m := range s.Cfg.Caster.Mountpoint {
		mounts = append(mounts, effectiveMountpoint(s.Cfg, m))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"position": s.Cfg.Station.Position,
		"station": map[string]any{"name": s.Cfg.Station.Name, "station_id": s.Cfg.Station.StationID,
			"antenna": s.Cfg.Station.Antenna, "receiver": s.Cfg.Station.Receiver},
		"mountpoints": mounts,
		"general": map[string]any{
			"station_name": s.Cfg.Station.Name,
			"antenna":      s.Cfg.Station.Antenna, "receiver_description": s.Cfg.Station.Receiver,
			"caster_listen": s.Cfg.Caster.Listen, "proxy_listen": s.Cfg.Caster.ProxyListen,
			"web_listen": s.Cfg.Web.Listen, "hub_listeners": s.Cfg.Hub.Listener,
			"archive_mount": s.Cfg.Archive.MountPoint, "archive_spool": s.Cfg.Archive.SpoolDir,
			"archive_retention_days":        s.Cfg.Archive.RetentionDays,
			"archive_sync_seconds":          s.Cfg.Archive.SyncEverySec,
			"telemetry_retention_days":      s.Cfg.Telemetry.RetentionDays,
			"logging_level":                 s.Cfg.Logging.Level,
			"receiver_profile":              profile.ID,
			"display_hidden_constellations": displayHiddenConstellations(s.Cfg),
			"diagnostics_auto":              s.Cfg.Web.DiagnosticsAuto,
			"diagnostics_interval_minutes":  diagnosticsIntervalMinutes(s.Cfg),
			"map_tiles":                     s.Cfg.Web.MapTiles,
			"map_attribution":               s.Cfg.Web.MapAttribution,
			"map_contact":                   s.Cfg.Web.MapContact,
		},
		"board_profiles": boardprofile.All(),
	})
}

type generalSettingsInput struct {
	StationName string `json:"station_name"`
	// StationID is accepted only for compatibility with dashboard pages that
	// loaded the old Settings response before an upgrade. The guarded station
	// ID endpoint remains the only path allowed to change it.
	StationID                   *uint16           `json:"station_id,omitempty"`
	Antenna                     string            `json:"antenna"`
	ReceiverDescription         string            `json:"receiver_description"`
	CasterListen                string            `json:"caster_listen"`
	ProxyListen                 string            `json:"proxy_listen"`
	WebListen                   string            `json:"web_listen"`
	HubListeners                []config.Listener `json:"hub_listeners"`
	ArchiveMount                string            `json:"archive_mount"`
	ArchiveSpool                string            `json:"archive_spool"`
	ArchiveRetentionDays        int               `json:"archive_retention_days"`
	ArchiveSyncSeconds          int               `json:"archive_sync_seconds"`
	TelemetryRetentionDays      int               `json:"telemetry_retention_days"`
	LoggingLevel                string            `json:"logging_level"`
	ReceiverProfile             string            `json:"receiver_profile"`
	DisplayHiddenConstellations []string          `json:"display_hidden_constellations"`
	DiagnosticsAuto             bool              `json:"diagnostics_auto"`
	DiagnosticsIntervalMinutes  int               `json:"diagnostics_interval_minutes"`
	MapTiles                    string            `json:"map_tiles"`
	MapAttribution              string            `json:"map_attribution"`
	MapContact                  string            `json:"map_contact"`
}

func (s *Server) handleSettingsGeneral(w http.ResponseWriter, r *http.Request) {
	var in generalSettingsInput
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		writeErr(w, 400, "bad request: "+err.Error())
		return
	}
	// Preserve compatibility with clients from before these UI preferences
	// were added; the current dashboard always sends both fields explicitly.
	if in.DisplayHiddenConstellations == nil {
		in.DisplayHiddenConstellations = displayHiddenConstellations(s.Cfg)
	}
	if in.DiagnosticsIntervalMinutes == 0 {
		in.DiagnosticsIntervalMinutes = diagnosticsIntervalMinutes(s.Cfg)
	}
	if strings.TrimSpace(in.ReceiverProfile) == "" {
		profile, _ := boardprofile.Resolve(s.Cfg.Receiver.Profile, s.Cfg.Receiver.Model)
		in.ReceiverProfile = profile.ID
	}
	profile, err := boardprofile.Resolve(in.ReceiverProfile, s.Cfg.Receiver.Model)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !profile.Implemented {
		writeErr(w, 400, profile.Name+" is a declared profile stub; its receiver driver is not implemented")
		return
	}
	if strings.TrimSpace(in.StationName) == "" || strings.TrimSpace(in.Antenna) == "" {
		writeErr(w, 400, "station name and antenna descriptor are required")
		return
	}
	if strings.ContainsAny(in.StationName+in.Antenna+in.ReceiverDescription, "\r\n") {
		writeErr(w, http.StatusBadRequest, "station identity fields cannot contain line breaks")
		return
	}
	addresses := []struct {
		name, value string
		optional    bool
	}{{"caster listen", in.CasterListen, false}, {"PROXY listen", in.ProxyListen, true}, {"web listen", in.WebListen, false}}
	for _, l := range in.HubListeners {
		addresses = append(addresses, struct {
			name, value string
			optional    bool
		}{"hub listener " + l.Name, l.Listen, false})
	}
	seenAddr := map[string]string{}
	for _, a := range addresses {
		v := strings.TrimSpace(a.value)
		if v == "" && a.optional {
			continue
		}
		if _, _, err := net.SplitHostPort(v); err != nil {
			writeErr(w, 400, a.name+" must be a host:port address")
			return
		}
		if old := seenAddr[v]; old != "" {
			writeErr(w, 400, a.name+" conflicts with "+old)
			return
		}
		seenAddr[v] = a.name
	}
	if !filepath.IsAbs(in.ArchiveMount) || filepath.Clean(in.ArchiveMount) == "/" || !filepath.IsAbs(in.ArchiveSpool) || filepath.Clean(in.ArchiveSpool) == "/" {
		writeErr(w, 400, "archive mount and spool must be absolute, non-root paths")
		return
	}
	if filepath.Clean(in.ArchiveMount) == filepath.Clean(in.ArchiveSpool) {
		writeErr(w, 400, "archive mount and local spool must be different paths")
		return
	}
	if filepath.Clean(in.ArchiveMount) != filepath.Clean(s.Cfg.Archive.MountPoint) {
		if mounted, _ := mountState(in.ArchiveMount)["mounted"].(bool); !mounted {
			writeErr(w, 400, "new archive path is not currently a mounted filesystem")
			return
		}
	}
	if in.ArchiveRetentionDays < 0 || in.ArchiveRetentionDays > 3650 || in.TelemetryRetentionDays < 1 || in.TelemetryRetentionDays > 365 || in.ArchiveSyncSeconds < 5 || in.ArchiveSyncSeconds > 3600 {
		writeErr(w, 400, "retention or archive interval is outside the allowed range")
		return
	}
	switch in.LoggingLevel {
	case "debug", "info", "warn", "error":
	default:
		writeErr(w, 400, "logging level must be debug, info, warn or error")
		return
	}
	validSystem := map[string]bool{"GPS": true, "Galileo": true, "BeiDou": true, "SBAS": true, "GLONASS": true, "QZSS": true, "NavIC": true}
	seenSystem := map[string]bool{}
	for _, system := range in.DisplayHiddenConstellations {
		if !validSystem[system] || seenSystem[system] {
			writeErr(w, 400, "default display constellations contain an unknown or duplicate system")
			return
		}
		seenSystem[system] = true
	}
	if in.DiagnosticsIntervalMinutes < 1 || in.DiagnosticsIntervalMinutes > 1440 {
		writeErr(w, 400, "automatic diagnosis interval must be 1-1440 minutes")
		return
	}
	next := *s.Cfg
	next.Station.Name = strings.TrimSpace(in.StationName)
	next.Station.Antenna = strings.TrimSpace(in.Antenna)
	next.Station.Receiver = strings.TrimSpace(in.ReceiverDescription)
	next.Caster.Listen = strings.TrimSpace(in.CasterListen)
	next.Caster.ProxyListen = strings.TrimSpace(in.ProxyListen)
	next.Web.Listen = strings.TrimSpace(in.WebListen)
	next.Hub.Listener = append([]config.Listener(nil), in.HubListeners...)
	next.Archive.MountPoint = strings.TrimSpace(in.ArchiveMount)
	next.Archive.SpoolDir = strings.TrimSpace(in.ArchiveSpool)
	next.Archive.RetentionDays = in.ArchiveRetentionDays
	next.Archive.SyncEverySec = in.ArchiveSyncSeconds
	next.Telemetry.RetentionDays = in.TelemetryRetentionDays
	next.Logging.Level = in.LoggingLevel
	next.Receiver.Profile = profile.ID
	next.Web.DisplayHiddenConstellations = append([]string{}, in.DisplayHiddenConstellations...)
	next.Web.DiagnosticsAuto = in.DiagnosticsAuto
	next.Web.MapTiles = strings.TrimSpace(in.MapTiles)
	next.Web.MapAttribution = strings.TrimSpace(in.MapAttribution)
	next.Web.MapContact = strings.TrimSpace(in.MapContact)
	next.Web.DiagnosticsIntervalMinutes = in.DiagnosticsIntervalMinutes
	if err := next.Validate(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.saveConfig(&next); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.Log.Warn("general configuration changed; restarting", "by", adminOf(r))
	writeJSON(w, 200, map[string]any{"ok": true, "restarting": true})
	go s.restartSoon()
}

func (s *Server) handleSettingsStationIDApply(w http.ResponseWriter, r *http.Request) {
	var in struct {
		StationID uint16 `json:"station_id"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, in.StationID)
	next := *s.Cfg
	next.Station.StationID = in.StationID
	old := *s.Cfg
	hooks := receiverTxnHooks{beforeCommit: func() error { return s.saveConfig(&next) }, rollbackCommit: func() {
		if err := s.saveConfig(&old); err != nil {
			s.Log.Error("could not restore config after station ID commit failure", "err", err)
		}
	}, afterCommit: s.restartSoon}
	s.beginReceiverTxnWithHooks(w, r, []receiver.KV{{Key: keyRTCMStationID, Value: b}}, "RTCM station ID", nil, hooks)
}

type mountpointInput struct {
	Name      string              `json:"name"`
	Enabled   bool                `json:"enabled"`
	SourceID  int                 `json:"source_id"`
	Format    string              `json:"format"`
	Carrier   int                 `json:"carrier"`
	NavSystem string              `json:"nav_system"`
	Network   string              `json:"network"`
	Country   string              `json:"country"`
	NMEA      int                 `json:"nmea"`
	Solution  int                 `json:"solution"`
	Generator string              `json:"generator"`
	Compress  string              `json:"compress"`
	Auth      string              `json:"auth"`
	Fee       string              `json:"fee"`
	Bitrate   int                 `json:"bitrate"`
	MSMDetail string              `json:"msm_detail"`
	Messages  []config.MessageSel `json:"messages"`
}

func (in mountpointInput) config() config.Mountpoint {
	carrier := in.Carrier
	return config.Mountpoint{Name: strings.TrimSpace(in.Name), Disabled: !in.Enabled,
		SourceID: in.SourceID, Format: strings.TrimSpace(in.Format), Carrier: &carrier,
		NavSystem: strings.TrimSpace(in.NavSystem), Network: strings.TrimSpace(in.Network),
		Country: strings.ToUpper(strings.TrimSpace(in.Country)), NMEA: in.NMEA, Solution: in.Solution,
		Generator: strings.TrimSpace(in.Generator), Compress: strings.TrimSpace(in.Compress),
		Auth: strings.ToUpper(strings.TrimSpace(in.Auth)), Fee: strings.ToUpper(strings.TrimSpace(in.Fee)),
		Bitrate: in.Bitrate, MSMDetail: strings.TrimSpace(in.MSMDetail), Messages: in.Messages}
}

func (s *Server) saveConfig(next *config.Config) error {
	if s.ConfigPath == "" {
		return errors.New("configuration is not writable in this environment")
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	return config.Save(s.ConfigPath, next)
}

func (s *Server) restartSoon() {
	if s.Restart == nil {
		return
	}
	time.Sleep(500 * time.Millisecond)
	if err := s.Restart(); err != nil {
		s.Log.Error("settings restart failed", "err", err)
	}
}

func (s *Server) handleSettingsMountpoints(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mountpoints []mountpointInput `json:"mountpoints"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	if len(in.Mountpoints) > 32 {
		writeErr(w, http.StatusBadRequest, "at most 32 mountpoints are supported")
		return
	}
	next := *s.Cfg
	next.Caster.Mountpoint = make([]config.Mountpoint, 0, len(in.Mountpoints))
	for _, m := range in.Mountpoints {
		next.Caster.Mountpoint = append(next.Caster.Mountpoint, m.config())
	}
	if err := next.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.saveConfig(&next); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Warn("mountpoint configuration changed; restarting", "by", adminOf(r), "mountpoints", len(in.Mountpoints))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarting": true})
	go s.restartSoon()
}

type positionInput struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Height    float64 `json:"height"`
}

func splitScaled(value float64, mainScale, hpScale float64) (int32, int8, error) {
	main := math.Round(value / mainScale)
	hp := math.Round((value - main*mainScale) / hpScale)
	if main < math.MinInt32 || main > math.MaxInt32 || hp < math.MinInt8 || hp > math.MaxInt8 {
		return 0, 0, errors.New("coordinate is outside receiver representation")
	}
	return int32(main), int8(hp), nil
}

func signedLE32(v int32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(v))
	return b
}

func positionKVs(p positionInput) ([]receiver.KV, positionInput, error) {
	if math.IsNaN(p.Latitude) || math.IsInf(p.Latitude, 0) || p.Latitude < -90 || p.Latitude > 90 {
		return nil, p, errors.New("latitude must be between -90 and 90")
	}
	if math.IsNaN(p.Longitude) || math.IsInf(p.Longitude, 0) || p.Longitude < -180 || p.Longitude > 180 {
		return nil, p, errors.New("longitude must be between -180 and 180")
	}
	if math.IsNaN(p.Height) || math.IsInf(p.Height, 0) || p.Height < -10000 || p.Height > 100000 {
		return nil, p, errors.New("ellipsoidal height must be between -10000 and 100000 metres")
	}
	lat, latHP, err := splitScaled(p.Latitude, 1e-7, 1e-9)
	if err != nil {
		return nil, p, err
	}
	lon, lonHP, err := splitScaled(p.Longitude, 1e-7, 1e-9)
	if err != nil {
		return nil, p, err
	}
	h, hHP, err := splitScaled(p.Height, .01, .0001)
	if err != nil {
		return nil, p, err
	}
	actual := positionInput{Latitude: float64(lat)*1e-7 + float64(latHP)*1e-9,
		Longitude: float64(lon)*1e-7 + float64(lonHP)*1e-9, Height: float64(h)*.01 + float64(hHP)*.0001}
	return []receiver.KV{
		{Key: keyTModeMode, Value: []byte{2}}, {Key: keyTModePosType, Value: []byte{1}},
		{Key: keyTModeLat, Value: signedLE32(lat)}, {Key: keyTModeLon, Value: signedLE32(lon)},
		{Key: keyTModeHeight, Value: signedLE32(h)}, {Key: keyTModeLatHP, Value: []byte{byte(latHP)}},
		{Key: keyTModeLonHP, Value: []byte{byte(lonHP)}}, {Key: keyTModeHeightHP, Value: []byte{byte(hHP)}},
	}, actual, nil
}

func (s *Server) handleSettingsPositionApply(w http.ResponseWriter, r *http.Request) {
	var in positionInput
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	kvs, actual, err := positionKVs(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	next := *s.Cfg
	next.Station.Position.Mode = "fixed"
	next.Station.Position.Format = "llh"
	next.Station.Position.Latitude = actual.Latitude
	next.Station.Position.Longitude = actual.Longitude
	next.Station.Position.Height = actual.Height
	if err := next.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	old := *s.Cfg
	hooks := receiverTxnHooks{
		beforeCommit: func() error { return s.saveConfig(&next) },
		rollbackCommit: func() {
			if err := s.saveConfig(&old); err != nil {
				s.Log.Error("could not restore config after receiver commit failure", "err", err)
			}
		},
		afterCommit: s.restartSoon,
	}
	s.beginReceiverTxnWithHooks(w, r, kvs, "Base position", nil, hooks)
}
