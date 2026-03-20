// Package hashstore persists historical hashrate samples to disk with
// automatic downsampling so the dashboard can render 24h / 7d / 30d trends.
//
// Resolution tiers:
//
//	age < 1h   → raw 15-second samples (kept as-is)
//	age 1h–24h → 1-minute buckets   (≤1 440 points per coin)
//	age 24h–7d → 5-minute buckets   (≤2 016 points per coin)
//	age 7d–30d → 15-minute buckets  (≤2 880 points per coin)
//	age > 30d  → deleted
package hashstore

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Sample is a single hashrate data point.  Field names are kept short to
// minimise on-disk JSON size.
type Sample struct {
	KHs    float64 `json:"k"`
	TimeMS int64   `json:"t"`
}

// Store holds per-coin hashrate time-series and handles persistence.
type Store struct {
	mu        sync.RWMutex
	samples   map[string][]Sample // coin → time-ordered samples
	stateFile string
	lastSave  time.Time
	lastComp  time.Time
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

// persistState is the JSON-serialised representation of a Store.
type persistState struct {
	Samples map[string][]Sample `json:"s"`
}

// ---------------------------------------------------------------------------
// Construction / lifecycle
// ---------------------------------------------------------------------------

// New creates a Store that persists to stateFile.  It attempts to load
// existing history from that path; if the file is missing or corrupt,
// it starts with an empty dataset.
func New(stateFile string) *Store {
	s := &Store{
		samples:   make(map[string][]Sample),
		stateFile: stateFile,
		lastSave:  time.Now(),
		lastComp:  time.Now(),
		stopCh:    make(chan struct{}),
	}
	s.load()

	// Background goroutine: compact + save every 5 minutes.
	s.wg.Add(1)
	go s.backgroundLoop()

	return s
}

// Close stops the background goroutine and performs a final save.
func (s *Store) Close() {
	close(s.stopCh)
	s.wg.Wait()
	s.Save()
}

// backgroundLoop runs compaction and persistence on a 5-minute tick.
func (s *Store) backgroundLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.compact()
			s.Save()
		case <-s.stopCh:
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Ingestion
// ---------------------------------------------------------------------------

// Add records a hashrate sample for the given coin.
func (s *Store) Add(coin string, khs float64, timeMS int64) {
	s.mu.Lock()
	s.samples[coin] = append(s.samples[coin], Sample{KHs: khs, TimeMS: timeMS})
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

// Query returns samples for coin within the last windowHours.
// The returned slice is a copy, safe for concurrent use.
func (s *Store) Query(coin string, windowHours int) []Sample {
	cutoff := time.Now().Add(-time.Duration(windowHours) * time.Hour).UnixMilli()
	s.mu.RLock()
	src := s.samples[coin]
	// Binary-search for the start index (samples are time-ordered).
	idx := sort.Search(len(src), func(i int) bool { return src[i].TimeMS >= cutoff })
	out := make([]Sample, len(src)-idx)
	copy(out, src[idx:])
	s.mu.RUnlock()
	return out
}

// QueryAll returns samples for every coin within the last windowHours.
func (s *Store) QueryAll(windowHours int) map[string][]Sample {
	cutoff := time.Now().Add(-time.Duration(windowHours) * time.Hour).UnixMilli()
	s.mu.RLock()
	result := make(map[string][]Sample, len(s.samples))
	for coin, src := range s.samples {
		idx := sort.Search(len(src), func(i int) bool { return src[i].TimeMS >= cutoff })
		if idx < len(src) {
			cp := make([]Sample, len(src)-idx)
			copy(cp, src[idx:])
			result[coin] = cp
		}
	}
	s.mu.RUnlock()
	return result
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// Save writes the current state to disk as compact JSON.  It writes to a
// temporary file first then renames to avoid partial-write corruption.
func (s *Store) Save() {
	s.mu.RLock()
	state := persistState{Samples: s.samples}
	data, err := json.Marshal(&state)
	s.mu.RUnlock()
	if err != nil {
		log.Printf("[hashstore] marshal error: %v", err)
		return
	}

	dir := filepath.Dir(s.stateFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[hashstore] mkdir error: %v", err)
		return
	}

	tmp := s.stateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("[hashstore] write error: %v", err)
		return
	}
	if err := os.Rename(tmp, s.stateFile); err != nil {
		log.Printf("[hashstore] rename error: %v", err)
		return
	}
	s.mu.Lock()
	s.lastSave = time.Now()
	s.mu.Unlock()
}

// load reads state from disk.  Errors are logged but not fatal.
func (s *Store) load() {
	data, err := os.ReadFile(s.stateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[hashstore] load error: %v", err)
		}
		return
	}
	var state persistState
	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("[hashstore] unmarshal error: %v", err)
		return
	}
	if state.Samples != nil {
		s.samples = state.Samples
	}
	// Ensure every coin slice is sorted (defensive).
	for coin := range s.samples {
		sl := s.samples[coin]
		sort.Slice(sl, func(i, j int) bool { return sl[i].TimeMS < sl[j].TimeMS })
	}
	log.Printf("[hashstore] loaded %d coins from %s", len(s.samples), s.stateFile)
}

