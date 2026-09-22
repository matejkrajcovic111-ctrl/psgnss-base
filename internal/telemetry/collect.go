package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Store is the persistence the collector needs.
type Store interface {
	PutEpoch(ts time.Time, sats []Sat, signals []Signal, fixType uint8) error
	DecimateOld(fineWindow time.Duration, coarseInterval int) (int64, error)
	PruneTelemetry(retentionDays int) (int64, error)
}

// Collector turns UBX frames from the hub into stored epochs.
type Collector struct {
	store   Store
	log     *slog.Logger
	mu      sync.RWMutex
	latest  Epoch
	signals []Signal
	lastAt  time.Time
	// GPS time reference, read from the receiver.
	leapSec int8
	gpsWeek uint16
	gpsTOW  float64
	gpsAt   time.Time

	FineWindow     time.Duration
	CoarseInterval int
	RetentionDays  int
}

func NewCollector(s Store, fineWindow time.Duration, coarse, retentionDays int,
	log *slog.Logger) *Collector {
	if log == nil {
		log = slog.Default()
	}
	if fineWindow <= 0 {
		fineWindow = time.Hour
	}
	if coarse <= 0 {
		coarse = 30
	}
	return &Collector{store: s, log: log, FineWindow: fineWindow,
		CoarseInterval: coarse, RetentionDays: retentionDays}
}

// Latest returns the most recent epoch seen, for the live dashboard.
func (c *Collector) Latest() (Epoch, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest, c.lastAt
}

// LatestSignals returns the most recent per-band signal observations.
func (c *Collector) LatestSignals() []Signal {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]Signal(nil), c.signals...)
}

// GPSTime reports the receiver's GPS time reference: leap seconds, week, and
// time of week, with the wall-clock instant they were observed.
func (c *Collector) GPSTime() (leap int8, week uint16, tow float64, at time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.leapSec, c.gpsWeek, c.gpsTOW, c.gpsAt
}

// Run consumes UBX frames until ctx is cancelled.
//
// frames carries whole UBX messages; NAV-SAT drives the skyplot and SNR, and
// NAV-PVT supplies the fix type.
func (c *Collector) Run(ctx context.Context, frames <-chan []byte) {
	maint := time.NewTicker(10 * time.Minute)
	defer maint.Stop()
	var pending Epoch
	var lastWrite time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-maint.C:
			c.maintain()
		case b, ok := <-frames:
			if !ok {
				return
			}
			if len(b) < 8 || b[0] != 0xB5 || b[1] != 0x62 {
				continue
			}
			cls, id := b[2], b[3]
			payload := b[6 : len(b)-2]
			switch {
			case cls == 0x01 && id == 0x35: // NAV-SAT
				sats, err := ParseNavSat(payload)
				if err != nil {
					c.log.Debug("NAV-SAT parse failed", "err", err)
					continue
				}
				pending.Sats = sats
				now := time.Now()
				// One row per second at most; NAV-SAT arrives at 1 Hz but a
				// burst after a reconnect should not multiply rows.
				if now.Sub(lastWrite) >= time.Second {
					c.mu.RLock()
					signals := append([]Signal(nil), c.signals...)
					c.mu.RUnlock()
					if err := c.store.PutEpoch(now, sats, signals, pending.FixType); err != nil {
						c.log.Error("telemetry write failed", "err", err)
					}
					lastWrite = now
				}
				c.mu.Lock()
				c.latest = Epoch{Sats: sats, FixType: pending.FixType, NumSV: pending.NumSV}
				c.lastAt = now
				c.mu.Unlock()
			case cls == 0x02 && id == 0x15: // RXM-RAWX
				if leap, week, tow, ok := ParseRawxLeapSeconds(payload); ok {
					c.mu.Lock()
					c.leapSec, c.gpsWeek, c.gpsTOW, c.gpsAt = leap, week, tow, time.Now()
					c.mu.Unlock()
				}
			case cls == 0x01 && id == 0x43: // NAV-SIG
				signals, err := ParseNavSig(payload)
				if err != nil {
					c.log.Debug("NAV-SIG parse failed", "err", err)
					continue
				}
				c.mu.Lock()
				c.signals = signals
				c.mu.Unlock()
			case cls == 0x01 && id == 0x07: // NAV-PVT
				pvt, err := ParseNavPVT(payload)
				if err != nil {
					continue
				}
				pending.FixType, pending.NumSV = pvt.FixType, pvt.NumSV
			}
		}
	}
}

func (c *Collector) maintain() {
	if n, err := c.store.DecimateOld(c.FineWindow, c.CoarseInterval); err != nil {
		c.log.Error("telemetry decimation failed", "err", err)
	} else if n > 0 {
		c.log.Info("telemetry decimated", "rows_removed", n)
	}
	if n, err := c.store.PruneTelemetry(c.RetentionDays); err != nil {
		c.log.Error("telemetry prune failed", "err", err)
	} else if n > 0 {
		c.log.Info("telemetry pruned", "rows_removed", n, "retention_days", c.RetentionDays)
	}
}
