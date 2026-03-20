package uptime

import (
	"encoding/json"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// UptimeEvent records a single online/offline transition.
type UptimeEvent struct {
	Online     bool          `json:"online"`
	At         time.Time     `json:"at"`
	SessionDur time.Duration `json:"session_dur"` // duration of the session that just ended (offline events only)
}

// workerState is the internal per-worker tracking state.
type workerState struct {
	Name          string        `json:"name"`
	Coin          string        `json:"coin"`
	Online        bool          `json:"online"`
	StatusSince   time.Time     `json:"status_since"`
	TotalSessions int           `json:"total_sessions"`
	Events        []UptimeEvent `json:"events"`
}

// Tracker maintains per-worker uptime history and persists it to disk.
type Tracker struct {
	mu        sync.Mutex
	workers   map[string]*workerState // key: "coin/name"
	stateFile string
}

// WorkerUptime is a read-only snapshot of a single worker's uptime stats.
type WorkerUptime struct {
	Name              string
	Coin              string
	CurrentStatus     bool          // true = online
	StatusSince       time.Time     // when current status started
	Uptime24h         float64       // 0.0–100.0
	Uptime7d          float64
	Uptime30d         float64
	TotalSessions     int
	TotalDowntime     time.Duration
	AvgSessionDur     time.Duration
	LongestSession    time.Duration
	DisconnectPattern string        // "" or e.g. "disconnects every ~6h"
	Events            []UptimeEvent // recent events for timeline
}

// persistState is the JSON-serializable form of the full tracker state.
type persistState struct {
	Workers map[string]*workerState `json:"workers"`
}

// New creates a Tracker that persists to stateFile.
// It loads any existing state from disk on creation.
func New(stateFile string) *Tracker {
	t := &Tracker{
		workers:   make(map[string]*workerState),
		stateFile: stateFile,
	}
	t.loadState()
	return t
}

// workerKey returns the map key for a worker.
func workerKey(name, coin string) string {
	return coin + "/" + name
}

// WorkerOnline records that a worker has connected.
func (t *Tracker) WorkerOnline(name, coin string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := workerKey(name, coin)
	now := time.Now()

	ws, ok := t.workers[key]
	if !ok {
		ws = &workerState{
			Name: name,
			Coin: coin,
		}
		t.workers[key] = ws
	}

	// Only record a transition if the worker was previously offline (or brand new).
	if !ws.Online {
		ws.Online = true
		ws.StatusSince = now
		ws.TotalSessions++
		ws.Events = append(ws.Events, UptimeEvent{
			Online: true,
			At:     now,
		})
		t.trimEvents(ws)
		t.saveState()
	}
}

// WorkerOffline records that a worker has disconnected.
func (t *Tracker) WorkerOffline(name, coin string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := workerKey(name, coin)
	now := time.Now()

	ws, ok := t.workers[key]
	if !ok {
		// Offline event for an unknown worker — create minimal state.
		ws = &workerState{
			Name: name,
			Coin: coin,
		}
		t.workers[key] = ws
	}

	if ws.Online {
		sessionDur := now.Sub(ws.StatusSince)
		ws.Online = false
		ws.StatusSince = now
		ws.Events = append(ws.Events, UptimeEvent{
			Online:     false,
			At:         now,
			SessionDur: sessionDur,
		})
		t.trimEvents(ws)
		t.saveState()
	}
}

// trimEvents keeps only the most recent maxEvents entries (covers 30d+ for any
// reasonable reconnect frequency) and drops anything older than 31 days.
func (t *Tracker) trimEvents(ws *workerState) {
	const maxEvents = 2000
	cutoff := time.Now().Add(-31 * 24 * time.Hour)

	// Drop events older than 31 days.
	start := 0
	for start < len(ws.Events) && ws.Events[start].At.Before(cutoff) {
		start++
	}
	if start > 0 {
		ws.Events = ws.Events[start:]
	}

	// Hard cap.
	if len(ws.Events) > maxEvents {
		ws.Events = ws.Events[len(ws.Events)-maxEvents:]
	}
}

// Snapshot returns a point-in-time view of all tracked workers.
func (t *Tracker) Snapshot() []WorkerUptime {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	out := make([]WorkerUptime, 0, len(t.workers))

	for _, ws := range t.workers {
		wu := t.computeUptime(ws, now)
		out = append(out, wu)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Coin != out[j].Coin {
			return out[i].Coin < out[j].Coin
		}
		return out[i].Name < out[j].Name
	})

	return out
}

