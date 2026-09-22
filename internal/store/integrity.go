package store

import (
	"database/sql"
	"time"
)

// IntegritySettings configures the independent external position comparison.
// The password remains encrypted; only the integrity runner opens it.
type IntegritySettings struct {
	Enabled               bool   `json:"enabled"`
	Schedule              string `json:"schedule"`
	DurationMinutes       int    `json:"duration_minutes"`
	ToleranceHorizontalMM int    `json:"tolerance_horizontal_mm"`
	ToleranceVerticalMM   int    `json:"tolerance_vertical_mm"`
	Host                  string `json:"host"`
	Mountpoint            string `json:"mountpoint"`
	Username              string `json:"username"`
	PasswordEnc           []byte `json:"-"`
	PasswordNonce         []byte `json:"-"`
	UpdatedAt             int64  `json:"updated_at"`
}

func (v IntegritySettings) HasPassword() bool {
	return len(v.PasswordEnc) > 0 && len(v.PasswordNonce) > 0
}

func (s *Store) IntegritySettings() (IntegritySettings, error) {
	var v IntegritySettings
	err := s.db.QueryRow(`SELECT enabled,schedule,duration_minutes,tolerance_horizontal_mm,
		tolerance_vertical_mm,host,mountpoint,username,password_enc,password_nonce,updated_at
		FROM integrity_settings WHERE id=1`).Scan(&v.Enabled, &v.Schedule, &v.DurationMinutes,
		&v.ToleranceHorizontalMM, &v.ToleranceVerticalMM, &v.Host, &v.Mountpoint, &v.Username,
		&v.PasswordEnc, &v.PasswordNonce, &v.UpdatedAt)
	return v, err
}

func (s *Store) SaveIntegritySettings(v IntegritySettings) error {
	_, err := s.db.Exec(`UPDATE integrity_settings SET enabled=?,schedule=?,duration_minutes=?,
		tolerance_horizontal_mm=?,tolerance_vertical_mm=?,host=?,mountpoint=?,username=?,
		password_enc=?,password_nonce=?,updated_at=? WHERE id=1`, v.Enabled,
		v.Schedule, v.DurationMinutes, v.ToleranceHorizontalMM, v.ToleranceVerticalMM, v.Host,
		v.Mountpoint, v.Username, v.PasswordEnc, v.PasswordNonce, time.Now().Unix())
	return err
}

type IntegrityRun struct {
	ID           int64    `json:"id"`
	StartedAt    int64    `json:"started_at"`
	FinishedAt   int64    `json:"finished_at"`
	Status       string   `json:"status"`
	Detail       string   `json:"detail"`
	Solution     string   `json:"solution"`
	Samples      int      `json:"samples"`
	Latitude     *float64 `json:"latitude,omitempty"`
	Longitude    *float64 `json:"longitude,omitempty"`
	Height       *float64 `json:"height,omitempty"`
	HorizontalMM *float64 `json:"horizontal_mm,omitempty"`
	VerticalMM   *float64 `json:"vertical_mm,omitempty"`
}

func (s *Store) SaveIntegrityRun(v IntegrityRun) error {
	_, err := s.db.Exec(`INSERT INTO integrity_runs(started_at,finished_at,status,detail,solution,samples,
		latitude,longitude,height,horizontal_mm,vertical_mm) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		v.StartedAt, v.FinishedAt, v.Status, v.Detail, v.Solution, v.Samples,
		v.Latitude, v.Longitude, v.Height, v.HorizontalMM, v.VerticalMM)
	return err
}

func (s *Store) LatestIntegrityRun() (*IntegrityRun, error) {
	var v IntegrityRun
	var lat, lon, h, horizontal, vertical sql.NullFloat64
	err := s.db.QueryRow(`SELECT id,started_at,finished_at,status,detail,solution,samples,latitude,
		longitude,height,horizontal_mm,vertical_mm FROM integrity_runs ORDER BY started_at DESC LIMIT 1`).
		Scan(&v.ID, &v.StartedAt, &v.FinishedAt, &v.Status, &v.Detail, &v.Solution, &v.Samples,
			&lat, &lon, &h, &horizontal, &vertical)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lat.Valid {
		v.Latitude = &lat.Float64
	}
	if lon.Valid {
		v.Longitude = &lon.Float64
	}
	if h.Valid {
		v.Height = &h.Float64
	}
	if horizontal.Valid {
		v.HorizontalMM = &horizontal.Float64
	}
	if vertical.Valid {
		v.VerticalMM = &vertical.Float64
	}
	return &v, nil
}
