// Package pplns implements Pay-Per-Last-N-Shares proportional payout logic.
// When enabled, it tracks shares submitted by each worker within a sliding
// window and distributes block rewards proportionally when blocks are found.
// State is persisted to disk so pending balances survive restarts.
package pplns

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ────────────────────────────────────────────────────────────────────────────
// Configuration
// ────────────────────────────────────────────────────────────────────────────

// Config controls the PPLNS payout engine behaviour.
type Config struct {
	Enabled        bool               `json:"enabled"`
	WindowSize     int                `json:"window_size"`          // N in PPLNS (default 100000)
	PoolFeeRate    float64            `json:"pool_fee_pct"`         // pool operator fee 0-100 (default 1.0 = 1%)
	MinPayout      map[string]float64 `json:"min_payout"`           // coin -> minimum payout amount
	PayoutInterval int                `json:"payout_interval_min"`  // how often to process payouts (default 60)
}

// defaults fills zero-valued fields with sane defaults.
func (c *Config) defaults() {
	if c.WindowSize <= 0 {
		c.WindowSize = 100000
	}
	if c.PoolFeeRate < 0 {
		c.PoolFeeRate = 0
	}
	if c.PoolFeeRate > 100 {
		c.PoolFeeRate = 100
	}
	if c.PayoutInterval <= 0 {
		c.PayoutInterval = 60
	}
	if c.MinPayout == nil {
		c.MinPayout = make(map[string]float64)
	}
}

// ────────────────────────────────────────────────────────────────────────────
// CoinRPC interface — implemented by the caller (e.g. rpc.Client wrapper)
// ────────────────────────────────────────────────────────────────────────────

// CoinRPC is the minimal interface the payout engine needs to send coins.
type CoinRPC interface {
	SendToAddress(addr string, amount float64) (txid string, err error)
}

// ────────────────────────────────────────────────────────────────────────────
// Share ring buffer
// ────────────────────────────────────────────────────────────────────────────

// Share records a single accepted share from a worker.
type Share struct {
	WorkerAddr string    `json:"worker_addr"`
	Coin       string    `json:"coin"`
	Difficulty float64   `json:"difficulty"`
	Timestamp  time.Time `json:"timestamp"`
}

// ringBuffer is a fixed-capacity circular buffer of shares.
type ringBuffer struct {
	buf   []Share
	cap   int
	head  int // next write position
	count int // number of valid entries (up to cap)
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{
		buf: make([]Share, capacity),
		cap: capacity,
	}
}

// push appends a share, overwriting the oldest entry when full.
func (r *ringBuffer) push(s Share) {
	r.buf[r.head] = s
	r.head = (r.head + 1) % r.cap
	if r.count < r.cap {
		r.count++
	}
}

// snapshot returns all valid shares in insertion order (oldest first).
// The returned slice is a copy safe for concurrent reads.
func (r *ringBuffer) snapshot() []Share {
	if r.count == 0 {
		return nil
	}
	out := make([]Share, r.count)
	if r.count < r.cap {
		// Buffer has not wrapped yet; entries are at [0..count).
		copy(out, r.buf[:r.count])
	} else {
		// Buffer has wrapped; oldest entry is at head.
		n := copy(out, r.buf[r.head:r.cap])
		copy(out[n:], r.buf[:r.head])
	}
	return out
}

// len returns the number of valid entries.
func (r *ringBuffer) len() int {
	return r.count
}

// ────────────────────────────────────────────────────────────────────────────
// Payout record
// ────────────────────────────────────────────────────────────────────────────

// PayoutRecord tracks a completed payout.
type PayoutRecord struct {
	Coin       string    `json:"coin"`
	WorkerAddr string    `json:"worker_addr"`
	Amount     float64   `json:"amount"`
	TxID       string    `json:"txid"`
	PaidAt     time.Time `json:"paid_at"`
}

// ────────────────────────────────────────────────────────────────────────────
// Status (dashboard)
// ────────────────────────────────────────────────────────────────────────────

