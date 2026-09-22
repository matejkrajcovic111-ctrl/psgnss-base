package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/integrity"
)

func TestIntegritySettingsAPIEncryptsPassword(t *testing.T) {
	f := newUserAPIFixture(t)
	body := []byte(`{"enabled":true,"schedule":"03:00","duration_minutes":15,"tolerance_horizontal_mm":30,"tolerance_vertical_mm":50,"host":"skpos.gku.sk:2101","mountpoint":"SKPOS_CM_32_MSM7","username":"fixture","password":"secret-value"}`)
	rr := f.request(t, http.MethodPut, "/api/integrity/settings", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	stored, err := f.store.IntegritySettings()
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.PasswordEnc) == "secret-value" || !stored.HasPassword() {
		t.Fatal("password was not encrypted")
	}
	rr = f.request(t, http.MethodGet, "/api/integrity", nil)
	var out struct {
		Settings struct {
			HasPassword bool   `json:"has_password"`
			Username    string `json:"username"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Settings.HasPassword || out.Settings.Username != "fixture" {
		t.Fatalf("redacted status wrong: %+v", out)
	}
}

func TestIntegritySettingsAPIRejectsUnknownFields(t *testing.T) {
	f := newUserAPIFixture(t)
	rr := f.request(t, http.MethodPut, "/api/integrity/settings", []byte(`{"station_id":1}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestCancelRequiresARunningCheck(t *testing.T) {
	// No monitor at all: the endpoint must say so rather than pretend success.
	s := &Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rr := httptest.NewRecorder()
	s.handleIntegrityCancel(rr, httptest.NewRequest(http.MethodPost, "/api/integrity/cancel", nil))
	if rr.Code != 503 {
		t.Errorf("no monitor: status %d, want 503", rr.Code)
	}
	s.Integrity = integrity.New(nil, nil, &config.Config{}, nil, s.Log)
	rr = httptest.NewRecorder()
	s.handleIntegrityCancel(rr, httptest.NewRequest(http.MethodPost, "/api/integrity/cancel", nil))
	if rr.Code != 409 {
		t.Errorf("nothing running: status %d, want 409", rr.Code)
	}
}
