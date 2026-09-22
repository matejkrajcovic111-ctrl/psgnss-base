// Package config loads and validates the PSGNSS TOML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/psgnss/psgnss-base/internal/boardprofile"
)

type Config struct {
	Station   Station   `toml:"station"`
	TimeSync  TimeSync  `toml:"timesync"`
	RTCMOut   []RTCMOut `toml:"rtcm_out"`
	Receiver  Receiver  `toml:"receiver"`
	Hub       Hub       `toml:"hub"`
	Caster    Caster    `toml:"caster"`
	Archive   Archive   `toml:"archive"`
	RINEX     RINEX     `toml:"rinex"`
	Telemetry Telemetry `toml:"telemetry"`
	Web       Web       `toml:"web"`
	Update    Update    `toml:"update"`
	Security  Security  `toml:"security"`
	Logging   Logging   `toml:"logging"`
}

type Station struct {
	Name      string   `toml:"name"`
	StationID uint16   `toml:"station_id"`
	Antenna   string   `toml:"antenna"`
	Receiver  string   `toml:"receiver"`
	Position  Position `toml:"position"`
}

// Position is the broadcast base coordinate, authoritative for RTCM 1005/1006.
type Position struct {
	Mode      string     `toml:"mode" json:"mode"`     // fixed | survey_in
	Format    string     `toml:"format" json:"format"` // llh | ecef
	Latitude  float64    `toml:"latitude" json:"latitude"`
	Longitude float64    `toml:"longitude" json:"longitude"`
	Height    float64    `toml:"height" json:"height"`
	X, Y, Z   float64    `toml:"-"`
	ENUOffset [3]float64 `toml:"enu_offset" json:"enu_offset"`
}

type Receiver struct {
	Device        string `toml:"device"`
	Baud          int    `toml:"baud"`
	Profile       string `toml:"profile,omitempty"`
	Model         string `toml:"model"`
	VerifyModel   bool   `toml:"verify_model"`
	RevertTimeout int    `toml:"revert_timeout"`
}

type Hub struct {
	// Input source. "serial" is production; "tcp" lets the hub run alongside
	// the existing str2str for verification, since only one process may hold
	// the serial device. "file" replays a capture.
	Input      string     `toml:"input"`
	InputAddr  string     `toml:"input_addr"`
	ReadBuffer int        `toml:"read_buffer"`
	StallWarn  int        `toml:"stall_warn"`
	RetryDelay int        `toml:"retry_delay"`
	Listener   []Listener `toml:"listener"`
}

type Listener struct {
	Name   string `toml:"name"`
	Listen string `toml:"listen"`
	Filter string `toml:"filter"` // none | rtcm3 | ubx
}

type Caster struct {
	Listen       string       `toml:"listen"`
	ProxyListen  string       `toml:"proxy_listen"`
	ProxyTrusted []string     `toml:"proxy_trusted"`
	NtripV1      bool         `toml:"ntrip_v1"`
	NtripV2      bool         `toml:"ntrip_v2"`
	Operator     string       `toml:"operator"`
	Country      string       `toml:"country"`
	FormatString string       `toml:"format_string"`
	Carrier      int          `toml:"carrier"`
	Mountpoint   []Mountpoint `toml:"mountpoint"`
}

type Mountpoint struct {
	Name      string       `toml:"name" json:"name"`
	Disabled  bool         `toml:"disabled,omitempty" json:"disabled"`
	SourceID  int          `toml:"source_id" json:"source_id"`
	Format    string       `toml:"format,omitempty" json:"format"`
	Carrier   *int         `toml:"carrier,omitempty" json:"carrier"`
	NavSystem string       `toml:"nav_system" json:"nav_system"`
	Network   string       `toml:"network,omitempty" json:"network"`
	Country   string       `toml:"country,omitempty" json:"country"`
	NMEA      int          `toml:"nmea,omitempty" json:"nmea"`
	Solution  int          `toml:"solution,omitempty" json:"solution"`
	Generator string       `toml:"generator,omitempty" json:"generator"`
	Compress  string       `toml:"compress,omitempty" json:"compress"`
	Auth      string       `toml:"auth,omitempty" json:"auth"`
	Fee       string       `toml:"fee,omitempty" json:"fee"`
	Bitrate   int          `toml:"bitrate,omitempty" json:"bitrate"`
	MSMDetail string       `toml:"msm_detail,omitempty" json:"msm_detail"`
	Messages  []MessageSel `toml:"messages" json:"messages"`
}

