// Package boardprofile describes receiver-board families independently of the
// wire drivers. It lets setup and the web UI state exactly which boards are
// supported without ever guessing that one vendor's control protocol is safe
// to send to another receiver.
package boardprofile

import (
	"fmt"
	"strings"
)

const (
	DriverUBXVal  = "ubx-valget-valset"
	DriverUnicore = "unicore-cli"
	DriverStub    = "not-implemented"
)

// Profile is one supported or deliberately stubbed receiver family.
type Profile struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Receiver    string   `json:"receiver"`
	Vendor      string   `json:"vendor"`
	Protocol    string   `json:"protocol"`
	Driver      string   `json:"driver"`
	Implemented bool     `json:"implemented"`
	FormFactors []string `json:"form_factors"`
	Note        string   `json:"note"`
	// NativeEphemeris means the receiver emits RTCM ephemeris itself, so the
	// caster forwards it rather than synthesising it from UBX subframes.
	NativeEphemeris bool `json:"native_ephemeris"`
}

// UBX and Unicore report whether the profile is driven over that protocol.
func (p Profile) UBX() bool     { return p.Implemented && p.Driver == DriverUBXVal }
func (p Profile) Unicore() bool { return p.Implemented && p.Driver == DriverUnicore }

var profiles = []Profile{
	{ID: "simplertk4-optimum", Name: "simpleRTK4 Optimum", Receiver: "ZED-X20P",
		Vendor: "ArduSimple / u-blox", Protocol: "UBX", Driver: DriverUBXVal, Implemented: true,
		FormFactors: []string{"standard", "Micro", "M.2", "mPCIe"},
		Note:        "Full configuration, telemetry, fixed-base and message-rate support."},
	{ID: "simplertk2b", Name: "simpleRTK2B", Receiver: "ZED-F9P",
		Vendor: "ArduSimple / u-blox", Protocol: "UBX", Driver: DriverStub,
		FormFactors: []string{"standard", "Micro", "M.2", "mPCIe"},
		Note:        "Profile metadata only; safe configuration-key coverage still needs hardware validation."},
	{ID: "simplertk3b-pro", Name: "simpleRTK3B Pro", Receiver: "mosaic-X5",
		Vendor: "ArduSimple / Septentrio", Protocol: "SBF / CLI", Driver: DriverStub,
		FormFactors: []string{"standard", "Micro", "mPCIe"},
		Note:        "Profile stub; Septentrio control and telemetry drivers are not implemented."},
	{ID: "simplertk3b-budget", Name: "simpleRTK3B Budget", Receiver: "UM980",
		Vendor: "ArduSimple / Unicore", Protocol: "Unicore binary / CLI", Driver: DriverUnicore, Implemented: true,
		FormFactors: []string{"standard"}, NativeEphemeris: true,
		Note: "Command console, telemetry, fixed-base and message-output support. " +
			"Verified on firmware R4.10Build20739."},
}

// All returns an independent copy safe for JSON responses and callers.
func All() []Profile {
	out := make([]Profile, len(profiles))
	copy(out, profiles)
	for i := range out {
		out[i].FormFactors = append([]string(nil), out[i].FormFactors...)
	}
	return out
}

func Lookup(id string) (Profile, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range profiles {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// Resolve accepts an explicit profile ID or derives it from the configured
// receiver model for backward-compatible existing installations.
func Resolve(id, model string) (Profile, error) {
	if strings.TrimSpace(id) != "" && !strings.EqualFold(strings.TrimSpace(id), "auto") {
		p, ok := Lookup(id)
		if !ok {
			return Profile{}, fmt.Errorf("unknown receiver profile %q", id)
		}
		if model != "" && !strings.EqualFold(strings.TrimSpace(model), p.Receiver) {
			return Profile{}, fmt.Errorf("receiver profile %q expects %s, configured model is %s", p.ID, p.Receiver, model)
		}
		return p, nil
	}
	for _, p := range profiles {
		if strings.EqualFold(strings.TrimSpace(model), p.Receiver) {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("no board profile matches receiver model %q", model)
}
