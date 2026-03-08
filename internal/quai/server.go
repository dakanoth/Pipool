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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/scrypt"
)

// ─── Job encoding ─────────────────────────────────────────────────────────────
//
// Quai work is encoded into a Bitcoin-style Stratum v1 job so standard
// SHA-256d and Scrypt ASICs can hash it without any firmware changes.
//
// The 80-byte "fake" Bitcoin header is constructed as:
//   version  (4B)  = 0x20000000 (BIP9 bits)
//   prevhash (32B) = Quai sealhash (the 32-byte header hash miners work on)
//   merkle   (32B) = Quai sealhash (same — no real txs, just needs to be stable)
//   ntime    (4B)  = current unix time
//   nbits    (4B)  = target packed bits derived from workshare difficulty
//   nonce    (4B)  = miner-supplied
//
// The miner hashes this 80-byte blob with SHA-256d (or Scrypt for Scrypt miners).
// When the result meets the workshare target, the pool submits it to the Quai node
// via quai_receiveMinedHeader with the nonce filled in.

const (
	extranonce1Len = 4 // bytes
	extranonce2Len = 4 // bytes
)

// Job is a unit of mining work derived from a Quai PendingHeader.
type Job struct {
	ID        string
	Header    *PendingHeader
	SealHash  []byte   // 32-byte header hash
	NBits     string   // 8 hex chars, packed difficulty target
	NTime     string   // 8 hex chars, unix timestamp
	Target    *big.Int // full 256-bit target for share validation
	CreatedAt time.Time
}

// miningNotify builds the mining.notify params array for this job.
// Follows standard Bitcoin Stratum v1 encoding.
func (j *Job) miningNotify(cleanJobs bool) []interface{} {
	sealHex := hex.EncodeToString(j.SealHash)
	// prevhash in Stratum is sent in 32-bit chunk-reversed little-endian
	prevhash := reverseChunks32(sealHex)
	// Minimal coinbase: just the seal hash split in two
	coinb1 := sealHex[:32] // 16 bytes
	coinb2 := sealHex[32:] // 16 bytes
	return []interface{}{
		j.ID,
		prevhash,
		coinb1,
		coinb2,
		[]string{}, // empty merkle branch (no real txs)
		"20000000", // version
		j.NBits,
		j.NTime,
		cleanJobs,
	}
}

