package stratum

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dakota/pipool/internal/config"
	"github.com/dakota/pipool/internal/merge"
	"github.com/dakota/pipool/internal/rpc"
	"github.com/dakota/pipool/internal/zmq"
	"golang.org/x/crypto/scrypt"
)

const (
	// extranonce1Size is the size of extranonce1 in bytes (4 bytes = 8 hex chars).
	extranonce1Size = 4
	// defaultEN2Size is the default extranonce2 size for standard miners (4 bytes).
	defaultEN2Size = 4
	// maxEN2Size is the maximum extranonce2 size we support (Braiins firmware requests 7).
	maxEN2Size = 7
	// coinbaseENSlot is the total extranonce space reserved in the coinbase script.
	// Must be extranonce1Size + maxEN2Size = 11 bytes. Miners with smaller en2 are zero-padded.
	coinbaseENSlot = extranonce1Size + maxEN2Size
)

// AuxBlockInfo bundles the data needed to submit a found parent block to aux chains.
type AuxBlockInfo struct {
	// CoinbaseTx is the full serialized coinbase transaction (for the AuxPoW proof).
	CoinbaseTx []byte
	// MerkleBranch is the branch from the coinbase to the block merkle root (each element is 32 raw bytes).
	MerkleBranch [][]byte
	// HeaderBytes is the 80-byte block header.
	HeaderBytes []byte
	// AuxSortedSyms is the deterministically sorted aux chain symbol list used to build the aux merkle tree.
	AuxSortedSyms []string
	// AuxWorkSnap is a snapshot of the aux work at the time this job was built.
	AuxWorkSnap map[string]*merge.AuxWork
}

// Server is a Stratum V1 server for a single primary coin (with optional merge mining)
type Server struct {
	coin        config.CoinConfig
	poolCfg     *config.PoolConfig
	coinbaseTag string // e.g. "/PiPool/" — embedded in every block found
	rpcClient   *rpc.Client
	coordinator *merge.Coordinator

	mu          sync.RWMutex
	workers     map[string]*Worker
	seenWorkers map[string]*SeenWorker // all workers ever connected this session, capped at 500
	shareSamples    []ShareSample      // rolling buffer of last 200 shares (for difficulty chart)
	hashrateSamples []HashrateSample   // rolling buffer of last 288 hashrate snapshots (24h @ 5min)
	bestShareEver   float64            // all-time highest share difficulty this process
	blockLog        []BlockEntry       // last 50 blocks found, shown on dashboard
	currentJob      *Job
	recentJobs      map[string]*Job    // recent jobs by ID — used to validate non-stale shares
	recentJobsMax   int                // max jobs to keep (from config, default 4)

	// Luck tracking — valid work accumulated since last block found or session start
	validWorkSinceBlock float64 // sum of accepted share diffs since last block (under mu)
	lastNetworkDiff     float64 // network difficulty of most recent job (under mu)

	// Stats (updated atomically for lock-free reads)
	totalShares   atomic.Uint64
	validShares   atomic.Uint64
	blocksFound   atomic.Uint64
	connectedMiners atomic.Int32

	// Channel to broadcast new jobs to all workers
	jobBroadcast chan *Job
	stopCh       chan struct{}
	ln           net.Listener   // stored so Stop() can close the TCP port cleanly
	wg           sync.WaitGroup // tracks active worker goroutines for graceful drain

	backupRPC   *rpc.Client   // optional backup node RPC (nil if not configured)
	usingBackup atomic.Bool   // true = currently reading templates from backup node

	// Upstream proxy fallback — activated when the local node goes offline
	upstream     *UpstreamProxy // nil = solo mode
	proxyActive  atomic.Bool
	upstreamEn1  string  // extranonce1 from upstream pool (under mu)
	upstreamDiff float64 // difficulty from upstream pool (under mu)

	// Callbacks for Discord notifications
	OnBlockFound      func(coin, hash, worker string, reward, luck float64)
	OnMinerConnect    func(worker string, addr string)
	OnMinerDisconnect func(worker string)
	OnNodeUnreachable func(err error)
	OnNodeOnline      func()
	// OnNodeWatchdogRestart fires when the watchdog auto-restarts a node daemon.
	OnNodeWatchdogRestart func(service string)
	// Called when a block is found — provides components needed for AuxPoW submission.
	// Only fires when there are active merge-mining children.
	OnAuxBlockFound func(info AuxBlockInfo)
	// Callback fired when a block becomes orphaned
	OnBlockOrphaned func(coin, hash string, height int64)
	// Callback fired when a block crosses the maturity threshold (confirmations become spendable)
	OnBlockMature func(coin, hash string, height int64, reward string)
	// OnStaleKick fires when a worker's hourly stale-kick count crosses the threshold.
	OnStaleKick func(workerName string, kickCount int)
	OnShareAccepted func(coin, workerAddr string, difficulty float64)

	// workerKickTimes tracks rolling 1-hour stale kick timestamps per worker (under mu).
	workerKickTimes map[string][]time.Time
}

// Worker represents a connected miner
type Worker struct {
	id           string
	conn         net.Conn
	writer       *bufio.Writer
	mu           sync.Mutex
	authorized   bool
	workerName   string
	remoteAddr   string
	deviceName   string  // from Spiral Router classification
	userAgent    string  // from mining.subscribe

	// Per-worker difficulty (vardiff)
	difficulty          float64
	prevDifficulty      float64 // difficulty before last increase — grace period for in-flight shares
	lastShareAt         time.Time
	lastAcceptedAt      time.Time // time of last ACCEPTED share — used for vardiff decay
	lastRetargetAt      time.Time
	sharesSinceRetarget uint64
	fixedDiff           float64 // if > 0: skip vardiff and pin to this value
	diffHistory  []DiffEvent  // last 100 difficulty changes (under Server.mu for writes)

	// Share counters
	sharesAccepted  uint64
	sharesRejected  uint64
	sharesStale     uint64
	connectedAt     time.Time
	bestShare       float64  // highest difficulty share submitted this session
	suggestedMinDiff float64 // floor set by mining.suggest_difficulty

	// Stale-job kick: disconnect miners that ignore clean_jobs and keep mining old work
	lastStaleJobID  string // job ID of the most recent stale submission
	staleJobStreak  int    // consecutive stales for the same old job

	// Extranonce assigned to this worker
	extranonce1    string
	en2Size        int  // extranonce2 size in bytes (default 4, Braiins requests 7)
	supportsEnSub  bool // true if miner sent mining.extranonce.subscribe
}

// SeenWorker tracks all workers ever connected this session (for dashboard history)
type SeenWorker struct {
	Name           string
	DeviceName     string
	UserAgent      string
	LastAddr       string
	Coin           string
	SharesAccepted uint64
	SharesRejected uint64
	SharesStale    uint64
	// Base counts accumulated from all prior sessions; current session adds on top.
	SharesAcceptedBase uint64
	SharesRejectedBase uint64
	SharesStaleBase    uint64
	BestShare        float64
	LastDifficulty   float64    // difficulty at last disconnect — restored on next reconnect
	DiffHistory      []DiffEvent // last 100 difficulty changes, persisted across reconnects
	ReconnectCount   int        // number of times this worker has reconnected (0 = first session)
	SessionStartedAt time.Time  // time of the most recent authorization
	ConnectedAt      time.Time
	LastSeenAt       time.Time
	Online           bool
	LiveWatts float64  // live watts from miner HTTP API (0 = not available)
	LiveTemp  float64  // live ASIC temp from miner HTTP API (0 = not available)
}

// ShareSample records a single submitted share for the difficulty chart
type ShareSample struct {
	Difficulty float64
	TimeMS     int64  // unix milliseconds
	Accepted   bool
	Worker     string // worker name — used for per-worker hashrate estimation
}

// HashrateSample is a periodic hashrate snapshot for the chart
type HashrateSample struct {
	KHs    float64
	TimeMS int64
}

// Job represents a unit of mining work sent to miners
type Job struct {
	ID             string
	PrevHash       string
	CoinbasePart1  string
	CoinbasePart2  string
	MerkleBranches []string
	Version        string
	NBits          string
	NTime          string
	CleanJobs      bool
	Height         int64
	Target         string
	CreatedAt      time.Time
	// Merge mining commitment included in coinbase
	AuxCommitment   []byte
	AuxSortedSyms   []string // aux chain symbols in sorted order used to build AuxCommitment
	AuxWorks        map[string]*merge.AuxWork // snapshot of aux work at job-build time
	// RawTxHexes holds the serialized transactions from getblocktemplate (excluding coinbase).
	// Required for correct full-block assembly when submitting a found block.
	RawTxHexes     []string
	// PowAlgo is the proof-of-work algorithm the node expects for this block (DGB multi-algo).
	// Empty for non-DGB coins. Values: "sha256d", "scrypt", "skein", "qubit", "odo".
	PowAlgo        string
}

// NewServer creates a Stratum server for the given coin
func NewServer(coin config.CoinConfig, poolCfg *config.PoolConfig, coord *merge.Coordinator) *Server {
	cli := rpc.NewClient(coin.Node.Host, coin.Node.Port, coin.Node.User, coin.Node.Password, coin.Symbol)

	tag := poolCfg.Pool.CoinbaseTag
	if tag == "" {
		tag = "/PiPool/"
	}

	var backupRPC *rpc.Client
	if coin.BackupNode != nil {
		backupRPC = rpc.NewClient(coin.BackupNode.Host, coin.BackupNode.Port,
			coin.BackupNode.User, coin.BackupNode.Password, coin.Symbol)
	}

	maxJobs := coin.Stratum.RecentJobsSize
	if maxJobs <= 0 {
		maxJobs = 4
	}

	return &Server{
		coin:            coin,
		seenWorkers:     make(map[string]*SeenWorker),
		shareSamples:    make([]ShareSample, 0, 200),
		hashrateSamples: make([]HashrateSample, 0, 288),
		poolCfg:         poolCfg,
		coinbaseTag:     tag,
		rpcClient:       cli,
		backupRPC:       backupRPC,
		coordinator:     coord,
		workers:         make(map[string]*Worker),
		recentJobs:      make(map[string]*Job),
		recentJobsMax:   maxJobs,
		jobBroadcast:    make(chan *Job, 256),
		stopCh:          make(chan struct{}),
		workerKickTimes: make(map[string][]time.Time),
	}
}

// Start begins listening for miner connections and polling for new work
func (s *Server) Start() error {
	// Restore persisted worker state (LastDifficulty, BestShare) and block log from previous run
	s.loadWorkerState()
	s.loadBlockLog()

	// Bind on all interfaces so startup succeeds even if the pool's public IP
	// is not yet assigned (e.g. immediately after a reboot / DHCP renewal).
	listenAddr := fmt.Sprintf("0.0.0.0:%d", s.coin.Stratum.Port)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("[%s] stratum listen on %s: %w", s.coin.Symbol, listenAddr, err)
	}
	s.ln = ln

	log.Printf("[%s] Stratum server listening on %s (pool addr %s)", s.coin.Symbol, listenAddr, s.poolCfg.Pool.Host)

	// TLS listener (optional — only if enabled in coin config)
	if s.coin.Stratum.TLS.Enabled {
		tlsPort := s.coin.Stratum.TLS.Port
		if tlsPort == 0 {
			tlsPort = defaultTLSPort(s.coin.Stratum.Port)
		}
		tlsCfg, tlsErr := buildTLSConfig(TLSConfig{
			Enabled:  s.coin.Stratum.TLS.Enabled,
			Port:     s.coin.Stratum.TLS.Port,
			CertFile: s.coin.Stratum.TLS.CertFile,
			KeyFile:  s.coin.Stratum.TLS.KeyFile,
		}, s.coin.Symbol)
		if tlsErr != nil {
			log.Printf("[%s] TLS setup failed: %v — TLS disabled", s.coin.Symbol, tlsErr)
		} else {
			tlsAddr := fmt.Sprintf("0.0.0.0:%d", tlsPort)
			tlsLn, tlsErr := net.Listen("tcp", tlsAddr)
			if tlsErr != nil {
				log.Printf("[%s] TLS stratum listen on %s failed: %v — TLS disabled", s.coin.Symbol, tlsAddr, tlsErr)
			} else {
				tlsLn = tls.NewListener(tlsLn, tlsCfg)
				log.Printf("[%s] TLS stratum listening on %s (auto-cert)", s.coin.Symbol, tlsAddr)
				go s.acceptLoop(tlsLn)
			}
		}
	}

	// Start block template poller — recovers from panics internally
	go s.pollBlockTemplate()

	// Time-based vardiff decay — lowers difficulty for miners that connect at a
	// too-high StartDiff and never submit shares (vardiff normally only triggers
	// on share submissions, so without this such miners would be stuck forever).
	go s.idleVardiffDecay()

	// Accept miner connections on the plain TCP listener
	go s.acceptLoop(ln)

	// Broadcast new jobs as they arrive — recovers from panics internally
	go s.broadcastJobs()

	// Start block confirmation tracker
	go s.trackConfirmations()

	return nil
}

