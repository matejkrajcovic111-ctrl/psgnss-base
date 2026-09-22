// Package updater replaces this station's binary with a signed release, and
// puts the old one back if the new one does not serve.
//
// # The order of operations is the whole design
//
// This runs on a base station that rovers are connected to. Every check that
// can be made before anything is replaced, is made before anything is
// replaced: the signature, the size, the hash, whether the new binary runs at
// all, and --- the one that matters most --- whether the new binary accepts
// the configuration this station is actually using. Configuration validation
// here is strict and gets stricter: a release that added a required key, or
// tightened a rule, would start, refuse the config and leave the station down.
// Asking it first costs a fraction of a second and turns that into a refusal
// to upgrade.
//
// Only then is the old binary kept aside and the new one moved into place. If
// the restarted service does not answer, the old binary goes back and the
// service is restarted again. The station is either running the new release or
// the old one; it is never left running neither.
//
// # What it does not do
//
// It does not update on its own. Nothing here runs on a timer. A base station
// swapping its own binary unattended, at an hour nobody chose, is not a
// feature its operator asked for; `psgnssd --update` is a thing a person does.
package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/psgnss/psgnss-base/internal/release"
)

// Options is everything the updater needs to know about this station.
type Options struct {
	ManifestURL string
	BinaryPath  string
	ConfigPath  string
	ServiceUnit string
	// WebListen is the daemon's own listen address, used to ask it whether it
	// came back. Empty disables the health check, which also disables the
	// automatic rollback: say so rather than pretending to have checked.
	WebListen string
	// HealthTimeout is how long the restarted service has to answer.
	HealthTimeout time.Duration
	// HTTPClient is replaceable for tests.
	HTTPClient *http.Client
	// Runner runs external commands; replaceable for tests.
	Runner func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Log receives progress, one line at a time.
	Log func(string)
	// PublicKey verifies the release. Empty means this build's compiled-in
	// key, which is what a station always uses; a test supplies its own.
	PublicKey ed25519.PublicKey
}

const (
	// MaxArtifact is a sanity bound on a download. The real bound is the size
	// in the signed manifest, which is checked first; this stops a manifest
	// claiming something absurd from filling the card before the hash is
	// reached.
	MaxArtifact = 256 << 20
	// DefaultHealthTimeout is generous: a cold start rebuilds the ephemeris
	// store and reopens the archive.
	DefaultHealthTimeout = 45 * time.Second
)

var (
	ErrNoKey       = errors.New("this build carries no release public key, so it cannot verify any release")
	ErrNoManifest  = errors.New("no release manifest URL is configured")
	ErrSameVersion = errors.New("already on this release")
	ErrRolledBack  = errors.New("the new release did not serve; the previous binary was put back")
)

