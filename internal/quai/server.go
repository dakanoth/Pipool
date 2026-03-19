package quai

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/scrypt"
)

// ─── Constants ────────────────────────────────────────────────────────────────

const (
	extranonce1Len = 4 // bytes — assigned by pool per connection
	extranonce2Len = 8 // bytes — filled in by miner
)

// diff1Target is the standard Bitcoin SHA-256d difficulty-1 target.
var diff1Target, _ = new(big.Int).SetString(
	"00000000FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", 16)

// ─── Observable types ─────────────────────────────────────────────────────────

// ShareSample records an accepted share for hashrate estimation.
type ShareSample struct {
	Difficulty float64
	TimeMS     int64
	Accepted   bool
}

// HashrateSample is a periodic KH/s snapshot.
type HashrateSample struct {
	KHs    float64
	TimeMS int64
}

// BlockInfo records a block or workshare found by the pool.
type BlockInfo struct {
	Height  uint64
	Hash    string
	Worker  string
	FoundAt time.Time
}

// ─── Job ──────────────────────────────────────────────────────────────────────

// Job is a unit of mining work derived from a BlockTemplate plus per-worker difficulty.
type Job struct {
	ID           string
	Template     *BlockTemplate
	WorkerTarget *big.Int  // share acceptance threshold (from vardiff)
	CreatedAt    time.Time
}

// miningNotify builds the mining.notify params array for this job.
// Follows standard Bitcoin Stratum v1 encoding.
func (j *Job) miningNotify(cleanJobs bool) []interface{} {
	t := j.Template
	// prevhash: chunk-reverse each 4-byte word (standard stratum convention)
	prevBytes, _ := hex.DecodeString(strings.TrimPrefix(t.PrevHash, "0x"))
	prevhash := hex.EncodeToString(reverseChunks32Bytes(prevBytes))
	// merkle_branches are already in internal byte order
	branches := make([]string, len(t.MerkleBranch))
	copy(branches, t.MerkleBranch)
	return []interface{}{
		j.ID,
		prevhash,
		t.Coinb1,
		t.Coinb2,
		branches,
		fmt.Sprintf("%08x", t.Version),
		t.Bits,
		fmt.Sprintf("%08x", t.CurTime),
		cleanJobs,
	}
}

// workerTargetFromDiff returns the 256-bit target for a given vardiff difficulty.
func workerTargetFromDiff(diff float64) *big.Int {
	if diff <= 0 {
		return new(big.Int).Set(diff1Target)
	}
	diffInt := new(big.Int).SetInt64(int64(diff))
	return new(big.Int).Div(diff1Target, diffInt)
}

func newJob(t *BlockTemplate, workerDiff float64) *Job {
	idb := make([]byte, 4)
	rand.Read(idb)
	return &Job{
		ID:           hex.EncodeToString(idb),
		Template:     t,
		WorkerTarget: workerTargetFromDiff(workerDiff),
		CreatedAt:    time.Now(),
	}
}

// ─── Worker ───────────────────────────────────────────────────────────────────

type Worker struct {
	conn        net.Conn
	enc         *json.Encoder
	mu          sync.Mutex
	name        string
	authorized  bool
	difficulty  float64
	extraNonce1 string // 4-byte hex, unique per connection
	// Counters
	sharesAccepted uint64
	sharesRejected uint64
	sharesStale    uint64
	bestShare      float64
	connectedAt    time.Time
	lastShareAt    time.Time
	remoteAddr     string
}

func (w *Worker) send(v interface{}) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(v)
}

func (w *Worker) notify(method string, params interface{}) error {
	return w.send(map[string]interface{}{
		"id":     nil,
		"method": method,
		"params": params,
	})
}

func (w *Worker) respond(id interface{}, result interface{}, errMsg interface{}) error {
	return w.send(map[string]interface{}{
		"id":     id,
		"result": result,
		"error":  errMsg,
	})
}

// ─── WorkerInfo (public snapshot) ─────────────────────────────────────────────

type WorkerInfo struct {
	Name           string
	Difficulty     float64
	HashrateKHs    float64
	SharesAccepted uint64
	SharesRejected uint64
	SharesStale    uint64
	BestShare      float64
	RemoteAddr     string
	ConnectedAt    time.Time
	LastShareAt    time.Time
	Online         bool
}