// Stop shuts down the server gracefully.
// Closes the TCP listener (stops new connections), signals all goroutines,
// then waits up to 10s for in-flight workers to finish their current share.
func (s *Server) Stop() {
	close(s.stopCh)
	if s.ln != nil {
		s.ln.Close() // unblocks ln.Accept() in the accept goroutine
	}

	// Close all worker connections so their handleWorker goroutines unblock
	// from reads and drain via the deferred cleanup path.
	s.mu.RLock()
	for _, w := range s.workers {
		w.conn.Close()
	}
	s.mu.RUnlock()

	// Drain in-flight worker goroutines with a timeout
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Printf("[%s] all workers drained cleanly", s.coin.Symbol)
	case <-time.After(10 * time.Second):
		log.Printf("[%s] shutdown timeout — %d workers still active, forcing close", s.coin.Symbol, s.connectedMiners.Load())
	}
	s.saveWorkerState() // persist difficulty state for next startup
	s.saveBlockLog()    // persist block log on clean shutdown
}

// activateProxy starts the upstream stratum proxy fallback.
func (s *Server) activateProxy() {
	if s.upstream != nil {
		return // already active
	}
	cfg := s.coin.UpstreamPool
	log.Printf("[%s] node offline — activating upstream proxy to %s:%d", s.coin.Symbol, cfg.Host, cfg.Port)
	u := newUpstreamProxy(cfg, s.coin.Symbol, func(job *Job, en1 string, diff float64) {
		s.mu.Lock()
		s.upstreamEn1 = en1
		s.upstreamDiff = diff
		s.currentJob = job
		if len(s.recentJobs) > s.recentJobsMax {
			// keep only the latest
			s.recentJobs = map[string]*Job{job.ID: job}
		} else {
			s.recentJobs[job.ID] = job
		}
		s.mu.Unlock()
		// Update extranonce1 for all workers; miners that support set_extranonce
		// get a notification, others are disconnected to force a reconnect.
		s.updateExtraNonce(en1)
		select {
		case s.jobBroadcast <- job:
		default:
			log.Printf("[%s] WARNING: proxy job broadcast channel full, job dropped", s.coin.Symbol)
		}
	})
	if err := u.Start(); err != nil {
		log.Printf("[%s] proxy start failed: %v", s.coin.Symbol, err)
		return
	}
	s.upstream = u
	s.proxyActive.Store(true)
}

// deactivateProxy stops the upstream stratum proxy fallback.
func (s *Server) deactivateProxy() {
	if s.upstream == nil {
		return
	}
	log.Printf("[%s] node back online — deactivating upstream proxy", s.coin.Symbol)
	s.upstream.Stop()
	s.upstream = nil
	s.proxyActive.Store(false)
	// Kick workers so they reconnect and get local extranonce1
	s.kickAllConnected()
}

// kickAllConnected closes all active miner connections,
// causing them to reconnect and receive updated session parameters.
func (s *Server) kickAllConnected() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, w := range s.workers {
		w.conn.Close()
	}
}

// updateExtraNonce pushes a new extranonce1 to all connected workers.
// Workers that sent mining.extranonce.subscribe get a mining.set_extranonce
// notification; others are disconnected so they reconnect with the new value.
func (s *Server) updateExtraNonce(en1 string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, w := range s.workers {
		w.mu.Lock()
		sub := w.supportsEnSub
		w.mu.Unlock()
		if sub {
			w.mu.Lock()
			w.extranonce1 = en1
			en2 := w.en2Size
			w.mu.Unlock()
			s.sendSetExtranonce(w, en1, en2)
		} else {
			w.conn.Close()
		}
	}
}

// kickStaleStreakWorkers disconnects any worker that currently has a non-zero
// stale streak (i.e. they have already submitted at least one stale share for
// an aged-out job). Called on every genuine new-block broadcast so that
// firmware which ignores clean_jobs=true is kicked at the next block boundary
// rather than waiting for N more stale submissions.
func (s *Server) kickStaleStreakWorkers() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, w := range s.workers {
		if w.staleJobStreak > 0 {
			log.Printf("[%s] new block: kicking %s (stale streak %d, job %s ignores clean_jobs)",
				s.coin.Symbol, w.workerName, w.staleJobStreak, w.lastStaleJobID)
			s.recordStaleKick(w.workerName)
			w.conn.Close()
		}
	}
}

// coinFixedDiff returns the effective fixed difficulty for a worker name,
// checking coin-level config first, then global pool config. Returns 0 if none set.
func (s *Server) coinFixedDiff(workerName string) float64 {
	if s.coin.WorkerFixedDiff != nil {
		if fd, ok := s.coin.WorkerFixedDiff[workerName]; ok && fd > 0 {
			return fd
		}
	}
	if s.poolCfg.Pool.WorkerFixedDiff != nil {
		if fd, ok := s.poolCfg.Pool.WorkerFixedDiff[workerName]; ok && fd > 0 {
			return fd
		}
	}
	return 0
}

// recordStaleKick records a kick for the worker and fires OnStaleKick if the
// 1-hour kick count exceeds the configured threshold.
func (s *Server) recordStaleKick(workerName string) {
	s.mu.Lock()
	now := time.Now()
	cutoff := now.Add(-time.Hour)
	kicks := s.workerKickTimes[workerName]
	pruned := kicks[:0]
	for _, t := range kicks {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	pruned = append(pruned, now)
	s.workerKickTimes[workerName] = pruned
	count := len(pruned)
	s.mu.Unlock()

	const threshold = 3
	if count >= threshold && count%threshold == 0 {
		if s.OnStaleKick != nil {
			s.OnStaleKick(workerName, count)
		}
	}
}

// acceptLoop accepts connections on ln and spawns a goroutine for each.
func (s *Server) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				log.Printf("[%s] accept error: %v", s.coin.Symbol, err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}
		// IP allowlist check
		if len(s.poolCfg.Pool.IPAllowlist) > 0 && !isAllowedIP(conn.RemoteAddr().String(), s.poolCfg.Pool.IPAllowlist) {
			log.Printf("[%s] connection rejected (not in allowlist): %s", s.coin.Symbol, conn.RemoteAddr())
			conn.Close()
			continue
		}
		// Enforce connection cap for RAM safety
		if int(s.connectedMiners.Load()) >= s.poolCfg.Pool.MaxConnections {
			log.Printf("[%s] connection limit reached, rejecting %s", s.coin.Symbol, conn.RemoteAddr())
			conn.Close()
			continue
		}
		s.wg.Add(1)
		go s.handleWorker(conn)
	}
}

// activeRPC returns the backup RPC client if we're currently using it, else primary.
func (s *Server) activeRPC() *rpc.Client {
	if s.usingBackup.Load() && s.backupRPC != nil {
		return s.backupRPC
	}
	return s.rpcClient
}

// ─── Block template polling ───────────────────────────────────────────────────

func (s *Server) pollBlockTemplate() {
	// If this goroutine panics for any reason, restart it after a short delay
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[%s] panic in block template poller (restarting in 5s): %v", s.coin.Symbol, r)
			time.Sleep(5 * time.Second)
			select {
			case <-s.stopCh:
				return
			default:
				go s.pollBlockTemplate() // restart
			}
		}
	}()
	var lastHash string
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	// Periodic job refresh — keeps cpuminer-based miners (e.g. Elphapex DG1 Home) connected
	// by sending an updated mining.notify even when the block hasn't changed.
	refreshTicker := time.NewTicker(45 * time.Second)
	defer refreshTicker.Stop()

	// ZMQ instant block notification — fires immediately when a new block is found.
	// Falls back to 2-second polling if the endpoint is unconfigured or unreachable.
	zmqNotify := make(chan struct{}, 1)
	if ep := s.coin.Node.ZmqPubHashBlock; ep != "" {
		go func() {
			for {
				select {
				case <-s.stopCh:
					return
				default:
				}
				err := zmq.Subscribe(ep, "hashblock", func(_ [][]byte) {
					select {
					case zmqNotify <- struct{}{}:
					default: // already a pending notification
					}
				})
				if err != nil {
					log.Printf("[%s] ZMQ %s disconnected, reconnecting in 5s: %v", s.coin.Symbol, ep, err)
				}
				select {
				case <-time.After(5 * time.Second):
				case <-s.stopCh:
					return
				}
			}
		}()
	}

	daemonWasDown := false
	retryDelay := 5 * time.Second
	const maxRetryDelay = 60 * time.Second

	// Watchdog state — tracks how long the daemon has been unreachable so we
	// can issue a systemctl restart after a configurable quiet period.
	var daemonDownSince time.Time
	var lastWatchdogRestart time.Time
	const watchdogDelay   = 2 * time.Minute
	const watchdogCooldown = 10 * time.Minute

	broadcastJob := func(cleanJobs bool) {
		job, err := s.buildJob(cleanJobs)
		if err != nil {
			log.Printf("[%s] buildJob: %v", s.coin.Symbol, err)
			return
		}
		s.mu.Lock()
		s.currentJob = job
		s.recentJobs[job.ID] = job
		if len(s.recentJobs) > s.recentJobsMax {
			var oldestID string
			var oldestTime time.Time
			for id, j := range s.recentJobs {
				if oldestID == "" || j.CreatedAt.Before(oldestTime) {
					oldestID = id
					oldestTime = j.CreatedAt
				}
			}
			delete(s.recentJobs, oldestID)
		}
		s.mu.Unlock()
		select {
		case s.jobBroadcast <- job:
		default:
			log.Printf("[%s] WARNING: job broadcast channel full, job %s dropped — miners may have stale work", s.coin.Symbol, job.ID)
		}
		// On a real new block (clean_jobs=true), immediately kick any worker that
		// is already in a stale streak — they are confirmed to be ignoring clean_jobs
		// and mining old work. Kicking at block notification time (rather than waiting
		// for N more stale submissions) caps the stale/reconnect window to at most
		// one block interval. This is especially important for fast-block coins like
		// DGBS (~15s blocks) where DG1Home firmware never self-reconnects.
		if cleanJobs {
			s.kickStaleStreakWorkers()
		}
	}

	for {
		select {
		case <-s.stopCh:
			return
		case <-zmqNotify:
			// Instant block notification via ZMQ — fetch hash and broadcast immediately
			if daemonWasDown {
				continue
			}
			hash, err := s.activeRPC().GetBestBlockHash()
			if err != nil || hash == lastHash {
				continue
			}
			lastHash = hash
			broadcastJob(true)
		case <-refreshTicker.C:
			if lastHash == "" || daemonWasDown {
				continue // not ready yet
			}
			s.mu.RLock()
			hasWorkers := len(s.workers) > 0
			s.mu.RUnlock()
			if !hasWorkers {
				continue
			}
			broadcastJob(false)
		case <-ticker.C:
			hash, err := s.activeRPC().GetBestBlockHash()
			// If the active node failed, try the other one
			if err != nil && s.backupRPC != nil {
				if s.usingBackup.Load() {
					// Backup failed — try primary
					if h2, e2 := s.rpcClient.GetBestBlockHash(); e2 == nil {
						log.Printf("[%s] backup node failed, falling back to primary", s.coin.Symbol)
						s.usingBackup.Store(false)
						hash, err = h2, nil
					}
				} else {
					// Primary failed — try backup
					if h2, e2 := s.backupRPC.GetBestBlockHash(); e2 == nil {
						log.Printf("[%s] primary node unreachable, failing over to backup node", s.coin.Symbol)
						s.usingBackup.Store(true)
						hash, err = h2, nil
					}
				}
			}
			if err != nil {
				if !daemonWasDown {
					// First failure — log and fire Discord alert
					log.Printf("[%s] daemon unreachable: %v — will retry every %v", s.coin.Symbol, err, retryDelay)
					if s.OnNodeUnreachable != nil {
						s.OnNodeUnreachable(err)
					}
					daemonWasDown = true
					daemonDownSince = time.Now()
				}
				// Watchdog: auto-restart via systemctl if down too long
				svc := s.coin.Node.SystemdService
				if svc != "" &&
					time.Since(daemonDownSince) > watchdogDelay &&
					time.Since(lastWatchdogRestart) > watchdogCooldown {
					lastWatchdogRestart = time.Now()
					log.Printf("[%s] watchdog: restarting %s after %v offline",
						s.coin.Symbol, svc, time.Since(daemonDownSince).Round(time.Second))
					if out, rerr := exec.Command("sudo", "systemctl", "restart", svc).CombinedOutput(); rerr != nil {
						log.Printf("[%s] watchdog restart %s failed: %v — %s", s.coin.Symbol, svc, rerr, out)
					} else {
						log.Printf("[%s] watchdog: %s restart issued", s.coin.Symbol, svc)
						if s.OnNodeWatchdogRestart != nil {
							s.OnNodeWatchdogRestart(svc)
						}
					}
				}
				// Activate upstream proxy fallback when node has been down > 30 seconds
				if s.coin.UpstreamPool.Enabled && !s.proxyActive.Load() &&
					!daemonDownSince.IsZero() && time.Since(daemonDownSince) > 30*time.Second {
					s.activateProxy()
				}
				// Exponential backoff up to maxRetryDelay
				time.Sleep(retryDelay)
				if retryDelay < maxRetryDelay {
					retryDelay *= 2
				}
				continue
			}

			// Daemon came back online after being down
			if daemonWasDown {
				log.Printf("[%s] daemon back online", s.coin.Symbol)
				if s.OnNodeOnline != nil {
					s.OnNodeOnline()
				}
				daemonWasDown = false
				retryDelay = 5 * time.Second
				// Deactivate upstream proxy now that the local node is back
				if s.proxyActive.Load() {
					s.deactivateProxy()
				}
			}

			if hash == lastHash {
				continue
			}
			lastHash = hash
			broadcastJob(true)
		}
	}
}

