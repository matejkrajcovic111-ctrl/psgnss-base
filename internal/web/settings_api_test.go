package web

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/psgnss/psgnss-base/internal/config"
)

func signed32(b []byte) int32 { return int32(binary.LittleEndian.Uint32(b)) }

func TestPositionKVsExactReceiverRepresentation(t *testing.T) {
	in := positionInput{Latitude: 48.12345678, Longitude: 17.98765432, Height: 287.275}
	kvs, got, err := positionKVs(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(kvs) != 8 {
		t.Fatalf("got %d keys, want 8", len(kvs))
	}
	values := map[uint32][]byte{}
	for _, kv := range kvs {
		values[kv.Key] = kv.Value
	}
	lat := float64(signed32(values[keyTModeLat]))*1e-7 + float64(int8(values[keyTModeLatHP][0]))*1e-9
	lon := float64(signed32(values[keyTModeLon]))*1e-7 + float64(int8(values[keyTModeLonHP][0]))*1e-9
	h := float64(signed32(values[keyTModeHeight]))*.01 + float64(int8(values[keyTModeHeightHP][0]))*.0001
	if math.Abs(lat-in.Latitude) > 5e-10 || math.Abs(lon-in.Longitude) > 5e-10 || math.Abs(h-in.Height) > 0.00005 {
		t.Fatalf("receiver representation %.9f %.9f %.4f, want %.9f %.9f %.4f", lat, lon, h, in.Latitude, in.Longitude, in.Height)
	}
	if got.Latitude != lat || got.Longitude != lon || got.Height != h {
		t.Fatalf("persisted position differs from receiver: %+v", got)
	}
}

func TestPositionKVsRejectsInvalidCoordinates(t *testing.T) {
	for _, p := range []positionInput{{Latitude: 91}, {Longitude: -181}, {Height: 100001}, {Latitude: math.NaN()}} {
		if _, _, err := positionKVs(p); err == nil {
			t.Fatalf("accepted invalid position %+v", p)
		}
	}
}

func settingsTestConfig() *config.Config {
	carrier := 3
	return &config.Config{Station: config.Station{Name: "Test", Position: config.Position{Mode: "fixed", Format: "llh", Latitude: 48, Longitude: 17, Height: 250}},
		Receiver: config.Receiver{Device: "/not-required", Baud: 460800, Model: "ZED-X20P"}, Hub: config.Hub{Input: "serial"},
		Caster: config.Caster{NtripV1: true, FormatString: "RTCM 3.2", Carrier: 3, Operator: "test", Country: "SVK",
			Mountpoint: []config.Mountpoint{{Name: "OLD", SourceID: 1, Carrier: &carrier, NavSystem: "GPS", Messages: []config.MessageSel{{Type: 1077, Interval: 1}}}}},
		RINEX: config.RINEX{Frequencies: 4}, Telemetry: config.Telemetry{DBPath: "state/test.db"}}
}

func TestHandleSettingsMountpointsPersistsValidatedConfig(t *testing.T) {
	cfg := settingsTestConfig()
	path := filepath.Join(t.TempDir(), "psgnss.toml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	s := New(&Server{Cfg: cfg, ConfigPath: path})
	body := map[string]any{"mountpoints": []map[string]any{{"name": "NEW_MSM7", "enabled": true, "source_id": 2,
		"format": "RTCM 3.2", "carrier": 3, "nav_system": "GPS+GAL", "network": "test", "country": "SVK",
		"nmea": 0, "solution": 0, "generator": "PSGNSS", "compress": "none", "auth": "B", "fee": "N", "bitrate": 7000, "msm_detail": "",
		"messages": []map[string]int{{"type": 1006, "interval": 10}, {"type": 1077, "interval": 1}}}}}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/settings/mountpoints", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	s.handleSettingsMountpoints(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Caster.Mountpoint) != 1 || got.Caster.Mountpoint[0].Name != "NEW_MSM7" || got.Caster.Mountpoint[0].Bitrate != 7000 {
		t.Fatalf("not persisted: %+v", got.Caster.Mountpoint)
	}
}

func TestHandleSettingsMountpointsRejectsSourcetableInjection(t *testing.T) {
	cfg := settingsTestConfig()
	s := New(&Server{Cfg: cfg, ConfigPath: filepath.Join(t.TempDir(), "psgnss.toml")})
	body := `{"mountpoints":[{"name":"BAD","enabled":true,"source_id":1,"format":"RTCM;evil","carrier":3,"nav_system":"GPS","network":"test","country":"SVK","nmea":0,"solution":0,"generator":"PSGNSS","compress":"none","auth":"B","fee":"N","bitrate":0,"msm_detail":"","messages":[{"type":1077,"interval":1}]}]}`
	req := httptest.NewRequest(http.MethodPut, "/api/settings/mountpoints", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.handleSettingsMountpoints(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSettingsGeneralPersistsOperationalValues(t *testing.T) {
	cfg := settingsTestConfig()
	cfg.Caster.Listen = ":2101"
	cfg.Web.Listen = ":8090"
	cfg.Archive = config.Archive{MountPoint: "/mnt/archive", SpoolDir: "/var/old", RetentionDays: 365, SyncEverySec: 60}
	cfg.Telemetry.RetentionDays = 7
	cfg.Logging.Level = "info"
	path := filepath.Join(t.TempDir(), "psgnss.toml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	s := New(&Server{Cfg: cfg, ConfigPath: path})
	body := `{"station_name":"Station B","station_id":4095,"antenna":"ANT","receiver_description":"RX","caster_listen":":2201","proxy_listen":"","web_listen":":8091","hub_listeners":[],"archive_mount":"/mnt/archive","archive_spool":"/var/spool/gnss","archive_retention_days":730,"archive_sync_seconds":30,"telemetry_retention_days":14,"logging_level":"warn","display_hidden_constellations":["SBAS","GLONASS"],"diagnostics_auto":true,"diagnostics_interval_minutes":15}`
	req := httptest.NewRequest(http.MethodPut, "/api/settings/general", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.handleSettingsGeneral(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Station.Name != "Station B" || got.Station.StationID != 0 || got.Caster.Listen != ":2201" || got.Archive.RetentionDays != 730 || got.Telemetry.RetentionDays != 14 || got.Logging.Level != "warn" || !got.Web.DiagnosticsAuto || got.Web.DiagnosticsIntervalMinutes != 15 || len(got.Web.DisplayHiddenConstellations) != 2 {
		t.Fatalf("not persisted: %+v", got)
	}
}

func TestSettingsGeneralOmitsGuardedStationID(t *testing.T) {
	s := New(&Server{Cfg: settingsTestConfig()})
	rec := httptest.NewRecorder()
	s.handleSettings(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	general := body["general"].(map[string]any)
	station := body["station"].(map[string]any)
	if _, exists := general["station_id"]; exists {
		t.Fatal("general form still receives guarded station_id")
	}
	if _, exists := station["station_id"]; !exists {
		t.Fatal("station details lost station_id")
	}
	profiles, ok := body["board_profiles"].([]any)
	if !ok || len(profiles) != 4 || general["receiver_profile"] != "simplertk4-optimum" {
		t.Fatalf("board profile catalogue missing: general=%+v profiles=%+v", general, body["board_profiles"])
	}
}

func TestSettingsGeneralRejectsUnimplementedBoardDriver(t *testing.T) {
	cfg := settingsTestConfig()
	s := New(&Server{Cfg: cfg})
	body := `{"station_name":"Test","antenna":"ANT","receiver_description":"RX","receiver_profile":"simplertk3b-pro","caster_listen":":2101","proxy_listen":"","web_listen":":8090","hub_listeners":[],"archive_mount":"/mnt/archive","archive_spool":"/var/spool","archive_retention_days":1,"archive_sync_seconds":60,"telemetry_retention_days":7,"logging_level":"info","display_hidden_constellations":[],"diagnostics_auto":false,"diagnostics_interval_minutes":60}`
	rec := httptest.NewRecorder()
	s.handleSettingsGeneral(rec, httptest.NewRequest(http.MethodPut, "/api/settings/general", bytes.NewBufferString(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestStationIDUsesReceiverDF003OutputKey(t *testing.T) {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, 4095)
	if keyRTCMStationID != 0x30090001 || binary.LittleEndian.Uint16(b) != 4095 {
		t.Fatal("wrong station ID representation")
	}
}
