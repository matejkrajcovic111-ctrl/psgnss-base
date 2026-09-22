package setup

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/secrets"
	"github.com/psgnss/psgnss-base/internal/store"
)

// Step is one thing the installer will do to the filesystem. A plan is a list
// of these and nothing else, so --dry-run can print exactly what will happen
// and a test can assert it without root.
type Step struct {
	// Path is the logical destination, without any --root prefix.
	Path string
	Mode fs.FileMode
	Dir  bool
	// Data is the file's contents. Source, when set instead, is a file to copy.
	Data   []byte
	Source string
	// Secret suppresses the contents from any human-readable rendering. The
	// master key is equivalent to every NTRIP password at once and the share
	// credentials are a live login; neither belongs in a terminal scrollback.
	Secret bool
	Note   string
}

// Plan is a complete install, computed before anything is written.
type Plan struct {
	Answers Answers
	Paths   Paths
	Config  *config.Config
	Steps   []Step
	// Existing lists the paths in the plan that are already on disk. A plan
	// with any of these refuses to apply unless the operator forces it.
	Existing []string
	// Conflicts are services found holding the serial port or a listen port.
	Conflicts []string
}

// ExecutableSource is the binary to install as /opt/psgnss/bin/psgnssd. It is
// a variable so a test can point it at a fixture instead of the test runner.
var ExecutableSource = os.Executable

// BuildPlan computes an install from answers. It reads the filesystem only to
// notice what is already there; it writes nothing.
func BuildPlan(a Answers, p Paths, root string) (*Plan, error) {
	cfg, err := BuildConfig(a, p)
	if err != nil {
		return nil, err
	}

	// A fresh key, generated here rather than in Apply so that a dry run
	// exercises exactly the same code path and the plan is complete.
	key := make([]byte, secrets.KeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}

	var cfgBuf bytes.Buffer
	cfgBuf.WriteString("# PSGNSS configuration — written by the first-run installer.\n")
	cfgBuf.WriteString("# Edit here or in Settings; the daemon refuses unknown keys rather than\n")
	cfgBuf.WriteString("# running with a setting you believe is applied.\n")
	if err := toml.NewEncoder(&cfgBuf).Encode(cfg); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}

	conflicts := detectConflicts(root)
	v := varsFor(a, p, conflicts)

	service, err := renderUnit("psgnss.service.tmpl", v)
	if err != nil {
		return nil, err
	}
	tmpfiles, err := renderUnit("psgnss.tmpfiles.tmpl", v)
	if err != nil {
		return nil, err
	}

	self, err := ExecutableSource()
	if err != nil {
		return nil, fmt.Errorf("locate this binary to install it: %w", err)
	}

	steps := []Step{
		{Path: p.Etc, Dir: true, Mode: 0o750},
		{Path: p.SecretsDir(), Dir: true, Mode: 0o700, Note: "root only"},
		{Path: p.Var, Dir: true, Mode: 0o750},
		{Path: p.Spool(), Dir: true, Mode: 0o750, Note: "archive spool"},
		{Path: p.RINEX(), Dir: true, Mode: 0o750},
		{Path: p.Backups(), Dir: true, Mode: 0o750},
		{Path: p.Log, Dir: true, Mode: 0o750},
		{Path: p.Opt, Dir: true, Mode: 0o755},
		{Path: p.Bin(), Dir: true, Mode: 0o755},
		{Path: p.MasterKey(), Mode: 0o600, Secret: true,
			Data: []byte(hex.EncodeToString(key) + "\n"),
			Note: "AES-256 master key — back this up separately from the database"},
		{Path: p.ConfigFile(), Mode: 0o640, Data: cfgBuf.Bytes()},
		{Path: p.Binary(), Mode: 0o755, Source: self, Note: "this binary"},
		{Path: p.ServiceUnit(), Mode: 0o644, Data: service},
		{Path: p.TmpfilesConf(), Mode: 0o644, Data: tmpfiles},
	}

	if a.Archive.Enabled {
		mount, err := renderUnit("archive.mount.tmpl", v)
		if err != nil {
			return nil, err
		}
		steps = append(steps,
			Step{Path: a.Archive.MountPoint, Dir: true, Mode: 0o755, Note: "mount point"},
			Step{Path: p.SMBCred(), Mode: 0o600, Secret: true,
				Data: []byte(fmt.Sprintf("username=%s\npassword=%s\n", a.Archive.Username, a.Archive.Password)),
				Note: "share credentials"},
			Step{Path: p.MountUnit(a.Archive.MountPoint), Mode: 0o644, Data: mount,
				Note: "unit name must match the mount path"})
	}

	pl := &Plan{Answers: a, Paths: p, Config: cfg, Steps: steps, Conflicts: conflicts}
	for _, s := range steps {
		if s.Dir {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, s.Path)); err == nil {
			pl.Existing = append(pl.Existing, s.Path)
		}
	}
	sort.Strings(pl.Existing)
	return pl, nil
}

// ErrAlreadyInstalled is returned when the plan would overwrite a configured
// station. The installer never clobbers a working base by accident: the
// configuration, the master key and the database of a running station are not
// replaceable, and a second run is far more likely to be a mistake than a
// deliberate rebuild.
var ErrAlreadyInstalled = errors.New("this station is already configured")

