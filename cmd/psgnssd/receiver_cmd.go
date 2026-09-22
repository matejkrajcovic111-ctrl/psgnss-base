package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/hub"
	"github.com/psgnss/psgnss-base/internal/receiver"
)

// openReceiver takes exclusive ownership of the serial port. Only one process
// can hold it, so the caller must have stopped anything else using it.
func openReceiver(cfg *config.Config) (*os.File, error) {
	src := hub.SerialSource{Device: cfg.Receiver.Device, Baud: cfg.Receiver.Baud}
	rc, err := src.Open()
	if err != nil {
		return nil, err
	}
	f, ok := rc.(*os.File)
	if !ok {
		rc.Close()
		return nil, fmt.Errorf("serial source did not yield a file")
	}
	return f, nil
}

// receiverInfo prints identification and a configuration summary. Read-only:
// it sends polls, never VALSET.
func receiverInfo(cfg *config.Config) error {
	f, err := openReceiver(cfg)
	if err != nil {
		return err
	}
	defer f.Close()
	s := receiver.NewSession(f)
	s.Timeout = 4 * time.Second

	v, err := s.Version()
	if err != nil {
		return fmt.Errorf("MON-VER: %w", err)
	}
	fmt.Println("Receiver identification")
	fmt.Printf("  model         %s\n", v.Model())
	fmt.Printf("  firmware      %s\n", v.Firmware())
	fmt.Printf("  protocol      %s\n", v.Protver())
	fmt.Printf("  sw / hw       %s / %s\n", v.SwVersion, v.HwVersion)
	cons := v.Constellations()
	fmt.Printf("  constellations %v\n", cons)
	hasGlonass := false
	for _, c := range cons {
		if c == "GLO" {
			hasGlonass = true
		}
	}
	fmt.Printf("  GLONASS supported by this firmware: %v\n", hasGlonass)

	if cfg.Receiver.VerifyModel && v.Model() != "" && v.Model() != cfg.Receiver.Model {
		return fmt.Errorf("config expects model %q but receiver reports %q",
			cfg.Receiver.Model, v.Model())
	}

	// Groups that matter operationally.
	for _, g := range []struct {
		id   byte
		name string
	}{
		{0x03, "CFG-TMODE (base position)"},
		{0x09, "CFG-RTCM"},
		{0x31, "CFG-SIGNAL (constellations)"},
		{0x21, "CFG-RATE"},
	} {
		kv, err := s.ValGet(receiver.LayerRAM, []uint32{receiver.GroupWildcard(g.id)})
		if err != nil {
			fmt.Printf("\n%s: read failed: %v\n", g.name, err)
			continue
		}
		fmt.Printf("\n%s -- %d keys\n", g.name, len(kv))
		for _, k := range receiver.SortedKeys(kv) {
			fmt.Printf("    0x%08X = %x\n", k, kv[k])
		}
	}
	return nil
}

// receiverRevertTest proves the auto-revert path end to end against real
// hardware, without ever committing. It writes one benign key to RAM, verifies
// the read-back, lets the timer expire, and confirms the original value is
// restored. Flash is never touched.
func receiverRevertTest(cfg *config.Config, key uint32, newVal []byte, wait time.Duration) error {
	f, err := openReceiver(cfg)
	if err != nil {
		return err
	}
	defer f.Close()
	s := receiver.NewSession(f)
	s.Timeout = 4 * time.Second

	before, err := s.ValGet(receiver.LayerRAM, []uint32{key})
	if err != nil {
		return err
	}
	orig, ok := before[key]
	if !ok {
		return fmt.Errorf("key 0x%08X not readable", key)
	}
	fmt.Printf("auto-revert test on 0x%08X\n  initial RAM value : %x\n", key, orig)

	txn, err := s.Apply([]receiver.KV{{Key: key, Value: newVal}}, wait, slog.Default())
	if err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	for _, c := range txn.Changes() {
		fmt.Printf("  applied           : %x -> %x (verified=%v)\n", c.Old, c.New, c.Verified)
	}
	fmt.Printf("  NOT committing; waiting %s for auto-revert...\n", wait)

	select {
	case <-txn.Reverted:
		fmt.Println("  auto-revert fired")
	case <-time.After(wait + 10*time.Second):
		return fmt.Errorf("auto-revert did not fire within %s", wait+10*time.Second)
	}

	after, err := s.ValGet(receiver.LayerRAM, []uint32{key})
	if err != nil {
		return err
	}
	fmt.Printf("  final RAM value   : %x\n", after[key])
	if string(after[key]) != string(orig) {
		return fmt.Errorf("RECEIVER NOT RESTORED: was %x, now %x", orig, after[key])
	}
	fmt.Println("  RESTORED to original value. Flash was never written.")
	return nil
}