// MessageSel is a pure filter: message type plus decimation in seconds.
// There is no transcoding anywhere in this design.
type MessageSel struct {
	Type     int `toml:"type" json:"type"`
	Interval int `toml:"interval" json:"interval"`
}

type Archive struct {
	SpoolDir   string `toml:"spool_dir"`
	MountPoint string `toml:"mount_point"`
	// SyncEverySec is how often the growing archive file is appended to the
	// share. Waiting for the daily swap would leave a day of data on one disk.
	SyncEverySec  int        `toml:"sync_every_sec"`
	RetentionDays int        `toml:"retention_days"`
	RTCM          ArchiveSet `toml:"rtcm"`
	UBX           ArchiveSet `toml:"ubx"`
	// Nav records the ephemeris and timed raw-measurement messages required to
	// build RINEX navigation files. It remains smaller than full UBX.
	Nav ArchiveSet `toml:"nav"`
}

type ArchiveSet struct {
	Enabled    bool   `toml:"enabled"`
	Pattern    string `toml:"pattern"`
	Subdir     string `toml:"subdir"`
	SwapHours  int    `toml:"swap_hours"`
	SwapMargin int    `toml:"swap_margin"`
	Filter     string `toml:"filter"`
	// Messages narrows a UBX archive to specific message types by name, e.g.
	// ["RXM-SFRBX", "RXM-RAWX"] to record the complete RINEX NAV source. Empty
	// means every message of the filtered protocol.
	Messages []string `toml:"messages"`
}

type RINEX struct {
	ConvbinPath string `toml:"convbin_path"`
	Version     string `toml:"version"`
	Frequencies int    `toml:"frequencies"`
	RawFormat   string `toml:"raw_format"`
	OutputDir   string `toml:"output_dir"`
	Compression string `toml:"compression"`
}

type Telemetry struct {
	DBPath         string `toml:"db_path"`
	RetentionDays  int    `toml:"retention_days"`
	FineRateHz     int    `toml:"fine_rate_hz"`
	FineWindowMin  int    `toml:"fine_window_min"`
	CoarseInterval int    `toml:"coarse_interval"`
}

type Web struct {
	Listen                      string   `toml:"listen"`
	Language                    string   `toml:"language"`
	DisplayHiddenConstellations []string `toml:"display_hidden_constellations"`
	DiagnosticsAuto             bool     `toml:"diagnostics_auto"`
	DiagnosticsIntervalMinutes  int      `toml:"diagnostics_interval_minutes"`
	// MapTiles is an XYZ raster tile template for the base position map, e.g.
	// https://tile.openstreetmap.org/{z}/{x}/{y}.png. The browser never fetches
	// it: psgnssd proxies and caches tiles so it can send a User-Agent that
	// identifies this station, which OpenStreetMap's tile usage policy requires
	// and a browser cannot do. Empty means no map.
	MapTiles string `toml:"map_tiles"`
	// MapAttribution is shown on the map. Tile providers require credit.
	MapAttribution string `toml:"map_attribution"`
	// MapContact goes into the outgoing User-Agent: an email or URL a tile
	// provider can use to ask this station to stop. Without one the station
	// name is used, which is weaker but still traceable.
	MapContact string `toml:"map_contact"`
	// MapCacheDir holds fetched tiles. Caching is not an optimisation here: it
	// is the part of the usage policy that asks clients not to re-fetch.
	MapCacheDir  string `toml:"map_cache_dir"`
	MapCacheDays int    `toml:"map_cache_days"`
}

