// Package integrity independently solves the base antenna position against an
// external NTRIP reference stream -- any NTRIP network covering this area --
// and compares it with the position broadcast by PSGNSS. It shells out to RTKLIB's rtkrcv, which is already the project's
// trusted GNSS processing runtime.
package integrity

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/hub"
	"github.com/psgnss/psgnss-base/internal/secrets"
	"github.com/psgnss/psgnss-base/internal/store"
)

type Monitor struct {
	store     *store.Store
	keyring   *secrets.Keyring
	cfg       *config.Config
	hub       *hub.Hub
	log       *slog.Logger
	mu        sync.Mutex
	running   bool
	prog      Progress
	cancel    context.CancelFunc
	cancelled bool
}

// Progress is a snapshot of what a running comparison has achieved so far,
// read straight from the solution file rtkrcv is still appending to. It exists
// so the web UI can show real movement -- epochs, fixes, the deltas as they
// settle -- rather than a spinner for a quarter of an hour.
type Progress struct {
	Running      bool     `json:"running"`
	StartedAt    int64    `json:"started_at,omitempty"`
	ElapsedSec   int      `json:"elapsed_s"`
	PlannedSec   int      `json:"planned_s"`
	Phase        string   `json:"phase"`
	Epochs       int      `json:"epochs"`
	Fixed        int      `json:"fixed"`
	Float        int      `json:"float"`
	Solution     string   `json:"solution"`
	HorizontalMM *float64 `json:"horizontal_mm,omitempty"`
	VerticalMM   *float64 `json:"vertical_mm,omitempty"`
}

// Progress returns the live snapshot. Elapsed time is not clamped to the
// planned duration: an overrun is information, not something to hide.
func (m *Monitor) Progress() Progress {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.prog
	p.Running = m.running
	if m.running && p.StartedAt > 0 {
		p.ElapsedSec = int(time.Since(time.Unix(p.StartedAt, 0)).Seconds())
	}
	return p
}

// Cancel stops a running check. It reports whether there was one to stop. The
// partial result is discarded rather than recorded as a measurement: a window
// cut short has not met the selection criteria, so publishing its median would
// be a number nobody should compare against a tolerance.
func (m *Monitor) Cancel() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running || m.cancel == nil {
		return false
	}
	m.cancelled = true
	m.prog.Phase = "cancelling"
	m.cancel()
	return true
}

func (m *Monitor) wasCancelled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelled
}

func (m *Monitor) setPhase(phase string) {
	m.mu.Lock()
	m.prog.Phase = phase
	m.mu.Unlock()
}

// observe folds the solutions written so far into the live snapshot, using the
// same selection and comparison the final result uses, so the numbers on screen
// during the run are the numbers that will be recorded.
func (m *Monitor) observe(samples []solution) {
	var fixed, floats int
	for _, s := range samples {
		switch s.quality {
		case 1:
			fixed++
		case 2:
			floats++
		}
	}
	chosen, quality := selectSolutions(samples)
	m.mu.Lock()
	defer m.mu.Unlock()
	first := len(samples) > 0 && m.prog.Epochs == 0
	m.prog.Epochs, m.prog.Fixed, m.prog.Float, m.prog.Solution = len(samples), fixed, floats, quality
	if len(samples) > 0 && m.prog.Phase == "connecting" {
		m.prog.Phase = "observing"
	}
	if first {
		// RTKLIB solves nothing until it has collected broadcast ephemeris from
		// RXM-SFRBX, which takes around a minute from a cold start. Saying so
		// once means a check that is merely warming up cannot be mistaken for a
		// broken one.
		m.log.Info("external integrity check is solving", "first_epochs", len(samples),
			"after", time.Since(time.Unix(m.prog.StartedAt, 0)).Round(time.Second).String())
	}
	if len(chosen) == 0 {
		m.prog.HorizontalMM, m.prog.VerticalMM = nil, nil
		return
	}
	_, _, _, horizontal, vertical := m.compare(chosen)
	m.prog.HorizontalMM, m.prog.VerticalMM = &horizontal, &vertical
}

// watch re-reads the growing solution file until the run ends. A partially
// written final line simply fails to parse and is picked up on the next pass.
func (m *Monitor) watch(path string, done <-chan struct{}) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			samples, err := parseSolutions(path)
			if err != nil && len(samples) == 0 {
				continue // rtkrcv has not created the file yet
			}
			m.observe(samples)
		}
	}
}

// compare reduces the selected solutions to one position and its offset from
// the broadcast base position.
func (m *Monitor) compare(v []solution) (lat, lon, height, horizontalMM, verticalMM float64) {
	lat, lon, height = medianPosition(v)
	horizontalMM = horizontalDistanceMM(m.cfg.Station.Position.Latitude, m.cfg.Station.Position.Longitude, lat, lon)
	verticalMM = math.Abs(height-m.cfg.Station.Position.Height) * 1000
	return
}