// ─── Server ───────────────────────────────────────────────────────────────────

// Server is a Stratum v1 mining pool server for a single Quai zone/algorithm.
type Server struct {
	algo         string // "sha256d" or "scrypt"
	ruleAlgo     string // "sha" or "scrypt" (for quai_getBlockTemplate rules param)
	listenAddr   string
	coinbaseAddr string // Quai wallet address
	node         *NodeClient

	mu              sync.RWMutex
	workers         map[string]*Worker // keyed by extranonce1
	currentTemplate *BlockTemplate
	jobHistory      map[string]*Job // recent jobs for share validation
	blocksFound     atomic.Uint64
	validShares     atomic.Uint64
	staleShares     atomic.Uint64
	rejectedShares  atomic.Uint64
	online          atomic.Bool

	// Vardiff settings
	minDiff    float64
	maxDiff    float64
	targetTime float64 // seconds per share

	// Callbacks
	onBlock func(height uint64, hash string, worker string)
	onShare func(worker string, accepted bool)

	// Stats
	connectedMiners atomic.Int32

	symbol          string
	sampleMu        sync.Mutex
	shareSamples    []ShareSample    // ring buffer, last 1000 accepted shares
	hashrateSamples []HashrateSample // ring buffer, last 288 snapshots (24h at 5-min interval)
	blockLog        []BlockInfo      // ring buffer, last 50 blocks

	ln     net.Listener
	stopCh chan struct{}
}

// ServerConfig holds the config for a Quai stratum server instance.
type ServerConfig struct {
	Algo         string  // "sha256d" or "scrypt"
	ListenAddr   string  // e.g. "0.0.0.0:3340"
	CoinbaseAddr string  // Quai wallet address (0x...)
	MinDiff      float64 // vardiff floor
	MaxDiff      float64
	TargetTime   float64 // seconds (default 15)
}

// NewServer creates a Quai stratum server.
func NewServer(cfg ServerConfig, node *NodeClient) *Server {
	if cfg.TargetTime == 0 {
		cfg.TargetTime = 15
	}
	if cfg.MinDiff == 0 {
		if cfg.Algo == "scrypt" {
			cfg.MinDiff = 64
		} else {
			cfg.MinDiff = 65536
		}
	}
	if cfg.MaxDiff == 0 {
		cfg.MaxDiff = cfg.MinDiff * 1024
	}
	ruleAlgo := "sha"
	if cfg.Algo == "scrypt" {
		ruleAlgo = "scrypt"
	}
	sym := "QUAI"
	if cfg.Algo == "scrypt" {
		sym = "QUAIS"
	}
	return &Server{
		symbol:       sym,
		algo:         cfg.Algo,
		ruleAlgo:     ruleAlgo,
		listenAddr:   cfg.ListenAddr,
		coinbaseAddr: cfg.CoinbaseAddr,
		node:         node,
		workers:      make(map[string]*Worker),
		jobHistory:   make(map[string]*Job),
		minDiff:      cfg.MinDiff,
		maxDiff:      cfg.MaxDiff,
		targetTime:   cfg.TargetTime,
		stopCh:       make(chan struct{}),
	}
}

// SetCallbacks wires optional block/share notification callbacks.
func (s *Server) SetCallbacks(onBlock func(uint64, string, string), onShare func(string, bool)) {
	s.onBlock = onBlock
	s.onShare = onShare
}

// Start begins listening for miner connections and polling the node for work.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("quai stratum listen %s: %w", s.listenAddr, err)
	}
	s.ln = ln
	log.Printf("[quai/%s] stratum listening on %s", s.algo, s.listenAddr)

	go s.acceptLoop(ln)
	go s.pollLoop()
	go s.vardiffLoop()
	go s.jobCleanupLoop()
	return nil
}

// Stop shuts down the Quai stratum server gracefully.
func (s *Server) Stop() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	if s.ln != nil {
		s.ln.Close()
	}
}

