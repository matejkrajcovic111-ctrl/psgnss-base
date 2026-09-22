package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func manifest() *Manifest {
	return &Manifest{
		Version:  "v0.7.0",
		Commit:   "abc1234",
		Released: time.Now().UTC().Truncate(time.Second),
		Artifacts: []Artifact{{
			GOOS: "linux", GOARCH: "arm64",
			URL:    "https://example.invalid/psgnssd-linux-arm64",
			Size:   13041812,
			SHA256: strings.Repeat("ab", 32),
		}},
	}
}

func TestSignedReleaseRoundTrips(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	env, err := Sign(manifest(), priv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(env, pub)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "v0.7.0" || len(got.Artifacts) != 1 {
		t.Fatalf("round trip lost content: %+v", got)
	}
	if _, err := got.ArtifactFor("linux", "arm64"); err != nil {
		t.Error(err)
	}
	if _, err := got.ArtifactFor("linux", "amd64"); err == nil {
		t.Error("returned an artifact for a platform the release does not carry")
	}
}

// A signature that covers a different document, or a different key, must not
// verify. Both of these are the whole point of the format.
func TestVerifyRefusesWhatItShould(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	env, err := Sign(manifest(), priv)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Verify(env, other); err == nil {
		t.Error("a release verified against a key that did not sign it")
	}
	if _, err := Verify(env, nil); err == nil {
		t.Error("a release verified against no key at all")
	}

	// Swap the manifest for another one, keeping the signature.
	var e Envelope
	if err := json.Unmarshal(env, &e); err != nil {
		t.Fatal(err)
	}
	swapped := manifest()
	swapped.Artifacts[0].URL = "https://elsewhere.invalid/psgnssd-linux-arm64"
	body, _ := json.MarshalIndent(swapped, "", "  ")
	e.Manifest = base64.StdEncoding.EncodeToString(body)
	tampered, _ := json.Marshal(e)
	if _, err := Verify(tampered, pub); err == nil {
		t.Error("a substituted manifest verified under the original signature")
	}

	// A future envelope format must be refused, not half-read.
	if err := json.Unmarshal(env, &e); err != nil {
		t.Fatal(err)
	}
	e.Format = Format + 1
	future, _ := json.Marshal(e)
	if _, err := Verify(future, pub); err == nil {
		t.Error("an envelope from a later format was accepted")
	}
}

// A signature proves who wrote the manifest, not that it makes sense.
func TestASignedManifestStillHasToBeUsable(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	cases := map[string]func(*Manifest){
		"no version":           func(m *Manifest) { m.Version = "" },
		"no artifacts":         func(m *Manifest) { m.Artifacts = nil },
		"plain http":           func(m *Manifest) { m.Artifacts[0].URL = "http://example.invalid/x" },
		"short hash":           func(m *Manifest) { m.Artifacts[0].SHA256 = "abcd" },
		"hash that is not hex": func(m *Manifest) { m.Artifacts[0].SHA256 = strings.Repeat("zz", 32) },
		"no size":              func(m *Manifest) { m.Artifacts[0].Size = 0 },
		"no platform":          func(m *Manifest) { m.Artifacts[0].GOARCH = "" },
		"same platform twice": func(m *Manifest) {
			m.Artifacts = append(m.Artifacts, m.Artifacts[0])
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			m := manifest()
			breakIt(m)
			if _, err := Sign(m, priv); err == nil {
				t.Error("signed a manifest that could not be acted on")
			}
		})
	}
}

// The inventory is only worth having if it cannot disagree with the build. Go
// module versions come from the linker, and every browser work's version comes
// from the filename it is vendored under, so this checks that each declared
// work is actually found and that nothing is reported without a licence.
func TestDependencyInventoryMatchesWhatIsReallyVendored(t *testing.T) {
	assetDir := filepath.Join("..", "web", "assets")
	entries, err := os.ReadDir(assetDir)
	if err != nil {
		t.Skip("web assets not present")
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}

	deps := Dependencies(names, "")
	found := map[string]Dependency{}
	for _, d := range deps {
		found[d.Name] = d
		if d.Licence == "" {
			t.Errorf("%s (%s) is reported with no licence; add it to goLicences "+
				"or to browserWorkList", d.Name, d.Kind)
		}
		if d.Version == "" {
			t.Errorf("%s is reported with no version", d.Name)
		}
	}

	// Every declared browser work must actually be vendored. One that is not
	// has either been removed --- in which case it should leave this list and
	// CREDITS.md together --- or renamed, which would make the reported
	// version silently wrong.
	for _, w := range browserWorkList {
		if _, ok := found[w.name]; !ok {
			t.Errorf("%s is declared as vendored but no asset matches %s", w.name, w.match)
		}
	}

	// And the versions are the ones in the filenames, not a second copy.
	for name, want := range map[string]string{"uPlot": "1.6.32", "Tabler": "1.4.0", "Leaflet": "1.9.4"} {
		matched := false
		for _, n := range names {
			if strings.Contains(n, strings.ToLower(name)+"-"+want) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("no vendored file names %s %s; this test's expectation is stale, "+
				"which is exactly what it exists to notice", name, want)
		}
		if got := found[name].Version; got != want {
			t.Errorf("%s reported as %s, vendored as %s", name, got, want)
		}
	}

	// The Go module list comes from the build information of whatever binary
	// is asking, so in this test it is the test binary's and not the daemon's.
	// That is the point rather than a limitation --- the inventory describes
	// the build it ships inside --- so all that can be asserted here is the
	// toolchain, which every build has. The daemon's own list is what
	// `psgnssd --dependencies` prints and what goes into a release manifest.
	if _, ok := found["Go"]; !ok {
		t.Error("the inventory does not report the Go toolchain version")
	}
	for _, d := range deps {
		if d.Kind == "go" && d.Name != "Go" && !strings.Contains(d.Version, ".") {
			t.Errorf("module %s has an implausible version %q", d.Name, d.Version)
		}
	}
}

// A build without a key must verify nothing rather than everything.
func TestABuildWithoutAKeyHasNone(t *testing.T) {
	if publicKeyHex == "" && HaveKey() {
		t.Fatal("a build with no key claims to have one")
	}
	if publicKeyHex != "" && !HaveKey() {
		t.Fatalf("publicKeyHex is set but unusable: %q", publicKeyHex)
	}
}

// convbin's banner, read off the station rather than out of the manual:
// `convbin --version` prints "convbin RTKLIB demo5 b34L", and called with no
// arguments it prints "no input file" and nothing more, which is what the
// first attempt at this parsed and got nothing from.
func TestConvbinVersionIsReadFromItsRealBanner(t *testing.T) {
	cases := map[string]string{
		"convbin RTKLIB demo5 b34L\n":           "RTKLIB demo5 b34L",
		"convbin ver.2.4.3 b34\n":               "ver.2.4.3 b34",
		"  convbin RTKLIB 2.4.3 b34\r\n":        "RTKLIB 2.4.3 b34",
		"some preamble\nconvbin RTKLIB demo5\n": "RTKLIB demo5",
	}
	for banner, want := range cases {
		m := convbinVersion.FindStringSubmatch(banner)
		if m == nil {
			t.Errorf("no version found in %q", banner)
			continue
		}
		if m[1] != want {
			t.Errorf("read %q from %q, want %q", m[1], banner, want)
		}
	}
	// What it prints with no arguments must not be mistaken for a version.
	if convbinVersion.MatchString("no input file\n") {
		t.Error("the no-arguments output is being read as a version")
	}
}
