package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/psgnss/psgnss-base/internal/config"
)

// good returns answers a real interview would produce, so each test can break
// exactly one thing.
func good() Answers {
	a := Defaults()
	a.StationName = "Example"
	a.Device = "/dev/serial/by-id/usb-u-blox_AG_-_www.u-blox.com_u-blox_GNSS_receiver-if00"
	// Not this station: a fixture coordinate, so nothing in the test suite is
	// a full-precision base position (spec rule 6).
	a.Position = config.Position{Mode: "fixed", Format: "llh",
		Latitude: 49.5, Longitude: 18.5, Height: 300}
	a.MSM7Name = "Example_MSM7"
	a.MSM4Name = "Example_MSM4"
	a.Operator = "example.org"
	a.AdminUser = "admin"
	a.AdminPass = "a-long-enough-one"
	return a
}

func TestAnswersRejectWhatWouldNotWork(t *testing.T) {
	cases := map[string]func(*Answers){
		"no station name":               func(a *Answers) { a.StationName = "" },
		"no position":                   func(a *Answers) { a.Position.Latitude, a.Position.Longitude = 0, 0 },
		"impossible latitude":           func(a *Answers) { a.Position.Latitude = 91 },
		"no device":                     func(a *Answers) { a.Device = "" },
		"device not in dev":             func(a *Answers) { a.Device = "COM3" },
		"unsupported baud":              func(a *Answers) { a.Baud = 12345 },
		"same mountpoint twice":         func(a *Answers) { a.MSM4Name = a.MSM7Name },
		"mountpoint with a semicolon":   func(a *Answers) { a.MSM7Name = "Example;MSM7" },
		"listen address without a port": func(a *Answers) { a.CasterListen = "0.0.0.0" },
		"short admin password":          func(a *Answers) { a.AdminPass = "short" },
		// A board with a profile but no driver would install cleanly and then
		// fail to configure or read the receiver at all.
		"board with no driver":      func(a *Answers) { a.ProfileID = "simplertk3b-pro" },
		"board that does not exist": func(a *Answers) { a.ProfileID = "simplertk9000" },
		"share without credentials": func(a *Answers) {
			a.Archive.Enabled = true
			a.Archive.UNC = "//host/share"
		},
		"share that is not a UNC path": func(a *Answers) {
			a.Archive.Enabled = true
			a.Archive.UNC = "/mnt/elsewhere"
			a.Archive.Username = "u"
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			a := good()
			breakIt(&a)
			if err := a.Validate(); err == nil {
				t.Fatal("accepted answers that would not produce a working station")
			}
		})
	}
	if err := good().Validate(); err != nil {
		t.Fatalf("rejected good answers: %v", err)
	}
}

// The configuration the installer writes has to be one the daemon will load.
// Anything else moves a whole class of mistakes from install time, where they
// can be re-asked, to the first start, where they are a failed unit.
func TestGeneratedConfigIsOneTheDaemonAccepts(t *testing.T) {
	cfg, err := BuildConfig(good(), DefaultPaths())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "psgnss.toml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("the installer wrote a config the daemon refuses: %v", err)
	}
	if loaded.Station.Name != "Example" || len(loaded.Caster.Mountpoint) != 2 {
		t.Fatalf("round trip lost content: %+v", loaded.Station)
	}
	// Measured on this receiver: at fewer than four frequency slots RINEX
	// output silently loses BeiDou, GPS L5 and Galileo E6.
	if loaded.RINEX.Frequencies != 4 {
		t.Errorf("rinex.frequencies = %d, want 4", loaded.RINEX.Frequencies)
	}
	// GLONASS is not supported by this receiver's firmware; advertising it
	// would promise a constellation the stream does not carry.
	for _, m := range loaded.Caster.Mountpoint {
		if strings.Contains(m.NavSystem, "GLO") {
			t.Errorf("mountpoint %q advertises %q", m.Name, m.NavSystem)
		}
	}
}