func (s *Server) buildJob(cleanJobs bool) (*Job, error) {
	bt, err := s.activeRPC().GetBlockTemplate([]string{"coinbasetxn", "workid"})
	if err != nil {
		return nil, err
	}

	// Gather AuxPoW commitment if we have merge mining children
	var auxCommitment []byte
	var auxSortedSyms []string
	var auxWorkSnap map[string]*merge.AuxWork
	if s.coordinator != nil {
		children := s.poolCfg.MergeChildren(s.coin.Symbol)
		if len(children) > 0 {
			auxWorks := make(map[string]*merge.AuxWork)
			for _, child := range children {
				if w, ok := s.coordinator.GetAuxWork(child.Symbol); ok {
					auxWorks[child.Symbol] = w
				}
			}
			if len(auxWorks) > 0 {
				auxCommitment, auxSortedSyms = merge.BuildCoinbaseCommitment(auxWorks)
				auxWorkSnap = auxWorks
			}
		}
	}

	// Build merkle branch from transactions.
	// Use TxID (non-witness), reversed to internal little-endian byte order for Stratum.
	txHashes := make([]string, len(bt.Transactions))
	txHexes := make([]string, len(bt.Transactions))
	for i, tx := range bt.Transactions {
		txHashes[i] = hexReverseBytes(tx.TxID)
		txHexes[i] = tx.Data // raw serialized transaction for block assembly
	}
	merkleBranches := buildMerkleBranch(txHashes)

	// Build coinbase parts with tag embedded
	// part1 ends just before extranonce, part2 starts after
	// The tag (e.g. "/PiPool/") sits between the BIP34 height and the extranonce
	cbPart1, cbPart2 := rpc.CreateCoinbaseTx(
		s.coin.Wallet,
		bt.CoinbaseValue,
		bt.Height,
		coinbaseENSlot, // extranonce1 (4) + max extranonce2 (7) = 11 bytes; shorter en2 is zero-padded
		s.coinbaseTag,
		bt.DefaultWitnessCommitment,
		auxCommitment, // ← new parameter
	)

	// Update network difficulty from this job's nBits
	if nd := diffFromBits(bt.Bits, s.coin.Algorithm); nd > 0 {
		s.mu.Lock()
		s.lastNetworkDiff = nd
		s.mu.Unlock()
	}

	job := &Job{
		ID:             fmt.Sprintf("%x", rand.Uint32()),
		// ESP-Miner/BitAxe firmware applies reverseByteOrder to the pool's prevhash
		// before placing it in the ASIC header. To ensure the miner hashes the correct
		// wire-format prevhash (hexReverseBytes of display hash), we send the inverse:
		// reverseByteOrder(wire) so that reverseByteOrder(reverseByteOrder(wire)) = wire.
		PrevHash: reverseByteOrder(hexReverseBytes(bt.PreviousBlockHash)),
		CoinbasePart1:  fmt.Sprintf("%x", cbPart1),
		CoinbasePart2:  fmt.Sprintf("%x", cbPart2),
		MerkleBranches: merkleBranches,
		Version:        fmt.Sprintf("%08x", bt.Version),
		NBits:          bt.Bits,
		NTime:          fmt.Sprintf("%08x", bt.CurTime),
		CleanJobs:      cleanJobs,
		Height:         bt.Height,
		Target:         bt.Target,
		AuxCommitment:  auxCommitment,
		AuxSortedSyms:  auxSortedSyms,
		AuxWorks:       auxWorkSnap,
		RawTxHexes:     txHexes,
		PowAlgo:        bt.PowAlgo,
		CreatedAt:      time.Now(),
	}

	return job, nil
}

// ─── Worker connection handler ────────────────────────────────────────────────

func (s *Server) handleWorker(conn net.Conn) {
	defer s.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[%s] panic in worker handler (recovered): %v", s.coin.Symbol, r)
		}
	}()
	workerID := fmt.Sprintf("%x", rand.Uint32())
	w := &Worker{
		id:             workerID,
		conn:           conn,
		writer:         bufio.NewWriter(conn),
		remoteAddr:     conn.RemoteAddr().String(),
		difficulty:     s.coin.Stratum.Vardiff.MinDiff,
		lastShareAt:    time.Now(),
		lastRetargetAt: time.Now(),
		connectedAt:    time.Now(),
		extranonce1:    fmt.Sprintf("%08x", rand.Uint32()),
		en2Size:        defaultEN2Size,
	}

	conn.SetReadDeadline(time.Now().Add(s.poolCfg.Pool.WorkerTimeoutDuration()))

	s.mu.Lock()
	s.workers[workerID] = w
	s.mu.Unlock()

	log.Printf("[%s] miner connected: %s", s.coin.Symbol, conn.RemoteAddr())

	defer func() {
		conn.Close()
		s.mu.Lock()
		delete(s.workers, workerID)
		delete(s.workerKickTimes, w.workerName)
		// Update seenWorkers on disconnect
		if w.authorized && w.workerName != "" {
			if seen, ok := s.seenWorkers[w.workerName]; ok {
				seen.Online = false
				seen.LastSeenAt = time.Now()
				seen.SharesAccepted = seen.SharesAcceptedBase + w.sharesAccepted
				seen.SharesRejected = seen.SharesRejectedBase + w.sharesRejected
				seen.SharesStale    = seen.SharesStaleBase    + w.sharesStale
				if w.bestShare > seen.BestShare {
					seen.BestShare = w.bestShare
				}
				seen.LastDifficulty = w.difficulty
				seen.DiffHistory = w.diffHistory
			}
		}
		s.mu.Unlock()
		if w.authorized {
			s.connectedMiners.Add(-1)
			go s.saveWorkerState() // persist LastDifficulty and BestShare for next startup
		}
		if w.authorized {
			log.Printf("[%s] miner disconnected: %s (%s)", s.coin.Symbol, w.workerName, conn.RemoteAddr())
		}
		// Smarter offline alert: wait 3 minutes before firing Discord notification.
		// If the worker reconnects within that window, skip the alert to avoid noise.
		if s.OnMinerDisconnect != nil && w.authorized {
			disconnectedName := w.workerName
			onDisconnect := s.OnMinerDisconnect
			go func() {
				select {
				case <-time.After(3 * time.Minute):
				case <-s.stopCh:
					return
				}
				s.mu.RLock()
				sw, exists := s.seenWorkers[disconnectedName]
				stillOffline := !exists || !sw.Online
				s.mu.RUnlock()
				if stillOffline {
					onDisconnect(disconnectedName)
				}
			}()
		}
	}()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), 4096) // cap per-worker buffer for RAM

	for scanner.Scan() {
		conn.SetReadDeadline(time.Now().Add(s.poolCfg.Pool.WorkerTimeoutDuration()))

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg stratumMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			log.Printf("[%s] bad message from %s: %v", s.coin.Symbol, conn.RemoteAddr(), err)
			continue
		}

		if err := s.handleMessage(w, &msg); err != nil {
			log.Printf("[%s] handle message error: %v", s.coin.Symbol, err)
		}
	}
}

// ─── Stratum message types ────────────────────────────────────────────────────

type stratumMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type stratumReply struct {
	ID     json.RawMessage `json:"id"`
	Result any             `json:"result"`
	Error  any             `json:"error"`
}

type stratumNotify struct {
	ID     json.Number `json:"id"`
	Method string      `json:"method"`
	Params any         `json:"params"`
}

func (s *Server) handleMessage(w *Worker, msg *stratumMsg) error {
	switch msg.Method {
	case "mining.subscribe":
		return s.handleSubscribe(w, msg)
	case "mining.authorize":
		return s.handleAuthorize(w, msg)
	case "mining.submit":
		return s.handleSubmit(w, msg)
	case "mining.configure":
		// Parse requested extensions and acknowledge version rolling (BIP310 / overt AsicBoost).
		// Params: [["version-rolling", ...], {"version-rolling.mask": "1fffe000", ...}]
		result := map[string]any{}
		var rawParams []json.RawMessage
		if json.Unmarshal(msg.Params, &rawParams) == nil && len(rawParams) >= 1 {
			var exts []string
			if json.Unmarshal(rawParams[0], &exts) == nil {
				for _, ext := range exts {
					if ext == "version-rolling" {
						// Allow overt AsicBoost. Use the mask the miner requested if available,
						// falling back to the standard BIP310 range.
						mask := "1fffe000"
						if len(rawParams) >= 2 {
							var opts map[string]json.RawMessage
							if json.Unmarshal(rawParams[1], &opts) == nil {
								if m, ok := opts["version-rolling.mask"]; ok {
									var s string
									if json.Unmarshal(m, &s) == nil && s != "" {
										mask = s
									}
								}
							}
						}
						result["version-rolling"] = true
						result["version-rolling.mask"] = mask
					}
				}
			}
		}
		return s.reply(w, msg.ID, result, nil)
	case "mining.suggest_difficulty":
		// Parse the requested difficulty and store as per-worker vardiff floor.
		var sdParams []json.RawMessage
		if json.Unmarshal(msg.Params, &sdParams) == nil && len(sdParams) > 0 {
			var sugDiff float64
			if json.Unmarshal(sdParams[0], &sugDiff) == nil && sugDiff > 0 {
				vd := s.coin.Stratum.Vardiff
				if sugDiff < vd.MinDiff {
					sugDiff = vd.MinDiff
				}
				if sugDiff > vd.MaxDiff {
					sugDiff = vd.MaxDiff
				}
				w.mu.Lock()
				w.suggestedMinDiff = sugDiff
				// Immediately apply if current difficulty is below the requested floor
				if w.difficulty < sugDiff {
					w.difficulty = sugDiff
					w.mu.Unlock()
					s.sendDifficulty(w, sugDiff)
				} else {
					w.mu.Unlock()
				}
			}
		}
		return s.reply(w, msg.ID, true, nil)
	case "mining.extranonce.subscribe":
		w.mu.Lock()
		w.supportsEnSub = true
		w.mu.Unlock()
		log.Printf("[%s] %s supports mining.extranonce.subscribe", s.coin.Symbol, w.remoteAddr)
		return s.reply(w, msg.ID, true, nil)
	default:
		log.Printf("[%s] unknown method: %s", s.coin.Symbol, msg.Method)
		return s.reply(w, msg.ID, nil, []any{20, "Unknown method", nil})
	}
}

func (s *Server) handleSubscribe(w *Worker, msg *stratumMsg) error {
	// Extract user-agent from subscribe params if present
	// mining.subscribe params: ["user-agent/version", "session-id"]
	// Braiins firmware may also send a requested extranonce2 size.
	var params []json.RawMessage
	json.Unmarshal(msg.Params, &params)
	if len(params) > 0 {
		var ua string
		if json.Unmarshal(params[0], &ua) == nil {
			w.userAgent = ua
		}
	}
	log.Printf("[%s] subscribe from %s: user-agent=%q params=%s", s.coin.Symbol, w.remoteAddr, w.userAgent, msg.Params)

	// Detect Braiins firmware extranonce2 size request.
	// Braiins sends mining.subscribe with params like:
	//   ["braiins-os/version", null, "host", port]
	// and expects the pool to honor larger extranonce2 sizes.
	// We also check the user-agent string for "braiins" or "bos" as a signal.
	requestedEN2 := defaultEN2Size
	ua := strings.ToLower(w.userAgent)
	if strings.Contains(ua, "braiins") || strings.Contains(ua, "bosminer") || strings.Contains(ua, "/bos") {
		requestedEN2 = maxEN2Size
	}
	if requestedEN2 > maxEN2Size {
		requestedEN2 = maxEN2Size
	}
	if requestedEN2 < defaultEN2Size {
		requestedEN2 = defaultEN2Size
	}
	w.en2Size = requestedEN2

	// When upstream proxy is active, use the upstream extranonce1 so miner
	// shares are valid against the upstream pool's template.
	en1 := w.extranonce1
	if s.proxyActive.Load() {
		s.mu.RLock()
		upEn1 := s.upstreamEn1
		s.mu.RUnlock()
		if upEn1 != "" {
			en1 = upEn1
			w.extranonce1 = upEn1
		}
	}

	// Reply: [session_id, extranonce1, extranonce2_size]
	result := []any{
		[]any{
			[]string{"mining.set_difficulty", w.id},
			[]string{"mining.notify", w.id},
		},
		en1,
		w.en2Size,
	}
	if err := s.reply(w, msg.ID, result, nil); err != nil {
		return err
	}
	if w.en2Size != defaultEN2Size {
		log.Printf("[%s] %s granted extranonce2_size=%d (firmware: %s)", s.coin.Symbol, w.remoteAddr, w.en2Size, w.userAgent)
	}
	// Send initial difficulty
	return s.sendDifficulty(w, w.difficulty)
}

