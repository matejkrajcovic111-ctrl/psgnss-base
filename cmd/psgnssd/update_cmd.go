package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/release"
	"github.com/psgnss/psgnss-base/internal/updater"
	"github.com/psgnss/psgnss-base/internal/version"
	"github.com/psgnss/psgnss-base/internal/web"
)

// inventory prints this build's third-party dependencies. The release tool
// folds the JSON form into a manifest, so a station can be told what a newer
// release would change about its dependencies before it takes it.
func inventory(cfg *config.Config, asJSON bool) error {
	convbin := ""
	if cfg != nil {
		convbin = cfg.RINEX.ConvbinPath
	}
	deps := release.Dependencies(web.AssetNames(), convbin)
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(deps)
	}
	fmt.Printf("psgnssd %s\n\n", version.String())
	byKind := map[string][]release.Dependency{}
	for _, d := range deps {
		byKind[d.Kind] = append(byKind[d.Kind], d)
	}
	titles := map[string]string{
		"go":       "Linked in",
		"browser":  "Loaded by the page",
		"external": "Run as a process",
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		fmt.Printf("%s\n", titles[kind])
		for _, d := range byKind[kind] {
			licence := d.Licence
			if licence == "" {
				licence = "licence not recorded"
			}
			fmt.Printf("  %-38s %-28s %s\n", d.Name, d.Version, licence)
		}
		fmt.Println()
	}
	return nil
}

func updaterOptions(cfg *config.Config, cfgPath string) updater.Options {
	binary := cfg.Update.BinaryPath
	if binary == "" {
		if self, err := os.Executable(); err == nil {
			binary = self
		}
	}
	return updater.Options{
		ManifestURL: cfg.Update.ManifestURL,
		BinaryPath:  binary,
		ConfigPath:  cfgPath,
		ServiceUnit: cfg.Update.ServiceUnit,
		WebListen:   cfg.Web.Listen,
		Log:         func(s string) { fmt.Println("  " + s) },
	}
}

// updateCheck reports what is available without changing anything.
func updateCheck(cfg *config.Config, cfgPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	opts := updaterOptions(cfg, cfgPath)
	m, err := updater.Check(ctx, opts)
	if err != nil {
		return explainUpdateError(err)
	}
	fmt.Printf("Installed  %s\n", version.String())
	fmt.Printf("Available  %s", m.Version)
	if m.Commit != "" {
		fmt.Printf(" (%s)", m.Commit)
	}
	fmt.Printf(", released %s\n", m.Released.Format("2006-01-02"))
	if m.Notes != "" {
		fmt.Printf("           %s\n", m.Notes)
	}
	a, err := m.ArtifactForThisBuild()
	if err != nil {
		return err
	}
	fmt.Printf("Artifact   %s, %d bytes, sha256 %s…\n", a.URL, a.Size, a.SHA256[:16])

	if strings.Contains(version.String(), m.Version) {
		fmt.Printf("\nThis station is already on %s.\n", m.Version)
		return nil
	}
	diffDependencies(release.Dependencies(web.AssetNames(), cfg.RINEX.ConvbinPath), m.Dependencies)
	fmt.Printf("\nInstall it with:  psgnssd --update --config %s\n", cfgPath)
	return nil
}

// diffDependencies says what a release would change about the outside works
// this station carries. Answering "what am I actually about to pull in" is the
// point of recording the inventory in the manifest at all.
func diffDependencies(have, want []release.Dependency) {
	if len(want) == 0 {
		fmt.Printf("\nThe release records no dependency inventory, so there is nothing to compare.\n")
		return
	}
	index := func(list []release.Dependency) map[string]release.Dependency {
		m := make(map[string]release.Dependency, len(list))
		for _, d := range list {
			m[d.Kind+"/"+d.Name] = d
		}
		return m
	}
	h, w := index(have), index(want)
	var lines []string
	for key, d := range w {
		switch old, ok := h[key]; {
		case !ok:
			lines = append(lines, fmt.Sprintf("  + %-34s %s", d.Name, d.Version))
		case old.Version != d.Version:
			lines = append(lines, fmt.Sprintf("  ~ %-34s %s -> %s", d.Name, old.Version, d.Version))
		}
	}
	for key, d := range h {
		if _, ok := w[key]; !ok {
			lines = append(lines, fmt.Sprintf("  - %-34s %s", d.Name, d.Version))
		}
	}
	if len(lines) == 0 {
		fmt.Printf("\nDependencies are unchanged.\n")
		return
	}
	sort.Strings(lines)
	fmt.Printf("\nDependency changes:\n")
	for _, l := range lines {
		fmt.Println(l)
	}
}

// updateApply fetches, verifies and installs. It is deliberately a command a
// person runs: nothing in this daemon updates itself on a timer.
func updateApply(cfg *config.Config, cfgPath string, yes bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	opts := updaterOptions(cfg, cfgPath)
	fmt.Println("Checking for a signed release…")
	m, err := updater.Check(ctx, opts)
	if err != nil {
		return explainUpdateError(err)
	}
	if strings.Contains(version.String(), m.Version) {
		fmt.Printf("Already on %s. Nothing to do.\n", m.Version)
		return nil
	}
	fmt.Printf("\n  from  %s\n  to    %s", version.String(), m.Version)
	if m.Notes != "" {
		fmt.Printf("\n  notes %s", m.Notes)
	}
	fmt.Printf("\n  this replaces %s and restarts %s\n\n", opts.BinaryPath, opts.ServiceUnit)

	if !yes {
		fmt.Print("Install it? Rovers will lose the stream for a few seconds (y/n): ")
		var answer string
		fmt.Scanln(&answer)
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Println("Nothing was changed.")
			return nil
		}
		fmt.Println()
	}

	res, err := updater.Apply(ctx, opts, m, version.String())
	if err != nil {
		if res != nil && errors.Is(err, updater.ErrRolledBack) {
			//lint:ignore ST1005 operator-facing guidance, printed verbatim
			return fmt.Errorf("%w\n\nThe station is back on %s. The release that failed is not "+
				"installed and %s holds the binary that is running.", err, res.From, res.PreviousPath)
		}
		return err
	}
	fmt.Printf("\nUpdated %s -> %s\n", res.From, res.To)
	fmt.Printf("Health: %s\n", res.Health)
	fmt.Printf("The previous binary is at %s. Go back with:\n", res.PreviousPath)
	fmt.Printf("    sudo install -m0755 %s %s && sudo systemctl restart %s\n",
		res.PreviousPath, opts.BinaryPath, opts.ServiceUnit)
	return nil
}

func explainUpdateError(err error) error {
	switch {
	case errors.Is(err, updater.ErrNoKey):
		//lint:ignore ST1005 operator-facing guidance, printed verbatim
		return fmt.Errorf("%w\n\nThis is a build made without a release signing key. Generate one with\n"+
			"  psgnss-release keygen\nput the public key in internal/release/pubkey.go and rebuild.", err)
	case errors.Is(err, updater.ErrNoManifest):
		//lint:ignore ST1005 operator-facing guidance, printed verbatim
		return fmt.Errorf("%w\n\nSet update.manifest_url in the configuration to the URL of the signed\n"+
			"release envelope. Until then this station is updated by hand.", err)
	case errors.Is(err, release.ErrBadSignature):
		//lint:ignore ST1005 operator-facing guidance, printed verbatim
		return fmt.Errorf("%w\n\nNothing was downloaded and nothing was changed. Either the release was not\n"+
			"signed with the key this build trusts, or what is being served is not the\n"+
			"release it claims to be.", err)
	}
	return err
}
