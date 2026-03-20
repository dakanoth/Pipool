// Package earnings aggregates block rewards across all coins into lifetime
// earnings statistics. It reads the per-coin blocklog.json files written by
// the stratum server and maintains an aggregate snapshot suitable for the
// dashboard.
package earnings

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ────────────────────────────────────────────────────────────────────────────
// On-disk format (mirrors stratum/server.go blockLogFile)
// ────────────────────────────────────────────────────────────────────────────

type blockLogFileEntry struct {
	Coin           string    `json:"coin"`
	Height         int64     `json:"height"`
	Hash           string    `json:"hash"`
	Reward         string    `json:"reward"`
	Worker         string    `json:"worker"`
	FoundAt        time.Time `json:"found_at"`
	Confirmations  int64     `json:"confirmations"`
	IsOrphaned     bool      `json:"is_orphaned"`
	IsAux          bool      `json:"is_aux"`
	AuxParentCoin  string    `json:"aux_parent_coin,omitempty"`
	BlockLuck      float64   `json:"luck"`
	MatureNotified bool      `json:"mature_notified"`
}

type blockLogFile struct {
	Version int                 `json:"version"`
	Saved   time.Time           `json:"saved"`
	Entries []blockLogFileEntry `json:"entries"`
}

// ────────────────────────────────────────────────────────────────────────────
// Public snapshot types (returned to the dashboard)
// ────────────────────────────────────────────────────────────────────────────

// CoinStats holds lifetime statistics for a single coin.
type CoinStats struct {
	Symbol          string  `json:"symbol"`
	BlocksFound     int     `json:"blocks_found"`
	BlocksConfirmed int     `json:"blocks_confirmed"`
	BlocksOrphaned  int     `json:"blocks_orphaned"`
	TotalReward     float64 `json:"total_reward"`
	TotalUSD        float64 `json:"total_usd"`
	AvgLuck         float64 `json:"avg_luck"`
	BestLuck        float64 `json:"best_luck"`
	WorstLuck       float64 `json:"worst_luck"`
	LastBlockAt     string  `json:"last_block_at,omitempty"`
	LastBlockHeight int64   `json:"last_block_height,omitempty"`
}

// AllTimeStats holds totals across every coin.
type AllTimeStats struct {
	TotalBlocksFound int     `json:"total_blocks_found"`
	TotalUSD         float64 `json:"total_usd"`
	FirstBlockAt     string  `json:"first_block_at,omitempty"`
}

// RecentBlock is one entry in the recent-blocks list.
type RecentBlock struct {
	Coin          string  `json:"coin"`
	Height        int64   `json:"height"`
	Hash          string  `json:"hash"`
	Reward        float64 `json:"reward"`
	RewardLabel   string  `json:"reward_label"`
	Worker        string  `json:"worker"`
	FoundAt       string  `json:"found_at"`
	Confirmations int64   `json:"confirmations"`
	IsOrphaned    bool    `json:"is_orphaned"`
	Luck          float64 `json:"luck"`
}

// EarningsSnapshot is the complete snapshot served to the dashboard.
type EarningsSnapshot struct {
	PerCoin      []CoinStats   `json:"per_coin"`
	AllTime      AllTimeStats  `json:"all_time"`
	RecentBlocks []RecentBlock `json:"recent_blocks"`
}

// ────────────────────────────────────────────────────────────────────────────
// Internal tracked block
// ────────────────────────────────────────────────────────────────────────────

type trackedBlock struct {
	Coin          string
	Height        int64
	Hash          string
	Reward        float64
	RewardLabel   string
	Worker        string
	FoundAt       time.Time
	Confirmations int64
	IsOrphaned    bool
	Luck          float64
}

// ────────────────────────────────────────────────────────────────────────────
// Persisted aggregate state (earnings.json)
// ────────────────────────────────────────────────────────────────────────────

type persistedState struct {
	Version    int                       `json:"version"`
	Saved      time.Time                 `json:"saved"`
	CoinStats  map[string]*persistedCoin `json:"coin_stats"`
	FirstBlock time.Time                 `json:"first_block"`
	Blocks     []persistedBlock          `json:"blocks"`
}

