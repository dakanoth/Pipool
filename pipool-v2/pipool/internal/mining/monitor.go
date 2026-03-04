package mining

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// SystemMonitor reads Pi hardware stats from sysfs/procfs
type SystemMonitor struct {
	startTime   time.Time
	lastTempC   atomic.Value // float64
	throttling  atomic.Bool

	// Callbacks
	OnHighTemp func(tempC float64)
}

func NewSystemMonitor() *SystemMonitor {
	m := &SystemMonitor{startTime: time.Now()}
	m.lastTempC.Store(float64(0))
	return m
}

// Start begins polling hardware metrics every 10 seconds
func (m *SystemMonitor) Start(limitC int) {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
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

// ReadRAMUsage returns used RAM in GB
func (m *SystemMonitor) ReadRAMUsage() float64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.Sys) / (1024 * 1024 * 1024)
}

// ReadCPUUsage reads /proc/stat to compute CPU utilization %
func (m *SystemMonitor) ReadCPUUsage() float64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 {
		return 0
	}
	// Format: cpu  user nice system idle iowait irq softirq steal
	fields := strings.Fields(lines[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0
	}

	var vals []float64
	for _, f := range fields[1:] {
		v, _ := strconv.ParseFloat(f, 64)
		vals = append(vals, v)
	}
	if len(vals) < 4 {
		return 0
	}

	total := 0.0
	for _, v := range vals {
		total += v
	}
	idle := vals[3]
	if len(vals) > 4 {
		idle += vals[4] // iowait
	}

	if total == 0 {
		return 0
	}
	return (1.0 - idle/total) * 100.0
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