func New(db *store.Store, keyring *secrets.Keyring, cfg *config.Config, h *hub.Hub, log *slog.Logger) *Monitor {
	if log == nil {
		log = slog.Default()
	}
	return &Monitor{store: db, keyring: keyring, cfg: cfg, hub: h, log: log}
}

func (m *Monitor) Running() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.running }

// StartScheduler runs one check per UTC day at the configured time. Settings
// are read on every tick, so enabling or rescheduling does not require restart.
func (m *Monitor) StartScheduler(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s, err := m.store.IntegritySettings()
			if err != nil || !s.Enabled || now.UTC().Format("15:04") != s.Schedule {
				continue
			}
			last, _ := m.store.LatestIntegrityRun()
			if last != nil && time.Unix(last.StartedAt, 0).UTC().Format("2006-01-02") == now.UTC().Format("2006-01-02") {
				continue
			}
			go func() {
				if _, err := m.Run(ctx); err != nil {
					m.log.Warn("external integrity check failed", "err", err)
				}
			}()
		}
	}
}

// Run performs one bounded comparison. A concurrent manual or scheduled run
// is rejected because both would consume the same live station stream.
func (m *Monitor) Run(ctx context.Context) (*store.IntegrityRun, error) {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil, errors.New("an external integrity check is already running")
	}
	m.running, m.cancelled, m.cancel = true, false, nil
	m.prog = Progress{StartedAt: time.Now().UTC().Unix(), Phase: "preparing"}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.running, m.cancel = false, nil
		m.mu.Unlock()
	}()

	started := time.Now().UTC()
	run := store.IntegrityRun{StartedAt: started.Unix(), Status: "error"}
	settings, err := m.store.IntegritySettings()
	if err != nil {
		return nil, err
	}
	finish := func(status, detail string) (*store.IntegrityRun, error) {
		run.Status, run.Detail, run.FinishedAt = status, detail, time.Now().UTC().Unix()
		if saveErr := m.store.SaveIntegrityRun(run); saveErr != nil {
			return &run, saveErr
		}
		if status == "error" {
			return &run, errors.New(detail)
		}
		return &run, nil
	}
	if !settings.HasPassword() || strings.TrimSpace(settings.Username) == "" {
		return finish("error", "reference network credentials are not configured")
	}
	password, err := m.keyring.Open(settings.PasswordEnc, settings.PasswordNonce)
	if err != nil {
		return finish("error", "cannot decrypt reference network credentials")
	}
	if err := validateSettings(settings, password); err != nil {
		return finish("error", err.Error())
	}
	if settings.DurationMinutes < 1 {
		settings.DurationMinutes = 15
	}
	m.mu.Lock()
	m.prog.PlannedSec = settings.DurationMinutes * 60
	m.mu.Unlock()

	tmp, err := os.MkdirTemp("", "psgnss-integrity-")
	if err != nil {
		return finish("error", err.Error())
	}
	defer os.RemoveAll(tmp)
	conf, solution := filepath.Join(tmp, "rtkrcv.conf"), filepath.Join(tmp, "solution.pos")
	feed, err := newRawFeed(m.hub)
	if err != nil {
		return finish("error", "cannot open private receiver feed: "+err.Error())
	}
	defer feed.Close()
	if err := os.WriteFile(conf, []byte(m.rtkConfig(settings, password, feed.Addr(), solution)), 0o600); err != nil {
		return finish("error", "cannot create RTKLIB configuration: "+err.Error())
	}
	limit := time.Duration(settings.DurationMinutes)*time.Minute + 20*time.Second
	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	m.mu.Lock()
	m.cancel = cancel
	m.mu.Unlock()
	cmd := exec.CommandContext(runCtx, "/usr/local/bin/rtkrcv", "-nc", "-o", conf)
	var diagnostic strings.Builder
	cmd.Stdout, cmd.Stderr = &diagnostic, &diagnostic
	m.setPhase("connecting")
	watching := make(chan struct{})
	go m.watch(solution, watching)
	err = cmd.Run()
	close(watching)
	m.setPhase("comparing")
	samples, parseErr := parseSolutions(solution)
	if m.wasCancelled() {
		m.log.Info("external integrity check cancelled", "epochs", len(samples))
		return finish("cancelled", fmt.Sprintf("stopped by an operator after %s · %d epochs solved, discarded",
			time.Since(started).Round(time.Second), len(samples)))
	}
	if parseErr != nil && len(samples) == 0 {
		detail := "RTKLIB produced no usable position solution"
		if err != nil && !errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			detail += ": " + safeDiagnostic(diagnostic.String())
		}
		return finish("error", detail)
	}
	chosen, quality := selectSolutions(samples)
	if len(chosen) == 0 {
		return finish("error", "the comparison did not reach a fixed or float RTK solution")
	}
	lat, lon, height, horizontal, vertical := m.compare(chosen)
	run.Solution, run.Samples = quality, len(chosen)
	run.Latitude, run.Longitude, run.Height = &lat, &lon, &height
	run.HorizontalMM, run.VerticalMM = &horizontal, &vertical
	status := "pass"
	if horizontal > float64(settings.ToleranceHorizontalMM) || vertical > float64(settings.ToleranceVerticalMM) {
		status = "fail"
	}
	detail := fmt.Sprintf("%s solution · %d of %d epochs used · horizontal %.0f mm · vertical %.0f mm",
		quality, len(chosen), len(samples), horizontal, vertical)
	m.log.Info("external integrity check complete", "status", status, "solution", quality, "samples", len(chosen), "horizontal_mm", horizontal, "vertical_mm", vertical)
	return finish(status, detail)
}