// computeUptime derives all statistics for a single worker from its event log.
func (t *Tracker) computeUptime(ws *workerState, now time.Time) WorkerUptime {
	wu := WorkerUptime{
		Name:          ws.Name,
		Coin:          ws.Coin,
		CurrentStatus: ws.Online,
		StatusSince:   ws.StatusSince,
		TotalSessions: ws.TotalSessions,
	}

	// Copy events for the caller (shallow copy is fine, UptimeEvent is small).
	wu.Events = make([]UptimeEvent, len(ws.Events))
	copy(wu.Events, ws.Events)

	// Calculate uptime percentages for rolling windows.
	wu.Uptime24h = t.calcUptimePct(ws, now, 24*time.Hour)
	wu.Uptime7d = t.calcUptimePct(ws, now, 7*24*time.Hour)
	wu.Uptime30d = t.calcUptimePct(ws, now, 30*24*time.Hour)

	// Session statistics from offline events (each carries the session duration).
	var totalSessionDur time.Duration
	var longestSession time.Duration
	sessionCount := 0

	for _, ev := range ws.Events {
		if !ev.Online && ev.SessionDur > 0 {
			sessionCount++
			totalSessionDur += ev.SessionDur
			if ev.SessionDur > longestSession {
				longestSession = ev.SessionDur
			}
		}
	}

	// If the worker is currently online, count the live session too.
	if ws.Online && !ws.StatusSince.IsZero() {
		liveDur := now.Sub(ws.StatusSince)
		sessionCount++
		totalSessionDur += liveDur
		if liveDur > longestSession {
			longestSession = liveDur
		}
	}

	wu.LongestSession = longestSession
	if sessionCount > 0 {
		wu.AvgSessionDur = totalSessionDur / time.Duration(sessionCount)
	}

	// Total downtime within event history.
	wu.TotalDowntime = t.calcTotalDowntime(ws, now)

	// Disconnect pattern detection.
	wu.DisconnectPattern = t.detectPattern(ws)

	return wu
}

// calcUptimePct computes the percentage of a rolling window that the worker was online.
// It walks the event log and tallies time spent in each state within the window.
func (t *Tracker) calcUptimePct(ws *workerState, now time.Time, window time.Duration) float64 {
	windowStart := now.Add(-window)

	// No events at all — if the worker is currently online and has been since before
	// the window, that's 100%; otherwise we have no data.
	if len(ws.Events) == 0 {
		if ws.Online && !ws.StatusSince.IsZero() && ws.StatusSince.Before(now) {
			return 100.0
		}
		return 0.0
	}

	// We reconstruct a timeline within [windowStart, now].
	// Walk events in chronological order. We need to know the state at windowStart.
	// The state just before the first event in the window is the opposite of that
	// event's Online field (since each event is a transition).
	var onlineTime time.Duration

	// Find the state at windowStart by looking at the last event before windowStart.
	stateAtStart := false
	for _, ev := range ws.Events {
		if ev.At.Before(windowStart) || ev.At.Equal(windowStart) {
			stateAtStart = ev.Online
		} else {
			break
		}
	}

	cursor := windowStart
	currentState := stateAtStart

	for _, ev := range ws.Events {
		if ev.At.Before(windowStart) || ev.At.Equal(windowStart) {
			continue
		}
		if ev.At.After(now) {
			break
		}

		// Accumulate time from cursor to this event in the current state.
		seg := ev.At.Sub(cursor)
		if currentState {
			onlineTime += seg
		}
		cursor = ev.At
		currentState = ev.Online
	}

	// Accumulate from the last processed event (or windowStart) to now.
	seg := now.Sub(cursor)
	if currentState {
		onlineTime += seg
	}

	pct := float64(onlineTime) / float64(window) * 100.0
	if pct > 100.0 {
		pct = 100.0
	}
	if pct < 0.0 {
		pct = 0.0
	}
	return math.Round(pct*100) / 100 // two decimal places
}

// calcTotalDowntime sums the offline time across all recorded events.
func (t *Tracker) calcTotalDowntime(ws *workerState, now time.Time) time.Duration {
	if len(ws.Events) == 0 {
		return 0
	}

	var downtime time.Duration
	var lastOfflineAt time.Time

	for _, ev := range ws.Events {
		if !ev.Online {
			lastOfflineAt = ev.At
		} else if !lastOfflineAt.IsZero() {
			// Transition back online — the gap is downtime.
			downtime += ev.At.Sub(lastOfflineAt)
			lastOfflineAt = time.Time{}
		}
	}

	// If currently offline, add time since last offline event.
	if !ws.Online && !lastOfflineAt.IsZero() {
		downtime += now.Sub(lastOfflineAt)
	}

	return downtime
}

