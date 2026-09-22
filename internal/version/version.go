// Package version carries build metadata injected via -ldflags.
package version

var (
	Version = "dev"
	Commit  = "unknown"
)

// ProjectURL is where this software comes from. It is one constant because it
// appears in places a person reads when something has gone wrong -- the
// Documentation= line of the systemd unit, which `systemctl status` prints --
// and those must not be allowed to drift apart or point somewhere stale.
const ProjectURL = "https://github.com/matejkrajcovic111-ctrl/psgnss-base"

func String() string { return Version + " (" + Commit + ")" }
