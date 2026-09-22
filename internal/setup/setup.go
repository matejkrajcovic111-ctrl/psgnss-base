// Package setup turns the answers to a first-run interview into a working
// station: a configuration file, a master key, the directories, the systemd
// units and an administrator account.
//
// # Why a CLI interview and not a web wizard
//
// The wizard would have to run before there is anything to serve it. A base
// station is installed over SSH on a headless Pi, by someone who has just
// flashed a card; the first thing they have is a shell. The spec left the
// choice open and asked for whichever is simpler for a non-expert, and for
// that person a question with a sensible default in brackets beats a browser
// they cannot yet reach.
//
// # Everything here is computed before anything is written
//
// The interview produces Answers, Answers produce a Plan, and only Apply
// touches the disk. That split is what makes the installer testable without a
// terminal and without root, and it is why --dry-run can show the operator the
// exact file list before they commit to it.
package setup

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/psgnss/psgnss-base/internal/boardprofile"
	"github.com/psgnss/psgnss-base/internal/config"
)

// Answers is everything the first-run interview collects. Nothing else is
// asked: every other setting has a default that is either measured on this
// hardware or safe to change later in the Settings UI.
type Answers struct {
	StationName string
	StationID   uint16
	Antenna     string

	ProfileID string // boardprofile ID, e.g. "simplertk4-optimum"
	Device    string
	Baud      int

	Position config.Position

	MSM7Name string
	MSM4Name string
	// Ephemeris adds the synthesised navigation messages (1019, 1042, 1046)
	// to both mountpoints. It is asked rather than assumed: it changes what
	// every connected rover receives, and on an existing station that is the
	// owner's call. On a new one there are no rovers yet, so the default is
	// yes -- a rover that already holds an ephemeris ignores them, and one
	// that does not gets a faster cold start.
	Ephemeris bool

	CasterListen string
	RawListen    string
	RawAltListen string
	WebListen    string
	Operator     string
	Country      string

	Archive ArchiveTarget

	AdminUser string
	AdminPass string
}

// ArchiveTarget is the SMB share the raw archive is flushed to. It is
// optional: a station with no share still records, into the local spool, and
// the operator can add one later. Saying so at install time is better than
// making them invent an answer.
type ArchiveTarget struct {
	Enabled       bool
	UNC           string // //host/share
	MountPoint    string // where it is mounted, e.g. /mnt/gnssraw
	Username      string
	Password      string
	RetentionDays int
}

// Defaults returns the answers a fresh install starts from. Several are not
// preferences but findings: see the comments, and HANDOVER section 4.
func Defaults() Answers {
	return Answers{
		StationName: "",
		// Station ID 0, deliberately. The receiver stamps DF003 = 0 into its
		// own MSM output and PSGNSS generates 1006/1008/1033 to match. When
		// the two disagree a rover receives a healthy-looking stream, tracks
		// satellites and never fixes, because it cannot associate the
		// observations with a base position. Moving to another ID means
		// writing CFG-RTCM DF003 on the receiver *and* setting this together.
		StationID: 0,
		// The surveyed coordinate of a station is derived against its antenna
		// descriptor. A null antenna is the honest default until the operator
		// has a calibration to name.
		Antenna:   "ADVNULLANTENNA",
		ProfileID: "simplertk4-optimum",
		Baud:      460800,
		Position: config.Position{
			Mode:   "fixed",
			Format: "llh",
		},
		Ephemeris:    true,
		CasterListen: "0.0.0.0:2101",
		RawListen:    "0.0.0.0:5003",
		RawAltListen: "0.0.0.0:5001",
		WebListen:    "0.0.0.0:8090",
		Country:      "SVK",
		Archive: ArchiveTarget{
			MountPoint:    "/mnt/gnssraw",
			RetentionDays: 365,
		},
		AdminUser: "admin",
	}
}

var (
	validMountName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$`)
	validUNC       = regexp.MustCompile(`^//[A-Za-z0-9._-]+/[A-Za-z0-9._ -]+$`)
	validUserName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
)

// MinAdminPassword is the shortest administrator password the installer will
// accept. The web UI is reachable on the LAN from the moment the service
// starts, and on this station it is also published through a tunnel.
const MinAdminPassword = 10

