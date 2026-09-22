package backup

import (
	"strings"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/config"
)

// A configuration a restore would actually accept: Open validates it, because
// it goes straight into the configuration file.
func validConfig() *config.Config {
	carrier := 3
	return &config.Config{
		Station:  config.Station{Name: "Example", Position: config.Position{Mode: "fixed", Format: "llh", Latitude: 48, Longitude: 17, Height: 250}},
		Receiver: config.Receiver{Device: "/dev/ttyACM0", Baud: 460800, Model: "ZED-X20P"},
		Hub:      config.Hub{Input: "serial"},
		Caster: config.Caster{NtripV1: true, FormatString: "RTCM 3.2", Carrier: 3,
			Mountpoint: []config.Mountpoint{{Name: "TEST_MSM7", SourceID: 1, Carrier: &carrier,
				NavSystem: "GPS+GAL", Auth: "B", Fee: "N",
				Messages: []config.MessageSel{{Type: 1006, Interval: 10}, {Type: 1077, Interval: 1}}}}},
		RINEX: config.RINEX{Frequencies: 4}, Telemetry: config.Telemetry{DBPath: "state/test.db"},
	}
}

func sample() *Document {
	cfg := validConfig()
	return &Document{Created: time.Now().UTC(), Station: "Example", AppVersion: "test",
		Config: cfg,
		Admins: []Admin{{Username: "matej", PasswordHash: "argon2id$1$65536$4$aa$bb", CreatedAt: 100}},
		Users: []User{{Username: "rover", Password: "rover-secret", ConnectionLimit: 3,
			Enabled: true, Mountpoints: []string{"Example_MSM7"}, IPRules: []string{"192.0.2.0/24"}}},
		Integrity:   &Integrity{Enabled: true, Schedule: "03:00", Username: "op", Password: "ref-secret"},
		PushTargets: []PushTarget{{Name: "remote", Host: "caster.example:2101", Password: "push-secret"}},
	}
}

func TestRoundTripKeepsEverySetting(t *testing.T) {
	blob, err := Seal(sample(), "a-long-enough-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	// The credentials must not be findable in the file: that is the whole point
	// of the envelope.
	for _, secret := range []string{"rover-secret", "ref-secret", "push-secret", "Example"} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("%q appears in the sealed file in the clear", secret)
		}
	}
	got, err := Open(blob, "a-long-enough-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Users) != 1 || got.Users[0].Password != "rover-secret" ||
		len(got.Users[0].Mountpoints) != 1 || len(got.Users[0].IPRules) != 1 {
		t.Fatalf("NTRIP account did not survive: %+v", got.Users)
	}
	if got.Integrity == nil || got.Integrity.Password != "ref-secret" || !got.Integrity.Enabled {
		t.Fatalf("integrity settings did not survive: %+v", got.Integrity)
	}
	if len(got.PushTargets) != 1 || got.PushTargets[0].Password != "push-secret" {
		t.Fatalf("push-out target did not survive: %+v", got.PushTargets)
	}
	if got.Config == nil || got.Config.Station.Name != "Example" ||
		len(got.Config.Caster.Mountpoint) != 1 {
		t.Fatalf("configuration did not survive: %+v", got.Config)
	}
	if s := got.Summary(); s.Admins != 1 || s.Users != 1 || !s.HasConfig || s.Station != "Example" {
		t.Errorf("summary misreports the file: %+v", s)
	}
}

func TestWrongPassphraseAndTamperingAreBothRefused(t *testing.T) {
	blob, err := Seal(sample(), "a-long-enough-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(blob, "a-long-wrong-passphrase"); err == nil {
		t.Fatal("a wrong passphrase opened the file")
	}
	// One flipped bit anywhere in the ciphertext, and in the authenticated
	// header, must both fail.
	for _, at := range []int{9, 20, len(blob) - 1} {
		bad := append([]byte(nil), blob...)
		bad[at] ^= 0x01
		if _, err := Open(bad, "a-long-enough-passphrase"); err == nil {
			t.Errorf("a file altered at byte %d still opened", at)
		}
	}
	if _, err := Open([]byte("not a backup at all"), "a-long-enough-passphrase"); err == nil {
		t.Fatal("arbitrary bytes were accepted as a backup")
	}
}

// A short passphrase is the one way to get a file that looks encrypted and is
// not, so it is refused where it is chosen rather than warned about later.
func TestShortPassphraseIsRefused(t *testing.T) {
	if _, err := Seal(sample(), "short"); err == nil {
		t.Fatal("a five-character passphrase was accepted")
	}
}

// A backup carries a configuration straight into the config file on restore, so
// one this binary would refuse at startup must be refused here.
func TestInvalidConfigurationIsRefusedOnOpen(t *testing.T) {
	doc := sample()
	doc.Config.Receiver.Baud = 0
	blob, err := Seal(doc, "a-long-enough-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(blob, "a-long-enough-passphrase"); err == nil {
		t.Fatal("a backup with an invalid configuration opened without complaint")
	}
}

func TestFilenameIsSafeAndDated(t *testing.T) {
	at := time.Date(2026, 9, 22, 8, 30, 0, 0, time.UTC)
	if got := Filename("Example", at); got != "psgnssb-Example-20260922-0830.psbk" {
		t.Errorf("filename = %q", got)
	}
	// A station name is operator text; it must not be able to name a path.
	if got := Filename("../../etc/passwd", at); strings.ContainsAny(got, "/.\\") &&
		!strings.HasSuffix(got, ".psbk") {
		t.Errorf("filename = %q", got)
	}
	if got := Filename("../..", at); strings.Contains(got, "..") {
		t.Errorf("filename = %q keeps a traversal", got)
	}
}