func (s *Server) handleAuthorize(w *Worker, msg *stratumMsg) error {
	var params []string
	json.Unmarshal(msg.Params, &params)

	workerName := "anonymous"
	if len(params) > 0 {
		workerName = params[0]
	}

	// ── Spiral Router: classify device BEFORE acquiring any locks ─────────────
	// Doing routing first avoids holding w.mu and s.mu simultaneously (deadlock risk:
	// broadcastJobs holds s.mu.RLock then tries w.mu; we must not do the reverse).
	device := RouteWorker(w.userAgent, workerName, s.coin.Algorithm)
	vd := s.coin.Stratum.Vardiff
	startDiff := device.StartDiff
	if startDiff < vd.MinDiff {
		startDiff = vd.MinDiff
	}
	if startDiff > vd.MaxDiff {
		startDiff = vd.MaxDiff
	}

	// Update seenWorkers under s.mu only (never while holding w.mu).
	s.mu.Lock()
	now := time.Now()
	if existing, ok := s.seenWorkers[workerName]; ok {
		// Snapshot the running total as the base for this new session so share
		// counts accumulate across reconnects rather than resetting to zero.
		existing.SharesAcceptedBase = existing.SharesAccepted
		existing.SharesRejectedBase = existing.SharesRejected
		existing.SharesStaleBase    = existing.SharesStale
		existing.Online = true
		existing.LastSeenAt = now
		existing.LastAddr = w.remoteAddr
		existing.DeviceName = device.Name
		existing.UserAgent = w.userAgent
		existing.ReconnectCount++
		existing.SessionStartedAt = now
		// Restore last-known difficulty so the miner doesn't flood the pool with
		// thousands of low-diff shares before vardiff catches up.
		if existing.LastDifficulty >= vd.MinDiff && existing.LastDifficulty <= vd.MaxDiff {
			startDiff = existing.LastDifficulty
		}
		w.diffHistory = existing.DiffHistory
	} else {
		if len(s.seenWorkers) >= 500 {
			// Evict the least-recently-seen offline worker
			var oldest string
			var oldestTime time.Time
			for k, v := range s.seenWorkers {
				if !v.Online && (oldest == "" || v.LastSeenAt.Before(oldestTime)) {
					oldest = k
					oldestTime = v.LastSeenAt
				}
			}
			if oldest != "" {
				delete(s.seenWorkers, oldest)
			}
		}
		s.seenWorkers[workerName] = &SeenWorker{
			Name:             workerName,
			DeviceName:       device.Name,
			UserAgent:        w.userAgent,
			LastAddr:         w.remoteAddr,
			Coin:             s.coin.Symbol,
			ConnectedAt:      now,
			LastSeenAt:       now,
			SessionStartedAt: now,
			Online:           true,
		}
	}
	s.mu.Unlock()

	// Check for a fixed difficulty override from config (coin-level takes priority over global)
	fixedDiff := s.coinFixedDiff(workerName)
	if fixedDiff > 0 {
		startDiff = fixedDiff
	}

	// Update worker fields under w.mu only (never while holding s.mu).
	w.mu.Lock()
	prevDiff := w.difficulty
	w.authorized = true
	w.workerName = workerName
	w.deviceName = device.Name
	w.difficulty = startDiff
	w.fixedDiff = fixedDiff
	w.mu.Unlock()

	// Count only authorized workers so the coin "Miners" card stays in sync
	// with the workers table (which only shows authorized workers).
	s.connectedMiners.Add(1)

	log.Printf("[%s] worker authorized: %s | device: %s | start-diff: %.4f | from: %s",
		s.coin.Symbol, workerName, device.Name, startDiff, w.remoteAddr)

	if err := s.reply(w, msg.ID, true, nil); err != nil {
		return err
	}

	// If device routing changed the difficulty from the subscribe-time default,
	// push an updated mining.set_difficulty before sending the job.
	if startDiff != prevDiff {
		if err := s.sendDifficulty(w, startDiff); err != nil {
			return err
		}
	}

	if s.OnMinerConnect != nil {
		s.OnMinerConnect(workerName, w.remoteAddr)
	}

	// Send current job
	s.mu.RLock()
	job := s.currentJob
	s.mu.RUnlock()

	if job != nil {
		return s.sendJob(w, job)
	}
	return nil
}

func (s *Server) handleSubmit(w *Worker, msg *stratumMsg) error {
	s.totalShares.Add(1)

	var params []string
	json.Unmarshal(msg.Params, &params)
	// params: [worker_name, job_id, extranonce2, ntime, nonce] or [... nonce, version_bits]
	if len(params) < 5 {
		w.sharesRejected++
		return s.reply(w, msg.ID, false, []any{20, "Malformed params", nil})
	}

	// Look up the exact job the miner was working on.
	// Shares for any recent job in the window are accepted (not stale).
	s.mu.RLock()
	submittedJobID := params[1]
	job, jobFound := s.recentJobs[submittedJobID]
	hasAnyJob := s.currentJob != nil
	s.mu.RUnlock()

	if !hasAnyJob {
		w.sharesRejected++
		return s.reply(w, msg.ID, false, []any{21, "No job", nil})
	}

	if !jobFound {
		w.sharesStale++
		s.updateSeenWorkerShares(w)
		// Track consecutive stales on the same old job (firmware ignoring clean_jobs).
		// Kick threshold is configurable per coin (stale_kick_count in stratum config).
		// Default 5; set lower (e.g. 2) for fast-block coins like DGBS to reduce
		// the stale/reconnect overhead window.
		kickAt := s.coin.Stratum.StaleKickCount
		if kickAt <= 0 {
			kickAt = 5
		}
		if submittedJobID == w.lastStaleJobID {
			w.staleJobStreak++
		} else {
			w.lastStaleJobID = submittedJobID
			w.staleJobStreak = 1
		}
		if w.staleJobStreak >= kickAt {
			log.Printf("[%s] kicking %s: %d consecutive stale shares for job %s (firmware ignoring clean_jobs)",
				s.coin.Symbol, w.workerName, w.staleJobStreak, submittedJobID)
			s.reply(w, msg.ID, false, []any{21, "Stale share", nil})
			s.recordStaleKick(w.workerName)
			w.conn.Close()
			return nil
		}
		log.Printf("[%s] stale share from %s (job %s not in recent window)", s.coin.Symbol, w.workerName, submittedJobID)
		return s.reply(w, msg.ID, false, []any{21, "Stale share", nil})
	}

	// Validate the share: assemble header and check hash meets worker difficulty
	extranonce2 := params[2]
	// Extranonce2 must match the size declared in mining.subscribe (en2Size bytes = en2Size*2 hex chars)
	expectedEN2Hex := w.en2Size * 2
	if len(extranonce2) != expectedEN2Hex {
		w.sharesRejected++
		s.recordShareSample(w.difficulty, false, w.workerName)
		return s.reply(w, msg.ID, false, []any{20, "Invalid extranonce2 length", nil})
	}
	ntime := params[3]
	nonce := params[4]

	// Handle version rolling (BIP310 / overt AsicBoost).
	// Miners like ESP-Miner/NerdOCTAXE submit a 6th param (version_bits) when
	// they modify the version field. The actual version = job.Version | version_bits.
	effectiveVersion := job.Version
	if len(params) >= 6 && params[5] != "" {
		jobVI, err1 := strconv.ParseUint(job.Version, 16, 32)
		vbI, err2 := strconv.ParseUint(params[5], 16, 32)
		if err1 == nil && err2 == nil {
			effectiveVersion = fmt.Sprintf("%08x", jobVI|vbI)
		}
	}

	log.Printf("[%s] SUBMIT from %s: extranonce1=%s en2=%s ntime=%s nonce=%s versionBits=%s effectiveVersion=%s diff=%.4f",
		s.coin.Symbol, w.workerName, w.extranonce1, extranonce2, ntime, nonce,
		func() string { if len(params) >= 6 { return params[5] }; return "none" }(),
		effectiveVersion, w.difficulty)

	// Use the lower of current and previous difficulty for validation.
	// When vardiff raises difficulty, the miner may still have in-flight shares
	// computed at the old (lower) difficulty. Validate at whichever is easier
	// so these transitional shares aren't unfairly rejected.
	validationDiff := w.difficulty
	if w.prevDifficulty > 0 && w.prevDifficulty < w.difficulty {
		validationDiff = w.prevDifficulty
	}

	valid, isBlock, effectiveNonce := s.validateShare(job, w.extranonce1, extranonce2, ntime, nonce, effectiveVersion, validationDiff)

	// DGB multi-algo: only submit blocks when the template's PoW algo matches
	// our configured algorithm. The node rotates through 5 algos and we can only
	// solve blocks for the algo our miners actually compute.
	if isBlock && job.PowAlgo != "" {
		algoMatch := (s.coin.Algorithm == "sha256d" && job.PowAlgo == "sha256d") ||
			(s.coin.Algorithm == "scrypt" && job.PowAlgo == "scrypt")
		if !algoMatch {
			isBlock = false
		}
	}

	if !valid {
		w.sharesRejected++
		s.updateSeenWorkerShares(w)
		s.recordShareSample(validationDiff, false, w.workerName)
		s.decayVardiff(w)
		log.Printf("[%s] rejected share from %s (hash above target)", s.coin.Symbol, w.workerName)
		// Auto-kick: disconnect workers with persistently high invalid share rates
		autoKickPct := s.poolCfg.Pool.AutoKickRejectPct
		autoKickMin := s.poolCfg.Pool.AutoKickMinShares
		if autoKickPct > 0 && autoKickMin > 0 {
			total := w.sharesAccepted + w.sharesRejected + w.sharesStale
			if int(total) >= autoKickMin {
				rejectPct := float64(w.sharesRejected) / float64(total) * 100
				if rejectPct >= float64(autoKickPct) {
					log.Printf("[%s] auto-kick %s: reject rate %.1f%% >= threshold %d%% after %d shares",
						s.coin.Symbol, w.workerName, rejectPct, autoKickPct, total)
					w.conn.Close()
				}
			}
		}
		return s.reply(w, msg.ID, false, []any{23, "Low difficulty share", nil})
	}

	w.sharesAccepted++
	w.lastShareAt = time.Now()
	w.lastAcceptedAt = time.Now()
	w.staleJobStreak = 0 // reset stale-kick counter on good share
	s.validShares.Add(1)
	// Track best share per worker and all-time
	if validationDiff > w.bestShare {
		w.bestShare = validationDiff
	}
	s.mu.Lock()
	if validationDiff > s.bestShareEver {
		s.bestShareEver = validationDiff
	}
	s.mu.Unlock()
	s.updateSeenWorkerShares(w)
	s.recordShareSample(validationDiff, true, w.workerName)

	// PPLNS share callback
	if s.OnShareAccepted != nil {
		s.OnShareAccepted(s.coin.Symbol, w.workerName, w.difficulty)
	}

	// Track work for luck computation
	s.mu.Lock()
	s.validWorkSinceBlock += validationDiff
	s.mu.Unlock()

	// Adjust vardiff
	s.adjustVardiff(w)

	log.Printf("[%s] share accepted from %s diff=%.4g", s.coin.Symbol, w.workerName, w.difficulty)

	if isBlock {
		log.Printf("[%s] BLOCK candidate by %s — assembling for submission", s.coin.Symbol, w.workerName)

		reward := s.coin.BlockReward
		blockHex, blockHash, headerBytes, coinbaseTxBytes := s.assembleBlockHex(job, w.extranonce1, extranonce2, ntime, effectiveNonce, effectiveVersion)
		if blockHex == "" {
			log.Printf("[%s] failed to assemble block hex — block cannot be submitted", s.coin.Symbol)
		} else {
			// Snapshot luck before resetting work counter (luck is per-block, not per-submit)
			s.mu.Lock()
			luck := -1.0
			validWork := s.validWorkSinceBlock
			netDiff := s.lastNetworkDiff
			if validWork > 0 && netDiff > 0 {
				luck = netDiff / validWork * 100
			}
			s.validWorkSinceBlock = 0 // reset for next block
			s.mu.Unlock()

			// Capture callbacks/aux info before goroutine — avoid closing over mutable state
			onBlockFound := s.OnBlockFound
			onAuxBlockFound := s.OnAuxBlockFound
			coinSym := s.coin.Symbol
			height := job.Height
			workerName := w.workerName

			var auxInfo *AuxBlockInfo
			if onAuxBlockFound != nil {
				merkleBranch := make([][]byte, len(job.MerkleBranches))
				for i, br := range job.MerkleBranches {
					b, _ := hex.DecodeString(br)
					merkleBranch[i] = b
				}
				auxInfo = &AuxBlockInfo{
					CoinbaseTx:    coinbaseTxBytes,
					MerkleBranch:  merkleBranch,
					HeaderBytes:   headerBytes,
					AuxSortedSyms: job.AuxSortedSyms,
					AuxWorkSnap:   job.AuxWorks,
				}
			}

			go func() {
				result, err := s.rpcClient.SubmitBlock(blockHex)
				if err != nil {
					log.Printf("[%s] submitblock error: %v", coinSym, err)
					return
				}
				if result != "" {
					log.Printf("[%s] block rejected by node: %s (submitted by %s)", coinSym, result, workerName)
					return
				}
				// Node accepted the block — now record it
				log.Printf("[%s] block #%d ACCEPTED by node! found by %s (luck %.1f%%)", coinSym, height, workerName, luck)
				s.blocksFound.Add(1)
				s.recordBlock(blockHash, height, reward, workerName, luck)
				if onBlockFound != nil {
					onBlockFound(coinSym, blockHash, workerName, reward, luck)
				}
				if onAuxBlockFound != nil && auxInfo != nil {
					onAuxBlockFound(*auxInfo)
				}
			}()
		}
	}

	// When proxy is active and the share is valid but not a block, forward to upstream.
	if s.proxyActive.Load() && s.upstream != nil {
		accepted := s.upstream.Submit(params[0], params[1], params[2], params[3], params[4])
		if !accepted {
			log.Printf("[%s] upstream rejected share from %s", s.coin.Symbol, w.workerName)
			return s.reply(w, msg.ID, false, []any{23, "Upstream rejected", nil})
		}
	}

	return s.reply(w, msg.ID, true, nil)
}