// RTCMOut is one non-caster output of a mountpoint's stream: a UDP target or a
// serial port feeding a radio. From RTKBase's [rtcm_udp_*] and [rtcm_serial]
// services; see CREDITS.md.
type RTCMOut struct {
	Name     string `toml:"name"`
	Disabled bool   `toml:"disabled"`
	// Kind is "udp" or "serial".
	Kind string `toml:"kind"`
	// Source is the local mountpoint whose exact stream is sent.
	Source string `toml:"source"`
	// Target is host:port for kind "udp".
	Target string `toml:"target"`
	// Device and Baud apply to kind "serial".
	Device string `toml:"device"`
	Baud   int    `toml:"baud"`
}

// TimeSync offers the receiver's time to chrony. Empty ChronySocket means the
// feature is off, which is the default: it writes to a socket another service
// owns, so it should never start behaving differently after an upgrade alone.
type TimeSync struct {
	ChronySocket   string `toml:"chrony_socket"`
	MinIntervalSec int    `toml:"min_interval_sec"`
	MaxAccuracyMS  int    `toml:"max_accuracy_ms"`
}

// Update points the updater at a signed release manifest. Empty manifest_url
// means this station is updated by hand, which is the default: nothing here
// runs on a timer, and `psgnssd --update` is always a thing a person does.
type Update struct {
	// ManifestURL serves the signed release envelope.
	ManifestURL string `toml:"manifest_url"`
	// ServiceUnit is restarted after a successful install.
	ServiceUnit string `toml:"service_unit"`
	// BinaryPath is what gets replaced. Empty means the running executable.
	BinaryPath string `toml:"binary_path"`
}

type Security struct {
	KeyFile string `toml:"key_file"`
}

