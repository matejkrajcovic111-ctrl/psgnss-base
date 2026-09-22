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

// String names the build. A source tarball carries no git metadata, so a
// station installed from the published one-liner has no commit to report;
// "v1.0.0" reads better there than "v1.0.0 (unknown)".
func String() string {
	if Commit == "" || Commit == "unknown" {
		return Version
	}
	return Version + " (" + Commit + ")"
}