// reverseChunks32 reverses the byte order within each 4-byte chunk of a hex string.
// This is the standard Bitcoin Stratum prevhash encoding.
func reverseChunks32(hexStr string) string {
	b, _ := hex.DecodeString(hexStr)
	out := make([]byte, len(b))
	for i := 0; i+4 <= len(b); i += 4 {
		out[i], out[i+1], out[i+2], out[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	return hex.EncodeToString(out)
}

// difficultyToNBits converts a big.Int difficulty (not target) into packed nBits.
// diff1Target for Bitcoin SHA-256d is 0x00000000FFFF0000...0000 (26 zero bytes).
// For Scrypt: same diff1 target.
func difficultyToNBits(diff *big.Int) string {
	if diff == nil || diff.Sign() == 0 {
		return "1d00ffff" // genesis difficulty
	}
	// diff1 target = 2^224 - 1 / 256 (Bitcoin standard)
	diff1, _ := new(big.Int).SetString("00000000FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", 16)
	// target = diff1 / difficulty
	target := new(big.Int).Div(diff1, diff)
	if target.Sign() == 0 {
		target.SetInt64(1)
	}
	// Pack into nBits
	b := target.Bytes()
	// Strip leading zero bytes (but keep leading zero if MSB set)
	for len(b) > 1 && b[0] == 0 {
		b = b[1:]
	}
	exp := len(b)
	var mantissa uint32
	if exp >= 3 {
		mantissa = uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
	} else if exp == 2 {
		mantissa = uint32(b[0])<<8 | uint32(b[1])
	} else if exp == 1 {
		mantissa = uint32(b[0])
	}
	if mantissa&0x800000 != 0 {
		mantissa >>= 8
		exp++
	}
	packed := (uint32(exp) << 24) | mantissa
	return fmt.Sprintf("%08x", packed)
}

// nbitsToTarget converts packed nBits back to a full 256-bit target.
func nbitsToTarget(nbits string) *big.Int {
	b, _ := hex.DecodeString(nbits)
	if len(b) < 4 {
		return nil
	}
	exp := int(b[0])
	mantissa := new(big.Int).SetBytes(b[1:4])
	target := new(big.Int).Lsh(mantissa, uint(8*(exp-3)))
	return target
}

// newJob creates a Job from a PendingHeader with an optional difficulty override.
// If workerDiff is nil, the header's workshare difficulty is used.
func newJob(h *PendingHeader, workerDiff *big.Int) *Job {
	seal := h.SealHash()
	if len(seal) < 32 {
		padded := make([]byte, 32)
		copy(padded[32-len(seal):], seal)
		seal = padded
	}
	diff := workerDiff
	if diff == nil || diff.Sign() == 0 {
		diff = h.WorkDifficulty()
	}
	nbits := difficultyToNBits(diff)
	target := nbitsToTarget(nbits)
	// Random job ID
	idb := make([]byte, 4)
	rand.Read(idb)
	jobID := hex.EncodeToString(idb)

	return &Job{
		ID:        jobID,
		Header:    h,
		SealHash:  seal,
		NBits:     nbits,
		NTime:     fmt.Sprintf("%08x", time.Now().Unix()),
		Target:    target,
		CreatedAt: time.Now(),
	}
}

// ─── Worker ───────────────────────────────────────────────────────────────────

type Worker struct {
	conn       net.Conn
	enc        *json.Encoder
	mu         sync.Mutex
	name       string
	authorized bool
	difficulty float64
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
	algo       string // "sha256d" or "scrypt"
	listenAddr string
	node       *NodeClient

	mu          sync.RWMutex
	workers     map[string]*Worker // keyed by extranonce1
	currentJob  *Job
	jobHistory  map[string]*Job // recent jobs for share validation
	blocksFound atomic.Uint64
	validShares atomic.Uint64

	// Vardiff settings
	minDiff    float64
	maxDiff    float64
	targetTime float64 // seconds per share

	// Callbacks
	onBlock func(height uint64, hash string, worker string)
	onShare func(worker string, accepted bool)

	// Stats
	connectedMiners atomic.Int32

	stopCh chan struct{}
}

// ServerConfig holds the config for a Quai stratum server instance.
type ServerConfig struct {
	Algo       string  // "sha256d" or "scrypt"
	ListenAddr string  // e.g. "0.0.0.0:3340"
	MinDiff    float64 // vardiff floor (e.g. 1000 for SHA-256, 64 for Scrypt)
	MaxDiff    float64
	TargetTime float64 // seconds (default 15)
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
	return &Server{
		algo:       cfg.Algo,
		listenAddr: cfg.ListenAddr,
		node:       node,
		workers:    make(map[string]*Worker),
		jobHistory: make(map[string]*Job),
		minDiff:    cfg.MinDiff,
		maxDiff:    cfg.MaxDiff,
		targetTime: cfg.TargetTime,
		stopCh:     make(chan struct{}),
	}
}

// SetCallbacks wires optional block/share notification callbacks.
func (s *Server) SetCallbacks(onBlock func(uint64, string, string), onShare func(string, bool)) {
	s.onBlock = onBlock
	s.onShare = onShare
}

// Start begins listening for miner connections and subscribing to node work.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("quai stratum listen %s: %w", s.listenAddr, err)
	}
	log.Printf("[quai/%s] stratum listening on %s", s.algo, s.listenAddr)

	// Register for new work from the node
	s.node.onNewWork = s.onNewHeader

	go s.acceptLoop(ln)
	go s.vardiffLoop()
	go s.jobCleanupLoop()
	return nil
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

// onNewHeader is called whenever the node sends a new pending header.
func (s *Server) onNewHeader(h *PendingHeader) {
	s.mu.Lock()
	job := newJob(h, nil) // use header's workshare diff for pool-level target
	s.currentJob = job
	s.jobHistory[job.ID] = job
	workerList := make([]*Worker, 0, len(s.workers))
	for _, w := range s.workers {
		workerList = append(workerList, w)
	}
	s.mu.Unlock()

	// Broadcast new job to all connected miners
	for _, w := range workerList {
		if !w.authorized {
			continue
		}
		// Create a per-worker job with their current difficulty
		wJob := newJob(h, floatToBigInt(w.difficulty))
		s.mu.Lock()
		s.jobHistory[wJob.ID] = wJob
		s.mu.Unlock()
		_ = w.notify("mining.notify", wJob.miningNotify(true))
	}
	log.Printf("[quai/%s] new work broadcast to %d miners (sealhash %s...)", s.algo, len(workerList), hex.EncodeToString(h.SealHash())[:12])
}

// handleConn processes a single miner connection.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	// Generate unique extranonce1
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

	// Cleanup on disconnect
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
		// Miner is requesting a starting difficulty — honor within bounds
		var params []interface{}
		json.Unmarshal(msg["params"], &params)
		if len(params) > 0 {
			if d, ok := params[0].(float64); ok {
				if d >= s.minDiff && d <= s.maxDiff {
					w.difficulty = d
				}
			}
		}
	}
}

