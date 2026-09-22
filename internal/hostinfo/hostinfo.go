// Package hostinfo reads Raspberry Pi health from /proc and /sys.
//
// Read directly rather than by shelling out, so the daemon has no runtime
// dependency on vcgencmd or any external tool.
package hostinfo

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Info is a host health snapshot.
type Info struct {
	CPUPercent  float64 `json:"cpu_percent"`
	TempC       float64 `json:"temp_c"`
	MemTotalMB  int64   `json:"mem_total_mb"`
	MemFreeMB   int64   `json:"mem_free_mb"`
	DiskTotalMB int64   `json:"disk_total_mb"`
	DiskFreeMB  int64   `json:"disk_free_mb"`
	Uptime      float64 `json:"uptime_seconds"`
	Load1       float64 `json:"load1"`
}

var (
	mu       sync.Mutex
	lastIdle uint64
	lastAll  uint64
)

// Read collects a snapshot. diskPath selects the filesystem to report.
func Read(diskPath string) Info {
	var i Info
	i.CPUPercent = cpuPercent()
	i.TempC = temperature()
	i.MemTotalMB, i.MemFreeMB = memory()
	i.DiskTotalMB, i.DiskFreeMB = disk(diskPath)
	i.Uptime, i.Load1 = uptimeLoad()
	return i
}

// cpuPercent computes utilisation since the previous call, so the first call
// after start returns 0 rather than a meaningless since-boot average.
func cpuPercent() float64 {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0
	}
	var all, idle uint64
	for n, v := range fields[1:] {
		x, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			continue
		}
		all += x
		if n == 3 || n == 4 { // idle + iowait
			idle += x
		}
	}
	mu.Lock()
	defer mu.Unlock()
	dAll, dIdle := all-lastAll, idle-lastIdle
	lastAll, lastIdle = all, idle
	if lastAll == 0 || dAll == 0 {
		return 0
	}
	return 100 * float64(dAll-dIdle) / float64(dAll)
}

func temperature() float64 {
	for _, p := range []string{
		"/sys/class/thermal/thermal_zone0/temp",
		"/sys/devices/virtual/thermal/thermal_zone0/temp",
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		if err != nil {
			continue
		}
		return v / 1000
	}
	return 0
}

func memory() (totalMB, freeMB int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalMB = v / 1024
		case "MemAvailable:":
			// Available, not Free: free excludes reclaimable cache and badly
			// understates what a process can actually get.
			freeMB = v / 1024
		}
	}
	return totalMB, freeMB
}

func disk(path string) (totalMB, freeMB int64) {
	// Fall back to the root filesystem when the configured path is missing or
	// unreadable -- typically an SMB share that is not mounted. Reporting the
	// root volume is useful; reporting zero looks like a full disk.
	for _, p := range []string{path, "/"} {
		if p == "" {
			continue
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err != nil {
			continue
		}
		bs := int64(st.Bsize)
		return int64(st.Blocks) * bs / (1 << 20), int64(st.Bavail) * bs / (1 << 20)
	}
	return 0, 0
}

func uptimeLoad() (uptime, load1 float64) {
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			uptime, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	return uptime, load1
}

// Since converts an uptime reading to a start time.
func Since(uptimeSeconds float64) time.Time {
	return time.Now().Add(-time.Duration(uptimeSeconds) * time.Second)
}
