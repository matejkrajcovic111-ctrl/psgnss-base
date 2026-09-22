package setup

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/psgnss/psgnss-base/internal/version"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

var unitTemplates = template.Must(template.ParseFS(templateFS, "templates/*.tmpl"))

// unitVars is what the unit templates see. It is deliberately flat: a systemd
// unit is not the place to discover that a nested field was empty.
type unitVars struct {
	Documentation string

	Binary     string
	ConfigFile string
	Etc        string
	SecretsDir string
	SMBCred    string
	Var        string
	Spool      string
	RINEX      string
	Backups    string
	Log        string
	Opt        string
	Bin        string

	ArchiveUNC       string
	ArchiveMount     string
	ArchiveMountUnit string

	// Conflicts names units that must never run beside this one. It is empty
	// on a fresh machine; this station carries rtk-stream.service because that
	// is what used to hold the serial port here.
	Conflicts []string
}

func varsFor(a Answers, p Paths, conflicts []string) unitVars {
	v := unitVars{
		Documentation: version.ProjectURL,

		Binary:     p.Binary(),
		ConfigFile: p.ConfigFile(),
		Etc:        p.Etc,
		SecretsDir: p.SecretsDir(),
		SMBCred:    p.SMBCred(),
		Var:        p.Var,
		Spool:      p.Spool(),
		RINEX:      p.RINEX(),
		Backups:    p.Backups(),
		Log:        p.Log,
		Opt:        p.Opt,
		Bin:        p.Bin(),
		Conflicts:  conflicts,
	}
	if a.Archive.Enabled {
		v.ArchiveUNC = a.Archive.UNC
		v.ArchiveMount = a.Archive.MountPoint
		v.ArchiveMountUnit = unitNameForMount(a.Archive.MountPoint) + ".mount"
	}
	return v
}

func renderUnit(name string, v unitVars) ([]byte, error) {
	var b bytes.Buffer
	if err := unitTemplates.ExecuteTemplate(&b, name, v); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return b.Bytes(), nil
}

// unitNameForMount is systemd's path escaping, which is not optional: a mount
// unit is only ever read if its filename is the escaped mount path. /mnt/gnssraw
// is mnt-gnssraw, and a unit called anything else sits there doing nothing
// while the archive silently never reaches the share.
//
// Rules, from systemd.unit(5): leading and trailing slashes are dropped, "/"
// becomes "-", and anything outside [a-zA-Z0-9:_.] becomes \xNN. A leading dot
// is escaped too, since a unit name may not begin with one. The root path is
// the single character "-".
func unitNameForMount(path string) string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return "-"
	}
	var b strings.Builder
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == ':', c == '_':
			b.WriteByte(c)
		case c == '.' && i > 0:
			b.WriteByte('.')
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}