func validateSettings(s store.IntegritySettings, password string) error {
	if _, _, err := net.SplitHostPort(s.Host); err != nil {
		return errors.New("reference caster host must be host:port")
	}
	for name, value := range map[string]string{"username": s.Username, "password": password, "mountpoint": s.Mountpoint} {
		if value == "" || strings.ContainsAny(value, "\r\n@/#") {
			return fmt.Errorf("reference %s is empty or contains a character RTKLIB cannot encode", name)
		}
	}
	return nil
}

// rtkConfig writes the rtkrcv session.
//
// The processing options are chosen for the baseline this check actually has.
// A network reference stream is asked for a solution at this antenna's own
// latitude and longitude (inpstr2-nmeareq), so the baseline is effectively zero
// and the first configuration was wrong for it in two ways that cost a night's
// measurement:
//
//   - ionoopt was dual-freq, the ionosphere-free combination. Its ambiguities
//     are not integers, so RTKLIB could never validate a fix: every epoch came
//     back float with an AR ratio of 0.0. Over a zero baseline the differential
//     ionosphere cancels anyway, so the correction bought nothing and prevented
//     the fix it was supposed to improve.
//   - tropopt was est-ztd, which estimates zenith tropospheric delay. That
//     parameter is almost perfectly correlated with height, so on a short
//     baseline it absorbs the very quantity being measured. The first completed
//     run reported a 204 mm vertical offset against a base position the owner
//     has confirmed with a rover to sub-centimetre.
//
// A modelled troposphere and no ionosphere correction are the standard choice
// for a short baseline, and they leave L1/L2 ambiguities integer so
// pos2-armode can fix them.
//
// The position request is nmeareq=single rather than latlon. A network caster
// places its virtual station where the GGA says the receiver is, in three
// dimensions, and RTKLIB's latlon form has **no height option** --- only
// inpstr2-nmealat and inpstr2-nmealon exist --- so the altitude it advertises is
// not ours to set. This antenna is at about 289 m. Under single, RTKLIB sends
// its own single-point solution, which carries a real height, at the cost of the
// first GGA waiting for that solution.
func (m *Monitor) rtkConfig(s store.IntegritySettings, password, localStream, output string) string {
	return fmt.Sprintf(`console-passwd =
inpstr1-type =tcpcli
inpstr1-path =%s
inpstr1-format =ubx
inpstr2-type =ntripcli
inpstr2-path =%s:%s@%s/%s
inpstr2-format =rtcm3
inpstr2-nmeareq =single
outstr1-type =file
outstr1-path =%s
outstr1-format =llh
pos1-posmode =static
pos1-frequency =l1+l2
pos1-elmask =10
pos1-ionoopt =off
pos1-tropopt =saas
pos1-navsys =63
pos2-armode =continuous
pos2-gloarmode =autocal
pos2-arthres =3.0
pos2-arminfix =10
pos2-maxage =30
out-solformat =llh
out-outhead =on
out-timesys =utc
out-timeform =hms
out-height =ellipsoidal
out-solstatic =all
misc-timeout =10000
misc-reconnect =5000
misc-nmeacycle =5000
misc-buffsize =131072
misc-navmsgsel =all
ant2-postype =rtcm
`, localStream, s.Username, password, s.Host, s.Mountpoint, output)
}