// Validate reports every problem with the answers at once, so the interview
// can show them together rather than one re-ask at a time.
func (a Answers) Validate() error {
	var errs []error
	add := func(f string, v ...any) { errs = append(errs, fmt.Errorf(f, v...)) }

	if strings.TrimSpace(a.StationName) == "" {
		add("station name is required")
	} else if !validMountName.MatchString(strings.TrimSpace(a.StationName)) {
		add("station name %q must use letters, digits, dot, underscore or hyphen", a.StationName)
	}
	profile, ok := boardprofile.Lookup(a.ProfileID)
	if !ok {
		add("unknown receiver board %q", a.ProfileID)
	} else if !profile.Implemented {
		add("board %s (%s) has a profile but no driver yet: it cannot be configured or read by this release",
			profile.Name, profile.Receiver)
	}
	if strings.TrimSpace(a.Device) == "" {
		add("receiver device is required")
	} else if !strings.HasPrefix(a.Device, "/dev/") {
		add("receiver device %q must be a /dev path", a.Device)
	}
	if !supportedBaud[a.Baud] {
		add("baud %d is not one of the supported rates", a.Baud)
	}
	if a.Position.Mode == "fixed" {
		switch {
		case a.Position.Latitude == 0 && a.Position.Longitude == 0:
			add("base position is required for a fixed base")
		case a.Position.Latitude < -90 || a.Position.Latitude > 90:
			add("latitude %v is outside -90..90", a.Position.Latitude)
		case a.Position.Longitude < -180 || a.Position.Longitude > 180:
			add("longitude %v is outside -180..180", a.Position.Longitude)
		}
	}
	for label, name := range map[string]string{"MSM7": a.MSM7Name, "MSM4": a.MSM4Name} {
		if !validMountName.MatchString(name) {
			add("%s mountpoint name %q must use 1-80 letters, digits, dot, underscore or hyphen", label, name)
		}
	}
	if a.MSM7Name != "" && a.MSM7Name == a.MSM4Name {
		add("both mountpoints are called %q", a.MSM7Name)
	}
	for label, addr := range map[string]string{"caster": a.CasterListen, "web": a.WebListen,
		"raw": a.RawListen, "raw-alt": a.RawAltListen} {
		if _, _, e := net.SplitHostPort(addr); e != nil {
			add("%s listen address %q must be host:port", label, addr)
		}
	}
	if a.Country != "" && len(a.Country) != 3 {
		add("country %q must be a three-letter code", a.Country)
	}
	if a.Archive.Enabled {
		if !validUNC.MatchString(a.Archive.UNC) {
			add("archive share %q must look like //host/share", a.Archive.UNC)
		}
		if !strings.HasPrefix(a.Archive.MountPoint, "/") {
			add("archive mount point %q must be an absolute path", a.Archive.MountPoint)
		}
		if strings.TrimSpace(a.Archive.Username) == "" {
			add("archive share username is required")
		}
		if strings.ContainsAny(a.Archive.Username+a.Archive.Password, "\r\n") {
			add("archive credentials cannot contain a newline")
		}
		if a.Archive.RetentionDays < 0 {
			add("archive retention cannot be negative")
		}
	}
	if !validUserName.MatchString(a.AdminUser) {
		add("administrator name %q must use 1-64 letters, digits, dot, underscore or hyphen", a.AdminUser)
	}
	if len([]rune(a.AdminPass)) < MinAdminPassword {
		add("administrator password must be at least %d characters", MinAdminPassword)
	}
	return errors.Join(errs...)
}

var supportedBaud = map[int]bool{9600: true, 19200: true, 38400: true, 57600: true,
	115200: true, 230400: true, 460800: true, 921600: true}

// Standard message sets. A mountpoint is a pure filter over what the receiver
// already emits: this receiver family emits MSM4 and MSM7 concurrently, so
// serving both costs nothing and nothing is ever transcoded.
var (
	msm7Types = []int{1077, 1097, 1127}
	msm4Types = []int{1074, 1094, 1124}
	// Generated from configuration rather than emitted by the receiver, which
	// sends 1005 and never these.
	stationTypes = []int{1006, 1008, 1033}
	// Synthesised from UBX RXM-SFRBX, because no receiver in this family has a
	// configuration key for them. 30 s matches how often GPS rebroadcasts a
	// data set, so a rover never waits longer for a synthesised orbit than for
	// a real one.
	ephemerisTypes = []int{1019, 1042, 1046}
)

func messagesFor(msm []int, ephemeris bool) []config.MessageSel {
	out := make([]config.MessageSel, 0, len(stationTypes)+len(msm)+len(ephemerisTypes))
	for _, t := range stationTypes {
		out = append(out, config.MessageSel{Type: t, Interval: 10})
	}
	if ephemeris {
		for _, t := range ephemerisTypes {
			out = append(out, config.MessageSel{Type: t, Interval: 30})
		}
	}
	for _, t := range msm {
		out = append(out, config.MessageSel{Type: t, Interval: 1})
	}
	return out
}

