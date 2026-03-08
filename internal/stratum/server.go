package stratum

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dakota/pipool/internal/config"
	"github.com/dakota/pipool/internal/merge"
	"github.com/dakota/pipool/internal/rpc"
)

// Server is a Stratum V1 server for a single primary coin (with optional merge mining)
type Server struct {
	coin        config.CoinConfig
	poolCfg     *config.PoolConfig
	coinbaseTag string // e.g. "/PiPool/" — embedded in every block found
	rpcClient   *rpc.Client
	coordinator *merge.Coordinator

	mu         sync.RWMutex
	workers    map[string]*Worker
	seenWorkers map[string]*SeenWorker // all workers ever connected this session
	blockLog   []BlockEntry // last 50 blocks found, shown on dashboard
	currentJob *Job

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

	// Callbacks for Discord notifications
	OnBlockFound      func(coin, hash string, reward float64)
	OnMinerConnect    func(worker string, addr string)
	OnMinerDisconnect func(worker string)
	OnNodeUnreachable func(err error)
	OnNodeOnline      func()
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
	difficulty   float64
	lastShareAt  time.Time

	// Share counters
	sharesAccepted uint64
	sharesRejected uint64
	sharesStale    uint64
	connectedAt    time.Time

	// Extranonce assigned to this worker
	extranonce1  string
}

// SeenWorker tracks all workers ever connected this session (for dashboard history)
type SeenWorker struct {
	Name           string
	DeviceName     string
	LastAddr       string
	Coin           string
	SharesAccepted uint64
	SharesRejected uint64
	SharesStale    uint64
	ConnectedAt    time.Time
	LastSeenAt     time.Time
	Online         bool
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
	// Merge mining commitment included in coinbase
	AuxCommitment  []byte
}

// NewServer creates a Stratum server for the given coin
func NewServer(coin config.CoinConfig, poolCfg *config.PoolConfig, coord *merge.Coordinator) *Server {
	cli := rpc.NewClient(coin.Node.Host, coin.Node.Port, coin.Node.User, coin.Node.Password, coin.Symbol)

	tag := poolCfg.Pool.CoinbaseTag
	if tag == "" {
		tag = "/PiPool/"
	}

	return &Server{
		coin:         coin,
		seenWorkers:  make(map[string]*SeenWorker),
		poolCfg:      poolCfg,
		coinbaseTag:  tag,
		rpcClient:    cli,
		coordinator:  coord,
		workers:      make(map[string]*Worker),
		jobBroadcast: make(chan *Job, 16),
		stopCh:       make(chan struct{}),
	}
}

// Start begins listening for miner connections and polling for new work
func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.poolCfg.Pool.Host, s.coin.Stratum.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("[%s] stratum listen on %s: %w", s.coin.Symbol, addr, err)
	}
	s.ln = ln

	log.Printf("[%s] Stratum server listening on %s", s.coin.Symbol, addr)

	// Start block template poller — recovers from panics internally
	go s.pollBlockTemplate()

	// Accept miner connections
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-s.stopCh:
					return // clean shutdown
				default:
					log.Printf("[%s] accept error: %v", s.coin.Symbol, err)
					continue
				}
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
	}()

	// Broadcast new jobs as they arrive — recovers from panics internally
	go s.broadcastJobs()

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
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	daemonWasDown := false
	retryDelay := 5 * time.Second
	const maxRetryDelay = 60 * time.Second

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			hash, err := s.rpcClient.GetBestBlockHash()
			if err != nil {
				if !daemonWasDown {
					// First failure — log and fire Discord alert
					log.Printf("[%s] daemon unreachable: %v — will retry every %v", s.coin.Symbol, err, retryDelay)
					if s.OnNodeUnreachable != nil {
						s.OnNodeUnreachable(err)
					}
					daemonWasDown = true
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
			}

			if hash == lastHash {
				continue
			}
			lastHash = hash

			job, err := s.buildJob(true)
			if err != nil {
				log.Printf("[%s] buildJob: %v", s.coin.Symbol, err)
				continue
			}

			s.mu.Lock()
			s.currentJob = job
			s.mu.Unlock()

			select {
			case s.jobBroadcast <- job:
			default:
				// channel full — miners will get next job
			}
		}
	}
}