func (s *Server) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				log.Printf("[quai/%s] accept error: %v", s.algo, err)
				continue
			}
		}
		go s.handleConn(conn)
	}
}

// pollLoop fetches block templates every second and broadcasts new work.
func (s *Server) pollLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastQuaiRoot string
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
		}
		t, err := s.node.GetBlockTemplate(s.ruleAlgo, s.coinbaseAddr)
		if err != nil {
			if s.online.Swap(false) {
				log.Printf("[quai/%s] node offline: %v", s.algo, err)
			}
			continue
		}
		if !s.online.Swap(true) {
			log.Printf("[quai/%s] node online", s.algo)
		}
		if t.QuaiRoot != lastQuaiRoot {
			lastQuaiRoot = t.QuaiRoot
			s.onNewTemplate(t)
		}
	}
}

// onNewTemplate broadcasts new work to all connected miners.
func (s *Server) onNewTemplate(t *BlockTemplate) {
	s.mu.Lock()
	s.currentTemplate = t
	workerList := make([]*Worker, 0, len(s.workers))
	for _, w := range s.workers {
		workerList = append(workerList, w)
	}
	s.mu.Unlock()

	for _, w := range workerList {
		w.mu.Lock()
		authorized := w.authorized
		diff := w.difficulty
		w.mu.Unlock()
		if !authorized {
			continue
		}
		job := newJob(t, diff)
		s.mu.Lock()
		s.jobHistory[job.ID] = job
		s.mu.Unlock()
		_ = w.notify("mining.notify", job.miningNotify(true))
	}
	log.Printf("[quai/%s] new work broadcast to %d miners (quairoot %s)", s.algo, len(workerList), t.QuaiRoot)
}

// handleConn processes a single miner connection.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	en1b := make([]byte, extranonce1Len)
	rand.Read(en1b)
	en1 := hex.EncodeToString(en1b)

	w := &Worker{
		conn:        conn,
		enc:         json.NewEncoder(conn),
		extraNonce1: en1,
		difficulty:  s.minDiff,
		connectedAt: time.Now(),
		remoteAddr:  conn.RemoteAddr().String(),
	}

	s.mu.Lock()
	s.workers[en1] = w
	s.mu.Unlock()
	s.connectedMiners.Add(1)

	log.Printf("[quai/%s] miner connected: %s", s.algo, w.remoteAddr)

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), 4096)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		s.handleMessage(w, msg)
	}

	s.mu.Lock()
	delete(s.workers, en1)
	s.mu.Unlock()
	s.connectedMiners.Add(-1)
	log.Printf("[quai/%s] miner disconnected: %s (%s)", s.algo, w.name, w.remoteAddr)
}

func (s *Server) handleMessage(w *Worker, msg map[string]json.RawMessage) {
	var id interface{}
	var method string
	json.Unmarshal(msg["id"], &id)
	json.Unmarshal(msg["method"], &method)

	switch method {
	case "mining.subscribe":
		s.handleSubscribe(w, id)
	case "mining.authorize":
		s.handleAuthorize(w, id, msg["params"])
	case "mining.submit":
		s.handleSubmit(w, id, msg["params"])
	case "mining.suggest_difficulty":
		var params []interface{}
		json.Unmarshal(msg["params"], &params)
		if len(params) > 0 {
			if d, ok := params[0].(float64); ok {
				w.mu.Lock()
				if d >= s.minDiff && d <= s.maxDiff {
					w.difficulty = d
				}
				w.mu.Unlock()
			}
		}
	}
}

func (s *Server) handleSubscribe(w *Worker, id interface{}) {
	_ = w.respond(id, []interface{}{
		[][]string{{"mining.notify", "ae6812eb4cd7735a302a8a9dd95cf71f"}},
		w.extraNonce1,
		extranonce2Len,
	}, nil)
	_ = w.notify("mining.set_difficulty", []interface{}{w.difficulty})
}