// ---------------------------------------------------------------------------
// Compaction / downsampling
// ---------------------------------------------------------------------------

// compact merges samples into coarser buckets based on age and trims anything
// older than 30 days.  It operates in-place per coin to minimise allocations.
func (s *Store) compact() {
	now := time.Now()
	cutoff30d := now.Add(-30 * 24 * time.Hour).UnixMilli()
	cutoff7d := now.Add(-7 * 24 * time.Hour).UnixMilli()
	cutoff24h := now.Add(-24 * time.Hour).UnixMilli()
	cutoff1h := now.Add(-1 * time.Hour).UnixMilli()

	s.mu.Lock()
	defer s.mu.Unlock()

	for coin, sl := range s.samples {
		if len(sl) == 0 {
			continue
		}

		// 1. Drop samples older than 30d.
		startIdx := sort.Search(len(sl), func(i int) bool { return sl[i].TimeMS >= cutoff30d })
		if startIdx > 0 {
			sl = sl[startIdx:]
		}

		// 2. Merge zones working from oldest to newest.
		//    We split into four zones and downsample the three oldest.
		var (
			zone30d []Sample // 30d..7d  → 15-min buckets
			zone7d  []Sample // 7d..24h  → 5-min buckets
			zone24h []Sample // 24h..1h  → 1-min buckets
			zoneRaw []Sample // <1h      → keep raw
		)

		for i := range sl {
			t := sl[i].TimeMS
			switch {
			case t < cutoff7d:
				zone30d = append(zone30d, sl[i])
			case t < cutoff24h:
				zone7d = append(zone7d, sl[i])
			case t < cutoff1h:
				zone24h = append(zone24h, sl[i])
			default:
				zoneRaw = append(zoneRaw, sl[i])
			}
		}

		zone30d = downsample(zone30d, 15*60*1000) // 15 min in ms
		zone7d = downsample(zone7d, 5*60*1000)    // 5 min
		zone24h = downsample(zone24h, 1*60*1000)  // 1 min

		// Reassemble — reuse the backing array when possible.
		total := len(zone30d) + len(zone7d) + len(zone24h) + len(zoneRaw)
		merged := make([]Sample, 0, total)
		merged = append(merged, zone30d...)
		merged = append(merged, zone7d...)
		merged = append(merged, zone24h...)
		merged = append(merged, zoneRaw...)

		s.samples[coin] = merged
	}

	s.lastComp = now
}

// downsample merges a sorted slice of samples into buckets of bucketMS
// milliseconds, averaging KHs within each bucket.  The output timestamp is
// the midpoint of the bucket.
func downsample(samples []Sample, bucketMS int64) []Sample {
	if len(samples) == 0 {
		return nil
	}

	out := make([]Sample, 0, len(samples)/2+1)
	bucketStart := (samples[0].TimeMS / bucketMS) * bucketMS
	var sum float64
	var count int

	flush := func() {
		if count > 0 {
			out = append(out, Sample{
				KHs:    sum / float64(count),
				TimeMS: bucketStart + bucketMS/2,
			})
		}
	}

	for i := range samples {
		t := samples[i].TimeMS
		b := (t / bucketMS) * bucketMS
		if b != bucketStart {
			flush()
			bucketStart = b
			sum = 0
			count = 0
		}
		sum += samples[i].KHs
		count++
	}
	flush()
	return out
}
