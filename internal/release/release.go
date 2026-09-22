// Package release is the signed-release format the updater trusts.
//
// # What is signed, and why it is the manifest
//
// A release is a manifest describing one or more binaries, and an Ed25519
// signature over the manifest's exact bytes. The binaries themselves are not
// signed individually: the manifest names each one's SHA-256, so a valid
// signature over the manifest plus a matching hash is the same guarantee with
// one signature to verify instead of several, and no question about which
// bytes were covered.
//
// The two are carried together in an Envelope so there is never a manifest
// without its signature, or a signature whose subject has to be guessed. The
// manifest travels as opaque bytes inside it and is parsed only after the
// signature verifies, so a forged document is never decoded at all.
//
// # The public key is compiled in
//
// A release can only be trusted by a binary that already knows the key, so the
// verifying key is a constant in this package and the signing key lives on the
// maintainer's machine. That has a consequence worth stating plainly: lose the
// signing key and no already-installed station can ever be updated in place
// again --- each one has to be given a new binary by hand, carrying the new
// public key, before automatic updates work again.
package release

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// Manifest describes one release.
type Manifest struct {
	Version   string     `json:"version"`
	Commit    string     `json:"commit"`
	Released  time.Time  `json:"released"`
	Notes     string     `json:"notes,omitempty"`
	Artifacts []Artifact `json:"artifacts"`
	// Dependencies is the third-party inventory of this release. Carrying it
	// in the manifest is what lets a station say what a newer release would
	// change about its dependencies before it takes it.
	Dependencies []Dependency `json:"dependencies,omitempty"`
}

// Artifact is one built binary.
type Artifact struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Dependency is one outside work this release carries or calls.
type Dependency struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Licence string `json:"licence"`
	// Kind is "go", "browser" or "external": a module linked in, a file the
	// browser loads, or a program invoked as a process.
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

// Envelope is what is published and fetched: a signature and the exact bytes
// it covers.
type Envelope struct {
	// Format guards against a future change being read as this one.
	Format    int    `json:"format"`
	Signature string `json:"signature"` // base64, Ed25519 over Manifest
	Manifest  string `json:"manifest"`  // base64 of the manifest JSON
}

// Format is the current envelope version.
const Format = 1

var (
	ErrBadSignature = errors.New("release signature does not verify against this build's public key")
	ErrNoArtifact   = errors.New("release has no artifact for this platform")
)

// Sign produces an envelope. It is used by the release tool, never on a
// station.
func Sign(m *Manifest, priv ed25519.PrivateKey) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	// Indented, because a human will read this in a release page and a diff of
	// two releases should be legible. The bytes signed are exactly these.
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	env := Envelope{
		Format:    Format,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, body)),
		Manifest:  base64.StdEncoding.EncodeToString(body),
	}
	return json.MarshalIndent(env, "", "  ")
}

// Verify checks an envelope against a public key and returns the manifest it
// carries. The manifest is decoded only after the signature verifies.
func Verify(envelope []byte, pub ed25519.PublicKey) (*Manifest, error) {
	var env Envelope
	if err := json.Unmarshal(envelope, &env); err != nil {
		return nil, fmt.Errorf("parse release envelope: %w", err)
	}
	if env.Format != Format {
		return nil, fmt.Errorf("release envelope format %d, this build understands %d", env.Format, Format)
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}
	body, err := base64.StdEncoding.DecodeString(env.Manifest)
	if err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("this build has no usable release public key (%d bytes)", len(pub))
	}
	if !ed25519.Verify(pub, body, sig) {
		return nil, ErrBadSignature
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate refuses a manifest that could not be acted on. A signed document
// still has to make sense: the signature proves who wrote it, not that they
// were paying attention.
func (m *Manifest) Validate() error {
	var errs []error
	add := func(f string, v ...any) { errs = append(errs, fmt.Errorf(f, v...)) }
	if strings.TrimSpace(m.Version) == "" {
		add("release has no version")
	}
	if len(m.Artifacts) == 0 {
		add("release lists no artifacts")
	}
	seen := map[string]bool{}
	for i, a := range m.Artifacts {
		platform := a.GOOS + "/" + a.GOARCH
		if a.GOOS == "" || a.GOARCH == "" {
			add("artifact %d has no platform", i)
		} else if seen[platform] {
			add("release lists %s twice", platform)
		}
		seen[platform] = true
		if !strings.HasPrefix(a.URL, "https://") {
			// Plain HTTP would still be caught by the hash, but a release that
			// can be silently withheld or delayed by anyone on the path is not
			// something to normalise.
			add("artifact %s: URL must be https", platform)
		}
		if raw, err := hex.DecodeString(a.SHA256); err != nil || len(raw) != sha256.Size {
			add("artifact %s: sha256 must be 64 hex characters", platform)
		}
		if a.Size <= 0 {
			add("artifact %s: size must be positive", platform)
		}
	}
	return errors.Join(errs...)
}

// ArtifactFor returns the artifact for a platform.
func (m *Manifest) ArtifactFor(goos, goarch string) (Artifact, error) {
	for _, a := range m.Artifacts {
		if a.GOOS == goos && a.GOARCH == goarch {
			return a, nil
		}
	}
	return Artifact{}, fmt.Errorf("%w: %s/%s", ErrNoArtifact, goos, goarch)
}

// ArtifactForThisBuild returns the artifact matching the running binary.
func (m *Manifest) ArtifactForThisBuild() (Artifact, error) {
	return m.ArtifactFor(runtime.GOOS, runtime.GOARCH)
}