// updateSeenWorkerShares syncs current share counts to seenWorkers map.
// Total counts accumulate across reconnects by adding the current session
// onto the base saved when the worker last authorized.
func (s *Server) updateSeenWorkerShares(w *Worker) {
	if w.workerName == "" {
		return
	}
	s.mu.Lock()
	if seen, ok := s.seenWorkers[w.workerName]; ok {
		seen.SharesAccepted = seen.SharesAcceptedBase + w.sharesAccepted
		seen.SharesRejected = seen.SharesRejectedBase + w.sharesRejected
		seen.SharesStale    = seen.SharesStaleBase    + w.sharesStale
		seen.LastSeenAt     = time.Now()
		if w.bestShare > seen.BestShare {
			seen.BestShare = w.bestShare
		}
	}
	s.mu.Unlock()
}

// validateShare assembles the block header and checks whether the hash meets
// the worker's current difficulty target AND whether it meets the block target.
// Returns (shareValid, isBlock, effectiveNonce).
//
// effectiveNonce is the nonce hex string (possibly byte-reversed from submitted)
// that produced the valid hash. Some firmware (ESP-Miner/BitAxe) submits the
// nonce with bytes reversed relative to what they placed in the header; we try
// both orientations and accept whichever validates.
func (s *Server) validateShare(job *Job, extranonce1, extranonce2, ntime, nonce, effectiveVersion string, workerDiff float64) (bool, bool, string) {
	// Build coinbase transaction hash (same regardless of nonce orientation)
	// Zero-pad the extranonce to fill the full coinbase slot (11 bytes = 22 hex chars).
	// This ensures miners with different en2 sizes (4 vs 7) produce valid coinbases.
	enHex := extranonce1 + extranonce2
	if pad := coinbaseENSlot*2 - len(enHex); pad > 0 {
		enHex += strings.Repeat("0", pad)
	}
	coinbaseHex := job.CoinbasePart1 + enHex + job.CoinbasePart2
	coinbaseBytes, err := hex.DecodeString(coinbaseHex)
	if err != nil {
		return false, false, nonce
	}
	coinbaseHash := dsha256(coinbaseBytes)


	// Compute merkle root from coinbase hash + merkle branches
	merkleRoot := make([]byte, 32)
	copy(merkleRoot, coinbaseHash[:])
	for _, branch := range job.MerkleBranches {
		branchBytes, err := hex.DecodeString(branch)
		if err != nil {
			return false, false, nonce
		}
		combined := append(merkleRoot, branchBytes...)
		h := dsha256(combined)
		merkleRoot = make([]byte, 32)
		copy(merkleRoot, h[:])
	}

	// Decode fixed fields.
	// version, ntime, and nbits are sent by pools as big-endian hex strings, but
	// the Bitcoin wire format (what the ASIC actually hashes) requires them as
	// little-endian 4-byte values. We parse each as a uint32 and write LE.
	versionInt, err := strconv.ParseUint(effectiveVersion, 16, 32)
	if err != nil {
		return false, false, nonce
	}
	versionBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(versionBytes, uint32(versionInt))

	// job.PrevHash is sent as reverseByteOrder(wire). The miner applies reverseByteOrder
	// to produce wire-format bytes in the ASIC header. Reconstruct the same bytes here.
	prevHashBytes, err := hex.DecodeString(reverseByteOrder(job.PrevHash))
	if err != nil || len(prevHashBytes) != 32 {
		return false, false, nonce
	}

	ntimeInt, err := strconv.ParseUint(ntime, 16, 32)
	if err != nil {
		return false, false, nonce
	}
	ntimeBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(ntimeBytes, uint32(ntimeInt))

	nbitsInt, err := strconv.ParseUint(job.NBits, 16, 32)
	if err != nil {
		return false, false, nonce
	}
	nbitsBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(nbitsBytes, uint32(nbitsInt))

	// Compute share and block targets using big-endian nbits (as returned by node).
	nbitsForTarget, _ := hex.DecodeString(job.NBits)
	blockTarget := nbitsToTarget(nbitsForTarget)
	diff1 := diff1Target(s.coin.Algorithm)
	workerDiffBig := new(big.Float).SetFloat64(workerDiff)
	diff1Float := new(big.Float).SetInt(diff1)
	shareTargetFloat := new(big.Float).Quo(diff1Float, workerDiffBig)
	shareTarget, _ := shareTargetFloat.Int(nil)

	// Try both nonce orientations.
	// ESP-Miner/BitAxe firmware submits the nonce byte-reversed from what
	// the ASIC chip placed in the header — we must try both to be compatible.
	nonceHexes := []string{nonce, hexReverseBytes(nonce)}
	for _, tryNonce := range nonceHexes {
		nonceBytes, err := hex.DecodeString(tryNonce)
		if err != nil || len(nonceBytes) != 4 {
			continue
		}

		header := make([]byte, 80)
		copy(header[0:4], versionBytes)
		copy(header[4:36], prevHashBytes)
		copy(header[36:68], merkleRoot)
		copy(header[68:72], ntimeBytes)
		copy(header[72:76], nbitsBytes)
		copy(header[76:80], nonceBytes)

		hashRaw, err := hashHeader(header, s.coin.Algorithm)
		if err != nil {
			continue
		}

		// Reverse hash bytes for big-integer comparison (Bitcoin convention:
		// the display/comparison hash is the byte-reverse of the SHA-256d output).
		hashBig := new(big.Int).SetBytes(reverseBytes(hashRaw))

		if hashBig.Cmp(shareTarget) <= 0 {
			isBlock := hashBig.Cmp(blockTarget) <= 0
			return true, isBlock, tryNonce
		}
	}

	return false, false, nonce
}

// assembleBlockHex builds a complete serialized block hex ready for submitblock.
// Format: 80-byte header | varint(tx_count) | coinbase_tx | tx_1 | tx_2 | ...
//
// effectiveNonce is the nonce string (from validateShare) that produced the
// valid hash — it may differ from the originally submitted nonce if byte-reversal
// was needed for ESP-Miner/BitAxe compatibility.
//
// All header fields are converted to Bitcoin wire format (little-endian) as
// required by the node's submitblock RPC.
func (s *Server) assembleBlockHex(job *Job, extranonce1, extranonce2, ntime, effectiveNonce, effectiveVersion string) (blockHex, blockHash string, headerBytes, coinbaseTxBytes []byte) {
	// Zero-pad extranonce to fill the full coinbase slot (11 bytes).
	enHex := extranonce1 + extranonce2
	if pad := coinbaseENSlot*2 - len(enHex); pad > 0 {
		enHex += strings.Repeat("0", pad)
	}
	coinbaseHex := job.CoinbasePart1 + enHex + job.CoinbasePart2
	coinbaseBytes, err := hex.DecodeString(coinbaseHex)
	if err != nil {
		log.Printf("[%s] assembleBlockHex: invalid coinbase hex: %v", s.coin.Symbol, err)
		return "", "", nil, nil
	}
	coinbaseHash := dsha256(coinbaseBytes)

	merkleRoot := make([]byte, 32)
	copy(merkleRoot, coinbaseHash[:])
	for _, branch := range job.MerkleBranches {
		branchBytes, err := hex.DecodeString(branch)
		if err != nil {
			log.Printf("[%s] assembleBlockHex: invalid merkle branch hex: %v", s.coin.Symbol, err)
			return "", "", nil, nil
		}
		combined := append(merkleRoot, branchBytes...)
		h := dsha256(combined)
		merkleRoot = make([]byte, 32)
		copy(merkleRoot, h[:])
	}

	// Bitcoin wire format requires all 32-bit fields as little-endian uint32.
	// The Stratum fields are big-endian hex; reverse 4-byte groups to get LE.

	// version: parse effective version (big-endian Stratum hex) → little-endian wire bytes
	versionInt, _ := strconv.ParseUint(effectiveVersion, 16, 32)
	versionBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(versionBytes, uint32(versionInt))

	// prevhash: job.PrevHash is sent to miners as reverseByteOrder(wire), so the miner's
	// reverseByteOrder transform produces the correct wire bytes. Use the same wire bytes
	// here so the submitted block header matches what was actually hashed.
	prevHashBytes, err := hex.DecodeString(reverseByteOrder(job.PrevHash))
	if err != nil || len(prevHashBytes) != 32 {
		log.Printf("[%s] assembleBlockHex: invalid prevhash: %v", s.coin.Symbol, err)
		return "", "", nil, nil
	}

	// ntime: big-endian Stratum hex → little-endian wire bytes (byte-swap in place)
	ntimeBytes, err := hex.DecodeString(ntime)
	if err != nil || len(ntimeBytes) != 4 {
		log.Printf("[%s] assembleBlockHex: invalid ntime %q: %v", s.coin.Symbol, ntime, err)
		return "", "", nil, nil
	}
	ntimeBytes[0], ntimeBytes[3] = ntimeBytes[3], ntimeBytes[0]
	ntimeBytes[1], ntimeBytes[2] = ntimeBytes[2], ntimeBytes[1]

	// nbits: big-endian Stratum hex → little-endian wire bytes (byte-swap in place)
	nbitsBytes, err := hex.DecodeString(job.NBits)
	if err != nil || len(nbitsBytes) != 4 {
		log.Printf("[%s] assembleBlockHex: invalid nbits %q: %v", s.coin.Symbol, job.NBits, err)
		return "", "", nil, nil
	}
	nbitsBytes[0], nbitsBytes[3] = nbitsBytes[3], nbitsBytes[0]
	nbitsBytes[1], nbitsBytes[2] = nbitsBytes[2], nbitsBytes[1]

	// nonce: effectiveNonce bytes are already the correct wire bytes
	// (validateShare already resolved any byte-order ambiguity)
	nonceBytes, err := hex.DecodeString(effectiveNonce)
	if err != nil || len(nonceBytes) != 4 {
		log.Printf("[%s] assembleBlockHex: invalid nonce %q: %v", s.coin.Symbol, effectiveNonce, err)
		return "", "", nil, nil
	}

	header := make([]byte, 80)
	copy(header[0:4], versionBytes)
	copy(header[4:36], prevHashBytes)
	copy(header[36:68], merkleRoot)
	copy(header[68:72], ntimeBytes)
	copy(header[72:76], nbitsBytes)
	copy(header[76:80], nonceBytes)

	// Build the complete block: header + varint(txcount) + coinbase + other txs.
	var block bytes.Buffer
	block.Write(header)
	txCount := 1 + len(job.RawTxHexes)
	block.Write(varInt(uint64(txCount)))
	block.Write(coinbaseBytes)
	for _, txHex := range job.RawTxHexes {
		txBytes, err := hex.DecodeString(txHex)
		if err != nil {
			log.Printf("[%s] assembleBlockHex: invalid tx hex: %v", s.coin.Symbol, err)
			return "", "", nil, nil
		}
		block.Write(txBytes)
	}
	h := dsha256(header)
	blockHash = hex.EncodeToString(reverseBytes(h[:]))
	blockHex = hex.EncodeToString(block.Bytes())
	return blockHex, blockHash, header, coinbaseBytes
}

// ─── PoW math helpers ─────────────────────────────────────────────────────────

func dsha256(data []byte) [32]byte {
	h1 := sha256.Sum256(data)
	return sha256.Sum256(h1[:])
}

