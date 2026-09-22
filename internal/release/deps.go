package release

import (
	"context"
	"os/exec"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

// Dependencies is the third-party inventory of this build.
//
// # Nothing here is a hand-maintained version number
//
// A list of versions typed into a file is wrong the day after it is written,
// and wrong silently. Every version below is read from something that cannot
// disagree with reality:
//
//   - Go modules come from the build information the linker embeds, which
//     survives -trimpath and -buildvcs=false.
//   - Browser works come from the filename each one is vendored under. The
//     page loads uplot-1.6.32.min.js by name, so the name is the version.
//   - External programs are asked, by running them.
//
// assetNames is the list of vendored web asset filenames; the caller passes it
// so this package does not have to import the web server.
func Dependencies(assetNames []string, convbinPath string) []Dependency {
	out := goModules()
	out = append(out, browserWorks(assetNames)...)
	out = append(out, externalPrograms(convbinPath)...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// goLicences names the licence of each module that can be linked in. A module
// appearing here that is not in the build is simply not reported; one in the
// build that is missing here is reported with an empty licence, which the
// inventory test fails on.
var goLicences = map[string]string{
	"github.com/BurntSushi/toml":            "MIT",
	"golang.org/x/crypto":                   "BSD-3-Clause",
	"golang.org/x/sys":                      "BSD-3-Clause",
	"modernc.org/sqlite":                    "BSD-3-Clause",
	"modernc.org/libc":                      "BSD-3-Clause",
	"modernc.org/mathutil":                  "BSD-3-Clause",
	"modernc.org/memory":                    "BSD-3-Clause",
	"github.com/dustin/go-humanize":         "MIT",
	"github.com/google/uuid":                "BSD-3-Clause",
	"github.com/mattn/go-isatty":            "MIT",
	"github.com/ncruces/go-strftime":        "MIT",
	"github.com/remyoudompheng/bigfft":      "BSD-3-Clause",
	"modernc.org/gc/v3":                     "BSD-3-Clause",
	"modernc.org/strutil":                   "BSD-3-Clause",
	"modernc.org/token":                     "BSD-3-Clause",
	"golang.org/x/exp":                      "BSD-3-Clause",
	"github.com/google/pprof":               "Apache-2.0",
	"golang.org/x/text":                     "BSD-3-Clause",
	"golang.org/x/mod":                      "BSD-3-Clause",
	"golang.org/x/tools":                    "BSD-3-Clause",
	"github.com/hashicorp/golang-lru/v2":    "MPL-2.0",
	"github.com/ncruces/julianday":          "MIT",
	"github.com/dustin/go-humanize/english": "MIT",
}

func goModules() []Dependency {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	out := []Dependency{{
		Name: "Go", Version: strings.TrimPrefix(info.GoVersion, "go"),
		Licence: "BSD-3-Clause", Kind: "go", URL: "https://go.dev",
	}}
	for _, d := range info.Deps {
		if d.Replace != nil {
			d = d.Replace
		}
		out = append(out, Dependency{
			Name: d.Path, Version: d.Version, Licence: goLicences[d.Path],
			Kind: "go", URL: "https://" + d.Path,
		})
	}
	return out
}

// browserWork is one vendored work the page loads. Match extracts the version
// from the filename it is vendored under, which is the only place the version
// exists and the one the page itself reads.
type browserWork struct {
	name    string
	licence string
	url     string
	match   *regexp.Regexp
	// fixed is used for a work whose files carry no version, where the honest
	// answer is the state rather than a number.
	fixed string
}

var browserWorkList = []browserWork{
	{name: "uPlot", licence: "MIT", url: "https://github.com/leeoniya/uPlot",
		match: regexp.MustCompile(`^uplot-([0-9][0-9.]*)\.min\.js$`)},
	{name: "Tabler", licence: "MIT", url: "https://github.com/tabler/tabler",
		match: regexp.MustCompile(`^tabler-([0-9][0-9.]*)\.min\.css$`)},
	{name: "Leaflet", licence: "BSD-2-Clause", url: "https://github.com/Leaflet/Leaflet",
		match: regexp.MustCompile(`^leaflet-([0-9][0-9.]*)\.js$`)},
	{name: "Inter", licence: "OFL-1.1", url: "https://github.com/rsms/inter",
		match: regexp.MustCompile(`^inter-[0-9]+\.woff2$`), fixed: "latin subset, four weights"},
	{name: "React", licence: "MIT", url: "https://react.dev",
		match: regexp.MustCompile(`^react\.js$`), fixed: "UMD production build"},
	{name: "htm", licence: "Apache-2.0", url: "https://github.com/developit/htm",
		match: regexp.MustCompile(`^htm\.js$`), fixed: "UMD build"},
}

func browserWorks(assetNames []string) []Dependency {
	var out []Dependency
	for _, w := range browserWorkList {
		for _, name := range assetNames {
			m := w.match.FindStringSubmatch(name)
			if m == nil {
				continue
			}
			version := w.fixed
			if len(m) > 1 {
				version = m[1]
			}
			out = append(out, Dependency{Name: w.name, Version: version,
				Licence: w.licence, Kind: "browser", URL: w.url})
			break
		}
	}
	return out
}

// convbinVersion reads the banner `convbin --version` prints. On the station
// this is measured against, that is exactly:
//
//	convbin RTKLIB demo5 b34L
//
// so the useful part is everything after the program's own name. Called
// without arguments convbin prints "no input file" and nothing else, which is
// why the flag is passed; an output this cannot parse is reported as
// unrecognised rather than guessed at.
var convbinVersion = regexp.MustCompile(`(?im)^\s*convbin\s+(\S.*?)\s*$`)

func externalPrograms(convbinPath string) []Dependency {
	if convbinPath == "" {
		return nil
	}
	d := Dependency{Name: "RTKLIB convbin", Kind: "external", Licence: "BSD-2-Clause",
		URL: "https://github.com/rtklibexplorer/RTKLIB", Version: "not installed"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// The exit status is not the point; the banner is. convbin exits non-zero
	// for --version on some builds.
	out, err := exec.CommandContext(ctx, convbinPath, "--version").CombinedOutput()
	switch {
	case err != nil && len(out) == 0:
		// Left as "not installed": nothing ran and nothing was said.
	case convbinVersion.Match(out):
		d.Version = string(convbinVersion.FindSubmatch(out)[1])
	default:
		d.Version = "installed, version not recognised"
	}
	return []Dependency{d}
}