// WorkerBalance is the pending balance for one worker on one coin.
type WorkerBalance struct {
	WorkerAddr string  `json:"worker_addr"`
	Coin       string  `json:"coin"`
	Pending    float64 `json:"pending"`
	TotalPaid  float64 `json:"total_paid"`
	SharePct   float64 `json:"share_pct"` // current share of the window, 0-100
}

// PPLNSStatus is the snapshot returned by Engine.Status for the dashboard.
type PPLNSStatus struct {
	Enabled       bool            `json:"enabled"`
	WindowSize    int             `json:"window_size"`
	WindowFilled  int             `json:"window_filled"`
	PoolFeeRate   float64         `json:"pool_fee_pct"`
	Workers       []WorkerBalance `json:"workers"`
	RecentPayouts []PayoutRecord  `json:"recent_payouts"`
	TotalPending  map[string]float64 `json:"total_pending"` // coin -> total pending
}

// ────────────────────────────────────────────────────────────────────────────
// Persisted state
// ────────────────────────────────────────────────────────────────────────────

const (
	stateVersion = 1
	statePath    = "/opt/pipool/pplns_state.json"
	maxPayoutLog = 200 // keep last N payout records
)

type persistedState struct {
	Version  int                        `json:"version"`
	Saved    time.Time                  `json:"saved"`
	Shares   []Share                    `json:"shares"`
	Balances map[string]map[string]float64 `json:"balances"`      // coin -> addr -> amount
	Paid     map[string]map[string]float64 `json:"total_paid"`    // coin -> addr -> total paid
	Payouts  []PayoutRecord             `json:"recent_payouts"`
}

// ────────────────────────────────────────────────────────────────────────────
// Engine
// ────────────────────────────────────────────────────────────────────────────