// hashHeader hashes an 80-byte block header using the coin's proof-of-work algorithm.
// Returns the raw hash bytes in little-endian order (as produced by the hash function).
func hashHeader(header []byte, algo string) ([]byte, error) {
	switch algo {
	case "scrypt":
		// Standard Scrypt (LTC, DOGE, DGBS): N=1024, r=1, p=1, keyLen=32
		return scrypt.Key(header, header, 1024, 1, 1, 32)
	case "scryptn":
		// Scrypt-N (Pepecoin): N=2048, r=1, p=1, keyLen=32
		// Pepecoin uses a larger N value than standard Scrypt for increased memory hardness
		return scrypt.Key(header, header, 2048, 1, 1, 32)
	default:
		// sha256d for BTC, BCH, and any unknown algorithm.
		h := dsha256(header)
		return h[:], nil
	}
}

// varInt encodes n as a Bitcoin-style compact variable-length integer.
func varInt(n uint64) []byte {
	switch {
	case n < 0xfd:
		return []byte{byte(n)}
	case n <= 0xffff:
		return []byte{0xfd, byte(n), byte(n >> 8)}
	case n <= 0xffffffff:
		return []byte{0xfe, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
	default:
		return []byte{0xff,
			byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24),
			byte(n >> 32), byte(n >> 40), byte(n >> 48), byte(n >> 56),
		}
	}
}

func reverseBytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i, v := range b {
		out[len(b)-1-i] = v
	}
	return out
}

// nbitsToTarget expands compact nbits to a 32-byte big.Int target
func nbitsToTarget(nbits []byte) *big.Int {
	exp := int(nbits[0])
	mantissa := new(big.Int).SetBytes(nbits[1:4])
	shift := 8 * (exp - 3)
	if shift >= 0 {
		return new(big.Int).Lsh(mantissa, uint(shift))
	}
	return new(big.Int).Rsh(mantissa, uint(-shift))
}

// diff1Target returns the difficulty-1 target for a given algorithm
// This is the maximum hash value that counts as difficulty 1
func diff1Target(algo string) *big.Int {
	switch algo {
	case "sha256d":
		// Bitcoin diff-1: 0x00000000FFFF0000...0000 (26 zero bytes)
		t, _ := new(big.Int).SetString("00000000FFFF0000000000000000000000000000000000000000000000000000", 16)
		return t
	case "scrypt", "scryptn":
		// Litecoin diff-1: 0x0000FFFF00000000...0000
		t, _ := new(big.Int).SetString("0000FFFF00000000000000000000000000000000000000000000000000000000", 16)
		return t
	default:
		t, _ := new(big.Int).SetString("00000000FFFF0000000000000000000000000000000000000000000000000000", 16)
		return t
	}
}

// diffFromBits converts a compact nBits hex string to network difficulty as float64.
// Returns 0 on parse error.
func diffFromBits(nbitsHex string, algo string) float64 {
	nbitsBytes, err := hex.DecodeString(nbitsHex)
	if err != nil || len(nbitsBytes) != 4 {
		return 0
	}
	target := nbitsToTarget(nbitsBytes)
	if target.Sign() == 0 {
		return 0
	}
	diff1 := diff1Target(algo)
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(diff1), new(big.Float).SetInt(target)).Float64()
	return f
}

// ─── unused compat stub (kept to avoid removing reverseByteOrder callers) ────
var _ = binary.LittleEndian // ensure encoding/binary stays imported

// ─── Vardiff ──────────────────────────────────────────────────────────────────

func (s *Server) adjustVardiff(w *Worker) {
	// If the worker has a pinned fixed difficulty, re-apply it and skip auto-adjustment.
	// Re-read from config each time so live config changes take effect.
	// Coin-level WorkerFixedDiff takes priority over global pool config.
	if fd := s.coinFixedDiff(w.workerName); fd > 0 {
		if w.difficulty != fd {
			w.fixedDiff = fd
			w.difficulty = fd
			s.sendDifficulty(w, fd)
		}
		return
	}
	if w.fixedDiff > 0 {
		if w.difficulty != w.fixedDiff {
			w.difficulty = w.fixedDiff
			s.sendDifficulty(w, w.fixedDiff)
		}
		return
	}

	vd := s.coin.Stratum.Vardiff
	retarget := time.Duration(vd.RetargetS) * time.Second

	w.sharesSinceRetarget++

	// Emergency fast retarget: if we've already received 50+ shares in under
	// half the retarget window, the difficulty is clearly way too low (e.g.
	// after a restart that lost state). Retarget immediately instead of
	// waiting for the full retarget interval.
	sinceRetarget := time.Since(w.lastRetargetAt)
	emergency := w.sharesSinceRetarget >= 50 && sinceRetarget >= 5*time.Second && sinceRetarget < retarget
	if !emergency && sinceRetarget < retarget {
		return
	}

	// Calculate actual share rate over the retarget window
	elapsed := time.Since(w.lastRetargetAt).Milliseconds()
	if elapsed == 0 || w.sharesSinceRetarget == 0 {
		w.lastRetargetAt = time.Now()
		w.sharesSinceRetarget = 0
		return
	}
	actual := float64(elapsed) / float64(w.sharesSinceRetarget)
	target := float64(vd.TargetMs)

	ratio := target / actual
	newDiff := w.difficulty * ratio

	if newDiff < vd.MinDiff {
		newDiff = vd.MinDiff
	}
	// Respect per-worker suggested floor (from mining.suggest_difficulty)
	if w.suggestedMinDiff > 0 && newDiff < w.suggestedMinDiff {
		newDiff = w.suggestedMinDiff
	}
	if newDiff > vd.MaxDiff {
		newDiff = vd.MaxDiff
	}

	w.lastRetargetAt = time.Now()
	w.sharesSinceRetarget = 0

	// Grace period expired — clear prevDifficulty before possibly setting a new one
	w.prevDifficulty = 0

	if newDiff != w.difficulty {
		if newDiff > w.difficulty {
			// Raising difficulty: miner may still have in-flight shares at the old level
			w.prevDifficulty = w.difficulty
		}
		w.difficulty = newDiff
		ev := DiffEvent{Diff: newDiff, AtMS: time.Now().UnixMilli()}
		w.diffHistory = append(w.diffHistory, ev)
		if len(w.diffHistory) > 100 {
			w.diffHistory = w.diffHistory[len(w.diffHistory)-100:]
		}
		s.sendDifficulty(w, newDiff)
	}
}

// decayVardiff halves difficulty when all recent shares have been rejected.
// Called on each rejected share. Only acts if no accepted share has been seen
// for 2× the retarget window — prevents thrashing while allowing fast recovery.
func (s *Server) decayVardiff(w *Worker) {
	vd := s.coin.Stratum.Vardiff
	decayWindow := 2 * time.Duration(vd.RetargetS) * time.Second

	// Don't decay if we never had an accepted share yet (miner is still warming up)
	// or if the last accepted share was recent enough.
	if !w.lastAcceptedAt.IsZero() && time.Since(w.lastAcceptedAt) < decayWindow {
		return
	}
	// Also don't decay more often than once per retarget window.
	if time.Since(w.lastRetargetAt) < time.Duration(vd.RetargetS)*time.Second {
		return
	}

	newDiff := w.difficulty / 2
	if newDiff < vd.MinDiff {
		newDiff = vd.MinDiff
	}
	if newDiff == w.difficulty {
		return
	}

	w.prevDifficulty = 0
	w.difficulty = newDiff
	ev := DiffEvent{Diff: newDiff, AtMS: time.Now().UnixMilli()}
	w.diffHistory = append(w.diffHistory, ev)
	if len(w.diffHistory) > 100 {
		w.diffHistory = w.diffHistory[len(w.diffHistory)-100:]
	}
	w.lastRetargetAt = time.Now()
	w.sharesSinceRetarget = 0
	log.Printf("[%s] vardiff decay: %s difficulty %.4g → %.4g (no accepted share in %.0fs)",
		s.coin.Symbol, w.workerName, w.difficulty*2, newDiff, decayWindow.Seconds())
	s.sendDifficulty(w, newDiff)
}

// idleVardiffDecay runs as a background goroutine and halves the difficulty of
// workers that have been connected for a full retarget window without submitting
// any shares. This handles the case where a miner's StartDiff is too high for
// its actual hashrate — without this, vardiff never runs because it is only
// triggered by submitted shares.
func (s *Server) idleVardiffDecay() {
	vd := s.coin.Stratum.Vardiff
	retarget := time.Duration(vd.RetargetS) * time.Second
	ticker := time.NewTicker(retarget)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
		}

		type adj struct {
			w       *Worker
			newDiff float64
		}
		var adjustments []adj

		s.mu.Lock()
		for _, w := range s.workers {
			if !w.authorized || w.fixedDiff > 0 {
				continue
			}
			if w.sharesSinceRetarget > 0 {
				continue // active miner — adjustVardiff handles it
			}
			if time.Since(w.lastRetargetAt) < retarget {
				continue
			}
			newDiff := w.difficulty / 2
			if newDiff < vd.MinDiff {
				newDiff = vd.MinDiff
			}
			if newDiff == w.difficulty {
				w.lastRetargetAt = time.Now()
				continue
			}
			w.prevDifficulty = 0
			w.difficulty = newDiff
			w.lastRetargetAt = time.Now()
			ev := DiffEvent{Diff: newDiff, AtMS: time.Now().UnixMilli()}
			w.diffHistory = append(w.diffHistory, ev)
			if len(w.diffHistory) > 100 {
				w.diffHistory = w.diffHistory[len(w.diffHistory)-100:]
			}
			adjustments = append(adjustments, adj{w, newDiff})
		}
		s.mu.Unlock()

		for _, a := range adjustments {
			log.Printf("[%s] idle vardiff decay: %s → %.4g (no shares in %v)",
				s.coin.Symbol, a.w.workerName, a.newDiff, retarget)
			s.sendDifficulty(a.w, a.newDiff)
		}
	}
}

// ─── Send helpers ─────────────────────────────────────────────────────────────

func (s *Server) sendDifficulty(w *Worker, diff float64) error {
	msg := stratumNotify{
		Method: "mining.set_difficulty",
		Params: []float64{diff},
	}
	return s.writeJSON(w, msg)
}

// sendSetExtranonce pushes a new extranonce1 to a miner that has subscribed.
// This avoids a TCP reconnect when the extranonce needs to change (e.g. proxy switch).
func (s *Server) sendSetExtranonce(w *Worker, en1 string, en2Size int) {
	msg := stratumNotify{
		Method: "mining.set_extranonce",
		Params: []any{en1, en2Size},
	}
	if err := s.writeJSON(w, msg); err != nil {
		log.Printf("[%s] sendSetExtranonce to %s failed: %v", s.coin.Symbol, w.remoteAddr, err)
	}
}

func (s *Server) sendJob(w *Worker, job *Job) error {
	params := []any{
		job.ID,
		job.PrevHash,
		job.CoinbasePart1,
		job.CoinbasePart2,
		job.MerkleBranches,
		job.Version,
		job.NBits,
		job.NTime,
		job.CleanJobs,
	}
	msg := stratumNotify{
		Method: "mining.notify",
		Params: params,
	}
	return s.writeJSON(w, msg)
}

func (s *Server) reply(w *Worker, id json.RawMessage, result any, errVal any) error {
	return s.writeJSON(w, stratumReply{ID: id, Result: result, Error: errVal})
}

func (s *Server) writeJSON(w *Worker, v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
        if len(data) > 4096 {
		log.Printf("[%s] WARN large message to %s: %d bytes", s.coin.Symbol, w.workerName, len(data))
	}
	_, err = w.writer.Write(data)
	if err != nil {
		return err
	}
	return w.writer.Flush()
}

// broadcastJobs sends new jobs to all connected authorized workers
func (s *Server) broadcastJobs() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[%s] panic in job broadcaster (restarting in 2s): %v", s.coin.Symbol, r)
			time.Sleep(2 * time.Second)
			select {
			case <-s.stopCh:
				return
			default:
				go s.broadcastJobs() // restart
			}
		}
	}()
	for {
		select {
		case <-s.stopCh:
			return
		case job := <-s.jobBroadcast:
			s.mu.RLock()
			workers := make([]*Worker, 0, len(s.workers))
			for _, w := range s.workers {
				if w.authorized {
					workers = append(workers, w)
				}
			}
			s.mu.RUnlock()

			for _, w := range workers {
				if err := s.sendJob(w, job); err != nil {
					log.Printf("[%s] sendJob to %s: %v", s.coin.Symbol, w.workerName, err)
				}
			}
		}
	}
}

// ─── Worker introspection (for pipoolctl) ────────────────────────────────────

// BlockEntry records a found block for the dashboard log
type BlockEntry struct {
	Coin          string
	Height        int64
	Hash          string
	Reward        string
	Worker        string
	FoundAt       time.Time
	Confirmations int64   // -1 = orphaned, 0 = unconfirmed, N = confirmations
	IsOrphaned    bool
	IsAux         bool    // true for DOGE/BCH (merge-mined) blocks
	AuxParentCoin string  // e.g. "LTC" for a DOGE block
	BlockLuck     float64 // luck % (100=expected, 200=found at half expected work); -1=N/A (aux)
	MatureNotified bool   // true once the maturity Discord alert has been sent
}