func (s *Server) handleSubscribe(w *Worker, id interface{}) {
	// Respond: [subscription_id, extranonce1, extranonce2_size]
	_ = w.respond(id, []interface{}{
		[][]string{{"mining.notify", "ae6812eb4cd7735a302a8a9dd95cf71f"}},
		w.extraNonce1,
		extranonce2Len,
	}, nil)
	// Set initial difficulty
	_ = w.notify("mining.set_difficulty", []interface{}{w.difficulty})
}

func (s *Server) handleAuthorize(w *Worker, id interface{}, rawParams json.RawMessage) {
	var params []string
	json.Unmarshal(rawParams, &params)
	if len(params) == 0 {
		_ = w.respond(id, false, []interface{}{24, "No username"})
		return
	}
	// Username format: 0xQuaiAddress.workerName
	username := params[0]
	parts := strings.SplitN(username, ".", 2)
	address := parts[0]
	workerName := username
	if len(parts) == 2 {
		workerName = parts[1]
	}
	// Validate it looks like a Quai address (0x...)
	if !strings.HasPrefix(address, "0x") || len(address) < 10 {
		_ = w.respond(id, false, []interface{}{24, "Invalid Quai address format (use 0xAddress.worker)"})
		return
	}

	w.mu.Lock()
	w.authorized = true
	w.name = workerName
	w.mu.Unlock()

	_ = w.respond(id, true, nil)
	log.Printf("[quai/%s] authorized: %s from %s", s.algo, workerName, w.remoteAddr)

	// Send current job immediately
	s.mu.RLock()
	job := s.currentJob
	s.mu.RUnlock()
	if job != nil {
		wJob := newJob(job.Header, floatToBigInt(w.difficulty))
		s.mu.Lock()
		s.jobHistory[wJob.ID] = wJob
		s.mu.Unlock()
		_ = w.notify("mining.set_difficulty", []interface{}{w.difficulty})
		_ = w.notify("mining.notify", wJob.miningNotify(true))
	}
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

	// Look up job
	s.mu.RLock()
	job, ok := s.jobHistory[jobID]
	s.mu.RUnlock()
	if !ok {
		w.sharesStale++
		_ = w.respond(id, false, []interface{}{21, "Stale — job not found"})
		if s.onShare != nil {
			s.onShare(w.name, false)
		}
		return
	}

	// Build the 80-byte header the miner hashed
	header80 := buildHeader80(job.SealHash, ntime, job.NBits, w.extraNonce1, extranonce2, nonce)

	// Hash it with the appropriate algorithm
	var hashBytes []byte
	switch s.algo {
	case "scrypt":
		hashBytes = scryptHash(header80)
	default: // sha256d
		hashBytes = sha256dHash(header80)
	}

	hashInt := new(big.Int).SetBytes(hashBytes)

	// Check against worker difficulty target
	if job.Target == nil || hashInt.Cmp(job.Target) > 0 {
		w.sharesRejected++
		_ = w.respond(id, false, []interface{}{23, "Low difficulty share"})
		if s.onShare != nil {
			s.onShare(w.name, false)
		}
		return
	}

	// Valid workshare — update counters
	w.sharesAccepted++
	w.lastShareAt = time.Now()
	s.validShares.Add(1)
	shareDiff := bigIntToDiff(hashInt)
	if shareDiff > w.bestShare {
		w.bestShare = shareDiff
	}
	_ = w.respond(id, true, nil)
	if s.onShare != nil {
		s.onShare(w.name, true)
	}

	// Check if this meets the block's full difficulty
	blockTarget := difficultyToNBits(job.Header.BlockDifficulty())
	blockTargetInt := nbitsToTarget(blockTarget)
	if blockTargetInt != nil && hashInt.Cmp(blockTargetInt) <= 0 {
		// BLOCK FOUND — submit to node
		go s.submitBlock(job, w, nonce, extranonce2, ntime)
	} else {
		// Submit as workshare (contributes to network even without full block)
		go s.submitWorkshare(job, w, nonce, extranonce2)
	}
}

// buildHeader80 assembles the 80-byte Bitcoin-style header that the miner hashed.
func buildHeader80(sealHash []byte, ntime, nbits, en1, en2, nonce string) []byte {
	buf := make([]byte, 80)
	// version (4B LE)
	binary.LittleEndian.PutUint32(buf[0:], 0x20000000)
	// prevhash (32B) = sealHash
	copy(buf[4:], sealHash)
	// merkle root (32B) = sealHash (stable placeholder)
	copy(buf[36:], sealHash)
	// ntime (4B LE)
	ntimeBytes, _ := hex.DecodeString(ntime)
	copy(buf[68:], reverseBytes(ntimeBytes))
	// nbits (4B LE)
	nbitsBytes, _ := hex.DecodeString(nbits)
	copy(buf[72:], reverseBytes(nbitsBytes))
	// nonce (4B LE)
	nonceBytes, _ := hex.DecodeString(nonce)
	copy(buf[76:], reverseBytes(nonceBytes))
	return buf
}