func (s *Server) handleAuthorize(w *Worker, id interface{}, rawParams json.RawMessage) {
	var params []string
	json.Unmarshal(rawParams, &params)
	if len(params) == 0 {
		_ = w.respond(id, false, []interface{}{24, "No username"})
		return
	}
	username := params[0]
	parts := strings.SplitN(username, ".", 2)
	address := parts[0]
	workerName := username
	if len(parts) == 2 {
		workerName = parts[1]
	}
	if !strings.HasPrefix(address, "0x") || len(address) < 10 {
		_ = w.respond(id, false, []interface{}{24, "Invalid Quai address (use 0xAddress.worker)"})
		return
	}

	w.mu.Lock()
	w.authorized = true
	w.name = workerName
	diff := w.difficulty
	w.mu.Unlock()

	_ = w.respond(id, true, nil)
	log.Printf("[quai/%s] authorized: %s from %s", s.algo, workerName, w.remoteAddr)

	s.mu.RLock()
	tmpl := s.currentTemplate
	s.mu.RUnlock()
	if tmpl == nil {
		return
	}
	job := newJob(tmpl, diff)
	s.mu.Lock()
	s.jobHistory[job.ID] = job
	s.mu.Unlock()
	_ = w.notify("mining.set_difficulty", []interface{}{diff})
	_ = w.notify("mining.notify", job.miningNotify(true))
}

func (s *Server) handleSubmit(w *Worker, id interface{}, rawParams json.RawMessage) {
	// params: [worker_name, job_id, extranonce2, ntime, nonce]
	var params []string
	if err := json.Unmarshal(rawParams, &params); err != nil || len(params) < 5 {
		_ = w.respond(id, false, []interface{}{20, "Malformed params"})
		return
	}
	jobID := params[1]
	extranonce2 := params[2]
	ntime := params[3]
	nonce := params[4]

	s.mu.RLock()
	job, ok := s.jobHistory[jobID]
	s.mu.RUnlock()
	if !ok {
		w.mu.Lock()
		w.sharesStale++
		w.mu.Unlock()
		s.staleShares.Add(1)
		_ = w.respond(id, false, []interface{}{21, "Stale — job not found"})
		if s.onShare != nil {
			s.onShare(w.name, false)
		}
		return
	}

	w.mu.Lock()
	en1 := w.extraNonce1
	w.mu.Unlock()

	// Assemble coinbase transaction
	coinbaseTx := assembleCoinbase(job.Template.Coinb1, en1, extranonce2, job.Template.Coinb2)

	// Compute merkle root (internal byte order, not reversed)
	mRoot := computeMerkleRoot(coinbaseTx, job.Template.MerkleBranch)

	// Build the 80-byte header the miner hashed
	header80 := buildHeader80(job.Template, mRoot, ntime, nonce)

	// Hash with the appropriate algorithm (result is big-endian for comparison)
	var hashBytes []byte
	switch s.algo {
	case "scrypt":
		hashBytes = scryptHash(header80)
	default:
		hashBytes = sha256dHash(header80)
	}
	hashInt := new(big.Int).SetBytes(hashBytes)

	// Reject if below worker's vardiff target
	if job.WorkerTarget == nil || hashInt.Cmp(job.WorkerTarget) > 0 {
		w.mu.Lock()
		w.sharesRejected++
		w.mu.Unlock()
		s.rejectedShares.Add(1)
		_ = w.respond(id, false, []interface{}{23, "Low difficulty share"})
		if s.onShare != nil {
			s.onShare(w.name, false)
		}
		return
	}

	// Valid share — update counters
	shareDiff := bigIntToDiff(hashInt)
	w.mu.Lock()
	w.sharesAccepted++
	w.lastShareAt = time.Now()
	if shareDiff > w.bestShare {
		w.bestShare = shareDiff
	}
	w.mu.Unlock()
	s.validShares.Add(1)
	_ = w.respond(id, true, nil)
	if s.onShare != nil {
		s.onShare(w.name, true)
	}
	s.sampleMu.Lock()
	s.shareSamples = append(s.shareSamples, ShareSample{Difficulty: shareDiff, TimeMS: time.Now().UnixMilli(), Accepted: true})
	if len(s.shareSamples) > 1000 {
		s.shareSamples = s.shareSamples[len(s.shareSamples)-1000:]
	}
	s.sampleMu.Unlock()

	// Submit to node if it also meets the template's workshare/block target
	nodeTarget := job.Template.TargetInt
	if nodeTarget != nil && hashInt.Cmp(nodeTarget) <= 0 {
		go s.submitToNode(job, coinbaseTx, header80, w)
	}
}