// BuildConfig turns answers into the configuration the daemon will load. It
// validates through config.Validate, so an interview that produced something
// the daemon would refuse fails here rather than at the first start.
func BuildConfig(a Answers, paths Paths) (*config.Config, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	profile, _ := boardprofile.Lookup(a.ProfileID)

	// GLONASS is absent deliberately: this receiver family's firmware reports
	// GPS;GAL;BDS / SBAS;QZSS / NAVIC and emits nothing on 1084/1087 even with
	// them enabled. Advertising a constellation the stream does not carry is
	// worse than not advertising it.
	const navSystem = "GPS+GAL+BDS"

	c := &config.Config{
		Station: config.Station{
			Name:      strings.TrimSpace(a.StationName),
			StationID: a.StationID,
			Antenna:   a.Antenna,
			Position:  a.Position,
		},
		Receiver: config.Receiver{
			Device:        a.Device,
			Baud:          a.Baud,
			Profile:       profile.ID,
			Model:         profile.Receiver,
			VerifyModel:   true,
			RevertTimeout: 60,
		},
		Hub: config.Hub{
			Input:      "serial",
			ReadBuffer: 32768,
			StallWarn:  10,
			Listener: []config.Listener{
				{Name: "raw", Listen: a.RawListen, Filter: "none"},
				{Name: "raw-alt", Listen: a.RawAltListen, Filter: "none"},
			},
		},
		Caster: config.Caster{
			Listen: a.CasterListen,
			// Built, and off. It costs nothing unused, and it means real
			// client IPs can be turned on later by putting a proxy in front
			// without touching this station.
			ProxyListen:  proxyListenFor(a.CasterListen),
			ProxyTrusted: []string{},
			NtripV1:      true,
			NtripV2:      true,
			Operator:     a.Operator,
			Country:      a.Country,
			// What a live sourcetable actually advertises for this stream.
			FormatString: "RTCM 3.2",
			Carrier:      3,
			Mountpoint: []config.Mountpoint{
				{Name: a.MSM7Name, SourceID: 1, NavSystem: navSystem,
					Messages: messagesFor(msm7Types, a.Ephemeris)},
				{Name: a.MSM4Name, SourceID: 2, NavSystem: navSystem,
					Messages: messagesFor(msm4Types, a.Ephemeris)},
			},
		},
		Archive: config.Archive{
			SpoolDir:      paths.Spool(),
			MountPoint:    a.Archive.MountPoint,
			SyncEverySec:  60,
			RetentionDays: a.Archive.RetentionDays,
			RTCM: config.ArchiveSet{
				Enabled: true, Pattern: "Base1_%Y%m%d%h00", Subdir: "RAW",
				SwapHours: 24, SwapMargin: 30, Filter: "rtcm3",
			},
			// Off by default: the nav archive below carries everything RINEX
			// needs at a fraction of the size.
			UBX: config.ArchiveSet{
				Enabled: false, Pattern: "Base1_ubx_%Y%m%d%h00", Subdir: "UBX",
				SwapHours: 24, SwapMargin: 30, Filter: "ubx",
			},
			// RXM-SFRBX carries the broadcast ephemeris and RXM-RAWX supplies
			// the receiver time RTKLIB needs to date those subframes. An
			// SFRBX-only file looks valid and cannot be converted.
			Nav: config.ArchiveSet{
				Enabled: true, Pattern: "Base1_nav_%Y%m%d%h00", Subdir: "NAV",
				SwapHours: 24, SwapMargin: 30, Filter: "ubx",
				Messages: []string{"RXM-SFRBX", "RXM-RAWX"},
			},
		},
		RINEX: config.RINEX{
			ConvbinPath: paths.Bin() + "/convbin",
			Version:     "3.04",
			// Measured on this receiver: -f 2 loses BeiDou entirely plus GPS
			// L5 and Galileo E5a/E6; -f 3 still omits E6 and B3I; 5 and above
			// are identical to 4.
			Frequencies: 4,
			RawFormat:   "ubx",
			OutputDir:   paths.RINEX(),
			Compression: "zip",
		},
		Telemetry: config.Telemetry{
			DBPath:         paths.Database(),
			RetentionDays:  7,
			FineRateHz:     1,
			FineWindowMin:  60,
			CoarseInterval: 30,
		},
		Web: config.Web{
			Listen:   a.WebListen,
			Language: "en",
		},
		Security: config.Security{KeyFile: paths.MasterKey()},
		Logging:  config.Logging{Level: "info", Dir: paths.LogDir()},
	}
	if !a.Archive.Enabled {
		// Nothing to flush to, so the archive stays in the spool. The writer
		// treats an empty mount point as local-only.
		c.Archive.MountPoint = ""
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("the answers produce a configuration this daemon would refuse: %w", err)
	}
	return c, nil
}

// proxyListenFor puts the PROXY-protocol listener one port above the caster,
// which is the arrangement this station uses and the one the documentation
// describes.
func proxyListenFor(casterListen string) string {
	host, port, err := net.SplitHostPort(casterListen)
	if err != nil {
		return ""
	}
	var n int
	if _, err := fmt.Sscanf(port, "%d", &n); err != nil || n <= 0 || n >= 65535 {
		return ""
	}
	return net.JoinHostPort(host, fmt.Sprint(n+1))
}