func (s *Server) buildJob(cleanJobs bool) (*Job, error) {
	bt, err := s.rpcClient.GetBlockTemplate([]string{"coinbasetxn", "workid"})
	if err != nil {
		return nil, err
	}

	// Gather AuxPoW commitment if we have merge mining children
	var auxCommitment []byte
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
				auxCommitment = merge.BuildCoinbaseCommitment(auxWorks)
			}
		}
	}

	// Build merkle branch from transactions
	txHashes := make([]string, len(bt.Transactions))
	for i, tx := range bt.Transactions {
		txHashes[i] = tx.Hash
	}
	merkleBranches := buildMerkleBranch(txHashes)

	// Build coinbase parts with tag embedded
	// part1 ends just before extranonce, part2 starts after
	// The tag (e.g. "/PiPool/") sits between the BIP34 height and the extranonce
	cbPart1, cbPart2 := rpc.CreateCoinbaseTx(
		s.coin.Wallet,
		bt.CoinbaseValue,
		bt.Height,
		4, // extranonce2 size in bytes
		s.coinbaseTag,
	)

	job := &Job{
		ID:             fmt.Sprintf("%x", rand.Uint32()),
		PrevHash:       reverseByteOrder(bt.PreviousBlockHash),
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
		id:          workerID,
		conn:        conn,
		writer:      bufio.NewWriter(conn),
		remoteAddr:  conn.RemoteAddr().String(),
		difficulty:  s.coin.Stratum.Vardiff.MinDiff,
		lastShareAt: time.Now(),
		connectedAt: time.Now(),
		extranonce1: fmt.Sprintf("%08x", rand.Uint32()),
	}

	conn.SetDeadline(time.Now().Add(s.poolCfg.Pool.WorkerTimeoutDuration()))

	s.mu.Lock()
	s.workers[workerID] = w
	s.mu.Unlock()
	s.connectedMiners.Add(1)

	log.Printf("[%s] miner connected: %s", s.coin.Symbol, conn.RemoteAddr())

	defer func() {
		conn.Close()
		s.mu.Lock()
		delete(s.workers, workerID)
		// Update seenWorkers on disconnect
		if w.authorized && w.workerName != "" {
			if seen, ok := s.seenWorkers[w.workerName]; ok {
				seen.Online = false
				seen.LastSeenAt = time.Now()
				seen.SharesAccepted = w.sharesAccepted
				seen.SharesRejected = w.sharesRejected
				seen.SharesStale = w.sharesStale
			}
		}
		s.mu.Unlock()
		s.connectedMiners.Add(-1)
		log.Printf("[%s] miner disconnected: %s (%s)", s.coin.Symbol, w.workerName, conn.RemoteAddr())
		if s.OnMinerDisconnect != nil && w.authorized {
			s.OnMinerDisconnect(w.workerName)
		}
	}()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), 4096) // cap per-worker buffer for RAM

	for scanner.Scan() {
		conn.SetDeadline(time.Now().Add(s.poolCfg.Pool.WorkerTimeoutDuration()))

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
	case "mining.extranonce.subscribe":
		// Acknowledge but don't change extranonce dynamically for now
		return s.reply(w, msg.ID, true, nil)
	default:
		log.Printf("[%s] unknown method: %s", s.coin.Symbol, msg.Method)
		return s.reply(w, msg.ID, nil, []any{20, "Unknown method", nil})
	}
}