type Logging struct {
	Level string `toml:"level"`
	Dir   string `toml:"dir"`
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		// Unknown keys are almost always typos. Refuse rather than silently
		// running with a setting the operator thinks is applied.
		return nil, fmt.Errorf("unknown config key(s): %v", undec)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save validates and atomically replaces a configuration file. The temporary
// file lives beside the destination so rename remains atomic across filesystems.
func Save(path string, c *Config) error {
	if path == "" {
		return errors.New("config path is empty")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	var b bytes.Buffer
	b.WriteString("# PSGNSS configuration — managed by the Settings UI.\n")
	if err := toml.NewEncoder(&b).Encode(c); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	var roundTrip Config
	md, err := toml.Decode(b.String(), &roundTrip)
	if err != nil {
		return fmt.Errorf("verify encoded config: %w", err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		return fmt.Errorf("verify encoded config: unknown key(s): %v", undec)
	}
	if err := roundTrip.Validate(); err != nil {
		return fmt.Errorf("verify encoded config: %w", err)
	}
	mode := os.FileMode(0o640)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".psgnss-config-*")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// lostAt names the signals dropped at a given convbin -f, measured against a
// ZED-X20P on 2026-09-15.
func lostAt(f int) string {
	switch {
	case f <= 2:
		return "GPS L5, Galileo E5a and E6, and BeiDou entirely"
	case f == 3:
		return "Galileo E6 and BeiDou B3I"
	}
	return "some signals"
}

func (c *Config) Validate() error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }

	if c.Receiver.Device == "" {
		add("receiver.device is required")
	} else if _, err := os.Stat(c.Receiver.Device); err != nil {
		// A warning, not fatal: the device may appear after boot.
		_ = err
	}
	if c.Receiver.Baud <= 0 {
		add("receiver.baud must be positive, got %d", c.Receiver.Baud)
	}
	if _, err := boardprofile.Resolve(c.Receiver.Profile, c.Receiver.Model); err != nil {
		add("receiver: %v", err)
	}
	switch c.Station.Position.Mode {
	case "fixed":
		p := c.Station.Position
		if p.Format == "llh" && p.Latitude == 0 && p.Longitude == 0 {
			add("station.position: mode is fixed but latitude/longitude are unset")
		}
	case "survey_in", "":
	default:
		add("station.position.mode must be 'fixed' or 'survey_in', got %q", c.Station.Position.Mode)
	}
	if c.RINEX.Frequencies < 4 && c.Receiver.Model == "ZED-X20P" {
		// Measured: -f 2 loses BeiDou entirely and keeps only GPS L1/L2 and
		// Galileo E1; -f 3 still omits Galileo E6 and BeiDou B3I. Four slots
		// are needed to carry every signal this receiver tracks.
		add("rinex.frequencies is %d but %s needs 4: at %d, RINEX output loses "+
			"%s", c.RINEX.Frequencies, c.Receiver.Model, c.RINEX.Frequencies,
			lostAt(c.RINEX.Frequencies))
	}
	seen := map[string]bool{}
	validMountName := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$`)
	for _, m := range c.Caster.Mountpoint {
		m.Name = strings.TrimSpace(m.Name)
		if !validMountName.MatchString(m.Name) {
			add("mountpoint name %q must use 1-80 letters, digits, dot, underscore or hyphen", m.Name)
		}
		if seen[m.Name] {
			add("duplicate mountpoint %q", m.Name)
		}
		seen[m.Name] = true
		if m.SourceID < 1 {
			add("mountpoint %q source_id must be positive", m.Name)
		}
		if len(m.Messages) == 0 {
			add("mountpoint %q has no messages", m.Name)
		}
		msgSeen := map[int]bool{}
		for _, msg := range m.Messages {
			if msg.Type < 1 || msg.Type > 4095 {
				add("mountpoint %q has invalid RTCM type %d", m.Name, msg.Type)
			}
			if msg.Interval < 1 || msg.Interval > 3600 {
				add("mountpoint %q type %d interval must be 1-3600 seconds", m.Name, msg.Type)
			}
			if msgSeen[msg.Type] {
				add("mountpoint %q repeats RTCM type %d", m.Name, msg.Type)
			}
			msgSeen[msg.Type] = true
		}
		if m.Carrier != nil && (*m.Carrier < 0 || *m.Carrier > 3) {
			add("mountpoint %q carrier must be 0-3", m.Name)
		}
		if m.Bitrate < 0 {
			add("mountpoint %q bitrate cannot be negative", m.Name)
		}
		if m.NMEA < 0 || m.NMEA > 1 || m.Solution < 0 || m.Solution > 1 {
			add("mountpoint %q NMEA and solution fields must be 0 or 1", m.Name)
		}
		if m.Auth != "" && m.Auth != "B" {
			add("mountpoint %q auth must be B; this caster requires registered users", m.Name)
		}
		if m.Fee != "" && m.Fee != "N" && m.Fee != "Y" {
			add("mountpoint %q fee must be N or Y", m.Name)
		}
		for label, value := range map[string]string{"format": m.Format, "nav_system": m.NavSystem,
			"network": m.Network, "country": m.Country, "generator": m.Generator,
			"compress": m.Compress, "msm_detail": m.MSMDetail} {
			if strings.ContainsAny(value, ";\r\n") {
				add("mountpoint %q %s contains a sourcetable delimiter", m.Name, label)
			}
		}
		if m.Country != "" && len(m.Country) != 3 {
			add("mountpoint %q country must be a three-letter code", m.Name)
		}
	}
	switch c.Hub.Input {
	case "", "serial":
	case "tcp", "file":
		if c.Hub.InputAddr == "" {
			add("hub.input is %q but hub.input_addr is empty", c.Hub.Input)
		}
	default:
		add("hub.input must be serial, tcp or file, got %q", c.Hub.Input)
	}
	for _, l := range c.Hub.Listener {
		if l.Listen == "" {
			add("hub listener %q has no listen address", l.Name)
		}
	}
	if c.Telemetry.DBPath == "" {
		add("telemetry.db_path is required")
	}
	validSystem := map[string]bool{"GPS": true, "Galileo": true, "BeiDou": true, "SBAS": true, "GLONASS": true, "QZSS": true, "NavIC": true}
	seenSystem := map[string]bool{}
	for _, system := range c.Web.DisplayHiddenConstellations {
		if !validSystem[system] {
			add("web.display_hidden_constellations contains unknown system %q", system)
		}
		if seenSystem[system] {
			add("web.display_hidden_constellations repeats %q", system)
		}
		seenSystem[system] = true
	}
	if c.Web.DiagnosticsIntervalMinutes != 0 && (c.Web.DiagnosticsIntervalMinutes < 1 || c.Web.DiagnosticsIntervalMinutes > 1440) {
		add("web.diagnostics_interval_minutes must be 1-1440 minutes")
	}
	seenOut := map[string]bool{}
	for i, o := range c.RTCMOut {
		name := strings.TrimSpace(o.Name)
		if name == "" {
			add("rtcm_out[%d].name is required", i)
			continue
		}
		if seenOut[strings.ToLower(name)] {
			add("rtcm_out repeats the name %q", name)
		}
		seenOut[strings.ToLower(name)] = true
		if strings.TrimSpace(o.Source) == "" {
			add("rtcm_out %q needs a source mountpoint", name)
		}
		switch o.Kind {
		case "udp":
			if _, _, err := net.SplitHostPort(o.Target); err != nil {
				add("rtcm_out %q: target must be host:port", name)
			}
		case "serial":
			if !strings.HasPrefix(o.Device, "/dev/") {
				add("rtcm_out %q: device must be a /dev path", name)
			}
			if _, ok := serialBauds[o.Baud]; !ok {
				add("rtcm_out %q: baud %d is not one of the supported rates", name, o.Baud)
			}
		default:
			add("rtcm_out %q: kind must be \"udp\" or \"serial\"", name)
		}
	}
	if c.TimeSync.ChronySocket != "" {
		if !strings.HasPrefix(c.TimeSync.ChronySocket, "/") {
			add("timesync.chrony_socket must be an absolute path")
		}
		if c.TimeSync.MinIntervalSec < 0 || c.TimeSync.MinIntervalSec > 3600 {
			add("timesync.min_interval_sec must be 0-3600")
		}
		if c.TimeSync.MaxAccuracyMS < 0 || c.TimeSync.MaxAccuracyMS > 60000 {
			add("timesync.max_accuracy_ms must be 0-60000")
		}
	}
	if c.Update.ManifestURL != "" {
		u, err := url.Parse(c.Update.ManifestURL)
		switch {
		case err != nil || u.Host == "":
			add("update.manifest_url must be a URL, or empty for hand installation")
		case u.Scheme != "https":
			// The signature is what makes a release trustworthy, not the
			// transport. Plain HTTP would still be caught, but a release that
			// anyone on the path can withhold or delay is not worth
			// normalising.
			add("update.manifest_url must use https")
		}
	}
	if c.Update.BinaryPath != "" && !strings.HasPrefix(c.Update.BinaryPath, "/") {
		add("update.binary_path must be an absolute path")
	}
	if c.Web.MapTiles != "" {
		u, err := url.Parse(c.Web.MapTiles)
		switch {
		case err != nil || u.Host == "":
			add("web.map_tiles must be a tile URL template, or empty for no map")
		case u.Scheme != "https":
			// The UI is plain HTTP on the LAN, but a mixed-content tile request
			// would be blocked by the browser and look like a broken map.
			add("web.map_tiles must use https")
		case !strings.Contains(c.Web.MapTiles, "{z}") || !strings.Contains(c.Web.MapTiles, "{x}") ||
			!strings.Contains(c.Web.MapTiles, "{y}"):
			add("web.map_tiles needs the {z}, {x} and {y} placeholders")
		case c.Web.MapCacheDays < 0 || c.Web.MapCacheDays > 3650:
			add("web.map_cache_days must be 0-3650")
		case c.Web.MapAttribution == "":
			add("web.map_attribution is required when a tile source is set: " +
				"tile providers require credit, and OpenStreetMap's usage policy insists on it")
		}
	}
	return errors.Join(errs...)
}

// serialBauds are the rates the hub's termios helper understands. Keeping the
// list here means a typo in configuration is refused at load rather than at the
// moment a radio should have started transmitting.
var serialBauds = map[int]bool{9600: true, 19200: true, 38400: true, 57600: true,
	115200: true, 230400: true, 460800: true, 921600: true}