type persistedCoin struct {
	BlocksFound     int     `json:"blocks_found"`
	BlocksConfirmed int     `json:"blocks_confirmed"`
	BlocksOrphaned  int     `json:"blocks_orphaned"`
	TotalReward     float64 `json:"total_reward"`
	LuckSum         float64 `json:"luck_sum"`
	LuckCount       int     `json:"luck_count"`
	BestLuck        float64 `json:"best_luck"`
	WorstLuck       float64 `json:"worst_luck"`
	LastBlockAt     time.Time `json:"last_block_at"`
	LastBlockHeight int64   `json:"last_block_height"`
}

type persistedBlock struct {
	Coin          string    `json:"coin"`
	Height        int64     `json:"height"`
	Hash          string    `json:"hash"`
	Reward        float64   `json:"reward"`
	RewardLabel   string    `json:"reward_label"`
	Worker        string    `json:"worker"`
	FoundAt       time.Time `json:"found_at"`
	Confirmations int64     `json:"confirmations"`
	IsOrphaned    bool      `json:"is_orphaned"`
	Luck          float64   `json:"luck"`
}

// ────────────────────────────────────────────────────────────────────────────
// EarningsTracker
// ────────────────────────────────────────────────────────────────────────────

const (
	maxRecentBlocks   = 100
	defaultMaturity   = 120
	persistVersion    = 1
)

// PriceFn returns the current USD price for a coin symbol.
type PriceFn func(symbol string) float64

// EarningsTracker aggregates block data across all coins into lifetime
// earnings statistics.
type EarningsTracker struct {
	mu sync.RWMutex

	// Per-coin aggregate counters (keyed by uppercase symbol).
	coins map[string]*coinAccum

	// All tracked blocks, most-recent first. Capped at maxRecentBlocks.
	recent []trackedBlock

	// Earliest block across all coins.
	firstBlock time.Time

	// External price lookup injected by the caller.
	priceFn PriceFn

	// Where to persist aggregate stats.
	persistPath string

	// Glob pattern for discovering blocklog files.
	blocklogGlob string
}

// coinAccum is the mutable accumulator for one coin.
type coinAccum struct {
	blocksFound     int
	blocksConfirmed int
	blocksOrphaned  int
	totalReward     float64
	luckSum         float64
	luckCount       int
	bestLuck        float64
	worstLuck       float64
	lastBlockAt     time.Time
	lastBlockHeight int64
}

// New creates an EarningsTracker.
//
//   - blocklogGlob is the glob pattern for discovering block log files,
//     e.g. "/opt/pipool/worker_state_blocklog_*.json".
//   - persistPath is where the aggregated earnings state is written,
//     e.g. "/opt/pipool/earnings.json".
//   - priceFn is called at snapshot time to get current USD prices.
func New(blocklogGlob, persistPath string, priceFn PriceFn) *EarningsTracker {
	if priceFn == nil {
		priceFn = func(string) float64 { return 0 }
	}
	t := &EarningsTracker{
		coins:        make(map[string]*coinAccum),
		priceFn:      priceFn,
		persistPath:  persistPath,
		blocklogGlob: blocklogGlob,
	}
	t.loadPersisted()
	t.loadBlocklogs()
	return t
}

// ────────────────────────────────────────────────────────────────────────────
// Loading
// ────────────────────────────────────────────────────────────────────────────