// submitToNode sends the serialized block (header + coinbase tx) to the Quai node.
// Every share meeting the template target is submitted — the node decides if it's a
// workshare (status 0x0), valid block (0x1), or canonical block (0x2).
func (s *Server) submitToNode(job *Job, coinbaseTx, header80 []byte, w *Worker) {
	// Serialized block: 80-byte header + varint(1) + coinbase tx bytes
	block := make([]byte, 0, len(header80)+1+len(coinbaseTx))
	block = append(block, header80...)
	block = append(block, 0x01) // varint: 1 transaction
	block = append(block, coinbaseTx...)
	rawHex := hex.EncodeToString(block)

	result, err := s.node.SubmitBlock(rawHex, s.ruleAlgo)
	height := job.Template.Height
	if err != nil {
		log.Printf("[quai/%s] submit failed height=%d worker=%s: %v", s.algo, height, w.name, err)
		return
	}

	switch result.Status {
	case "0x2":
		s.blocksFound.Add(1)
		s.recordBlock(height, result.Hash, w.name)
		log.Printf("[quai/%s] *** CANONICAL BLOCK *** height=%d hash=%s worker=%s", s.algo, height, result.Hash, w.name)
		if s.onBlock != nil {
			s.onBlock(height, result.Hash, w.name)
		}
	case "0x1":
		s.blocksFound.Add(1)
		s.recordBlock(height, result.Hash, w.name)
		log.Printf("[quai/%s] *** BLOCK FOUND *** height=%d hash=%s worker=%s", s.algo, height, result.Hash, w.name)
		if s.onBlock != nil {
			s.onBlock(height, result.Hash, w.name)
		}
	default: // "0x0" = workshare
		log.Printf("[quai/%s] workshare submitted height=%d worker=%s", s.algo, height, w.name)
	}
}

// ─── Vardiff ──────────────────────────────────────────────────────────────────

func (s *Server) vardiffLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.adjustDifficulties()
		}
	}
}

func (s *Server) adjustDifficulties() {
	s.mu.RLock()
	workers := make([]*Worker, 0, len(s.workers))
	for _, w := range s.workers {
		workers = append(workers, w)
	}
	s.mu.RUnlock()

	for _, w := range workers {
		w.mu.Lock()
		authorized := w.authorized
		lastShareAt := w.lastShareAt
		connectedAt := w.connectedAt
		sharesAccepted := w.sharesAccepted
		sharesRejected := w.sharesRejected
		oldDiff := w.difficulty
		w.mu.Unlock()

		if !authorized || lastShareAt.IsZero() {
			continue
		}
		elapsed := time.Since(connectedAt).Seconds()
		if elapsed < 30 {
			continue
		}
		totalShares := float64(sharesAccepted + sharesRejected)
		if totalShares == 0 {
			continue
		}
		actualTime := elapsed / totalShares
		ratio := actualTime / s.targetTime
		if ratio < 0.5 || ratio > 2.0 {
			newDiff := oldDiff / ratio
			if newDiff < s.minDiff {
				newDiff = s.minDiff
			}
			if newDiff > s.maxDiff {
				newDiff = s.maxDiff
			}
			if newDiff != oldDiff {
				w.mu.Lock()
				w.difficulty = newDiff
				w.mu.Unlock()
				_ = w.notify("mining.set_difficulty", []interface{}{newDiff})
				log.Printf("[quai/%s] vardiff %s: %.0f → %.0f (ratio %.2f)", s.algo, w.name, oldDiff, newDiff, ratio)
			}
		}
	}
}

// ─── Job cleanup ──────────────────────────────────────────────────────────────