// DiffEvent records a single vardiff change for a worker.
type DiffEvent struct {
	Diff float64 `json:"diff"`
	AtMS int64   `json:"at_ms"`
}

// WorkerInfo is a public snapshot of a worker (online or previously seen)
type WorkerInfo struct {
	Name             string
	DeviceName       string
	UserAgent        string
	Difficulty       float64
	HashrateKHs      float64   // 5-minute rolling hashrate estimate
	SharesAccepted   uint64
	SharesRejected   uint64
	SharesStale      uint64
	BestShare        float64
	RemoteAddr       string
	ReconnectCount   int       // 0 = first session; N = Nth reconnect
	SessionStartedAt time.Time // time of the current session start (zero if offline)
	ConnectedAt      time.Time
	LastSeenAt       time.Time
	LastShareAt      time.Time // time of the most recent accepted share (zero if none)
	Online           bool
	WattsEstimate    float64   // estimated power consumption in watts (from RouterTable or manual override)
	AsicTempC        float64   // live ASIC temperature from miner HTTP API (0 = not available)
	DiffHistory      []DiffEvent
}

// AllWorkers returns all workers seen this session (online + offline)
func (s *Server) AllWorkers() []WorkerInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Build a set of currently online worker names with live stats
	onlineByName := make(map[string]*Worker)
	for _, w := range s.workers {
		if w.authorized && w.workerName != "" {
			onlineByName[w.workerName] = w
		}
	}

	// Compute 5-minute rolling hashrate per worker from recent share samples.
	// diff1 constant: sha256d=2^32, scrypt/scryptn=2^16 (LTC diff-1 is 65536x easier)
	diff1 := 4294967296.0
	if s.coin.Algorithm == "scrypt" || s.coin.Algorithm == "scryptn" {
		diff1 = 65536.0
	}
	const windowMs = 5 * 60 * 1000
	nowMs := time.Now().UnixMilli()
	workerDiffSum := make(map[string]float64)
	for _, ss := range s.shareSamples {
		if ss.Accepted && ss.Worker != "" && nowMs-ss.TimeMS <= windowMs {
			workerDiffSum[ss.Worker] += ss.Difficulty
		}
	}

	out := make([]WorkerInfo, 0, len(s.seenWorkers))
	for name, seen := range s.seenWorkers {
		khs := workerDiffSum[name] * diff1 / 300.0 / 1000.0
		wi := WorkerInfo{
			Name:           name,
			DeviceName:     seen.DeviceName,
			UserAgent:      seen.UserAgent,
			HashrateKHs:    khs,
			SharesAccepted: seen.SharesAccepted,
			SharesRejected: seen.SharesRejected,
			SharesStale:    seen.SharesStale,
			BestShare:      seen.BestShare,
			RemoteAddr:     seen.LastAddr,
			ReconnectCount: seen.ReconnectCount,
			ConnectedAt:    seen.ConnectedAt,
			LastSeenAt:     seen.LastSeenAt,
			Online:         seen.Online,
		}
		// Use live difficulty and session start from the connected Worker;
		// share counts come from seen.Shares* which updateSeenWorkerShares keeps current.
		if w, ok := onlineByName[name]; ok {
			wi.Difficulty       = w.difficulty
			wi.Online           = true
			wi.SessionStartedAt = w.connectedAt
			wi.LastShareAt      = w.lastShareAt
			dh := make([]DiffEvent, len(w.diffHistory))
			copy(dh, w.diffHistory)
			wi.DiffHistory = dh
		} else {
			// Offline — use last seen time as fallback
			wi.LastShareAt   = seen.LastSeenAt
			dh := make([]DiffEvent, len(seen.DiffHistory))
			copy(dh, seen.DiffHistory)
			wi.DiffHistory = dh
		}
		// Get wattage: live API data first, then manual override, then device class lookup
		watts := seen.LiveWatts  // set by background poller if miner has HTTP API
		if watts == 0 {
			if s.poolCfg.Pool.WorkerFixedWatts != nil {
				if w, ok := s.poolCfg.Pool.WorkerFixedWatts[name]; ok {
					watts = w
				}
			}
		}
		if watts == 0 {
			// Look up by device name in RouterTable
			for _, sig := range RouterTable {
				if sig.class.Name == seen.DeviceName {
					watts = sig.class.WattsEstimate
					break
				}
			}
		}
		if watts == 0 && seen.DeviceName != "" {
			// Device not in RouterTable (e.g. "Unknown (Scrypt)") — use the algorithm default
			watts = defaultDevice(s.coin.Algorithm).WattsEstimate
		}
		wi.WattsEstimate = watts
		if seen.LiveTemp > 0 {
			wi.AsicTempC = seen.LiveTemp
		}
		out = append(out, wi)
	}
	return out
}

// SetLiveWatts updates the live power and temperature for a worker from the miner's HTTP API.
func (s *Server) SetLiveWatts(workerName string, watts, temp float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sw, ok := s.seenWorkers[workerName]; ok {
		sw.LiveWatts = watts
		sw.LiveTemp = temp
	}
}

// Coin returns the coin symbol this server is mining.
func (s *Server) Coin() string { return s.coin.Symbol }

// WorkerShareSparkline returns per-minute share counts for a worker over the last 30 minutes.
// Returns 30 integers (oldest first, newest last).
func (s *Server) WorkerShareSparkline(name string) []int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	const buckets = 30
	counts := make([]int, buckets)
	now := time.Now().UnixMilli()
	for _, ss := range s.shareSamples {
		if ss.Worker != name || !ss.Accepted {
			continue
		}
		ageMin := int((now - ss.TimeMS) / 60000)
		if ageMin >= 0 && ageMin < buckets {
			counts[buckets-1-ageMin]++
		}
	}
	return counts
}

// ConnectedWorkers returns only currently connected workers (kept for ctl compat)
func (s *Server) ConnectedWorkers() []WorkerInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]WorkerInfo, 0, len(s.workers))
	for _, w := range s.workers {
		if w.authorized {
			out = append(out, WorkerInfo{
				Name:           w.workerName,
				DeviceName:     w.deviceName,
				Difficulty:     w.difficulty,
				SharesAccepted: w.sharesAccepted,
				SharesRejected: w.sharesRejected,
				SharesStale:    w.sharesStale,
				BestShare:      w.bestShare,
				RemoteAddr:     w.remoteAddr,
				ConnectedAt:    w.connectedAt,
				LastSeenAt:     time.Now(),
				Online:         true,
			})
		}
	}
	return out
}

// KickWorker disconnects a worker by name. Returns true if found and disconnected.
func (s *Server) KickWorker(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.workers {
		if w.workerName == name {
			w.conn.Close()
			return true
		}
	}
	return false
}

// SetCoinbaseTag updates the coinbase tag embedded in newly built jobs.
// Safe to call at any time — takes effect on the next block template poll.
func (s *Server) SetCoinbaseTag(tag string) {
	s.mu.Lock()
	s.coinbaseTag = tag
	s.mu.Unlock()
}

// RecordHashrateSample appends a hashrate snapshot (called from main.go on each push tick)
func (s *Server) RecordHashrateSample(khs float64) {
	s.mu.Lock()
	s.hashrateSamples = append(s.hashrateSamples, HashrateSample{
		KHs:    khs,
		TimeMS: time.Now().UnixMilli(),
	})
	if len(s.hashrateSamples) > 288 {
		s.hashrateSamples = s.hashrateSamples[len(s.hashrateSamples)-288:]
	}
	s.mu.Unlock()
}

// HashrateSamples returns a copy of the rolling hashrate history
func (s *Server) HashrateSamples() []HashrateSample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]HashrateSample, len(s.hashrateSamples))
	copy(out, s.hashrateSamples)
	return out
}

// BestShareEver returns the all-time highest share difficulty this session
func (s *Server) BestShareEver() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bestShareEver
}

// recordShareSample appends a share difficulty sample (ring buffer, max 200)
func (s *Server) recordShareSample(diff float64, accepted bool, worker string) {
	s.mu.Lock()
	s.shareSamples = append(s.shareSamples, ShareSample{
		Difficulty: diff,
		TimeMS:     time.Now().UnixMilli(),
		Accepted:   accepted,
		Worker:     worker,
	})
	if len(s.shareSamples) > 200 {
		s.shareSamples = s.shareSamples[len(s.shareSamples)-200:]
	}
	s.mu.Unlock()
}

// ShareSamples returns a copy of the recent share difficulty history
func (s *Server) ShareSamples() []ShareSample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ShareSample, len(s.shareSamples))
	copy(out, s.shareSamples)
	return out
}

// HistoBucket is one bin of the share difficulty distribution
type HistoBucket struct {
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Count int     `json:"count"`
}

// ShareHistogram returns a log10-spaced distribution of accepted share difficulties
// from the current rolling sample window (last 200 shares).
func (s *Server) ShareHistogram() []HistoBucket {
	bounds := []float64{0.001, 0.01, 0.1, 1, 10, 100, 1000, 10000, 100000, 1000000}
	buckets := make([]HistoBucket, len(bounds)-1)
	for i := 0; i < len(bounds)-1; i++ {
		buckets[i] = HistoBucket{Min: bounds[i], Max: bounds[i+1]}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ss := range s.shareSamples {
		if !ss.Accepted {
			continue
		}
		for i := range buckets {
			if ss.Difficulty >= buckets[i].Min && ss.Difficulty < buckets[i].Max {
				buckets[i].Count++
				break
			}
		}
	}
	return buckets
}

// BlockLog returns a copy of the found-block log for the dashboard
func (s *Server) BlockLog() []BlockEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]BlockEntry, len(s.blockLog))
	copy(out, s.blockLog)
	return out
}

// currentJobHeight returns the block height of the current job, or 0 if no job yet
func (s *Server) currentJobHeight() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.currentJob != nil {
		return s.currentJob.Height
	}
	return 0
}

// recordBlock appends a found block to the ring buffer (max 50 entries)
func (s *Server) recordBlock(hash string, height int64, reward float64, worker string, luck float64) {
	entry := BlockEntry{
		Coin:      s.coin.Symbol,
		Height:    height,
		Hash:      hash,
		Reward:    fmt.Sprintf("%.4f %s", reward, s.coin.Symbol),
		Worker:    worker,
		FoundAt:   time.Now(),
		BlockLuck: luck,
	}
	s.mu.Lock()
	s.blockLog = append(s.blockLog, entry)
	if len(s.blockLog) > 50 {
		s.blockLog = s.blockLog[len(s.blockLog)-50:]
	}
	s.mu.Unlock()
	go s.saveBlockLog()
}

// RecordBlock publicly records a found block (used by main.go for aux chain blocks).
func (s *Server) RecordBlock(symbol, hash string, height int64, reward float64, worker, auxParent string) {
	entry := BlockEntry{
		Coin:          symbol,
		Height:        height,
		Hash:          hash,
		Reward:        fmt.Sprintf("%.4f %s", reward, symbol),
		Worker:        worker,
		FoundAt:       time.Now(),
		IsAux:         auxParent != "",
		AuxParentCoin: auxParent,
		BlockLuck:     -1, // luck N/A for aux chain blocks
	}
	s.mu.Lock()
	s.blockLog = append(s.blockLog, entry)
	if len(s.blockLog) > 50 {
		s.blockLog = s.blockLog[len(s.blockLog)-50:]
	}
	s.mu.Unlock()
	go s.saveBlockLog()
}

// ─── Stats ────────────────────────────────────────────────────────────────────

type Stats struct {
	Symbol              string
	Algorithm           string
	ConnectedMiners     int32
	TotalShares         uint64
	ValidShares         uint64
	BlocksFound         uint64
	HashrateKHs         float64 // estimated from valid shares
	ValidWorkSinceBlock float64 // sum of accepted share diffs since last block
	LastNetworkDiff     float64 // network difficulty of most recent job
}

func (s *Server) Stats() Stats {
	s.mu.RLock()
	vwsb := s.validWorkSinceBlock
	lnd := s.lastNetworkDiff
	s.mu.RUnlock()
	return Stats{
		Symbol:              s.coin.Symbol,
		Algorithm:           s.coin.Algorithm,
		ConnectedMiners:     s.connectedMiners.Load(),
		TotalShares:         s.totalShares.Load(),
		ValidShares:         s.validShares.Load(),
		BlocksFound:         s.blocksFound.Load(),
		ValidWorkSinceBlock: vwsb,
		LastNetworkDiff:     lnd,
	}
}

// DiagStats holds per-chain diagnostics for the dashboard debug panel
type DiagStats struct {
	Symbol          string
	TotalShares     uint64
	ValidShares     uint64
	StaleShares     uint64
	RejectedShares  uint64
	CurrentJobID    string
	CurrentJobAge   int64  // seconds since job was issued
	WorkerCount     int
	HasJob          bool
}