// Engine is the PPLNS payout engine. All methods are safe for concurrent use.
type Engine struct {
	mu sync.RWMutex

	cfg Config

	// Per-coin share windows. Key is uppercase coin symbol.
	windows map[string]*ringBuffer

	// Pending balances: coin -> workerAddr -> amount owed.
	balances map[string]map[string]float64

	// Lifetime paid totals: coin -> workerAddr -> total amount paid.
	paid map[string]map[string]float64

	// Recent payout log (newest first).
	payouts []PayoutRecord

	// Lifecycle.
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// New creates a PPLNS payout engine with the given configuration.
// If cfg.Enabled is false the engine will accept calls but do nothing.
func New(cfg Config) *Engine {
	cfg.defaults()
	e := &Engine{
		cfg:      cfg,
		windows:  make(map[string]*ringBuffer),
		balances: make(map[string]map[string]float64),
		paid:     make(map[string]map[string]float64),
		stopCh:   make(chan struct{}),
	}
	e.loadState()
	return e
}

// ────────────────────────────────────────────────────────────────────────────
// Lifecycle
// ────────────────────────────────────────────────────────────────────────────

// Start begins the background payout processing loop. It is a no-op if
// PPLNS is disabled.
func (e *Engine) Start() {
	if !e.cfg.Enabled {
		log.Printf("[pplns] disabled, not starting payout loop")
		return
	}
	e.wg.Add(1)
	go e.loop()
	log.Printf("[pplns] started  window=%d  fee=%.2f%%  interval=%dm",
		e.cfg.WindowSize, e.cfg.PoolFeeRate, e.cfg.PayoutInterval)
}

// Stop gracefully shuts down the background loop and persists state.
func (e *Engine) Stop() {
	close(e.stopCh)
	e.wg.Wait()
	e.persist()
	log.Printf("[pplns] stopped, state persisted")
}

// loop is the background goroutine that periodically persists state.
// Actual payout execution happens via the explicit ProcessPayouts call so the
// caller can inject the correct RPC clients.
func (e *Engine) loop() {
	defer e.wg.Done()
	ticker := time.NewTicker(time.Duration(e.cfg.PayoutInterval) * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.persist()
		case <-e.stopCh:
			return
		}
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Share recording
// ────────────────────────────────────────────────────────────────────────────

// RecordShare records a valid share from a worker. It is called on every
// accepted share submission. If PPLNS is disabled this is a no-op.
func (e *Engine) RecordShare(coin, workerAddr string, difficulty float64) {
	if !e.cfg.Enabled {
		return
	}
	if coin == "" || workerAddr == "" || difficulty <= 0 {
		return
	}

	s := Share{
		WorkerAddr: workerAddr,
		Coin:       coin,
		Difficulty: difficulty,
		Timestamp:  time.Now(),
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	rb, ok := e.windows[coin]
	if !ok {
		rb = newRingBuffer(e.cfg.WindowSize)
		e.windows[coin] = rb
	}
	rb.push(s)
}

// ────────────────────────────────────────────────────────────────────────────
// Block found — proportional reward distribution
// ────────────────────────────────────────────────────────────────────────────

// BlockFound is called when a block is found for a given coin. It snapshots
// the current share window, computes each worker's proportional share of the
// reward (after the pool fee), and credits their pending balance.
func (e *Engine) BlockFound(coin string, reward float64) {
	if !e.cfg.Enabled {
		return
	}
	if reward <= 0 {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	rb, ok := e.windows[coin]
	if !ok || rb.len() == 0 {
		log.Printf("[pplns] block found for %s but no shares in window, skipping distribution", coin)
		return
	}

	shares := rb.snapshot()

	// Sum difficulty per worker for this coin.
	workerDiff := make(map[string]float64)
	var totalDiff float64
	for _, s := range shares {
		if s.Coin != coin {
			continue
		}
		workerDiff[s.WorkerAddr] += s.Difficulty
		totalDiff += s.Difficulty
	}

	if totalDiff == 0 {
		log.Printf("[pplns] block found for %s but zero difficulty in window, skipping", coin)
		return
	}

	// Deduct pool fee.
	feeRate := e.cfg.PoolFeeRate / 100.0
	distributable := reward * (1.0 - feeRate)
	poolFee := reward - distributable

	log.Printf("[pplns] %s block reward=%.8f  fee=%.8f (%.2f%%)  distributing=%.8f across %d worker(s)",
		coin, reward, poolFee, e.cfg.PoolFeeRate, distributable, len(workerDiff))

	// Credit each worker proportionally.
	if e.balances[coin] == nil {
		e.balances[coin] = make(map[string]float64)
	}
	for addr, diff := range workerDiff {
		share := diff / totalDiff
		credit := roundTo(distributable*share, 8)
		e.balances[coin][addr] += credit
		log.Printf("[pplns]   %s  %.2f%% -> +%.8f %s (pending: %.8f)",
			addr, share*100, credit, coin, e.balances[coin][addr])
	}

	// Persist after distribution.
	go e.persist()
}

// ────────────────────────────────────────────────────────────────────────────
// Payout execution
// ────────────────────────────────────────────────────────────────────────────

// ProcessPayouts iterates over all pending balances and sends payments for any
// worker whose balance meets or exceeds the minimum payout threshold for that
// coin. The rpcs map is keyed by coin symbol.
func (e *Engine) ProcessPayouts(rpcs map[string]CoinRPC) {
	if !e.cfg.Enabled {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for coin, addrMap := range e.balances {
		rpc, ok := rpcs[coin]
		if !ok {
			continue // no RPC client for this coin
		}

		minPayout := e.cfg.MinPayout[coin]
		if minPayout <= 0 {
			// Sane fallback: 0.001 to avoid dust.
			minPayout = 0.001
		}

		for addr, balance := range addrMap {
			if balance < minPayout {
				continue
			}

			amount := roundTo(balance, 8)
			txid, err := rpc.SendToAddress(addr, amount)
			if err != nil {
				log.Printf("[pplns] SendToAddress(%s, %.8f %s) failed: %v", addr, amount, coin, err)
				continue
			}

			log.Printf("[pplns] paid %.8f %s -> %s  txid=%s", amount, coin, addr, txid)

			// Deduct from pending balance.
			addrMap[addr] -= amount
			if addrMap[addr] < 1e-8 {
				delete(addrMap, addr)
			}

			// Track lifetime paid total.
			if e.paid[coin] == nil {
				e.paid[coin] = make(map[string]float64)
			}
			e.paid[coin][addr] += amount

			// Record payout.
			rec := PayoutRecord{
				Coin:       coin,
				WorkerAddr: addr,
				Amount:     amount,
				TxID:       txid,
				PaidAt:     time.Now(),
			}
			e.payouts = append([]PayoutRecord{rec}, e.payouts...)
			if len(e.payouts) > maxPayoutLog {
				e.payouts = e.payouts[:maxPayoutLog]
			}
		}
	}

	go e.persist()
}

// ────────────────────────────────────────────────────────────────────────────
// Status (dashboard)
// ────────────────────────────────────────────────────────────────────────────

// Status returns a point-in-time snapshot of the PPLNS engine state for
// display on the dashboard.
func (e *Engine) Status() PPLNSStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()

	st := PPLNSStatus{
		Enabled:      e.cfg.Enabled,
		WindowSize:   e.cfg.WindowSize,
		PoolFeeRate:  e.cfg.PoolFeeRate,
		TotalPending: make(map[string]float64),
	}

	// Compute per-worker share percentages from current windows and collect
	// pending balances.
	type workerKey struct {
		addr, coin string
	}
	sharePcts := make(map[workerKey]float64)

	for coin, rb := range e.windows {
		shares := rb.snapshot()
		st.WindowFilled += rb.len()

		var totalDiff float64
		workerDiff := make(map[string]float64)
		for _, s := range shares {
			if s.Coin != coin {
				continue
			}
			workerDiff[s.WorkerAddr] += s.Difficulty
			totalDiff += s.Difficulty
		}
		if totalDiff > 0 {
			for addr, diff := range workerDiff {
				sharePcts[workerKey{addr, coin}] = roundTo((diff/totalDiff)*100, 2)
			}
		}
	}

	// Build worker list from pending balances (union with share window workers).
	seen := make(map[workerKey]bool)
	addWorker := func(coin, addr string) {
		key := workerKey{addr, coin}
		if seen[key] {
			return
		}
		seen[key] = true

		pending := e.balances[coin][addr]
		totalPaid := 0.0
		if e.paid[coin] != nil {
			totalPaid = e.paid[coin][addr]
		}

		st.Workers = append(st.Workers, WorkerBalance{
			WorkerAddr: addr,
			Coin:       coin,
			Pending:    roundTo(pending, 8),
			TotalPaid:  roundTo(totalPaid, 8),
			SharePct:   sharePcts[key],
		})
	}

	for coin, addrMap := range e.balances {
		for addr := range addrMap {
			addWorker(coin, addr)
		}
		var total float64
		for _, amt := range addrMap {
			total += amt
		}
		st.TotalPending[coin] = roundTo(total, 8)
	}
	for coin, rb := range e.windows {
		for _, s := range rb.snapshot() {
			if s.Coin == coin {
				addWorker(coin, s.WorkerAddr)
			}
		}
	}

	// Copy recent payouts.
	st.RecentPayouts = make([]PayoutRecord, len(e.payouts))
	copy(st.RecentPayouts, e.payouts)

	return st
}

// ────────────────────────────────────────────────────────────────────────────
// Persistence
// ────────────────────────────────────────────────────────────────────────────

// persist writes the current engine state to disk atomically (write-tmp +
// rename). Safe to call from any goroutine; acquires a read lock internally
// when called without the lock held, but callers under lock should use
// persistLocked.
func (e *Engine) persist() {
	e.mu.RLock()
	state := e.buildPersistState()
	e.mu.RUnlock()

	e.writeState(state)
}

// buildPersistState constructs the on-disk representation. Caller must hold
// at least a read lock.
func (e *Engine) buildPersistState() persistedState {
	ps := persistedState{
		Version:  stateVersion,
		Saved:    time.Now(),
		Balances: make(map[string]map[string]float64, len(e.balances)),
		Paid:     make(map[string]map[string]float64, len(e.paid)),
	}

	// Serialize share windows.
	for coin, rb := range e.windows {
		shares := rb.snapshot()
		for _, s := range shares {
			if s.Coin == coin {
				ps.Shares = append(ps.Shares, s)
			}
		}
	}

	// Deep-copy balances.
	for coin, addrMap := range e.balances {
		m := make(map[string]float64, len(addrMap))
		for addr, amt := range addrMap {
			m[addr] = amt
		}
		ps.Balances[coin] = m
	}

	// Deep-copy paid totals.
	for coin, addrMap := range e.paid {
		m := make(map[string]float64, len(addrMap))
		for addr, amt := range addrMap {
			m[addr] = amt
		}
		ps.Paid[coin] = m
	}

	ps.Payouts = make([]PayoutRecord, len(e.payouts))
	copy(ps.Payouts, e.payouts)

	return ps
}

// writeState marshals and atomically writes the state file.
func (e *Engine) writeState(ps persistedState) {
	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		log.Printf("[pplns] marshal error: %v", err)
		return
	}

	dir := filepath.Dir(statePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[pplns] cannot create directory %s: %v", dir, err)
		return
	}

	tmp := statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("[pplns] write error: %v", err)
		return
	}
	if err := os.Rename(tmp, statePath); err != nil {
		log.Printf("[pplns] rename error: %v", err)
	}
}

// loadState restores engine state from the persisted JSON file.
func (e *Engine) loadState() {
	data, err := os.ReadFile(statePath)
	if err != nil {
		return // first run or file missing, start fresh
	}

	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		log.Printf("[pplns] failed to parse %s: %v", statePath, err)
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Restore share windows from persisted shares.
	// Group shares by coin and replay them in order.
	coinShares := make(map[string][]Share)
	for _, s := range ps.Shares {
		coinShares[s.Coin] = append(coinShares[s.Coin], s)
	}
	for coin, shares := range coinShares {
		rb := newRingBuffer(e.cfg.WindowSize)
		for _, s := range shares {
			rb.push(s)
		}
		e.windows[coin] = rb
	}

	// Restore balances.
	for coin, addrMap := range ps.Balances {
		m := make(map[string]float64, len(addrMap))
		for addr, amt := range addrMap {
			m[addr] = amt
		}
		e.balances[coin] = m
	}

	// Restore paid totals.
	for coin, addrMap := range ps.Paid {
		m := make(map[string]float64, len(addrMap))
		for addr, amt := range addrMap {
			m[addr] = amt
		}
		e.paid[coin] = m
	}

	// Restore payout log.
	e.payouts = make([]PayoutRecord, len(ps.Payouts))
	copy(e.payouts, ps.Payouts)

	totalShares := 0
	for _, rb := range e.windows {
		totalShares += rb.len()
	}
	totalPending := 0
	for _, addrMap := range e.balances {
		totalPending += len(addrMap)
	}

	log.Printf("[pplns] restored state: %d share(s) across %d coin(s), %d pending balance(s), %d payout record(s)",
		totalShares, len(e.windows), totalPending, len(e.payouts))
}

// ────────────────────────────────────────────────────────────────────────────
// Helpers
// ────────────────────────────────────────────────────────────────────────────

// roundTo rounds a float to n decimal places.
func roundTo(v float64, n int) float64 {
	pow := math.Pow(10, float64(n))
	return math.Round(v*pow) / pow
}

// String returns a human-readable summary for logging.
func (e *Engine) String() string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	totalShares := 0
	for _, rb := range e.windows {
		totalShares += rb.len()
	}
	return fmt.Sprintf("PPLNS(enabled=%t window=%d shares=%d coins=%d)",
		e.cfg.Enabled, e.cfg.WindowSize, totalShares, len(e.windows))
}
