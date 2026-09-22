// Command psgnss-release signs releases. It runs on the maintainer's machine
// and never on a station: stations only ever verify.
//
//	psgnss-release keygen [-key path]
//	psgnss-release sign -version v0.7.0 -commit abc1234 -base https://host/path/ file...
//	psgnss-release verify release.json
//
// The signing key is the root of trust for every station's automatic updates.
// Keep it off the station, back it up, and know what losing it costs: no
// already-installed station can be updated in place again until each one has
// been given a binary carrying the new public key by hand.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/psgnss/psgnss-base/internal/release"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `psgnss-release signs PSGNSS releases.

  keygen [-key PATH]                      create a signing key
  sign -version V -commit C -base URL F…  sign a release of the given binaries
  verify FILE                             check a release against this build's public key

`)
	os.Exit(2)
}

func defaultKeyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "psgnss-release.key"
	}
	return filepath.Join(home, ".ssh", "psgnss-release.key")
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	path := fs.String("key", defaultKeyPath(), "where to write the signing key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(*path); err == nil {
		return fmt.Errorf("refusing to overwrite the existing signing key at %s "+
			"(every station trusting it would have to be reinstalled by hand)", *path)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(*path, []byte(hex.EncodeToString(priv)+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Printf("Signing key written to %s (mode 0600). Back it up; it is not recoverable.\n\n", *path)
	fmt.Printf("Put this in internal/release/pubkey.go and rebuild:\n\n")
	fmt.Printf("    const publicKeyHex = %q\n\n", hex.EncodeToString(pub))
	fmt.Printf("Only stations running a build that carries it will accept releases signed\n")
	fmt.Printf("with this key.\n")
	return nil
}

func loadKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("signing key is not hex: %w", err)
	}
	if len(b) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing key is %d bytes, want %d", len(b), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(b), nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	key := fs.String("key", defaultKeyPath(), "signing key")
	version := fs.String("version", "", "release version, e.g. v0.7.0")
	commit := fs.String("commit", "", "commit this was built from")
	base := fs.String("base", "", "base URL the artifacts will be published under (https)")
	notes := fs.String("notes", "", "one line about the release")
	out := fs.String("out", "release.json", "where to write the signed envelope")
	deps := fs.String("deps", "", "optional JSON file of the dependency inventory (psgnssd --update-inventory)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *version == "" || len(fs.Args()) == 0 {
		return fmt.Errorf("need -version and at least one binary")
	}
	priv, err := loadKey(*key)
	if err != nil {
		return err
	}

	m := &release.Manifest{
		Version: *version, Commit: *commit, Released: time.Now().UTC().Truncate(time.Second),
		Notes: *notes,
	}
	for _, path := range fs.Args() {
		goos, goarch, err := platformOf(path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		m.Artifacts = append(m.Artifacts, release.Artifact{
			GOOS: goos, GOARCH: goarch,
			URL:    strings.TrimSuffix(*base, "/") + "/" + filepath.Base(path),
			Size:   int64(len(body)),
			SHA256: hex.EncodeToString(sum[:]),
		})
		fmt.Printf("%s  %s  %s/%s  %d bytes\n", hex.EncodeToString(sum[:])[:16], filepath.Base(path), goos, goarch, len(body))
	}
	if *deps != "" {
		raw, err := os.ReadFile(*deps)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &m.Dependencies); err != nil {
			return fmt.Errorf("parse %s: %w", *deps, err)
		}
	}

	envelope, err := release.Sign(m, priv)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, envelope, 0o644); err != nil {
		return err
	}
	fmt.Printf("\nSigned %s -> %s\n", *version, *out)
	fmt.Printf("Publish %s and the binaries under %s\n", *out, *base)
	return nil
}

// platformOf reads the platform out of the artifact's filename, which is how
// the build names them: psgnssd-linux-arm64.
func platformOf(path string) (goos, goarch string, err error) {
	parts := strings.Split(filepath.Base(path), "-")
	if len(parts) < 3 {
		return "", "", fmt.Errorf("cannot tell the platform from %q; name it like psgnssd-linux-arm64", filepath.Base(path))
	}
	return parts[len(parts)-2], parts[len(parts)-1], nil
}

func verify(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("verify takes one file")
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	m, err := release.Verify(raw, release.PublicKey())
	if err != nil {
		return err
	}
	fmt.Printf("Signature verifies against this build's public key.\n\n")
	fmt.Printf("  version   %s\n  commit    %s\n  released  %s\n",
		m.Version, m.Commit, m.Released.Format(time.RFC3339))
	if m.Notes != "" {
		fmt.Printf("  notes     %s\n", m.Notes)
	}
	for _, a := range m.Artifacts {
		fmt.Printf("  %s/%-8s %s  %s\n", a.GOOS, a.GOARCH, a.SHA256[:16], a.URL)
	}
	fmt.Printf("  %d dependencies recorded\n", len(m.Dependencies))
	return nil
}
