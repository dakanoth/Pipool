package metrics

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Gauge is a simple thread-safe float64 metric
type Gauge struct {
	mu    sync.RWMutex
	value float64
}

func (g *Gauge) Set(v float64) { g.mu.Lock(); g.value = v; g.mu.Unlock() }
func (g *Gauge) Get() float64  { g.mu.RLock(); defer g.mu.RUnlock(); return g.value }

// Counter is a monotonically increasing uint64
type Counter struct {
	mu    sync.RWMutex
	value uint64
}

func (c *Counter) Add(v uint64) { c.mu.Lock(); c.value += v; c.mu.Unlock() }
func (c *Counter) Get() uint64  { c.mu.RLock(); defer c.mu.RUnlock(); return c.value }

// LabeledGauge holds gauges keyed by label string
type LabeledGauge struct {
	mu     sync.RWMutex
	values map[string]float64
}

func NewLabeledGauge() *LabeledGauge { return &LabeledGauge{values: make(map[string]float64)} }
func (lg *LabeledGauge) Set(label string, v float64) {
	lg.mu.Lock(); lg.values[label] = v; lg.mu.Unlock()
}
func (lg *LabeledGauge) GetAll() map[string]float64 {
	lg.mu.RLock(); defer lg.mu.RUnlock()
	out := make(map[string]float64, len(lg.values))
	for k, v := range lg.values { out[k] = v }
	return out
}

// LabeledCounter holds counters keyed by label
type LabeledCounter struct {
	mu     sync.RWMutex
	values map[string]uint64
}

func NewLabeledCounter() *LabeledCounter { return &LabeledCounter{values: make(map[string]uint64)} }
func (lc *LabeledCounter) Add(label string, v uint64) {
	lc.mu.Lock(); lc.values[label] += v; lc.mu.Unlock()
}
func (lc *LabeledCounter) GetAll() map[string]uint64 {
	lc.mu.RLock(); defer lc.mu.RUnlock()
	out := make(map[string]uint64, len(lc.values))
	for k, v := range lc.values { out[k] = v }
	return out
}

// Registry holds all PiPool metrics
type Registry struct {
	// System
	CPUTempC    Gauge
	CPUUsagePct Gauge
	RAMUsedGB   Gauge
	UptimeS     Gauge

	// Per-coin (label = coin symbol e.g. "LTC")
	ConnectedMiners LabeledGauge
	HashrateKHs     LabeledGauge
	BlocksFound     LabeledCounter
	ValidShares     LabeledCounter
	TotalShares     LabeledCounter
	NetworkDiff     LabeledGauge
	BlockHeight     LabeledGauge

	// Per-device class (label = device name e.g. "Antminer S19")
	DeviceCount LabeledGauge

	startTime time.Time
}

// NewRegistry creates a new metrics registry
func NewRegistry() *Registry {
	return &Registry{
		ConnectedMiners: *NewLabeledGauge(),
		HashrateKHs:     *NewLabeledGauge(),
		BlocksFound:     *NewLabeledCounter(),
		ValidShares:     *NewLabeledCounter(),
		TotalShares:     *NewLabeledCounter(),
		NetworkDiff:     *NewLabeledGauge(),
		BlockHeight:     *NewLabeledGauge(),
		DeviceCount:     *NewLabeledGauge(),
		startTime:       time.Now(),
	}
}

// Handler returns an HTTP handler that serves Prometheus text format metrics
func (r *Registry) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		var b strings.Builder

		r.UptimeS.Set(time.Since(r.startTime).Seconds())

		// ── System metrics ────────────────────────────────────────────────────
		writeMeta(&b, "pipool_cpu_temp_celsius", "gauge", "CPU temperature in degrees Celsius")
		writeSample(&b, "pipool_cpu_temp_celsius", nil, r.CPUTempC.Get())

		writeMeta(&b, "pipool_cpu_usage_percent", "gauge", "CPU usage percentage")
		writeSample(&b, "pipool_cpu_usage_percent", nil, r.CPUUsagePct.Get())

		writeMeta(&b, "pipool_ram_used_gigabytes", "gauge", "RAM used in gigabytes")
		writeSample(&b, "pipool_ram_used_gigabytes", nil, r.RAMUsedGB.Get())

		writeMeta(&b, "pipool_uptime_seconds", "counter", "Seconds since pool started")
		writeSample(&b, "pipool_uptime_seconds", nil, r.UptimeS.Get())

		// ── Per-coin metrics ──────────────────────────────────────────────────
		writeMeta(&b, "pipool_connected_miners", "gauge", "Number of currently connected miners per coin")
		for coin, v := range r.ConnectedMiners.GetAll() {
			writeSample(&b, "pipool_connected_miners", map[string]string{"coin": coin}, v)
		}

		writeMeta(&b, "pipool_hashrate_khs", "gauge", "Estimated pool hashrate in KH/s per coin")
		for coin, v := range r.HashrateKHs.GetAll() {
			writeSample(&b, "pipool_hashrate_khs", map[string]string{"coin": coin}, v)
		}

		writeMeta(&b, "pipool_blocks_found_total", "counter", "Total blocks found per coin")
		for coin, v := range r.BlocksFound.GetAll() {
			writeSampleUint(&b, "pipool_blocks_found_total", map[string]string{"coin": coin}, v)
		}

		writeMeta(&b, "pipool_valid_shares_total", "counter", "Total valid shares submitted per coin")
		for coin, v := range r.ValidShares.GetAll() {
			writeSampleUint(&b, "pipool_valid_shares_total", map[string]string{"coin": coin}, v)
		}

		writeMeta(&b, "pipool_total_shares_total", "counter", "Total shares (valid + invalid) per coin")
		for coin, v := range r.TotalShares.GetAll() {
			writeSampleUint(&b, "pipool_total_shares_total", map[string]string{"coin": coin}, v)
		}

		writeMeta(&b, "pipool_network_difficulty", "gauge", "Current network difficulty per coin")
		for coin, v := range r.NetworkDiff.GetAll() {
			writeSample(&b, "pipool_network_difficulty", map[string]string{"coin": coin}, v)
		}

		writeMeta(&b, "pipool_block_height", "gauge", "Current block height per coin")
		for coin, v := range r.BlockHeight.GetAll() {
			writeSample(&b, "pipool_block_height", map[string]string{"coin": coin}, v)
		}

		// ── Per-device metrics ────────────────────────────────────────────────
		writeMeta(&b, "pipool_device_count", "gauge", "Number of connected miners per device class")
		for device, v := range r.DeviceCount.GetAll() {
			writeSample(&b, "pipool_device_count", map[string]string{"device": device}, v)
		}

		fmt.Fprint(w, b.String())
	}
}

// ─── Prometheus text format helpers ──────────────────────────────────────────

func writeMeta(b *strings.Builder, name, kind, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

func writeSample(b *strings.Builder, name string, labels map[string]string, value float64) {
	fmt.Fprintf(b, "%s%s %g\n", name, formatLabels(labels), value)
}

func writeSampleUint(b *strings.Builder, name string, labels map[string]string, value uint64) {
	fmt.Fprintf(b, "%s%s %d\n", name, formatLabels(labels), value)
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
