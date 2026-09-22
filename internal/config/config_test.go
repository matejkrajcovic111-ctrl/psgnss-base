package config

import (
	"path/filepath"
	"testing"
)

func validTestConfig() *Config {
	carrier := 3
	return &Config{
		Station:  Station{Name: "Test", Position: Position{Mode: "fixed", Format: "llh", Latitude: 48, Longitude: 17, Height: 250}},
		Receiver: Receiver{Device: "/device/not-required-for-validation", Baud: 460800, Model: "ZED-X20P"},
		Hub:      Hub{Input: "serial"},
		Caster: Caster{NtripV1: true, FormatString: "RTCM 3.2", Carrier: 3,
			Mountpoint: []Mountpoint{{Name: "TEST_MSM7", SourceID: 1, Carrier: &carrier,
				NavSystem: "GPS+GAL", Auth: "B", Fee: "N",
				Messages: []MessageSel{{Type: 1006, Interval: 10}, {Type: 1077, Interval: 1}}}}},
		RINEX: RINEX{Frequencies: 4}, Telemetry: Telemetry{DBPath: filepath.Join("state", "test.db")},
	}
}

func TestSaveRoundTripManagedMountpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "psgnss.toml")
	want := validTestConfig()
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Caster.Mountpoint) != 1 || got.Caster.Mountpoint[0].Name != "TEST_MSM7" ||
		got.Caster.Mountpoint[0].Carrier == nil || *got.Caster.Mountpoint[0].Carrier != 3 {
		t.Fatalf("mountpoint did not survive round trip: %+v", got.Caster.Mountpoint)
	}
}

func TestMountpointValidation(t *testing.T) {
	c := validTestConfig()
	c.Caster.Mountpoint[0].Name = "bad/name"
	if err := c.Validate(); err == nil {
		t.Fatal("invalid mountpoint name accepted")
	}
	c = validTestConfig()
	c.Caster.Mountpoint[0].MSMDetail = "bad;injection"
	if err := c.Validate(); err == nil {
		t.Fatal("sourcetable delimiter accepted")
	}
}

func TestWebPreferenceValidation(t *testing.T) {
	c := validTestConfig()
	c.Web.DisplayHiddenConstellations = []string{"SBAS", "GLONASS"}
	c.Web.DiagnosticsIntervalMinutes = 15
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Web.DisplayHiddenConstellations = []string{"SBAS", "SBAS"}
	if err := c.Validate(); err == nil {
		t.Fatal("duplicate default display constellation accepted")
	}
	c = validTestConfig()
	c.Web.DiagnosticsIntervalMinutes = 1441
	if err := c.Validate(); err == nil {
		t.Fatal("out-of-range diagnosis interval accepted")
	}
}

func TestReceiverProfileValidation(t *testing.T) {
	c := validTestConfig()
	c.Receiver.Profile = "simplertk4-optimum"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Receiver.Profile = "simplertk3b-pro"
	if err := c.Validate(); err == nil {
		t.Fatal("profile/model mismatch accepted")
	}
}
