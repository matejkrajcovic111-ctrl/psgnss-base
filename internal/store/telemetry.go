package store

import (
	"time"

	"github.com/psgnss/psgnss-base/internal/telemetry"
)

// PutEpoch records one telemetry epoch.
func (s *Store) PutEpoch(ts time.Time, sats []telemetry.Sat, signals []telemetry.Signal, fixType uint8) error {
	_, err := s.db.Exec(`INSERT INTO telemetry_epoch (ts,sat_blob,sig_blob,sat_count,fix_type,fine)
		VALUES (?,?,?,?,?,1) ON CONFLICT(ts) DO UPDATE SET
			sat_blob=excluded.sat_blob, sat_count=excluded.sat_count,
			sig_blob=excluded.sig_blob, fix_type=excluded.fix_type`,
		ts.Unix(), telemetry.Pack(sats), telemetry.PackSignals(signals), len(sats), fixType)
	return err
}

// EpochRow is a stored epoch.
type EpochRow struct {
	TS       time.Time
	Sats     []telemetry.Sat
	Signals  []telemetry.Signal
	SatCount int
	FixType  uint8
}

// LatestEpoch returns the most recent epoch.
func (s *Store) LatestEpoch() (*EpochRow, error) {
	var ts int64
	var blob []byte
	var n int
	var fx uint8
	var sig []byte
	err := s.db.QueryRow(`SELECT ts,sat_blob,sig_blob,sat_count,fix_type FROM telemetry_epoch
		ORDER BY ts DESC LIMIT 1`).Scan(&ts, &blob, &sig, &n, &fx)
	if err != nil {
		return nil, err
	}
	return &EpochRow{TS: time.Unix(ts, 0), Sats: telemetry.Unpack(blob), Signals: telemetry.UnpackSignals(sig), SatCount: n, FixType: fx}, nil
}

// EpochRange returns epochs in [from, to], at most limit rows, oldest first.
func (s *Store) EpochRange(from, to time.Time, limit int) ([]EpochRow, error) {
	if limit <= 0 {
		limit = 5000
	}
	bucket := int64(1)
	if span := to.Unix() - from.Unix(); span > int64(limit) {
		bucket = (span + int64(limit) - 1) / int64(limit)
	}
	rows, err := s.db.Query(`SELECT ts,sat_blob,sig_blob,sat_count,fix_type FROM telemetry_epoch
		WHERE ts IN (SELECT MAX(ts) FROM telemetry_epoch WHERE ts BETWEEN ? AND ? GROUP BY ts / ?)
		ORDER BY ts LIMIT ?`, from.Unix(), to.Unix(), bucket, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EpochRow
	for rows.Next() {
		var ts int64
		var blob []byte
		var sig []byte
		var n int
		var fx uint8
		if err := rows.Scan(&ts, &blob, &sig, &n, &fx); err != nil {
			return nil, err
		}
		out = append(out, EpochRow{TS: time.Unix(ts, 0),
			Sats: telemetry.Unpack(blob), Signals: telemetry.UnpackSignals(sig), SatCount: n, FixType: fx})
	}
	return out, rows.Err()
}

// EpochCount returns the number of stored epochs in a window before display
// sampling. Keeping this separate lets the UI distinguish retention from the
// intentionally bounded number of chart points.
func (s *Store) EpochCount(from, to time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM telemetry_epoch WHERE ts BETWEEN ? AND ?`,
		from.Unix(), to.Unix()).Scan(&n)
	return n, err
}

// DecimateOld thins epochs older than fineWindow down to coarseInterval.
//
// Keeping 1 Hz for a week would be ~600k rows; this keeps recent detail where
// it is useful and thins history to something a browser can plot.
func (s *Store) DecimateOld(fineWindow time.Duration, coarseInterval int) (int64, error) {
	if coarseInterval <= 1 {
		return 0, nil
	}
	cutoff := time.Now().Add(-fineWindow).Unix()
	// Keep one row per coarseInterval bucket; delete the rest.
	r, err := s.db.Exec(`DELETE FROM telemetry_epoch WHERE ts < ? AND fine = 1
		AND ts NOT IN (SELECT MIN(ts) FROM telemetry_epoch WHERE ts < ?
		               GROUP BY ts / ?)`, cutoff, cutoff, coarseInterval)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	if n > 0 {
		_, _ = s.db.Exec(`UPDATE telemetry_epoch SET fine = 0 WHERE ts < ?`, cutoff)
	}
	return n, nil
}

// PruneTelemetry deletes epochs and health samples older than retentionDays.
func (s *Store) PruneTelemetry(retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays).Unix()
	r, err := s.db.Exec(`DELETE FROM telemetry_epoch WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	_, _ = s.db.Exec(`DELETE FROM health_sample WHERE ts < ?`, cutoff)
	return n, nil
}

// PutHealth records a host and stream health sample.
func (s *Store) PutHealth(ts time.Time, hubBytes, rtcmBps, ubxBps int64, clients int,
	cpuPct, tempC float64, diskFreeMB, memFreeMB int64) error {
	_, err := s.db.Exec(`INSERT INTO health_sample
		(ts,hub_bytes_in,rtcm_bps,ubx_bps,clients,cpu_pct,temp_c,disk_free_mb,mem_free_mb)
		VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(ts) DO NOTHING`,
		ts.Unix(), hubBytes, rtcmBps, ubxBps, clients, cpuPct, tempC, diskFreeMB, memFreeMB)
	return err
}

// HealthRow is one health sample.
type HealthRow struct {
	TS         time.Time
	RTCMBps    int64
	UBXBps     int64
	Clients    int
	CPUPct     float64
	TempC      float64
	DiskFreeMB int64
	MemFreeMB  int64
}

// HealthRange returns health samples in a window.
func (s *Store) HealthRange(from, to time.Time, limit int) ([]HealthRow, error) {
	if limit <= 0 {
		limit = 2000
	}
	bucket := int64(1)
	if span := to.Unix() - from.Unix(); span > int64(limit) {
		bucket = (span + int64(limit) - 1) / int64(limit)
	}
	rows, err := s.db.Query(`SELECT ts,rtcm_bps,ubx_bps,clients,cpu_pct,temp_c,
		disk_free_mb,mem_free_mb FROM health_sample
		WHERE ts IN (SELECT MAX(ts) FROM health_sample WHERE ts BETWEEN ? AND ? GROUP BY ts / ?)
		ORDER BY ts LIMIT ?`, from.Unix(), to.Unix(), bucket, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HealthRow
	for rows.Next() {
		var h HealthRow
		var ts int64
		if err := rows.Scan(&ts, &h.RTCMBps, &h.UBXBps, &h.Clients, &h.CPUPct,
			&h.TempC, &h.DiskFreeMB, &h.MemFreeMB); err != nil {
			return nil, err
		}
		h.TS = time.Unix(ts, 0)
		out = append(out, h)
	}
	return out, rows.Err()
}