// submitBlock sends a found block to the Quai node.
func (s *Server) submitBlock(job *Job, w *Worker, nonce, en2, ntime string) {
	nonceHex := "0x" + nonce
	mixHashHex := "0x" + hex.EncodeToString(job.SealHash) // simplified — real impl derives mixHash
	err := s.node.ReceiveMinedHeader(job.Header, nonceHex, mixHashHex)
	height := job.Header.ZoneNumber()
	if err != nil {
		log.Printf("[quai/%s] BLOCK SUBMIT FAILED height=%d worker=%s: %v", s.algo, height, w.name, err)
		return
	}
	s.blocksFound.Add(1)
	log.Printf("[quai/%s] *** BLOCK FOUND *** height=%d worker=%s", s.algo, height, w.name)
	if s.onBlock != nil {
		s.onBlock(height, nonceHex, w.name)
	}
}

// submitWorkshare sends a valid workshare to the node (contributes to Quai's merged PoW).
func (s *Server) submitWorkshare(job *Job, w *Worker, nonce, en2 string) {
	nonceHex := "0x" + nonce
	mixHashHex := "0x" + hex.EncodeToString(job.SealHash)
	if err := s.node.ReceiveMinedHeader(job.Header, nonceHex, mixHashHex); err != nil {
		// Workshare rejection is normal (stale, already submitted, etc.)
		log.Printf("[quai/%s] workshare rejected worker=%s: %v", s.algo, w.name, err)
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
		if !w.authorized || w.lastShareAt.IsZero() {
			continue
		}
		elapsed := time.Since(w.connectedAt).Seconds()
		if elapsed < 30 {
			continue
		}
		totalShares := float64(w.sharesAccepted + w.sharesRejected)
		if totalShares == 0 {
			continue
		}
		actualTime := elapsed / totalShares
		// Ratio of actual time per share vs target
		ratio := actualTime / s.targetTime
		if ratio < 0.5 || ratio > 2.0 {
			newDiff := w.difficulty / ratio
			if newDiff < s.minDiff {
				newDiff = s.minDiff
			}
			if newDiff > s.maxDiff {
				newDiff = s.maxDiff
			}
			if newDiff != w.difficulty {
				w.difficulty = newDiff
				_ = w.notify("mining.set_difficulty", []interface{}{newDiff})
				log.Printf("[quai/%s] vardiff %s: %.0f → %.0f (ratio %.2f)", s.algo, w.name, w.difficulty, newDiff, ratio)
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
}

func (s *Server) Stats() Stats {
	return Stats{
		Algo:            s.algo,
		ConnectedMiners: s.connectedMiners.Load(),
		ValidShares:     s.validShares.Load(),
		BlocksFound:     s.blocksFound.Load(),
	}
}

func (s *Server) Workers() []WorkerInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]WorkerInfo, 0, len(s.workers))
	for _, w := range s.workers {
		out = append(out, WorkerInfo{
			Name:           w.name,
			Difficulty:     w.difficulty,
			SharesAccepted: w.sharesAccepted,
			SharesRejected: w.sharesRejected,
			SharesStale:    w.sharesStale,
			BestShare:      w.bestShare,
			RemoteAddr:     w.remoteAddr,
			ConnectedAt:    w.connectedAt,
			LastShareAt:    w.lastShareAt,
			Online:         true,
		})
	}
	return out
}

// ─── Crypto helpers ───────────────────────────────────────────────────────────

func sha256dHash(data []byte) []byte {
	h1 := sha256.Sum256(data)
	h2 := sha256.Sum256(h1[:])
	// Return in little-endian (Bitcoin convention)
	out := make([]byte, 32)
	copy(out, h2[:])
	for i, j := 0, 31; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func scryptHash(data []byte) []byte {
	// Litecoin scrypt params: N=1024, r=1, p=1, keyLen=32
	dk, err := scrypt.Key(data, data, 1024, 1, 1, 32)
	if err != nil {
		return make([]byte, 32)
	}
	// Return in little-endian
	for i, j := 0, len(dk)-1; i < j; i, j = i+1, j-1 {
		dk[i], dk[j] = dk[j], dk[i]
	}
	return dk
}

func reverseBytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[len(b)-1-i]
	}
	return out
}

func floatToBigInt(f float64) *big.Int {
	b := new(big.Int)
	b.SetInt64(int64(f))
	return b
}

func bigIntToDiff(hash *big.Int) float64 {
	// diff = diff1Target / hash
	diff1, _ := new(big.Int).SetString("00000000FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", 16)
	if hash.Sign() == 0 {
		return 0
	}
	result := new(big.Float).SetInt(diff1)
	hashF := new(big.Float).SetInt(hash)
	result.Quo(result, hashF)
	f, _ := result.Float64()
	return f
}
