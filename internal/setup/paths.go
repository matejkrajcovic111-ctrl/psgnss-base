package setup

import "path/filepath"

// Paths are the well-known locations on a deployed station.
//
// They are absolute and go into the generated configuration verbatim. The
// installer's --root prefix applies only to where files are written, never to
// what the configuration says: a tree written under a temporary root is for
// inspecting the install, not for running it, and a config full of
// /tmp/xxx/var/lib paths would be a trap the day someone copied one.
type Paths struct {
	Etc          string
	Var          string
	Log          string
	Opt          string
	SystemdUnits string
	Tmpfiles     string
	ChronyConf   string
}

func DefaultPaths() Paths {
	return Paths{
		Etc:          "/etc/psgnss",
		Var:          "/var/lib/psgnss",
		Log:          "/var/log/psgnss",
		Opt:          "/opt/psgnss",
		SystemdUnits: "/etc/systemd/system",
		Tmpfiles:     "/etc/tmpfiles.d",
		ChronyConf:   "/etc/chrony/conf.d",
	}
}

func (p Paths) ConfigFile() string { return filepath.Join(p.Etc, "psgnss.toml") }
func (p Paths) SecretsDir() string { return filepath.Join(p.Etc, "secrets") }
func (p Paths) MasterKey() string  { return filepath.Join(p.SecretsDir(), "master.key") }
func (p Paths) SMBCred() string    { return filepath.Join(p.SecretsDir(), "smb.cred") }
func (p Paths) Spool() string      { return filepath.Join(p.Var, "spool") }
func (p Paths) RINEX() string      { return filepath.Join(p.Var, "rinex") }
func (p Paths) Backups() string    { return filepath.Join(p.Var, "backups") }
func (p Paths) Database() string   { return filepath.Join(p.Var, "psgnss.db") }
func (p Paths) LogDir() string     { return p.Log }
func (p Paths) Bin() string        { return filepath.Join(p.Opt, "bin") }
func (p Paths) Binary() string     { return filepath.Join(p.Bin(), "psgnssd") }

func (p Paths) ServiceUnit() string  { return filepath.Join(p.SystemdUnits, "psgnss.service") }
func (p Paths) TmpfilesConf() string { return filepath.Join(p.Tmpfiles, "psgnss.conf") }

// MountUnit is the unit file for an archive mount. systemd derives the unit
// name from the mount path and refuses any other: /mnt/gnssraw must be
// mnt-gnssraw.mount, or the unit is inert and the share is never mounted.
func (p Paths) MountUnit(mountPoint string) string {
	return filepath.Join(p.SystemdUnits, unitNameForMount(mountPoint)+".mount")
}