// rawFeed exposes one private, ephemeral loopback stream carrying this
// antenna's raw UBX measurements.
//
// This was originally the hub's RTCM output, which could never work: the
// station's RTCM is observations and a position (1005 + MSM4/MSM7) with **no
// broadcast ephemeris**, and this receiver family cannot emit any -- there is no
// CFG-MSGOUT key for RTCM 1019/1020/1042/1046. Neither stream reaching rtkrcv
// carried navigation data, so RTKLIB could not place a satellite in orbit and
// produced an empty solution file for the whole window, silently.
//
// RXM-SFRBX carries the broadcast navigation subframes and RXM-RAWX the raw
// observations, so the UBX stream supplies both, and the observations are the
// receiver's own rather than MSM re-encodings. Only those two messages are
// selected: NAV-PVT and the rest would be noise to rtkrcv.
type rawFeed struct {
	ln   net.Listener
	sub  *hub.Sub
	done chan struct{}
}

// integrityFilter selects what rtkrcv needs and nothing else.
func integrityFilter() (*hub.Filter, error) {
	return hub.NewFilter(hub.ProtoUBX, []hub.FilterSpec{
		{Type: 0x0215}, // RXM-RAWX: raw observations
		{Type: 0x0213}, // RXM-SFRBX: broadcast ephemeris
	})
}

func newRawFeed(h *hub.Hub) (*rawFeed, error) {
	if h == nil {
		return nil, errors.New("receiver hub is unavailable")
	}
	filter, err := integrityFilter()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &rawFeed{ln: ln, sub: h.Subscribe("external-integrity", filter, 4096), done: make(chan struct{})}
	go f.serve()
	return f, nil
}

func (f *rawFeed) Addr() string { return f.ln.Addr().String() }
func (f *rawFeed) Close() {
	select {
	case <-f.done:
		return
	default:
		close(f.done)
	}
	_ = f.ln.Close()
	f.sub.Close()
}
func (f *rawFeed) serve() {
	c, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer c.Close()
	for {
		select {
		case <-f.done:
			return
		case frame, ok := <-f.sub.C():
			if !ok {
				return
			}
			_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := c.Write(frame); err != nil {
				return
			}
		}
	}
}

type solution struct {
	lat, lon, height float64
	quality          int
}

func parseSolutions(path string) ([]solution, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []solution
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "%") {
			continue
		}
		v := strings.Fields(line)
		if len(v) < 7 {
			continue
		}
		lat, e1 := strconv.ParseFloat(v[2], 64)
		lon, e2 := strconv.ParseFloat(v[3], 64)
		h, e3 := strconv.ParseFloat(v[4], 64)
		q, e4 := strconv.Atoi(v[5])
		if e1 == nil && e2 == nil && e3 == nil && e4 == nil && (q == 1 || q == 2) {
			out = append(out, solution{lat, lon, h, q})
		}
	}
	return out, sc.Err()
}

// selectSolutions picks the epochs the comparison is measured from.
//
// pos1-posmode is static, so RTKLIB accumulates: its filter state at the end of
// the window carries the whole session, and the early epochs are convergence
// rather than measurement. Averaging the lot mixes the two — the first completed
// run drew its median from all 587 epochs, including the first minute where the
// vertical standard deviation was still 1.9 m. Only the tail is used, while the
// thresholds still consider every epoch so a run that fixed only briefly is not
// accepted.
func selectSolutions(all []solution) ([]solution, string) {
	var fixed, floats []solution
	for _, s := range all {
		if s.quality == 1 {
			fixed = append(fixed, s)
		} else if s.quality == 2 {
			floats = append(floats, s)
		}
	}
	if len(fixed) >= 3 {
		return convergedTail(fixed, 3), "fixed"
	}
	if len(floats) >= 10 {
		return convergedTail(floats, 10), "float"
	}
	return nil, "none"
}

// convergedTail returns the last quarter of v, never fewer than min epochs.
func convergedTail(v []solution, min int) []solution {
	n := len(v) / 4
	if n < min {
		n = min
	}
	if n > len(v) {
		n = len(v)
	}
	return v[len(v)-n:]
}

func medianPosition(v []solution) (float64, float64, float64) {
	lat, lon, h := make([]float64, len(v)), make([]float64, len(v)), make([]float64, len(v))
	for i, s := range v {
		lat[i], lon[i], h[i] = s.lat, s.lon, s.height
	}
	median := func(x []float64) float64 {
		sort.Float64s(x)
		n := len(x)
		if n%2 == 1 {
			return x[n/2]
		}
		return (x[n/2-1] + x[n/2]) / 2
	}
	return median(lat), median(lon), median(h)
}

func horizontalDistanceMM(aLat, aLon, bLat, bLon float64) float64 {
	const earth = 6378137.0
	r := math.Pi / 180
	dLat, dLon := (bLat-aLat)*r, (bLon-aLon)*r
	x := dLon * math.Cos((aLat+bLat)*r/2)
	return math.Hypot(x, dLat) * earth * 1000
}

func safeDiagnostic(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[len(s)-300:]
	}
	if s == "" {
		return "rtkrcv exited"
	}
	return s
}
