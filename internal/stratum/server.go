package stratum

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
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
	currentJob *Job

	// Stats (updated atomically for lock-free reads)
	totalShares   atomic.Uint64
	validShares   atomic.Uint64
	blocksFound   atomic.Uint64
	connectedMiners atomic.Int32

	// Channel to broadcast new jobs to all workers
	jobBroadcast chan *Job
	stopCh       chan struct{}

	// Callbacks for Discord notifications
	OnBlockFound    func(coin, hash string, reward float64)
	OnMinerConnect  func(worker string, addr string)
	OnMinerDisconnect func(worker string)
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
	shareCount   uint64

	// Extranonce assigned to this worker
	extranonce1  string
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

	log.Printf("[%s] Stratum server listening on %s", s.coin.Symbol, addr)

	// Start block template poller
	go s.pollBlockTemplate()

	// Accept miner connections
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-s.stopCh:
					return
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

			go s.handleWorker(conn)
		}
	}()

	// Broadcast new jobs as they arrive
	go s.broadcastJobs()

	return nil
}

// Stop shuts down the server
func (s *Server) Stop() {
	close(s.stopCh)
}

// ─── Block template polling ───────────────────────────────────────────────────

func (s *Server) pollBlockTemplate() {
	var lastHash string
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			hash, err := s.rpcClient.GetBestBlockHash()
			if err != nil {
				log.Printf("[%s] getbestblockhash: %v", s.coin.Symbol, err)
				time.Sleep(5 * time.Second)
				continue
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
				// channel full, drop old job - miners will get next one
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
	workerID := fmt.Sprintf("%x", rand.Uint32())
	w := &Worker{
		id:          workerID,
		conn:        conn,
		writer:      bufio.NewWriter(conn),
		remoteAddr:  conn.RemoteAddr().String(),
		difficulty:  s.coin.Stratum.Vardiff.MinDiff,
		lastShareAt: time.Now(),
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
		return s.reply(w, msg.ID, false, []any{20, "Malformed params", nil})
	}

	// Share validation would happen here in a full implementation.
	// We accept the share optimistically and check if it meets block target.
	s.validShares.Add(1)
	w.lastShareAt = time.Now()
	w.shareCount++

	// Adjust vardiff
	s.adjustVardiff(w)

	// Check if share meets block difficulty (simplified check)
	// In production, we'd do full block assembly and hash comparison here
	isBlock := s.checkBlockSolution(params)
	if isBlock {
		s.blocksFound.Add(1)
		log.Printf("[%s] 🏆 BLOCK FOUND by %s!", s.coin.Symbol, w.workerName)

		// Submit to daemon
		go s.submitBlock(params, w.workerName)
	}

	return s.reply(w, msg.ID, true, nil)
}

// checkBlockSolution is a placeholder — full impl assembles the block and
// double-sha256 hashes the header, comparing against the current nbits target
func (s *Server) checkBlockSolution(params []string) bool {
	// TODO: assemble full block header and validate hash <= target
	// For production: reconstruct coinbase, build merkle root, pack header, hash
	return false
}

func (s *Server) submitBlock(shareParams []string, workerName string) {
	// TODO: assemble full block hex from share params + current job template
	// result, err := s.rpcClient.SubmitBlock(blockHex)
	log.Printf("[%s] submitting block found by %s", s.coin.Symbol, workerName)
}

// ─── Vardiff ──────────────────────────────────────────────────────────────────

func (s *Server) adjustVardiff(w *Worker) {
	vd := s.coin.Stratum.Vardiff
	retarget := time.Duration(vd.RetargetS) * time.Second

	if time.Since(w.lastShareAt) < retarget {
		return
	}

	// Calculate actual share interval
	elapsed := time.Since(w.lastShareAt).Milliseconds()
	actual := float64(elapsed) / float64(w.shareCount+1)
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
		w.shareCount = 0
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

// WorkerInfo is a public snapshot of a connected worker
type WorkerInfo struct {
	Name       string
	DeviceName string
	Difficulty float64
	ShareCount uint64
	RemoteAddr string
}

// ConnectedWorkers returns a snapshot of all currently connected authorized workers
func (s *Server) ConnectedWorkers() []WorkerInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]WorkerInfo, 0, len(s.workers))
	for _, w := range s.workers {
		if w.authorized {
			out = append(out, WorkerInfo{
				Name:       w.workerName,
				DeviceName: w.deviceName,
				Difficulty: w.difficulty,
				ShareCount: w.shareCount,
				RemoteAddr: w.remoteAddr,
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