// Describe renders the plan for a human. Secret contents are never included.
func (pl *Plan) Describe(root string) string {
	var b strings.Builder
	prefix := ""
	if root != "" && root != "/" {
		prefix = root
		fmt.Fprintf(&b, "Writing under %s (a test root; the configuration still names the real paths)\n\n", root)
	}
	fmt.Fprintf(&b, "Station   %s, ID %d, %s\n", pl.Config.Station.Name, pl.Config.Station.StationID, pl.Config.Station.Antenna)
	fmt.Fprintf(&b, "Position  %.8f, %.8f, %.3f m ellipsoidal (%s)\n",
		pl.Config.Station.Position.Latitude, pl.Config.Station.Position.Longitude,
		pl.Config.Station.Position.Height, pl.Config.Station.Position.Mode)
	fmt.Fprintf(&b, "Receiver  %s on %s at %d baud\n",
		pl.Config.Receiver.Model, pl.Config.Receiver.Device, pl.Config.Receiver.Baud)
	for _, m := range pl.Config.Caster.Mountpoint {
		types := make([]string, 0, len(m.Messages))
		for _, msg := range m.Messages {
			types = append(types, fmt.Sprintf("%d(%d)", msg.Type, msg.Interval))
		}
		fmt.Fprintf(&b, "Mountpoint %-24s %s\n", m.Name, strings.Join(types, " "))
	}
	fmt.Fprintf(&b, "Caster    %s   Web %s   Raw %s, %s\n",
		pl.Config.Caster.Listen, pl.Config.Web.Listen,
		pl.Config.Hub.Listener[0].Listen, pl.Config.Hub.Listener[1].Listen)
	if pl.Answers.Archive.Enabled {
		fmt.Fprintf(&b, "Archive   %s on %s, %d day retention\n",
			pl.Answers.Archive.UNC, pl.Answers.Archive.MountPoint, pl.Answers.Archive.RetentionDays)
	} else {
		fmt.Fprintf(&b, "Archive   local spool only, no share\n")
	}
	fmt.Fprintf(&b, "Admin     %s\n", pl.Answers.AdminUser)

	b.WriteString("\nFiles:\n")
	for _, s := range pl.Steps {
		kind := "file"
		switch {
		case s.Dir:
			kind = "dir "
		case s.Source != "":
			kind = "copy"
		}
		note := s.Note
		if s.Secret {
			note = strings.TrimSpace(note + " (contents not shown)")
		}
		if note != "" {
			note = "   " + note
		}
		fmt.Fprintf(&b, "  %s %04o  %s%s\n", kind, s.Mode.Perm(), prefix+s.Path, note)
	}
	if len(pl.Conflicts) > 0 {
		fmt.Fprintf(&b, "\nUnits that must not run beside this one, and are named in the service file:\n")
		for _, c := range pl.Conflicts {
			fmt.Fprintf(&b, "  %s\n", c)
		}
	}
	return b.String()
}

// Apply writes the plan. Force allows overwriting an existing install; without
// it a plan with anything in Existing is refused.
//
// There is no rollback. A failure part-way leaves what it wrote, and says so:
// unwinding a half-written install is more likely to delete something the
// operator wanted than to help, and everything here is either a fresh file or
// one the operator explicitly chose to replace.
func (pl *Plan) Apply(root string, force bool) error {
	if len(pl.Existing) > 0 && !force {
		return fmt.Errorf("%w: %s already exist. Re-run with --force to replace them, "+
			"and back up the master key and the database first: without the key every stored "+
			"NTRIP password becomes unreadable", ErrAlreadyInstalled, strings.Join(pl.Existing, ", "))
	}
	for _, s := range pl.Steps {
		target := filepath.Join(root, s.Path)
		if s.Dir {
			if err := os.MkdirAll(target, s.Mode); err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			// MkdirAll honours umask, and the secrets directory has to be
			// 0700 whatever the shell's umask happens to be.
			if err := os.Chmod(target, s.Mode); err != nil {
				return fmt.Errorf("chmod %s: %w", target, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
		}
		data := s.Data
		if s.Source != "" {
			b, err := os.ReadFile(s.Source)
			if err != nil {
				return fmt.Errorf("read %s: %w", s.Source, err)
			}
			data = b
		}
		if err := writeFileAtomic(target, data, s.Mode); err != nil {
			return err
		}
	}
	return nil
}

// writeFileAtomic writes through a temporary file beside the destination, so a
// crash or a full disk never leaves a half-written unit or configuration in
// place of a whole one.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return os.Rename(name, path)
}

// Provision opens the database the configuration names, applies the schema and
// creates the administrator. It runs after Apply, because it needs the master
// key and the config on disk.
func (pl *Plan) Provision(root string) error {
	keyPath := filepath.Join(root, pl.Paths.MasterKey())
	if _, err := secrets.Load(keyPath); err != nil {
		return fmt.Errorf("read the master key just written: %w", err)
	}
	dbPath := filepath.Join(root, pl.Config.Telemetry.DBPath)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		return err
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("create the station database: %w", err)
	}
	defer db.Close()
	if err := db.CreateAdmin(pl.Answers.AdminUser, pl.Answers.AdminPass); err != nil {
		return fmt.Errorf("create administrator %q: %w", pl.Answers.AdminUser, err)
	}
	return nil
}

// detectConflicts names units that hold the serial port. Only one process may
// own the receiver, and on a machine that ran something else first the old
// unit will fight this one on every boot in a way that looks like a flapping
// receiver rather than a configuration mistake.
func detectConflicts(root string) []string {
	known := []string{"rtk-stream.service", "str2str.service", "rtkbase.service", "gpsd.service", "gpsd.socket"}
	var found []string
	for _, unit := range known {
		for _, dir := range []string{"/etc/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"} {
			if _, err := os.Stat(filepath.Join(root, dir, unit)); err == nil {
				found = append(found, unit)
				break
			}
		}
	}
	return found
}