func TestEphemerisIsOptedInto(t *testing.T) {
	has := func(cfg *config.Config, typ int) bool {
		for _, m := range cfg.Caster.Mountpoint {
			for _, msg := range m.Messages {
				if msg.Type == typ {
					return true
				}
			}
		}
		return false
	}
	a := good()
	a.Ephemeris = false
	off, err := BuildConfig(a, DefaultPaths())
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range ephemerisTypes {
		if has(off, typ) {
			t.Errorf("declining ephemeris still lists %d", typ)
		}
	}
	a.Ephemeris = true
	on, err := BuildConfig(a, DefaultPaths())
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range ephemerisTypes {
		if !has(on, typ) {
			t.Errorf("accepting ephemeris does not list %d", typ)
		}
	}
	// The observation messages are the point of the mountpoint and are never
	// conditional.
	for _, typ := range []int{1077, 1074} {
		if !has(off, typ) {
			t.Errorf("mountpoints lost observation type %d", typ)
		}
	}
}

// ProtectSystem=strict makes everything read-only except what is listed, and a
// share that is mounted rw still returns "read-only file system" to the
// service unless its mount point is in ReadWritePaths. That failed silently
// for a whole day after cutover: the archive wrote nothing and only the logs
// said so. Whatever mount point an operator picks, it has to end up in there.
func TestTheServiceUnitCanWriteToTheArchiveShare(t *testing.T) {
	a := good()
	a.Archive = ArchiveTarget{Enabled: true, UNC: "//nas/GNSSraw",
		MountPoint: "/srv/gnss-archive", Username: "u", Password: "p", RetentionDays: 365}
	unit, err := renderUnit("psgnss.service.tmpl", varsFor(a, DefaultPaths(), nil))
	if err != nil {
		t.Fatal(err)
	}
	s := string(unit)
	if !strings.Contains(s, "ProtectSystem=strict") {
		t.Error("the hardening that makes this necessary is gone; so is the reason for the rest of this test")
	}
	if !strings.Contains(s, "ReadWritePaths=-/srv/gnss-archive") {
		t.Errorf("the archive mount is not writable by the service:\n%s", s)
	}
	for _, want := range []string{"/var/lib/psgnss", "/var/log/psgnss", "/etc/psgnss"} {
		if !strings.Contains(s, want) {
			t.Errorf("ReadWritePaths lost %s", want)
		}
	}
	// A station with no share must not name an empty path, which systemd
	// rejects and which would stop the unit from loading at all.
	a.Archive.Enabled = false
	unit, err = renderUnit("psgnss.service.tmpl", varsFor(a, DefaultPaths(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unit), "ReadWritePaths=-\n") {
		t.Error("a station with no share emits an empty ReadWritePaths")
	}
}

// systemd only ever reads a mount unit whose filename is the escaped mount
// path. Get it wrong and the unit sits there inert while the archive silently
// never reaches the share. This is checked against systemd's own escaper
// rather than against a second reading of the manual.
func TestMountUnitNameMatchesSystemdsOwnEscaping(t *testing.T) {
	escape, err := exec.LookPath("systemd-escape")
	if err != nil {
		t.Skip("systemd-escape not installed; the manual's rules are asserted in the next test")
	}
	for _, path := range []string{"/mnt/gnssraw", "/srv/gnss", "/", "/mnt/my.share",
		"/mnt/a-b", "/media/USB", "/mnt/raw archive", "/mnt/.hidden"} {
		out, err := exec.Command(escape, "--path", path).Output()
		if err != nil {
			t.Fatalf("systemd-escape %s: %v", path, err)
		}
		want := strings.TrimSpace(string(out))
		if got := unitNameForMount(path); got != want {
			t.Errorf("unitNameForMount(%q) = %q, systemd-escape says %q", path, got, want)
		}
	}
}

func TestPlanWritesAWorkingInstall(t *testing.T) {
	root := t.TempDir()
	// Install this test binary rather than looking for the real one.
	self := filepath.Join(root, "fake-psgnssd")
	if err := os.WriteFile(self, []byte("#!/bin/false\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := ExecutableSource
	ExecutableSource = func() (string, error) { return self, nil }
	defer func() { ExecutableSource = old }()

	a := good()
	a.Archive = ArchiveTarget{Enabled: true, UNC: "//nas/GNSSraw",
		MountPoint: "/mnt/gnssraw", Username: "u", Password: "p", RetentionDays: 365}
	p := DefaultPaths()

	plan, err := BuildPlan(a, p, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Existing) != 0 {
		t.Fatalf("a clean root reports existing files: %v", plan.Existing)
	}
	if err := plan.Apply(root, false); err != nil {
		t.Fatal(err)
	}
	if err := plan.Provision(root); err != nil {
		t.Fatal(err)
	}

	// The config the daemon will actually load.
	if _, err := config.Load(filepath.Join(root, p.ConfigFile())); err != nil {
		t.Fatalf("the installed config does not load: %v", err)
	}

	// Modes, on the two files that are credentials.
	for path, want := range map[string]os.FileMode{
		p.MasterKey():  0o600,
		p.SMBCred():    0o600,
		p.ConfigFile(): 0o640,
		p.SecretsDir(): 0o700,
	} {
		fi, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s has mode %04o, want %04o", path, got, want)
		}
	}

	// The mount unit has to be findable by its escaped name.
	if _, err := os.Stat(filepath.Join(root, p.SystemdUnits, "mnt-gnssraw.mount")); err != nil {
		t.Errorf("mount unit not written under its escaped name: %v", err)
	}

	// Re-planning the same root now sees the install and refuses.
	again, err := BuildPlan(a, p, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Existing) == 0 {
		t.Fatal("a second plan over a finished install reports nothing existing")
	}
	if err := again.Apply(root, false); err == nil {
		t.Fatal("a second install overwrote a configured station without --force")
	}
	if err := again.Apply(root, true); err != nil {
		t.Fatalf("--force did not allow a deliberate rebuild: %v", err)
	}
}

// Every install gets its own key. Two stations sharing one would make either
// able to read the other's stored passwords, and a key that came from the
// binary would make all of them readable by anyone holding a release.
func TestEachInstallGetsItsOwnMasterKey(t *testing.T) {
	old := ExecutableSource
	ExecutableSource = func() (string, error) { return os.Args[0], nil }
	defer func() { ExecutableSource = old }()
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		plan, err := BuildPlan(good(), DefaultPaths(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range plan.Steps {
			if s.Path == DefaultPaths().MasterKey() {
				if seen[string(s.Data)] {
					t.Fatal("two installs were given the same master key")
				}
				seen[string(s.Data)] = true
			}
		}
	}
	if len(seen) != 5 {
		t.Fatalf("got %d keys from 5 plans", len(seen))
	}
}

// Nothing a human reads should carry the master key or a share password.
func TestDescribeNeverPrintsASecret(t *testing.T) {
	old := ExecutableSource
	ExecutableSource = func() (string, error) { return os.Args[0], nil }
	defer func() { ExecutableSource = old }()
	a := good()
	a.Archive = ArchiveTarget{Enabled: true, UNC: "//nas/GNSSraw", MountPoint: "/mnt/gnssraw",
		Username: "shareuser", Password: "hunter2-the-share-password", RetentionDays: 30}
	plan, err := BuildPlan(a, DefaultPaths(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := plan.Describe("")
	if strings.Contains(out, a.Archive.Password) {
		t.Error("the plan prints the share password")
	}
	if strings.Contains(out, a.AdminPass) {
		t.Error("the plan prints the administrator password")
	}
	for _, s := range plan.Steps {
		if s.Secret && len(s.Data) > 0 && strings.Contains(out, string(s.Data)) {
			t.Errorf("the plan prints the contents of %s", s.Path)
		}
	}
}

func TestSerialDevicesPreferByIDPaths(t *testing.T) {
	root := t.TempDir()
	byID := filepath.Join(root, "dev", "serial", "by-id")
	if err := os.MkdirAll(byID, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ttyACM0", "ttyACM1", "ttyUSB0"} {
		if err := os.WriteFile(filepath.Join(root, "dev", name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A GNSS receiver and something else, both with by-id links.
	if err := os.Symlink("../../ttyACM1", filepath.Join(byID, "usb-u-blox_AG_u-blox_GNSS_receiver-if00")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../ttyUSB0", filepath.Join(byID, "usb-FTDI_widget-if00")); err != nil {
		t.Fatal(err)
	}

	got := FindSerialDevices(root)
	if len(got) == 0 {
		t.Fatal("found nothing")
	}
	if !strings.Contains(got[0].Path, "u-blox") {
		t.Errorf("first offer is %q, want the u-blox by-id link", got[0].Path)
	}
	if !got[0].Likely {
		t.Error("the u-blox device is not marked as likely")
	}
	// The bare node the by-id link points at must not be offered twice, and a
	// bare node must never outrank a by-id path.
	for i, d := range got {
		if d.Path == "/dev/ttyACM1" || d.Path == "/dev/ttyUSB0" {
			t.Errorf("offer %d is the bare node %q that a by-id link already covers", i, d.Path)
		}
	}
}

// systemd's own parser is the authority on whether these units load. A unit
// that is merely plausible to read still fails at boot, and the failure of a
// mount unit in particular is quiet: the share is simply never there.
//
// The service's ExecStart is pointed at a real executable for the duration, so
// that a clean exit status means what it says rather than hiding behind the
// one complaint a development machine always produces.
func TestSystemdItselfAcceptsTheGeneratedUnits(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not installed")
	}
	dir := t.TempDir()
	paths := DefaultPaths()
	paths.Opt = filepath.Join(dir, "opt")
	if err := os.MkdirAll(paths.Bin(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Binary(), []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := good()
	a.Archive = ArchiveTarget{Enabled: true, UNC: "//nas/GNSSraw",
		MountPoint: "/mnt/gnssraw", Username: "u", Password: "p", RetentionDays: 365}
	v := varsFor(a, paths, []string{"rtk-stream.service"})

	for name, file := range map[string]string{
		"psgnss.service.tmpl": "psgnss.service",
		"archive.mount.tmpl":  "mnt-gnssraw.mount",
	} {
		unit, err := renderUnit(name, v)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, file)
		if err := os.WriteFile(path, unit, 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(analyze, "verify", path).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "" {
			t.Errorf("systemd rejects the generated %s: %v\n%s\n--- unit ---\n%s",
				file, err, out, unit)
		}
	}
}

// deploy/ holds the units this station was installed with by hand, before
// there was an installer. They are kept because they are the record of what is
// actually running on the Pi, which makes them the thing most likely to drift
// away from what a new install would get. Anything hardening-related that one
// carries, the other must carry too: the day someone tightens the running unit
// and not the template is the day a fresh station is quietly less protected.
func TestTheInstallerDoesNotDriftFromTheDeployedUnit(t *testing.T) {
	committed, err := os.ReadFile(filepath.Join("..", "..", "deploy", "psgnss.service"))
	if err != nil {
		t.Skip("deploy/psgnss.service not present")
	}
	a := good()
	a.Archive = ArchiveTarget{Enabled: true, UNC: "//nas/GNSSraw",
		MountPoint: "/mnt/gnssraw", Username: "u", Password: "p", RetentionDays: 365}
	generated, err := renderUnit("psgnss.service.tmpl", varsFor(a, DefaultPaths(), []string{"rtk-stream.service"}))
	if err != nil {
		t.Fatal(err)
	}
	gen := string(generated)
	for _, line := range strings.Split(string(committed), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(key, "Protect"), strings.HasPrefix(key, "Restrict"),
			key == "ReadWritePaths", key == "NoNewPrivileges", key == "LockPersonality",
			key == "MemoryDenyWriteExecute", key == "PrivateTmp", key == "DeviceAllow",
			key == "SupplementaryGroups", key == "UMask", key == "User", key == "Group",
			key == "StateDirectory", key == "LogsDirectory", key == "RuntimeDirectory",
			key == "Restart", key == "Conflicts":
			if !strings.Contains(gen, line) {
				t.Errorf("the running unit has %q and a fresh install would not", line)
			}
		}
	}
}
