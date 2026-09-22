package downloader

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Job is one conversion, tracked on the server.
//
// Conversion state lives here rather than in the browser so that navigating
// away from the tab does not lose a running conversion, and so progress
// survives a page reload.
type Job struct {
	ID       string    `json:"id"`
	Date     string    `json:"date"`
	Outputs  string    `json:"outputs"`
	Window   string    `json:"window"`
	Preset   string    `json:"preset"`
	State    string    `json:"state"` // running | done | error
	Percent  float64   `json:"percent"`
	Stage    string    `json:"stage"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	Elapsed  string    `json:"elapsed"`

	// Set when State == "done".
	Token   string   `json:"token,omitempty"`
	ZipName string   `json:"zip_name,omitempty"`
	Files   []string `json:"files,omitempty"`
	SizeMB  float64  `json:"size_mb,omitempty"`

	Error string `json:"error,omitempty"`

	// Owner is who asked for this conversion: an administrator by name, or an
	// anonymous visitor by cookie. It never leaves the daemon -- it exists so
	// one visitor cannot list, watch or cancel another's work on a public
	// dashboard.
	Owner string `json:"-"`

	// cancel stops the running conversion; nil once finished.
	cancel context.CancelFunc `json:"-"`
}

// JobTTL is how long a finished job stays queryable.
const JobTTL = 30 * time.Minute

// scanLine matches convbin's progress output, e.g.
// "scanning: 2026/09/14 14:47:36 GESC".
var scanLine = regexp.MustCompile(`(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})`)

// progress converts convbin's stderr chatter into a fraction.
//
// convbin walks the file several times and does not say how many times, so
// each pass is given half of whatever progress remains: pass 1 spans 0-50%,
// pass 2 spans 50-75%, pass 3 spans 75-87.5%, and so on. That is monotonic for
// any number of passes and never reaches 100% until the process actually
// exits, which is better than a bar that jumps backwards when a pass it did
// not expect begins.
type progress struct {
	start, end time.Time
	mu         sync.Mutex
	pass       int
	last       time.Time
	frac       float64
	stage      string
}

func newProgress(start, end time.Time) *progress {
	return &progress{start: start, end: end, pass: 1, stage: "scanning"}
}

func (p *progress) feed(line string) {
	m := scanLine.FindStringSubmatch(line)
	if m == nil {
		return
	}
	t, err := time.Parse("2006/01/02 15:04:05", m[1])
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.last.IsZero() && t.Before(p.last.Add(-5*time.Minute)) {
		// Timestamps went backwards: another pass over the file.
		p.pass++
		p.stage = "converting"
	}
	p.last = t
	span := p.end.Sub(p.start).Seconds()
	if span <= 0 {
		return
	}
	f := t.Sub(p.start).Seconds() / span
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	// Each pass consumes half the remaining bar.
	scale := math.Pow(0.5, float64(p.pass))
	base := 1 - math.Pow(0.5, float64(p.pass-1))
	v := base + scale*f
	// Never go backwards, and never claim completion before the process ends.
	if v > p.frac {
		p.frac = v
	}
	if p.frac > 0.99 {
		p.frac = 0.99
	}
}

func (p *progress) read() (float64, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.frac, p.stage
}

// watch consumes convbin's stderr, feeding the progress tracker and keeping the
// tail for error reporting.
func (p *progress) watch(r io.Reader, tail *strings.Builder, onUpdate func()) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	// convbin writes progress with carriage returns, so split on either.
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		for i, b := range data {
			if b == '\n' || b == '\r' {
				return i + 1, data[:i], nil
			}
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		p.feed(line)
		if tail.Len() < 4096 {
			tail.WriteString(line)
			tail.WriteByte('\n')
		}
		if onUpdate != nil {
			onUpdate()
		}
	}
}

// StartJob begins a conversion in the background and returns immediately.
func (d *Downloader) StartJob(owner, dateStr string, out Outputs, win Window, preset Preset) (*Job, error) {
	if _, err := time.Parse("2006-01-02", dateStr); err != nil {
		return nil, fmt.Errorf("invalid date %q, expected YYYY-MM-DD", dateStr)
	}
	if !win.Valid() {
		return nil, fmt.Errorf("invalid time range: start must be before end and the range may not exceed 31 days")
	}
	label := "full day"
	if !win.IsFullDay() {
		label = win.Label() + " UTC"
	}
	d.jmu.Lock()
	// One conversion per day at a time; hand back the running one instead of
	// starting a second that would compete for the same CPU.
	for _, j := range d.jobs {
		if j.Owner == owner && j.Date == dateStr && j.Outputs == out.String() && j.Window == label &&
			j.Preset == preset.ID && j.State == "running" {
			d.jmu.Unlock()
			return j, nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{ID: newToken(), Owner: owner, Date: dateStr, Outputs: out.String(), Window: label,
		Preset: preset.ID, State: "running", Stage: "starting", Started: time.Now(), cancel: cancel}
	if d.jobs == nil {
		d.jobs = map[string]*Job{}
	}
	d.jobs[job.ID] = job
	d.jmu.Unlock()

	go func() {
		defer cancel()
		res, err := d.Process(ctx, dateStr, job, out, win, preset)
		d.jmu.Lock()
		defer d.jmu.Unlock()
		job.Finished = time.Now()
		job.Elapsed = job.Finished.Sub(job.Started).Round(time.Second).String()
		job.cancel = nil
		if err != nil {
			if ctx.Err() != nil {
				job.State, job.Stage, job.Percent = "cancelled", "cancelled", 0
				job.Error = ""
				return
			}
			job.State, job.Error, job.Percent = "error", err.Error(), 0
			return
		}
		job.State, job.Percent, job.Stage = "done", 1, "complete"
		job.Token, job.ZipName, job.Files = res.Token, res.ZipName, res.Files
		job.SizeMB = res.SizeMB
	}()
	return job, nil
}

// Cancel stops a running conversion.
//
// A full day takes about a minute, so changing your mind should not mean
// waiting it out or competing for the Pi's CPU with the next request.
func (d *Downloader) Cancel(id string) bool {
	d.jmu.Lock()
	j, ok := d.jobs[id]
	var c context.CancelFunc
	if ok && j.State == "running" && j.cancel != nil {
		c = j.cancel
		j.Stage = "cancelling"
	}
	d.jmu.Unlock()
	if c == nil {
		return false
	}
	c()
	return true
}

// RunningFor counts a caller's running conversions, and Running counts every
// conversion an anonymous caller is responsible for. A public page has to be
// able to refuse work before it starts, not after it has taken the CPU.
func (d *Downloader) RunningFor(owner string) int {
	d.jmu.Lock()
	defer d.jmu.Unlock()
	n := 0
	for _, j := range d.jobs {
		if j.State == "running" && j.Owner == owner {
			n++
		}
	}
	return n
}

// RunningAnonymous counts conversions started by callers with no session.
func (d *Downloader) RunningAnonymous() int {
	d.jmu.Lock()
	defer d.jmu.Unlock()
	n := 0
	for _, j := range d.jobs {
		if j.State == "running" && strings.HasPrefix(j.Owner, "visitor:") {
			n++
		}
	}
	return n
}

// JobStatus returns a snapshot of a job.
func (d *Downloader) JobStatus(id string) (*Job, bool) {
	d.jmu.Lock()
	defer d.jmu.Unlock()
	j, ok := d.jobs[id]
	if !ok {
		return nil, false
	}
	cp := *j
	cp.cancel = nil
	if cp.State == "running" {
		cp.Elapsed = time.Since(cp.Started).Round(time.Second).String()
	}
	return &cp, true
}

// Jobs lists recent jobs, newest first. An owner is given only their own; the
// empty owner means an administrator and lists every one.
func (d *Downloader) Jobs(owner string) []*Job {
	d.jmu.Lock()
	defer d.jmu.Unlock()
	out := make([]*Job, 0, len(d.jobs))
	for _, j := range d.jobs {
		if owner != "" && j.Owner != owner {
			continue
		}
		cp := *j
		cp.cancel = nil
		if cp.State == "running" {
			cp.Elapsed = time.Since(cp.Started).Round(time.Second).String()
		}
		out = append(out, &cp)
	}
	for i := range out {
		for k := i + 1; k < len(out); k++ {
			if out[k].Started.After(out[i].Started) {
				out[i], out[k] = out[k], out[i]
			}
		}
	}
	return out
}

// expireJobs drops finished jobs past their TTL.
func (d *Downloader) expireJobs() {
	now := time.Now()
	d.jmu.Lock()
	defer d.jmu.Unlock()
	for id, j := range d.jobs {
		if j.State != "running" && now.Sub(j.Finished) > JobTTL {
			delete(d.jobs, id)
		}
	}
}