// Diag returns a snapshot of chain-level diagnostic data
func (s *Server) Diag() DiagStats {
	// Hold a single read lock for the entire snapshot to keep values consistent.
	s.mu.RLock()
	jobID := ""
	jobAge := int64(0)
	hasJob := false
	if s.currentJob != nil {
		jobID = s.currentJob.ID
		jobAge = int64(time.Since(s.currentJob.CreatedAt).Seconds())
		hasJob = true
	}
	var workerCount int
	for _, w := range s.workers {
		if w.authorized {
			workerCount++
		}
	}
	// Tally stale/rejected from seenWorkers (includes both online and offline workers,
	// and is kept in sync with active workers via updateSeenWorkerShares)
	var stale, rejected uint64
	for _, sw := range s.seenWorkers {
		stale += sw.SharesStale
		rejected += sw.SharesRejected
	}
	s.mu.RUnlock()

	return DiagStats{
		Symbol:         s.coin.Symbol,
		TotalShares:    s.totalShares.Load(),
		ValidShares:    s.validShares.Load(),
		StaleShares:    stale,
		RejectedShares: rejected,
		CurrentJobID:   jobID,
		CurrentJobAge:  jobAge,
		HasJob:         hasJob,
		WorkerCount:    workerCount,
	}
}

// CoinMaturityThreshold returns the number of confirmations required before a block reward is spendable.
func CoinMaturityThreshold(symbol string) int64 {
	switch strings.ToUpper(symbol) {
	case "DOGE":
		return 60
	default:
		return 100
	}
}

// trackConfirmations polls the node every 30 seconds and updates confirmation counts
// for all blocks in the log that haven't matured (120+ confirmations) or been orphaned.
func (s *Server) trackConfirmations() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			// Snapshot entries that need checking (avoid holding lock during RPC)
			s.mu.RLock()
			var toCheck []BlockEntry
			for _, e := range s.blockLog {
				if !e.IsOrphaned && e.Confirmations < 120 && e.Hash != "" && e.Hash != "(pending)" {
					toCheck = append(toCheck, e)
				}
			}
			s.mu.RUnlock()

			if len(toCheck) == 0 {
				continue
			}

			// RPC calls outside the lock
			type update struct {
				hash          string
				confirmations int64
				orphaned      bool
			}
			var updates []update
			for _, e := range toCheck {
				info, err := s.rpcClient.GetBlock(e.Hash)
				if err != nil {
					continue // daemon unreachable — skip this round
				}
				orphaned := info.Confirmations < 0
				updates = append(updates, update{e.Hash, info.Confirmations, orphaned})
			}

			// Apply updates under write lock
			s.mu.Lock()
			for i := range s.blockLog {
				for _, u := range updates {
					if s.blockLog[i].Hash != u.hash {
						continue
					}
					if u.orphaned {
						if !s.blockLog[i].IsOrphaned {
							s.blockLog[i].IsOrphaned = true
							if s.OnBlockOrphaned != nil {
								go s.OnBlockOrphaned(s.blockLog[i].Coin, u.hash, s.blockLog[i].Height)
							}
						}
					} else {
						s.blockLog[i].Confirmations = u.confirmations
						threshold := CoinMaturityThreshold(s.blockLog[i].Coin)
						if !s.blockLog[i].MatureNotified && s.blockLog[i].Confirmations >= threshold {
							s.blockLog[i].MatureNotified = true
							if s.OnBlockMature != nil {
								entry := s.blockLog[i]
								go s.OnBlockMature(entry.Coin, entry.Hash, entry.Height, entry.Reward)
							}
						}
					}
				}
			}
			s.mu.Unlock()
		}
	}
}

// ─── Merkle helpers ───────────────────────────────────────────────────────────

// buildMerkleBranch returns the merkle branch needed to combine coinbase with tx list.
// txHashes must NOT include the coinbase — the miner supplies the coinbase hash.
// At each level, hashes[0] is the right sibling of the coinbase path; the remaining
// hashes are collapsed one level up before the next iteration.
func buildMerkleBranch(txHashes []string) []string {
	if len(txHashes) == 0 {
		return nil
	}
	branch := []string{}
	hashes := make([]string, len(txHashes))
	copy(hashes, txHashes)

	for len(hashes) > 0 {
		branch = append(branch, hashes[0])
		if len(hashes) == 1 {
			break
		}
		// Remaining hashes (after the branch element) are collapsed one level.
		remaining := make([]string, len(hashes)-1)
		copy(remaining, hashes[1:])
		if len(remaining)%2 == 1 {
			remaining = append(remaining, remaining[len(remaining)-1])
		}
		next := make([]string, len(remaining)/2)
		for i := 0; i < len(remaining); i += 2 {
			next[i/2] = merkleHash(remaining[i], remaining[i+1])
		}
		hashes = next
	}
	return branch
}

// merkleHash returns double-SHA256(a || b) as a hex string.
func merkleHash(a, b string) string {
	aBytes, _ := hex.DecodeString(a)
	bBytes, _ := hex.DecodeString(b)
	data := append(aBytes, bBytes...)
	h1 := sha256.Sum256(data)
	h2 := sha256.Sum256(h1[:])
	return hex.EncodeToString(h2[:])
}

// hexReverseBytes reverses all bytes of a hex string (for converting display-order
// txids to the internal little-endian byte order expected by Stratum).
func hexReverseBytes(s string) string {
	b, _ := hex.DecodeString(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return hex.EncodeToString(b)
}

// reverseByteOrder reverses a hex string's byte order (for prevhash in Stratum)
func reverseByteOrder(hexStr string) string {
	if len(hexStr)%2 != 0 {
		return hexStr
	}
	result := make([]byte, len(hexStr))
	for i := 0; i < len(hexStr); i += 8 {
		end := i + 8
		if end > len(hexStr) {
			end = len(hexStr)
		}
		chunk := []byte(hexStr[i:end])
		for j, k := 0, len(chunk)-2; j < k; j, k = j+2, k-2 {
			chunk[j], chunk[j+1], chunk[k], chunk[k+1] = chunk[k], chunk[k+1], chunk[j], chunk[j+1]
		}
		copy(result[i:], chunk)
	}
	return string(result)
}

// ─── IP Allowlist ─────────────────────────────────────────────────────────────

// isAllowedIP checks whether remoteAddr (host:port) matches any entry in the allowlist.
// Each entry may be a CIDR range ("192.168.1.0/24") or a plain IP ("192.168.1.28").
func isAllowedIP(remoteAddr string, allowlist []string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr // no port component
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, entry := range allowlist {
		if strings.Contains(entry, "/") {
			_, cidr, err := net.ParseCIDR(entry)
			if err == nil && cidr.Contains(ip) {
				return true
			}
		} else {
			if net.ParseIP(entry).Equal(ip) {
				return true
			}
		}
	}
	return false
}

// ─── Worker state persistence ─────────────────────────────────────────────────

type workerStateEntry struct {
	Name           string  `json:"name"`
	Coin           string  `json:"coin"`
	LastDifficulty float64 `json:"last_difficulty"`
	BestShare      float64 `json:"best_share"`
}

type workerStateFile struct {
	Version int                         `json:"version"`
	Saved   time.Time                   `json:"saved"`
	Workers map[string]workerStateEntry `json:"workers"`
}

func (s *Server) stateFilePath() string {
	if s.poolCfg.Pool.StateFile == "" {
		return ""
	}
	return fmt.Sprintf("%s_%s.json", s.poolCfg.Pool.StateFile, s.coin.Symbol)
}

func (s *Server) blockLogFilePath() string {
	if s.poolCfg.Pool.StateFile == "" {
		return ""
	}
	return fmt.Sprintf("%s_blocklog_%s.json", s.poolCfg.Pool.StateFile, s.coin.Symbol)
}

// saveWorkerState writes LastDifficulty and BestShare for all known workers to disk.
func (s *Server) saveWorkerState() {
	path := s.stateFilePath()
	if path == "" {
		return
	}
	s.mu.RLock()
	state := workerStateFile{
		Version: 1,
		Saved:   time.Now(),
		Workers: make(map[string]workerStateEntry, len(s.seenWorkers)),
	}
	for name, sw := range s.seenWorkers {
		if sw.LastDifficulty <= 0 && sw.BestShare <= 0 {
			continue
		}
		state.Workers[name] = workerStateEntry{
			Name:           sw.Name,
			Coin:           sw.Coin,
			LastDifficulty: sw.LastDifficulty,
			BestShare:      sw.BestShare,
		}
	}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		log.Printf("[%s] state save marshal: %v", s.coin.Symbol, err)
		return
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		log.Printf("[%s] state save write %s: %v", s.coin.Symbol, path, err)
	}
}

// loadWorkerState reads persisted worker state and pre-populates seenWorkers
// with LastDifficulty and BestShare so they survive pool restarts.
func (s *Server) loadWorkerState() {
	path := s.stateFilePath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[%s] state load %s: %v", s.coin.Symbol, path, err)
		}
		return
	}
	var state workerStateFile
	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("[%s] state load parse: %v", s.coin.Symbol, err)
		return
	}
	s.mu.Lock()
	loaded := 0
	for name, entry := range state.Workers {
		if entry.Coin != s.coin.Symbol {
			continue
		}
		if entry.LastDifficulty <= 0 && entry.BestShare <= 0 {
			continue
		}
		s.seenWorkers[name] = &SeenWorker{
			Name:           entry.Name,
			Coin:           entry.Coin,
			LastDifficulty: entry.LastDifficulty,
			BestShare:      entry.BestShare,
		}
		loaded++
	}
	s.mu.Unlock()
	if loaded > 0 {
		log.Printf("[%s] restored state for %d worker(s) from %s", s.coin.Symbol, loaded, path)
	}
}

// blockLogFileEntry is the on-disk representation of a found block.
type blockLogFileEntry struct {
	Coin          string    `json:"coin"`
	Height        int64     `json:"height"`
	Hash          string    `json:"hash"`
	Reward        string    `json:"reward"`
	Worker        string    `json:"worker"`
	FoundAt       time.Time `json:"found_at"`
	Confirmations int64     `json:"confirmations"`
	IsOrphaned    bool      `json:"is_orphaned"`
	IsAux         bool      `json:"is_aux"`
	AuxParentCoin  string    `json:"aux_parent_coin,omitempty"`
	BlockLuck      float64   `json:"luck"`
	MatureNotified bool      `json:"mature_notified"`
}

type blockLogFile struct {
	Version int                  `json:"version"`
	Saved   time.Time            `json:"saved"`
	Entries []blockLogFileEntry  `json:"entries"`
}

// saveBlockLog persists the in-memory block log to disk.
func (s *Server) saveBlockLog() {
	path := s.blockLogFilePath()
	if path == "" {
		return
	}
	s.mu.RLock()
	entries := make([]blockLogFileEntry, len(s.blockLog))
	for i, b := range s.blockLog {
		entries[i] = blockLogFileEntry{
			Coin: b.Coin, Height: b.Height, Hash: b.Hash,
			Reward: b.Reward, Worker: b.Worker, FoundAt: b.FoundAt,
			Confirmations: b.Confirmations, IsOrphaned: b.IsOrphaned,
			IsAux: b.IsAux, AuxParentCoin: b.AuxParentCoin, BlockLuck: b.BlockLuck,
			MatureNotified: b.MatureNotified,
		}
	}
	s.mu.RUnlock()

	f := blockLogFile{Version: 1, Saved: time.Now(), Entries: entries}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		log.Printf("[%s] block log save marshal: %v", s.coin.Symbol, err)
		return
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		log.Printf("[%s] block log save write %s: %v", s.coin.Symbol, path, err)
	}
}

// loadBlockLog reads persisted block log entries and prepends them to the in-memory log.
// Existing in-memory entries (from this session) take precedence; historical entries are
// appended only when not already present (dedup by hash).
func (s *Server) loadBlockLog() {
	path := s.blockLogFilePath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[%s] block log load %s: %v", s.coin.Symbol, path, err)
		}
		return
	}
	var f blockLogFile
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("[%s] block log parse: %v", s.coin.Symbol, err)
		return
	}
	s.mu.Lock()
	// Build set of hashes already in memory
	known := make(map[string]bool, len(s.blockLog))
	for _, b := range s.blockLog {
		if b.Hash != "" {
			known[b.Hash] = true
		}
	}
	// Prepend historical entries not already present
	var historical []BlockEntry
	for _, e := range f.Entries {
		if known[e.Hash] {
			continue
		}
		historical = append(historical, BlockEntry{
			Coin: e.Coin, Height: e.Height, Hash: e.Hash,
			Reward: e.Reward, Worker: e.Worker, FoundAt: e.FoundAt,
			Confirmations: e.Confirmations, IsOrphaned: e.IsOrphaned,
			IsAux: e.IsAux, AuxParentCoin: e.AuxParentCoin, BlockLuck: e.BlockLuck,
			MatureNotified: e.MatureNotified,
		})
	}
	if len(historical) > 0 {
		combined := append(historical, s.blockLog...)
		if len(combined) > 50 {
			combined = combined[len(combined)-50:]
		}
		s.blockLog = combined
	}
	s.mu.Unlock()
	if len(historical) > 0 {
		log.Printf("[%s] restored %d block log entries from %s", s.coin.Symbol, len(historical), path)
	}
}