// loadPersisted restores aggregate state from the persisted earnings.json.
func (t *EarningsTracker) loadPersisted() {
	data, err := os.ReadFile(t.persistPath)
	if err != nil {
		return // first run, nothing to load
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("[earnings] failed to parse %s: %v", t.persistPath, err)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	t.firstBlock = state.FirstBlock

	for sym, pc := range state.CoinStats {
		t.coins[sym] = &coinAccum{
			blocksFound:     pc.BlocksFound,
			blocksConfirmed: pc.BlocksConfirmed,
			blocksOrphaned:  pc.BlocksOrphaned,
			totalReward:     pc.TotalReward,
			luckSum:         pc.LuckSum,
			luckCount:       pc.LuckCount,
			bestLuck:        pc.BestLuck,
			worstLuck:       pc.WorstLuck,
			lastBlockAt:     pc.LastBlockAt,
			lastBlockHeight: pc.LastBlockHeight,
		}
	}

	for _, pb := range state.Blocks {
		t.recent = append(t.recent, trackedBlock{
			Coin:          pb.Coin,
			Height:        pb.Height,
			Hash:          pb.Hash,
			Reward:        pb.Reward,
			RewardLabel:   pb.RewardLabel,
			Worker:        pb.Worker,
			FoundAt:       pb.FoundAt,
			Confirmations: pb.Confirmations,
			IsOrphaned:    pb.IsOrphaned,
			Luck:          pb.Luck,
		})
	}

	log.Printf("[earnings] restored %d coin(s), %d recent block(s) from %s",
		len(t.coins), len(t.recent), t.persistPath)
}

// loadBlocklogs reads all blocklog JSON files and merges any blocks not
// already tracked. This ensures we pick up historical blocks that were found
// before the earnings tracker existed.
func (t *EarningsTracker) loadBlocklogs() {
	matches, err := filepath.Glob(t.blocklogGlob)
	if err != nil {
		log.Printf("[earnings] glob error for %q: %v", t.blocklogGlob, err)
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Build a set of already-known hashes for dedup.
	known := make(map[string]bool, len(t.recent))
	for _, b := range t.recent {
		known[b.Hash] = true
	}

	imported := 0
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[earnings] cannot read %s: %v", path, err)
			continue
		}
		var f blockLogFile
		if err := json.Unmarshal(data, &f); err != nil {
			log.Printf("[earnings] cannot parse %s: %v", path, err)
			continue
		}
		for _, e := range f.Entries {
			if known[e.Hash] {
				continue
			}
			known[e.Hash] = true

			reward := parseReward(e.Reward)
			luck := e.BlockLuck
			sym := strings.ToUpper(e.Coin)

			acc := t.getOrCreateCoinLocked(sym)
			acc.blocksFound++
			if e.IsOrphaned {
				acc.blocksOrphaned++
			} else if e.Confirmations > 0 {
				acc.blocksConfirmed++
				acc.totalReward += reward
			}
			if luck > 0 {
				acc.luckSum += luck
				acc.luckCount++
				if acc.bestLuck == 0 || luck > acc.bestLuck {
					acc.bestLuck = luck
				}
				if acc.worstLuck == 0 || luck < acc.worstLuck {
					acc.worstLuck = luck
				}
			}
			if e.FoundAt.After(acc.lastBlockAt) {
				acc.lastBlockAt = e.FoundAt
				acc.lastBlockHeight = e.Height
			}
			if t.firstBlock.IsZero() || e.FoundAt.Before(t.firstBlock) {
				t.firstBlock = e.FoundAt
			}

			t.recent = append(t.recent, trackedBlock{
				Coin:          sym,
				Height:        e.Height,
				Hash:          e.Hash,
				Reward:        reward,
				RewardLabel:   e.Reward,
				Worker:        e.Worker,
				FoundAt:       e.FoundAt,
				Confirmations: e.Confirmations,
				IsOrphaned:    e.IsOrphaned,
				Luck:          luck,
			})
			imported++
		}
	}

	// Sort recent blocks newest-first and cap.
	sort.Slice(t.recent, func(i, j int) bool {
		return t.recent[i].FoundAt.After(t.recent[j].FoundAt)
	})
	if len(t.recent) > maxRecentBlocks {
		t.recent = t.recent[:maxRecentBlocks]
	}

	if imported > 0 {
		log.Printf("[earnings] imported %d block(s) from blocklog files", imported)
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Mutation methods (called by stratum/main when events occur)
// ────────────────────────────────────────────────────────────────────────────

// RecordBlock is called when a new block is found on any coin.
func (t *EarningsTracker) RecordBlock(coin string, height int64, hash string, reward float64, luck float64) {
	sym := strings.ToUpper(coin)
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	acc := t.getOrCreateCoinLocked(sym)
	acc.blocksFound++
	if luck > 0 {
		acc.luckSum += luck
		acc.luckCount++
		if acc.bestLuck == 0 || luck > acc.bestLuck {
			acc.bestLuck = luck
		}
		if acc.worstLuck == 0 || luck < acc.worstLuck {
			acc.worstLuck = luck
		}
	}
	acc.lastBlockAt = now
	acc.lastBlockHeight = height

	if t.firstBlock.IsZero() {
		t.firstBlock = now
	}

	label := fmt.Sprintf("%.4f %s", reward, sym)
	blk := trackedBlock{
		Coin:        sym,
		Height:      height,
		Hash:        hash,
		Reward:      reward,
		RewardLabel: label,
		FoundAt:     now,
		Luck:        luck,
	}

	// Prepend to recent list (newest first).
	t.recent = append([]trackedBlock{blk}, t.recent...)
	if len(t.recent) > maxRecentBlocks {
		t.recent = t.recent[:maxRecentBlocks]
	}

	// Persist asynchronously to avoid blocking the stratum hot path.
	go t.persist()
}

// UpdateConfirmations is called when the stratum confirmation tracker updates
// a block's maturity status.
func (t *EarningsTracker) UpdateConfirmations(coin, hash string, confirmations int64, orphaned bool) {
	sym := strings.ToUpper(coin)

	t.mu.Lock()
	defer t.mu.Unlock()

	acc := t.coins[sym]
	if acc == nil {
		return // unknown coin, nothing to update
	}

	for i := range t.recent {
		if t.recent[i].Hash != hash {
			continue
		}
		prev := t.recent[i]

		// Transition to orphaned.
		if orphaned && !prev.IsOrphaned {
			t.recent[i].IsOrphaned = true
			t.recent[i].Confirmations = -1
			acc.blocksOrphaned++
			// If it was previously counted as confirmed, undo the reward.
			if prev.Confirmations > 0 {
				acc.blocksConfirmed--
				acc.totalReward -= prev.Reward
			}
			go t.persist()
			return
		}

		// Transition from unconfirmed to confirmed (first confirmation).
		if !orphaned && prev.Confirmations == 0 && confirmations > 0 {
			acc.blocksConfirmed++
			acc.totalReward += prev.Reward
		}

		t.recent[i].Confirmations = confirmations
		go t.persist()
		return
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Snapshot (read-only, for the dashboard)
// ────────────────────────────────────────────────────────────────────────────

// Snapshot returns a point-in-time view of all earnings data, with USD values
// computed using the price function provided at construction time.
func (t *EarningsTracker) Snapshot() EarningsSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	snap := EarningsSnapshot{}

	totalFound := 0
	totalUSD := 0.0

	// Collect coin symbols in sorted order for deterministic output.
	syms := make([]string, 0, len(t.coins))
	for sym := range t.coins {
		syms = append(syms, sym)
	}
	sort.Strings(syms)

	for _, sym := range syms {
		acc := t.coins[sym]
		price := t.priceFn(sym)
		coinUSD := acc.totalReward * price

		var avgLuck float64
		if acc.luckCount > 0 {
			avgLuck = acc.luckSum / float64(acc.luckCount)
		}

		cs := CoinStats{
			Symbol:          sym,
			BlocksFound:     acc.blocksFound,
			BlocksConfirmed: acc.blocksConfirmed,
			BlocksOrphaned:  acc.blocksOrphaned,
			TotalReward:     roundTo(acc.totalReward, 8),
			TotalUSD:        roundTo(coinUSD, 2),
			AvgLuck:         roundTo(avgLuck, 1),
			BestLuck:        roundTo(acc.bestLuck, 1),
			WorstLuck:       roundTo(acc.worstLuck, 1),
		}
		if !acc.lastBlockAt.IsZero() {
			cs.LastBlockAt = acc.lastBlockAt.UTC().Format(time.RFC3339)
			cs.LastBlockHeight = acc.lastBlockHeight
		}
		snap.PerCoin = append(snap.PerCoin, cs)

		totalFound += acc.blocksFound
		totalUSD += coinUSD
	}

	snap.AllTime = AllTimeStats{
		TotalBlocksFound: totalFound,
		TotalUSD:         roundTo(totalUSD, 2),
	}
	if !t.firstBlock.IsZero() {
		snap.AllTime.FirstBlockAt = t.firstBlock.UTC().Format(time.RFC3339)
	}

	snap.RecentBlocks = make([]RecentBlock, len(t.recent))
	for i, b := range t.recent {
		snap.RecentBlocks[i] = RecentBlock{
			Coin:          b.Coin,
			Height:        b.Height,
			Hash:          b.Hash,
			Reward:        roundTo(b.Reward, 8),
			RewardLabel:   b.RewardLabel,
			Worker:        b.Worker,
			FoundAt:       b.FoundAt.UTC().Format(time.RFC3339),
			Confirmations: b.Confirmations,
			IsOrphaned:    b.IsOrphaned,
			Luck:          roundTo(b.Luck, 1),
		}
	}

	return snap
}

// ────────────────────────────────────────────────────────────────────────────
// Persistence
// ────────────────────────────────────────────────────────────────────────────

// persist writes the current aggregate state to disk. Safe to call from any
// goroutine; acquires a read lock internally.
func (t *EarningsTracker) persist() {
	t.mu.RLock()
	state := t.buildStateLocked()
	t.mu.RUnlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		log.Printf("[earnings] marshal error: %v", err)
		return
	}

	dir := filepath.Dir(t.persistPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[earnings] cannot create directory %s: %v", dir, err)
		return
	}

	tmp := t.persistPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("[earnings] write error: %v", err)
		return
	}
	if err := os.Rename(tmp, t.persistPath); err != nil {
		log.Printf("[earnings] rename error: %v", err)
	}
}

func (t *EarningsTracker) buildStateLocked() persistedState {
	state := persistedState{
		Version:    persistVersion,
		Saved:      time.Now(),
		CoinStats:  make(map[string]*persistedCoin, len(t.coins)),
		FirstBlock: t.firstBlock,
	}

	for sym, acc := range t.coins {
		state.CoinStats[sym] = &persistedCoin{
			BlocksFound:     acc.blocksFound,
			BlocksConfirmed: acc.blocksConfirmed,
			BlocksOrphaned:  acc.blocksOrphaned,
			TotalReward:     acc.totalReward,
			LuckSum:         acc.luckSum,
			LuckCount:       acc.luckCount,
			BestLuck:        acc.bestLuck,
			WorstLuck:       acc.worstLuck,
			LastBlockAt:     acc.lastBlockAt,
			LastBlockHeight: acc.lastBlockHeight,
		}
	}

	state.Blocks = make([]persistedBlock, len(t.recent))
	for i, b := range t.recent {
		state.Blocks[i] = persistedBlock{
			Coin:          b.Coin,
			Height:        b.Height,
			Hash:          b.Hash,
			Reward:        b.Reward,
			RewardLabel:   b.RewardLabel,
			Worker:        b.Worker,
			FoundAt:       b.FoundAt,
			Confirmations: b.Confirmations,
			IsOrphaned:    b.IsOrphaned,
			Luck:          b.Luck,
		}
	}

	return state
}

// ────────────────────────────────────────────────────────────────────────────
// Helpers
// ────────────────────────────────────────────────────────────────────────────

// getOrCreateCoinLocked returns the accumulator for a symbol, creating it if
// needed. Caller must hold t.mu (read or write).
func (t *EarningsTracker) getOrCreateCoinLocked(sym string) *coinAccum {
	acc, ok := t.coins[sym]
	if !ok {
		acc = &coinAccum{}
		t.coins[sym] = acc
	}
	return acc
}

// parseReward extracts the numeric reward from a label like "3.1250 DGB".
func parseReward(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	parts := strings.Fields(s)
	v, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}
	return v
}

// roundTo rounds a float to n decimal places.
func roundTo(v float64, n int) float64 {
	pow := math.Pow(10, float64(n))
	return math.Round(v*pow) / pow
}
