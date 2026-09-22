package setup

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SerialDevice is a candidate receiver found on the machine.
type SerialDevice struct {
	// Path is always the by-id symlink when one exists. /dev/ttyACM0 is
	// assignment-order dependent: plug in a second USB serial device, or
	// reboot with one attached, and the name moves to another receiver. A
	// station that addresses its receiver by ttyACM number will one day
	// configure something else.
	Path string
	// Target is what the symlink resolves to, shown so the operator can
	// recognise the device they just plugged in.
	Target string
	// Likely marks a device whose by-id name names a GNSS vendor.
	Likely bool
}

var gnssHints = []string{"u-blox", "ublox", "septentrio", "unicore", "ardusimple", "gnss", "gps"}

// FindSerialDevices lists the serial ports worth offering, by-id first.
func FindSerialDevices(root string) []SerialDevice {
	var out []SerialDevice
	seen := map[string]bool{}

	byID := filepath.Join(root, "/dev/serial/by-id")
	if entries, err := os.ReadDir(byID); err == nil {
		for _, e := range entries {
			link := filepath.Join("/dev/serial/by-id", e.Name())
			target, _ := os.Readlink(filepath.Join(root, link))
			target = filepath.Clean(filepath.Join("/dev/serial/by-id", target))
			lower := strings.ToLower(e.Name())
			likely := false
			for _, hint := range gnssHints {
				if strings.Contains(lower, hint) {
					likely = true
					break
				}
			}
			out = append(out, SerialDevice{Path: link, Target: target, Likely: likely})
			seen[target] = true
		}
	}
	// Bare device nodes, for a machine with no by-id links at all. They are
	// offered last and never preferred.
	if entries, err := os.ReadDir(filepath.Join(root, "/dev")); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "ttyACM") && !strings.HasPrefix(name, "ttyUSB") {
				continue
			}
			path := filepath.Join("/dev", name)
			if seen[path] {
				continue
			}
			out = append(out, SerialDevice{Path: path, Target: path})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Likely != out[j].Likely {
			return out[i].Likely
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// IsInstalled reports whether a station is already configured here.
func IsInstalled(root string, p Paths) bool {
	_, err := os.Stat(filepath.Join(root, p.ConfigFile()))
	return err == nil
}