func (o *Options) normalise() {
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	if o.Runner == nil {
		o.Runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
	if o.Log == nil {
		o.Log = func(string) {}
	}
	if o.HealthTimeout == 0 {
		o.HealthTimeout = DefaultHealthTimeout
	}
	if o.ServiceUnit == "" {
		o.ServiceUnit = "psgnss.service"
	}
	if o.PublicKey == nil {
		o.PublicKey = release.PublicKey()
	}
}

// Check fetches the manifest and verifies its signature. It changes nothing.
func Check(ctx context.Context, o Options) (*release.Manifest, error) {
	o.normalise()
	// A key of the wrong length is the same as no key: a build that cannot
	// tell a real release from a forged one must refuse both.
	if len(o.PublicKey) != ed25519.PublicKeySize {
		return nil, ErrNoKey
	}
	if strings.TrimSpace(o.ManifestURL) == "" {
		return nil, ErrNoManifest
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.ManifestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "psgnssd-updater")
	resp, err := o.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch release manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release manifest: HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return release.Verify(body, o.PublicKey)
}

// Result describes what an update did.
type Result struct {
	From         string
	To           string
	PreviousPath string
	Health       string
}

// Apply installs a verified release. The manifest must have come from Check:
// nothing here re-checks the signature, because nothing here should ever be
// handed a manifest that has not been through it.
func Apply(ctx context.Context, o Options, m *release.Manifest, currentVersion string) (*Result, error) {
	o.normalise()
	log := o.Log

	artifact, err := m.ArtifactForThisBuild()
	if err != nil {
		return nil, err
	}
	binDir := filepath.Dir(o.BinaryPath)
	download := filepath.Join(binDir, ".psgnssd.incoming")
	previous := o.BinaryPath + ".previous"

	// Only one at a time. Two updaters racing over one binary is a way to end
	// up with neither release installed.
	unlock, err := lock(filepath.Join(binDir, ".psgnssd.update.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()

	log(fmt.Sprintf("downloading %s (%d bytes)", artifact.URL, artifact.Size))
	if err := fetchArtifact(ctx, o, artifact, download); err != nil {
		os.Remove(download)
		return nil, err
	}
	defer os.Remove(download)

	log("verifying the download against the signed manifest")
	if err := verifyFile(download, artifact); err != nil {
		return nil, err
	}
	if err := os.Chmod(download, 0o755); err != nil {
		return nil, err
	}

	// Does it run, and does it accept this station's configuration? Both
	// before anything is replaced.
	log("asking the new binary for its version")
	out, err := o.Runner(ctx, download, "--version")
	if err != nil {
		return nil, fmt.Errorf("the downloaded binary does not run: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	newVersion := strings.TrimSpace(string(out))
	log("  " + newVersion)

	if o.ConfigPath != "" {
		log("checking the new binary accepts this station's configuration")
		out, err := o.Runner(ctx, download, "--check", "--config", o.ConfigPath)
		if err != nil {
			return nil, fmt.Errorf("the new release refuses this station's configuration, so it "+
				"would start and stop: %w\n%s", err, strings.TrimSpace(string(out)))
		}
	}

	log("keeping the running binary as " + previous)
	if err := copyFile(o.BinaryPath, previous, 0o755); err != nil {
		return nil, fmt.Errorf("could not keep a copy of the running binary, so there would be "+
			"nothing to go back to: %w", err)
	}

	log("installing")
	if err := os.Rename(download, o.BinaryPath); err != nil {
		return nil, fmt.Errorf("install %s: %w", o.BinaryPath, err)
	}

	res := &Result{From: currentVersion, To: m.Version, PreviousPath: previous}

	log("restarting " + o.ServiceUnit)
	if out, err := o.Runner(ctx, "systemctl", "restart", o.ServiceUnit); err != nil {
		rollback(ctx, o, previous, log)
		return res, fmt.Errorf("%w: restart failed: %v (%s)", ErrRolledBack, err, strings.TrimSpace(string(out)))
	}

	if o.WebListen == "" {
		res.Health = "not checked: no web listen address configured, so no rollback was possible"
		log(res.Health)
		return res, nil
	}
	log("waiting for it to serve")
	version, err := waitHealthy(ctx, o)
	if err != nil {
		rollback(ctx, o, previous, log)
		return res, fmt.Errorf("%w: %v", ErrRolledBack, err)
	}
	res.Health = "serving, reporting " + version
	log("  " + res.Health)
	if !strings.Contains(version, m.Version) {
		// Not a failure: a station can be running a build whose version string
		// is not the manifest's. Worth saying out loud all the same.
		log(fmt.Sprintf("  note: it reports %q, the release said %q", version, m.Version))
	}
	return res, nil
}

func rollback(ctx context.Context, o Options, previous string, log func(string)) {
	log("putting the previous binary back")
	if err := copyFile(previous, o.BinaryPath, 0o755); err != nil {
		log("ROLLBACK FAILED: " + err.Error())
		log("the previous binary is still at " + previous)
		return
	}
	if out, err := o.Runner(ctx, "systemctl", "restart", o.ServiceUnit); err != nil {
		log("ROLLBACK RESTART FAILED: " + err.Error() + " " + strings.TrimSpace(string(out)))
		return
	}
	log("previous binary restored and restarted")
}

func fetchArtifact(ctx context.Context, o Options, a release.Artifact, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "psgnssd-updater")
	resp, err := o.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download release: HTTP %s", resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	limit := a.Size
	if limit <= 0 || limit > MaxArtifact {
		limit = MaxArtifact
	}
	// One byte more than the manifest promises, so a body that is too long is
	// caught here rather than looking like a hash mismatch.
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n != a.Size {
		return fmt.Errorf("release is %d bytes, the signed manifest says %d", n, a.Size)
	}
	return f.Sync()
}

func verifyFile(path string, a release.Artifact) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, a.SHA256) {
		return fmt.Errorf("the download does not match the signed manifest: sha256 %s, expected %s", got, a.SHA256)
	}
	return nil
}

// waitHealthy polls the daemon's own status endpoint until it answers or the
// timeout runs out. "The unit is active" is not the check: systemd calls a
// process that started and is about to exit active too.
func waitHealthy(ctx context.Context, o Options) (string, error) {
	_, port, err := net.SplitHostPort(o.WebListen)
	if err != nil {
		return "", fmt.Errorf("web listen address %q: %w", o.WebListen, err)
	}
	url := "http://127.0.0.1:" + port + "/api/status"
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(o.HealthTimeout)
	var last error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
		resp, err := client.Get(url)
		if err != nil {
			last = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			last = fmt.Errorf("status endpoint answered HTTP %s", resp.Status)
			continue
		}
		var status struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(body, &status); err != nil {
			last = fmt.Errorf("status endpoint returned something that is not status JSON")
			continue
		}
		if status.Version == "" {
			last = errors.New("status endpoint reported no version")
			continue
		}
		return status.Version, nil
	}
	if last == nil {
		last = errors.New("no answer")
	}
	return "", fmt.Errorf("the new release did not serve within %s: %w", o.HealthTimeout, last)
}

func copyFile(from, to string, mode os.FileMode) error {
	body, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	tmp := to + ".tmp"
	if err := os.WriteFile(tmp, body, mode); err != nil {
		return err
	}
	return os.Rename(tmp, to)
}

// lock takes an exclusive lock for the duration of an update.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("another update is running (%s exists). If nothing is, remove it", path)
		}
		return nil, err
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Close()
	return func() { os.Remove(path) }, nil
}