func (s *Server) handleSubscribe(w *Worker, msg *stratumMsg) error {
	// Extract user-agent from subscribe params if present
	// mining.subscribe params: ["user-agent/version", "session-id"]
	var params []string
	json.Unmarshal(msg.Params, &params)
	if len(params) > 0 {
		w.userAgent = params[0]
	}

	// Reply: [session_id, extranonce1, extranonce2_size]
	result := []any{
		[]any{
			[]string{"mining.set_difficulty", w.id},
			[]string{"mining.notify", w.id},
		},
		w.extranonce1,
		4, // extranonce2 size in bytes
	}
	if err := s.reply(w, msg.ID, result, nil); err != nil {
		return err
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

	w.mu.Lock()
	w.authorized = true
	w.workerName = workerName

	// Track in seenWorkers map
	s.mu.Lock()
	if existing, ok := s.seenWorkers[workerName]; ok {
		existing.Online = true
		existing.LastSeenAt = time.Now()
		existing.LastAddr = w.remoteAddr
	} else {
		s.seenWorkers[workerName] = &SeenWorker{
			Name:        workerName,
			LastAddr:    w.remoteAddr,
			Coin:        s.coin.Symbol,
			ConnectedAt: time.Now(),
			LastSeenAt:  time.Now(),
			Online:      true,
		}
	}
	s.mu.Unlock()

	// ── Spiral Router: classify device and set starting difficulty ────────────
	device := RouteWorker(w.userAgent, workerName, s.coin.Algorithm)
	w.deviceName = device.Name
	// Use device's recommended starting diff, clamped to our vardiff bounds
	vd := s.coin.Stratum.Vardiff
	startDiff := device.StartDiff
	if startDiff < vd.MinDiff {
		startDiff = vd.MinDiff
	}
	if startDiff > vd.MaxDiff {
		startDiff = vd.MaxDiff
	}
	w.difficulty = startDiff
	w.mu.Unlock()

	log.Printf("[%s] worker authorized: %s | device: %s | start-diff: %.4f | from: %s",
		s.coin.Symbol, workerName, device.Name, startDiff, w.remoteAddr)

	if err := s.reply(w, msg.ID, true, nil); err != nil {
		return err
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
	// params: [worker_name, job_id, extranonce2, ntime, nonce]

	if len(params) < 5 {
		w.sharesRejected++
		return s.reply(w, msg.ID, false, []any{20, "Malformed params", nil})
	}

	// Get current job snapshot
	s.mu.RLock()
	job := s.currentJob
	s.mu.RUnlock()

	if job == nil {
		w.sharesRejected++
		return s.reply(w, msg.ID, false, []any{21, "No job", nil})
	}

	// Validate the share: assemble header and check hash meets worker difficulty
	extranonce2 := params[2]
	ntime := params[3]
	nonce := params[4]

	valid, isBlock := s.validateShare(job, w.extranonce1, extranonce2, ntime, nonce, w.difficulty)

	if !valid {
		w.sharesRejected++
		s.updateSeenWorkerShares(w)
		log.Printf("[%s] rejected share from %s (hash above target)", s.coin.Symbol, w.workerName)
		return s.reply(w, msg.ID, false, []any{23, "Low difficulty share", nil})
	}

	w.sharesAccepted++
	w.lastShareAt = time.Now()
	s.validShares.Add(1)
	s.updateSeenWorkerShares(w)

	// Adjust vardiff
	s.adjustVardiff(w)

	if isBlock {
		s.blocksFound.Add(1)
		log.Printf("[%s] BLOCK FOUND by %s!", s.coin.Symbol, w.workerName)

		reward := s.coin.BlockReward
		blockHex := s.assembleBlockHex(job, w.extranonce1, extranonce2, ntime, nonce)
		go s.submitBlock(blockHex, w.workerName, job.Height)

		s.recordBlock("(pending)", job.Height, reward, w.workerName)

		if s.OnBlockFound != nil {
			s.OnBlockFound(s.coin.Symbol, "(pending)", reward)
		}
	}

	return s.reply(w, msg.ID, true, nil)
}

// updateSeenWorkerShares syncs current share counts to seenWorkers map
func (s *Server) updateSeenWorkerShares(w *Worker) {
	if w.workerName == "" {
		return
	}
	s.mu.Lock()
	if seen, ok := s.seenWorkers[w.workerName]; ok {
		seen.SharesAccepted = w.sharesAccepted
		seen.SharesRejected = w.sharesRejected
		seen.SharesStale    = w.sharesStale
		seen.LastSeenAt     = time.Now()
	}
	s.mu.Unlock()
}

// validateShare assembles the block header and checks whether the hash meets
// the worker's current difficulty target AND whether it meets the block target.
// Returns (shareValid, isBlock).
func (s *Server) validateShare(job *Job, extranonce1, extranonce2, ntime, nonce string, workerDiff float64) (bool, bool) {
	// Build coinbase transaction hash
	coinbaseHex := job.CoinbasePart1 + extranonce1 + extranonce2 + job.CoinbasePart2
	coinbaseBytes, err := hex.DecodeString(coinbaseHex)
	if err != nil {
		return false, false
	}
	coinbaseHash := dsha256(coinbaseBytes)

	// Compute merkle root from coinbase hash + merkle branches
	merkleRoot := coinbaseHash[:]
	for _, branch := range job.MerkleBranches {
		branchBytes, err := hex.DecodeString(branch)
		if err != nil {
			return false, false
		}
		combined := append(merkleRoot, branchBytes...)
		h := dsha256(combined)
		merkleRoot = h[:]
	}

	// Assemble 80-byte block header
	versionBytes, err := hex.DecodeString(job.Version)
	if err != nil || len(versionBytes) != 4 {
		return false, false
	}
	prevHashBytes, err := hex.DecodeString(job.PrevHash)
	if err != nil || len(prevHashBytes) != 32 {
		return false, false
	}
	ntimeBytes, err := hex.DecodeString(ntime)
	if err != nil || len(ntimeBytes) != 4 {
		return false, false
	}
	nbitsBytes, err := hex.DecodeString(job.NBits)
	if err != nil || len(nbitsBytes) != 4 {
		return false, false
	}
	nonceBytes, err := hex.DecodeString(nonce)
	if err != nil || len(nonceBytes) != 4 {
		return false, false
	}

	header := make([]byte, 80)
	copy(header[0:4], versionBytes)
	copy(header[4:36], prevHashBytes)
	copy(header[36:68], merkleRoot)
	copy(header[68:72], ntimeBytes)
	copy(header[72:76], nbitsBytes)
	copy(header[76:80], nonceBytes)

	// Hash the header
	hashBytes := dsha256(header)

	// The hash is in little-endian; reverse for numeric comparison
	hashBig := new(big.Int).SetBytes(reverseBytes(hashBytes[:]))

	// Compute the block target from nbits
	blockTarget := nbitsToTarget(nbitsBytes)

	// Compute the share target from worker difficulty
	// shareTarget = blockTarget * (network_diff / worker_diff)
	// Simpler: shareTarget = maxTarget / workerDiff
	// We use a standard difficulty-1 target for the coin's algorithm
	diff1 := diff1Target(s.coin.Algorithm)
	workerDiffBig := new(big.Float).SetFloat64(workerDiff)
	diff1Float := new(big.Float).SetInt(diff1)
	shareTargetFloat := new(big.Float).Quo(diff1Float, workerDiffBig)
	shareTarget, _ := shareTargetFloat.Int(nil)

	shareValid := hashBig.Cmp(shareTarget) <= 0
	isBlock    := shareValid && hashBig.Cmp(blockTarget) <= 0

	return shareValid, isBlock
}

// assembleBlockHex builds the full block hex from share parameters
func (s *Server) assembleBlockHex(job *Job, extranonce1, extranonce2, ntime, nonce string) string {
	coinbaseHex := job.CoinbasePart1 + extranonce1 + extranonce2 + job.CoinbasePart2
	coinbaseBytes, _ := hex.DecodeString(coinbaseHex)
	coinbaseHash := dsha256(coinbaseBytes)

	merkleRoot := coinbaseHash[:]
	for _, branch := range job.MerkleBranches {
		branchBytes, _ := hex.DecodeString(branch)
		combined := append(merkleRoot, branchBytes...)
		h := dsha256(combined)
		merkleRoot = h[:]
	}

	versionBytes, _ := hex.DecodeString(job.Version)
	prevHashBytes, _ := hex.DecodeString(job.PrevHash)
	ntimeBytes, _ := hex.DecodeString(ntime)
	nbitsBytes, _ := hex.DecodeString(job.NBits)
	nonceBytes, _ := hex.DecodeString(nonce)

	header := make([]byte, 80)
	copy(header[0:4], versionBytes)
	copy(header[4:36], prevHashBytes)
	copy(header[36:68], merkleRoot)
	copy(header[68:72], ntimeBytes)
	copy(header[72:76], nbitsBytes)
	copy(header[76:80], nonceBytes)

	return hex.EncodeToString(header) + coinbaseHex
}

func (s *Server) submitBlock(blockHex, workerName string, height int64) {
	result, err := s.rpcClient.SubmitBlock(blockHex)
	if err != nil {
		log.Printf("[%s] submitblock error: %v", s.coin.Symbol, err)
		return
	}
	if result != "" {
		log.Printf("[%s] block rejected by node: %s (found by %s)", s.coin.Symbol, result, workerName)
		return
	}
	log.Printf("[%s] block #%d accepted! found by %s", s.coin.Symbol, height, workerName)
}

// ─── PoW math helpers ─────────────────────────────────────────────────────────

func dsha256(data []byte) [32]byte {
	h1 := sha256.Sum256(data)
	return sha256.Sum256(h1[:])
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

// ─── unused compat stub (kept to avoid removing reverseByteOrder callers) ────
var _ = binary.LittleEndian // ensure encoding/binary stays imported

// ─── Vardiff ──────────────────────────────────────────────────────────────────

func (s *Server) adjustVardiff(w *Worker) {
	vd := s.coin.Stratum.Vardiff
	retarget := time.Duration(vd.RetargetS) * time.Second

	if time.Since(w.lastShareAt) < retarget {
		return
	}

	// Calculate actual share interval
	elapsed := time.Since(w.lastShareAt).Milliseconds()
	actual := float64(elapsed) / float64(w.sharesAccepted+1)
	target := float64(vd.TargetMs)

	ratio := target / actual
	newDiff := w.difficulty * ratio

	if newDiff < vd.MinDiff {
		newDiff = vd.MinDiff
	}
	if newDiff > vd.MaxDiff {
		newDiff = vd.MaxDiff
	}

	if newDiff != w.difficulty {
		w.difficulty = newDiff
		// shares not reset on vardiff retarget
		s.sendDifficulty(w, newDiff)
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
	Coin    string
	Height  int64
	Hash    string
	Reward  string
	Worker  string
	FoundAt time.Time
}

// WorkerInfo is a public snapshot of a worker (online or previously seen)
type WorkerInfo struct {
	Name           string
	DeviceName     string
	Difficulty     float64
	SharesAccepted uint64
	SharesRejected uint64
	SharesStale    uint64
	RemoteAddr     string
	ConnectedAt    time.Time
	LastSeenAt     time.Time
	Online         bool
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

	out := make([]WorkerInfo, 0, len(s.seenWorkers))
	for name, seen := range s.seenWorkers {
		wi := WorkerInfo{
			Name:           name,
			DeviceName:     seen.DeviceName,
			SharesAccepted: seen.SharesAccepted,
			SharesRejected: seen.SharesRejected,
			SharesStale:    seen.SharesStale,
			RemoteAddr:     seen.LastAddr,
			ConnectedAt:    seen.ConnectedAt,
			LastSeenAt:     seen.LastSeenAt,
			Online:         seen.Online,
		}
		// Use live stats if currently connected
		if w, ok := onlineByName[name]; ok {
			wi.Difficulty     = w.difficulty
			wi.SharesAccepted = w.sharesAccepted
			wi.SharesRejected = w.sharesRejected
			wi.SharesStale    = w.sharesStale
			wi.Online         = true
		}
		out = append(out, wi)
	}
	return out
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
func (s *Server) recordBlock(hash string, height int64, reward float64, worker string) {
	entry := BlockEntry{
		Coin:    s.coin.Symbol,
		Height:  height,
		Hash:    hash,
		Reward:  fmt.Sprintf("%.4f %s", reward, s.coin.Symbol),
		Worker:  worker,
		FoundAt: time.Now(),
	}
	s.mu.Lock()
	s.blockLog = append(s.blockLog, entry)
	if len(s.blockLog) > 50 {
		s.blockLog = s.blockLog[len(s.blockLog)-50:]
	}
	s.mu.Unlock()
}

// ─── Stats ────────────────────────────────────────────────────────────────────

type Stats struct {
	Symbol          string
	ConnectedMiners int32
	TotalShares     uint64
	ValidShares     uint64
	BlocksFound     uint64
	HashrateKHs     float64 // estimated from valid shares
}

func (s *Server) Stats() Stats {
	return Stats{
		Symbol:          s.coin.Symbol,
		ConnectedMiners: s.connectedMiners.Load(),
		TotalShares:     s.totalShares.Load(),
		ValidShares:     s.validShares.Load(),
		BlocksFound:     s.blocksFound.Load(),
	}
}

// ─── Merkle helpers ───────────────────────────────────────────────────────────

// buildMerkleBranch returns the merkle branch needed to combine coinbase with tx list
func buildMerkleBranch(txHashes []string) []string {
	if len(txHashes) == 0 {
		return nil
	}
	// The branch is the list of hashes at each level that must be combined with
	// the running hash to reach the root (always taking the right sibling at each level)
	branch := []string{}
	hashes := make([]string, len(txHashes))
	copy(hashes, txHashes)

	for len(hashes) > 1 {
		branch = append(branch, hashes[0])
		if len(hashes)%2 != 0 {
			hashes = append(hashes, hashes[len(hashes)-1])
		}
		var next []string
		for i := 0; i < len(hashes); i += 2 {
			next = append(next, combinedHash(hashes[i], hashes[i+1]))
		}
		hashes = next
	}
	return branch
}

func combinedHash(a, b string) string {
	// double-sha256(decode(a) + decode(b)) — simplified placeholder
	return fmt.Sprintf("%x", a+b)
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