// detectPattern examines the last 10 disconnect events and checks whether the
// intervals between consecutive disconnects have a standard deviation below 20%
// of the mean. If so, it returns a human-readable pattern description.
func (t *Tracker) detectPattern(ws *workerState) string {
	// Collect timestamps of disconnect (offline) events.
	var disconnectTimes []time.Time
	for _, ev := range ws.Events {
		if !ev.Online {
			disconnectTimes = append(disconnectTimes, ev.At)
		}
	}

	// Use the last 10 disconnect events.
	if len(disconnectTimes) > 10 {
		disconnectTimes = disconnectTimes[len(disconnectTimes)-10:]
	}

	// Need at least 3 disconnects to form 2 intervals.
	if len(disconnectTimes) < 3 {
		return ""
	}

	// Compute intervals between consecutive disconnects.
	intervals := make([]float64, 0, len(disconnectTimes)-1)
	for i := 1; i < len(disconnectTimes); i++ {
		intervals = append(intervals, disconnectTimes[i].Sub(disconnectTimes[i-1]).Seconds())
	}

	// Mean.
	var sum float64
	for _, v := range intervals {
		sum += v
	}
	mean := sum / float64(len(intervals))

	if mean < 1.0 {
		// Intervals too short to be meaningful.
		return ""
	}

	// Standard deviation.
	var sqDiffSum float64
	for _, v := range intervals {
		d := v - mean
		sqDiffSum += d * d
	}
	stddev := math.Sqrt(sqDiffSum / float64(len(intervals)))

	// If stddev < 20% of mean, we have a pattern.
	if stddev/mean >= 0.20 {
		// Check for "unstable connection" — many disconnects in a short window.
		// If we have 5+ disconnects in the last 24h, flag it.
		cutoff24h := time.Now().Add(-24 * time.Hour)
		recent := 0
		for _, dt := range disconnectTimes {
			if dt.After(cutoff24h) {
				recent++
			}
		}
		if recent >= 5 {
			return "unstable connection"
		}
		return ""
	}

	// Format the interval nicely.
	dur := time.Duration(mean * float64(time.Second))
	return "disconnects every ~" + formatDuration(dur)
}

// formatDuration returns a human-friendly duration string like "6h", "2h30m", "45m".
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}

	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60

	if hours >= 24 {
		days := hours / 24
		remHours := hours % 24
		if remHours == 0 {
			return pluralize(days, "d")
		}
		return pluralize(days, "d") + pluralize(remHours, "h")
	}

	if hours > 0 && mins == 0 {
		return pluralize(hours, "h")
	}
	if hours > 0 {
		return pluralize(hours, "h") + pluralize(mins, "m")
	}
	return pluralize(mins, "m")
}

// pluralize returns e.g. "6h", "30m".
func pluralize(n int, unit string) string {
	return itoa(n) + unit
}

// itoa is a simple int-to-string without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// loadState reads persisted state from disk. Errors are logged and ignored
// (a missing file on first run is expected).
func (t *Tracker) loadState() {
	data, err := os.ReadFile(t.stateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[uptime] failed to read state file %s: %v", t.stateFile, err)
		}
		return
	}

	var ps persistState
	if err := json.Unmarshal(data, &ps); err != nil {
		log.Printf("[uptime] failed to parse state file %s: %v", t.stateFile, err)
		return
	}

	if ps.Workers != nil {
		t.workers = ps.Workers
	}
	log.Printf("[uptime] loaded %d workers from %s", len(t.workers), t.stateFile)
}

// saveState writes current state to disk atomically (write-tmp then rename).
// Must be called with t.mu held.
func (t *Tracker) saveState() {
	ps := persistState{Workers: t.workers}
	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		log.Printf("[uptime] failed to marshal state: %v", err)
		return
	}

	dir := filepath.Dir(t.stateFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[uptime] failed to create directory %s: %v", dir, err)
		return
	}

	tmp := t.stateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("[uptime] failed to write temp file %s: %v", tmp, err)
		return
	}

	if err := os.Rename(tmp, t.stateFile); err != nil {
		log.Printf("[uptime] failed to rename %s -> %s: %v", tmp, t.stateFile, err)
	}
}
