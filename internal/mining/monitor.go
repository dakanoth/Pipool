package mining

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SystemMonitor reads Pi hardware stats from sysfs/procfs
type SystemMonitor struct {
	startTime  time.Time
	lastTempC  atomic.Value // float64
	throttling atomic.Bool
	stopCh     chan struct{}

	// CPU delta tracking — need two snapshots to compute real utilisation
	cpuMu       sync.Mutex
	lastCPUIdle float64
	lastCPUTotal float64
	lastCPUPct  float64

	// Callbacks
	OnHighTemp func(tempC float64)
}

func NewSystemMonitor() *SystemMonitor {
	m := &SystemMonitor{
		startTime: time.Now(),
		stopCh:    make(chan struct{}),
	}
	m.lastTempC.Store(float64(0))
	return m
}

// Start begins polling hardware metrics every 10 seconds
func (m *SystemMonitor) Start(limitC int) {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-m.stopCh:
				return
			case <-ticker.C:
			}
			temp := m.ReadCPUTemp()
			m.lastTempC.Store(temp)

			if temp > float64(limitC) {
				m.throttling.Store(true)
				if m.OnHighTemp != nil {
					m.OnHighTemp(temp)
				}
			} else if temp < float64(limitC)-5 {
				m.throttling.Store(false)
			}
		}
	}()
}

// Stop signals the monitoring goroutine to exit.
func (m *SystemMonitor) Stop() {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
}

// ReadCPUTemp reads the Pi's CPU temperature from thermal zone sysfs
func (m *SystemMonitor) ReadCPUTemp() float64 {
	// Pi 5 thermal zone path
	paths := []string{
		"/sys/class/thermal/thermal_zone0/temp",
		"/sys/class/thermal/thermal_zone1/temp",
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		raw := strings.TrimSpace(string(data))
		val, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		// sysfs reports millidegrees
		return val / 1000.0
	}
	return 0
}

// ReadRAMUsage returns used system RAM in GB by reading /proc/meminfo.
// This reflects actual OS-wide memory usage, not just the Go runtime heap.
func (m *SystemMonitor) ReadRAMUsage() float64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	var memTotal, memAvailable float64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			memTotal = val
		case "MemAvailable:":
			memAvailable = val
		}
	}
	// /proc/meminfo reports in kB
	used := memTotal - memAvailable
	if used < 0 {
		used = 0
	}
	return used / (1024 * 1024) // convert kB → GB
}

// readCPURaw reads the raw cumulative CPU counters from /proc/stat.
func readCPURaw() (idle, total float64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(strings.Split(string(data), "\n")[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}
	var vals []float64
	for _, f := range fields[1:] {
		v, _ := strconv.ParseFloat(f, 64)
		vals = append(vals, v)
		total += v
	}
	idle = vals[3]
	if len(vals) > 4 {
		idle += vals[4] // iowait counts as idle for our purposes
	}
	return idle, total
}

// ReadCPUUsage returns current CPU utilisation % using a delta between
// the last two calls. First call always returns 0 (no baseline yet).
func (m *SystemMonitor) ReadCPUUsage() float64 {
	idle, total := readCPURaw()
	if total == 0 {
		return 0
	}

	m.cpuMu.Lock()
	defer m.cpuMu.Unlock()

	deltaIdle := idle - m.lastCPUIdle
	deltaTotal := total - m.lastCPUTotal

	m.lastCPUIdle = idle
	m.lastCPUTotal = total

	if deltaTotal <= 0 {
		return m.lastCPUPct // return last known value if no time has passed
	}

	pct := (1.0 - deltaIdle/deltaTotal) * 100.0
	if pct < 0 {
		pct = 0
	}
	m.lastCPUPct = pct
	return pct
}

// Uptime returns how long the pool has been running
func (m *SystemMonitor) Uptime() time.Duration {
	return time.Since(m.startTime)
}

// IsThrottling returns true if mining should be slowed due to temperature
func (m *SystemMonitor) IsThrottling() bool {
	return m.throttling.Load()
}

// CurrentTemp returns the last read CPU temperature
func (m *SystemMonitor) CurrentTemp() float64 {
	return m.lastTempC.Load().(float64)
}

// Summary returns a human-readable system status string
func (m *SystemMonitor) Summary() string {
	return fmt.Sprintf(
		"Temp: %.1f°C | RAM: %.2fGB | CPU: %.1f%% | Uptime: %s | Throttle: %v",
		m.CurrentTemp(),
		m.ReadRAMUsage(),
		m.ReadCPUUsage(),
		formatDuration(m.Uptime()),
		m.IsThrottling(),
	)
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02dh%02dm%02ds", h, m, s)
}