func (s *Server) jobCleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-10 * time.Minute)
			s.mu.Lock()
			for id, j := range s.jobHistory {
				if j.CreatedAt.Before(cutoff) {
					delete(s.jobHistory, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

// ─── Stats ────────────────────────────────────────────────────────────────────

type Stats struct {
	Algo            string
	ConnectedMiners int32
	ValidShares     uint64
	BlocksFound     uint64
	StaleShares     uint64
	RejectedShares  uint64
}

func (s *Server) Stats() Stats {
	return Stats{
		Algo:            s.algo,
		ConnectedMiners: s.connectedMiners.Load(),
		ValidShares:     s.validShares.Load(),
		BlocksFound:     s.blocksFound.Load(),
		StaleShares:     s.staleShares.Load(),
		RejectedShares:  s.rejectedShares.Load(),
	}
}

func (s *Server) Workers() []WorkerInfo {
	s.mu.RLock()
	wlist := make([]*Worker, 0, len(s.workers))
	for _, w := range s.workers {
		wlist = append(wlist, w)
	}
	s.mu.RUnlock()

	out := make([]WorkerInfo, 0, len(wlist))
	for _, w := range wlist {
		w.mu.Lock()
		diff1 := 65536.0
		if s.algo == "sha256d" {
			diff1 = 4294967296.0
		}
		info := WorkerInfo{
			Name:           w.name,
			Difficulty:     w.difficulty,
			HashrateKHs:    w.difficulty * diff1 / s.targetTime / 1000.0,
			SharesAccepted: w.sharesAccepted,
			SharesRejected: w.sharesRejected,
			SharesStale:    w.sharesStale,
			BestShare:      w.bestShare,
			RemoteAddr:     w.remoteAddr,
			ConnectedAt:    w.connectedAt,
			LastShareAt:    w.lastShareAt,
			Online:         true,
		}
		w.mu.Unlock()
		out = append(out, info)
	}
	return out
}

// ─── Block assembly helpers ───────────────────────────────────────────────────

// assembleCoinbase concatenates coinb1 + extranonce1 + extranonce2 + coinb2.
func assembleCoinbase(coinb1, en1, en2, coinb2 string) []byte {
	b1, _ := hex.DecodeString(coinb1)
	e1, _ := hex.DecodeString(en1)
	e2, _ := hex.DecodeString(en2)
	b2, _ := hex.DecodeString(coinb2)
	tx := make([]byte, len(b1)+len(e1)+len(e2)+len(b2))
	n := copy(tx, b1)
	n += copy(tx[n:], e1)
	n += copy(tx[n:], e2)
	copy(tx[n:], b2)
	return tx
}

// computeMerkleRoot builds the merkle root from a coinbase transaction and
// the merkle branch from the block template. Returns bytes in internal (LE) order.
func computeMerkleRoot(coinbaseTx []byte, branch []string) []byte {
	hash := sha256dInternal(coinbaseTx)
	for _, b := range branch {
		branchBytes, _ := hex.DecodeString(b)
		combined := append(hash, branchBytes...)
		hash = sha256dInternal(combined)
	}
	return hash
}

// buildHeader80 assembles the 80-byte Bitcoin-style block header.
func buildHeader80(t *BlockTemplate, merkleRoot []byte, ntime, nonce string) []byte {
	buf := make([]byte, 80)

	// version (4B LE)
	binary.LittleEndian.PutUint32(buf[0:4], t.Version)

	// prevhash (32B) in internal format (each 4-byte word reversed)
	prevBytes, _ := hex.DecodeString(strings.TrimPrefix(t.PrevHash, "0x"))
	copy(buf[4:36], reverseChunks32Bytes(prevBytes))

	// merkle root (32B) already in internal byte order
	copy(buf[36:68], merkleRoot)

	// ntime (4B LE)
	ntimeVal, _ := strconv.ParseUint(ntime, 16, 32)
	binary.LittleEndian.PutUint32(buf[68:72], uint32(ntimeVal))

	// bits (4B LE)
	bitsVal, _ := strconv.ParseUint(t.Bits, 16, 32)
	binary.LittleEndian.PutUint32(buf[72:76], uint32(bitsVal))

	// nonce (4B LE)
	nonceVal, _ := strconv.ParseUint(nonce, 16, 32)
	binary.LittleEndian.PutUint32(buf[76:80], uint32(nonceVal))

	return buf
}

// ─── Crypto helpers ───────────────────────────────────────────────────────────

// sha256dInternal returns the SHA256d of data in internal byte order (not reversed).
// Used for merkle tree computation where hashes are concatenated and re-hashed.
func sha256dInternal(data []byte) []byte {
	h1 := sha256.Sum256(data)
	h2 := sha256.Sum256(h1[:])
	return h2[:]
}

// sha256dHash returns the SHA256d of data in big-endian (display) byte order.
// Used for share difficulty comparison against big.Int targets.
func sha256dHash(data []byte) []byte {
	h1 := sha256.Sum256(data)
	h2 := sha256.Sum256(h1[:])
	out := make([]byte, 32)
	copy(out, h2[:])
	for i, j := 0, 31; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func scryptHash(data []byte) []byte {
	dk, err := scrypt.Key(data, data, 1024, 1, 1, 32)
	if err != nil {
		return make([]byte, 32)
	}
	for i, j := 0, len(dk)-1; i < j; i, j = i+1, j-1 {
		dk[i], dk[j] = dk[j], dk[i]
	}
	return dk
}

// reverseChunks32Bytes reverses the byte order within each 4-byte chunk.
// This converts between display byte order and Bitcoin internal header format.
func reverseChunks32Bytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i := 0; i+4 <= len(b); i += 4 {
		out[i], out[i+1], out[i+2], out[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	return out
}

// reverseChunks32 is the hex-string version of reverseChunks32Bytes.
func reverseChunks32(hexStr string) string {
	b, _ := hex.DecodeString(hexStr)
	return hex.EncodeToString(reverseChunks32Bytes(b))
}

func bigIntToDiff(hash *big.Int) float64 {
	if hash.Sign() == 0 {
		return 0
	}
	result := new(big.Float).SetInt(diff1Target)
	hashF := new(big.Float).SetInt(hash)
	result.Quo(result, hashF)
	f, _ := result.Float64()
	return f
}

// ─── Observable methods ───────────────────────────────────────────────────────

// Symbol returns the coin symbol used in dashboard snapshots ("QUAI" or "QUAIS").
func (s *Server) Symbol() string { return s.symbol }

// NodeOnline reports whether the Quai node is currently reachable.
func (s *Server) NodeOnline() bool { return s.online.Load() }

// CurrentHeight returns the block height from the most recent template, or 0 if none.
func (s *Server) CurrentHeight() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.currentTemplate == nil {
		return 0
	}
	return s.currentTemplate.Height
}

// ShareSamples returns a snapshot of recent accepted share samples.
func (s *Server) ShareSamples() []ShareSample {
	s.sampleMu.Lock()
	out := make([]ShareSample, len(s.shareSamples))
	copy(out, s.shareSamples)
	s.sampleMu.Unlock()
	return out
}

// RecordHashrateSample appends a periodic KH/s snapshot for the hashrate history chart.
func (s *Server) RecordHashrateSample(khs float64) {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	s.hashrateSamples = append(s.hashrateSamples, HashrateSample{KHs: khs, TimeMS: time.Now().UnixMilli()})
	if len(s.hashrateSamples) > 288 {
		s.hashrateSamples = s.hashrateSamples[len(s.hashrateSamples)-288:]
	}
}

// HashrateSamples returns a snapshot of the hashrate history.
func (s *Server) HashrateSamples() []HashrateSample {
	s.sampleMu.Lock()
	out := make([]HashrateSample, len(s.hashrateSamples))
	copy(out, s.hashrateSamples)
	s.sampleMu.Unlock()
	return out
}

// BlockLog returns a snapshot of found blocks.
func (s *Server) BlockLog() []BlockInfo {
	s.sampleMu.Lock()
	out := make([]BlockInfo, len(s.blockLog))
	copy(out, s.blockLog)
	s.sampleMu.Unlock()
	return out
}

func (s *Server) recordBlock(height uint64, hash, worker string) {
	s.sampleMu.Lock()
	s.blockLog = append(s.blockLog, BlockInfo{Height: height, Hash: hash, Worker: worker, FoundAt: time.Now()})
	if len(s.blockLog) > 50 {
		s.blockLog = s.blockLog[len(s.blockLog)-50:]
	}
	s.sampleMu.Unlock()
}
