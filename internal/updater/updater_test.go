package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/release"
)

// station is a whole fake install: a binary to replace, a release to fetch,
// somewhere to serve it from, and a recorded list of what was run.
type station struct {
	t          *testing.T
	dir        string
	binary     string
	configPath string

	pub  ed25519.PublicKey
	priv ed25519.PrivateKey

	artifact  []byte
	envelope  []byte
	files     *httptest.Server
	health    *httptest.Server
	healthy   bool
	reportVer string

	mu   sync.Mutex
	ran  [][]string
	fail map[string]string // command prefix -> error text
}

const oldBinary = "#!/bin/sh\necho old\n"

func newStation(t *testing.T, newBinary string) *station {
	t.Helper()
	dir := t.TempDir()
	s := &station{
		t: t, dir: dir,
		binary:     filepath.Join(dir, "bin", "psgnssd"),
		configPath: filepath.Join(dir, "psgnss.toml"),
		artifact:   []byte(newBinary),
		healthy:    true,
		reportVer:  "v9.9.9 (abc1234)",
		fail:       map[string]string{},
	}
	if err := os.MkdirAll(filepath.Dir(s.binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.binary, []byte(oldBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.configPath, []byte("# config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var err error
	s.pub, s.priv, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	// The daemon's status endpoint, which is what "did it come back" means.
	s.health = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.healthy {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"online":true,"version":%q}`, s.reportVer)
	}))
	t.Cleanup(s.health.Close)

	s.files = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/release.json"):
			w.Write(s.envelope)
		default:
			w.Write(s.artifact)
		}
	}))
	t.Cleanup(s.files.Close)
	return s
}

// publish signs a manifest for the current artifact. tweak may corrupt it
// after the hash is computed, to stand in for a substituted download.
func (s *station) publish(version string, tweak func(*release.Manifest)) {
	s.t.Helper()
	sum := sha256.Sum256(s.artifact)
	m := &release.Manifest{
		Version: version, Commit: "abc1234", Released: time.Now().UTC().Truncate(time.Second),
		Artifacts: []release.Artifact{{
			GOOS: osOf(), GOARCH: archOf(),
			URL:    s.files.URL + "/psgnssd-linux-arm64",
			Size:   int64(len(s.artifact)),
			SHA256: hex.EncodeToString(sum[:]),
		}},
	}
	if tweak != nil {
		tweak(m)
	}
	env, err := release.Sign(m, s.priv)
	if err != nil {
		s.t.Fatal(err)
	}
	s.envelope = env
}

func (s *station) options() Options {
	return Options{
		ManifestURL:   s.files.URL + "/release.json",
		BinaryPath:    s.binary,
		ConfigPath:    s.configPath,
		ServiceUnit:   "psgnss.service",
		WebListen:     strings.TrimPrefix(s.health.URL, "http://"),
		HealthTimeout: 5 * time.Second,
		HTTPClient:    s.files.Client(),
		PublicKey:     s.pub,
		Log:           func(string) {},
		Runner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			s.mu.Lock()
			s.ran = append(s.ran, append([]string{name}, args...))
			s.mu.Unlock()
			key := name
			if len(args) > 0 {
				key = filepath.Base(name) + " " + args[0]
			}
			for prefix, msg := range s.fail {
				if strings.Contains(key, prefix) {
					return []byte(msg), errors.New("exit status 1")
				}
			}
			return []byte("psgnssd v9.9.9 (abc1234)"), nil
		},
	}
}

func (s *station) installed() string {
	s.t.Helper()
	b, err := os.ReadFile(s.binary)
	if err != nil {
		s.t.Fatal(err)
	}
	return string(b)
}

func (s *station) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.ran {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func (s *station) restarts() int {
	n := 0
	for _, c := range s.commands() {
		if strings.HasPrefix(c, "systemctl restart") {
			n++
		}
	}
	return n
}

func osOf() string   { return runtimeGOOS }
func archOf() string { return runtimeGOARCH }

func TestUpdateInstallsAndKeepsTheOldBinary(t *testing.T) {
	s := newStation(t, "#!/bin/sh\necho new\n")
	s.publish("v9.9.9", nil)

	m, err := Check(context.Background(), s.options())
	if err != nil {
		t.Fatal(err)
	}
	res, err := Apply(context.Background(), s.options(), m, "v0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.installed(); got != "#!/bin/sh\necho new\n" {
		t.Errorf("installed binary is %q", got)
	}
	prev, err := os.ReadFile(s.binary + ".previous")
	if err != nil || string(prev) != oldBinary {
		t.Errorf("the replaced binary was not kept: %v %q", err, prev)
	}
	if s.restarts() != 1 {
		t.Errorf("restarted %d times, want 1: %v", s.restarts(), s.commands())
	}
	if !strings.Contains(res.Health, "serving") {
		t.Errorf("health = %q", res.Health)
	}
	// Nothing is left lying about in the binary directory.
	entries, _ := os.ReadDir(filepath.Dir(s.binary))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".psgnssd") {
			t.Errorf("left %s behind", e.Name())
		}
	}
}

// The check that earns its keep: a release whose configuration validation has
// got stricter would start, refuse the config and leave the station down. It
// must be caught before anything is replaced.
func TestAReleaseThatRefusesThisConfigIsNeverInstalled(t *testing.T) {
	s := newStation(t, "#!/bin/sh\necho new\n")
	s.publish("v9.9.9", nil)
	s.fail["psgnssd.incoming --check"] = "unknown config key(s): [caster.legacy_mode]"

	m, err := Check(context.Background(), s.options())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Apply(context.Background(), s.options(), m, "v0.0.1")
	if err == nil {
		t.Fatal("installed a release that rejects this station's configuration")
	}
	if !strings.Contains(err.Error(), "refuses this station's configuration") {
		t.Errorf("unhelpful error: %v", err)
	}
	if s.installed() != oldBinary {
		t.Error("the running binary was replaced anyway")
	}
	if s.restarts() != 0 {
		t.Errorf("the service was restarted: %v", s.commands())
	}
	if _, err := os.Stat(s.binary + ".previous"); err == nil {
		t.Error("a rollback copy was made for an update that never started")
	}
}

func TestATamperedDownloadIsRefused(t *testing.T) {
	const genuine = "#!/bin/sh\necho new\n"
	const substituted = "#!/bin/sh\necho EVL\n"
	s := newStation(t, genuine)
	s.publish("v9.9.9", nil)
	// The signed manifest still describes the genuine binary; the server now
	// hands out something else of exactly the same length, so it is the hash
	// that has to catch it and not the size.
	if len(substituted) != len(genuine) {
		t.Fatalf("this test needs both to be the same length: %d vs %d", len(substituted), len(genuine))
	}
	s.artifact = []byte(substituted)

	m, err := Check(context.Background(), s.options())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Apply(context.Background(), s.options(), m, "v0.0.1")
	if err == nil {
		t.Fatal("installed a binary that does not match the signed manifest")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("the size check caught this, not the hash: %v", err)
	}
	if s.installed() != oldBinary {
		t.Error("the running binary was replaced with the substituted download")
	}
}

func TestAForgedReleaseIsRefusedBeforeAnythingIsFetched(t *testing.T) {
	s := newStation(t, "#!/bin/sh\necho new\n")
	s.publish("v9.9.9", nil)

	// Signed by a key this station does not trust.
	other, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	_ = other
	sum := sha256.Sum256(s.artifact)
	forged, err := release.Sign(&release.Manifest{
		Version: "v99.0.0", Released: time.Now(),
		Artifacts: []release.Artifact{{GOOS: runtimeGOOS, GOARCH: runtimeGOARCH,
			URL: s.files.URL + "/x", Size: int64(len(s.artifact)), SHA256: hex.EncodeToString(sum[:])}},
	}, otherPriv)
	if err != nil {
		t.Fatal(err)
	}
	s.envelope = forged

	if _, err := Check(context.Background(), s.options()); !errors.Is(err, release.ErrBadSignature) {
		t.Fatalf("a release signed with another key gave %v", err)
	}
	if s.installed() != oldBinary {
		t.Error("something was installed")
	}
}

// If the new release does not serve, the station goes back to the one that
// did. Never neither.
func TestAReleaseThatDoesNotServeIsRolledBack(t *testing.T) {
	s := newStation(t, "#!/bin/sh\necho new\n")
	s.publish("v9.9.9", nil)
	s.healthy = false

	m, err := Check(context.Background(), s.options())
	if err != nil {
		t.Fatal(err)
	}
	opts := s.options()
	opts.HealthTimeout = 3 * time.Second
	res, err := Apply(context.Background(), opts, m, "v0.0.1")
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("err = %v, want a rollback", err)
	}
	if res == nil {
		t.Fatal("no result to tell the operator what happened")
	}
	if got := s.installed(); got != oldBinary {
		t.Errorf("after rollback the binary is %q, want the previous one", got)
	}
	if s.restarts() != 2 {
		t.Errorf("restarted %d times, want 2 (the new release and the rollback): %v",
			s.restarts(), s.commands())
	}
}

func TestASizeThatDisagreesWithTheManifestIsRefused(t *testing.T) {
	s := newStation(t, "#!/bin/sh\necho new\n")
	s.publish("v9.9.9", func(m *release.Manifest) { m.Artifacts[0].Size += 100 })

	m, err := Check(context.Background(), s.options())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Apply(context.Background(), s.options(), m, "v0.0.1")
	if err == nil || !strings.Contains(err.Error(), "signed manifest says") {
		t.Fatalf("err = %v, want a size mismatch", err)
	}
	if s.installed() != oldBinary {
		t.Error("the running binary was replaced")
	}
}

func TestTwoUpdatesDoNotRunAtOnce(t *testing.T) {
	s := newStation(t, "#!/bin/sh\necho new\n")
	s.publish("v9.9.9", nil)
	lockPath := filepath.Join(filepath.Dir(s.binary), ".psgnssd.update.lock")
	if err := os.WriteFile(lockPath, []byte("999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Check(context.Background(), s.options())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), s.options(), m, "v0.0.1"); err == nil ||
		!strings.Contains(err.Error(), "another update is running") {
		t.Fatalf("err = %v, want a refusal to run twice", err)
	}
	if s.installed() != oldBinary {
		t.Error("the second update ran anyway")
	}
}

func TestABuildWithNoKeyRefusesEveryRelease(t *testing.T) {
	s := newStation(t, "#!/bin/sh\necho new\n")
	s.publish("v9.9.9", nil)
	// An empty but non-nil key stands in for a build whose publicKeyHex is
	// unset; a nil one would simply fall back to this build's compiled-in key.
	opts := s.options()
	opts.PublicKey = ed25519.PublicKey{}
	if _, err := Check(context.Background(), opts); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v, want a refusal to verify anything", err)
	}
	// And a key of the wrong length is not quietly padded or truncated.
	opts.PublicKey = ed25519.PublicKey("too short")
	if _, err := Check(context.Background(), opts); !errors.Is(err, ErrNoKey) {
		t.Fatalf("a %d-byte key gave %v", len("too short"), err)
	}
}

// Tests describe the platform they are running on, since the manifest must
// carry a matching artifact.
var (
	runtimeGOOS   = runtime.GOOS
	runtimeGOARCH = runtime.GOARCH
)
