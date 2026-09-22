package store

import (
	"path/filepath"
	"testing"
)

func TestIntegritySettingsAndRunRoundTrip(t *testing.T) {
	s, kr := testStore(t)
	v, err := s.IntegritySettings()
	if err != nil {
		t.Fatal(err)
	}
	// A fresh install ships no reference network: the operator supplies one that
	// covers this area. Naming a specific provider in the default read like a
	// requirement.
	if v.Enabled || v.Schedule != "03:00" || v.Host != "" || v.Mountpoint != "" {
		t.Fatalf("bad defaults: %+v", v)
	}
	v.Enabled, v.Username = true, "fixture"
	v.PasswordEnc, v.PasswordNonce, err = kr.Seal("fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIntegritySettings(v); err != nil {
		t.Fatal(err)
	}
	got, err := s.IntegritySettings()
	if err != nil {
		t.Fatal(err)
	}
	plain, err := kr.Open(got.PasswordEnc, got.PasswordNonce)
	if err != nil || plain != "fixture-password" || !got.Enabled {
		t.Fatalf("settings did not round-trip: %q %v %+v", plain, err, got)
	}
	horizontal, vertical := 12.0, 18.0
	if err := s.SaveIntegrityRun(IntegrityRun{StartedAt: 1, FinishedAt: 2, Status: "pass", Detail: "ok", Solution: "fixed", Samples: 30, HorizontalMM: &horizontal, VerticalMM: &vertical}); err != nil {
		t.Fatal(err)
	}
	run, err := s.LatestIntegrityRun()
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.Status != "pass" || run.HorizontalMM == nil || *run.HorizontalMM != 12 {
		t.Fatalf("run did not round-trip: %+v", run)
	}
}

// v6 removes the original deployment's reference network from an installation
// that never configured one, and must leave a configured monitor alone -- even
// one that legitimately uses that same network.
func TestV6ClearsOnlyTheUnconfiguredLegacyReferenceNetwork(t *testing.T) {
	const legacyHost, legacyMount = "skpos.gku.sk:2101", "SKPOS_CM_32_MSM7"
	cases := map[string]struct {
		configure  bool
		wantHost   string
		wantAction string
	}{
		"never configured": {false, "", "cleared"},
		"in use":           {true, legacyHost, "kept"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "v5.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			// Rewind to a v5 database still holding the old defaults.
			if _, err := s.db.Exec(`UPDATE integrity_settings SET host=?,mountpoint=? WHERE id=1;
				DELETE FROM schema_version; INSERT INTO schema_version(version,applied_at) VALUES(5,0)`,
				legacyHost, legacyMount); err != nil {
				t.Fatal(err)
			}
			if tc.configure {
				v, err := s.IntegritySettings()
				if err != nil {
					t.Fatal(err)
				}
				v.Enabled, v.Username = true, "operator"
				v.PasswordEnc, v.PasswordNonce = []byte("sealed"), []byte("nonce")
				if err := s.SaveIntegritySettings(v); err != nil {
					t.Fatal(err)
				}
			}
			s.Close()

			s, err = Open(path) // applies v6
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			got, err := s.IntegritySettings()
			if err != nil {
				t.Fatal(err)
			}
			if got.Host != tc.wantHost {
				t.Errorf("host %q should have been %s, got %q", legacyHost, tc.wantAction, got.Host)
			}
		})
	}
}
