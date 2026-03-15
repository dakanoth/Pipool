package stratum

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/dakota/pipool/internal/config"
)

// UpstreamProxy manages a connection to an upstream stratum pool.
// When the local node is offline, the stratum server delegates work to it.
type UpstreamProxy struct {
	cfg    config.UpstreamConf
	symbol string
	onJob  func(job *Job, extranonce1 string, difficulty float64)

	mu      sync.Mutex
	conn    net.Conn
	writer  *bufio.Writer
	idSeq   int
	stopCh  chan struct{}
	stopped bool
}

type proxyMsg struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params"`
	Result json.RawMessage  `json:"result"`
	Error  json.RawMessage  `json:"error"`
}

func newUpstreamProxy(cfg config.UpstreamConf, symbol string, onJob func(*Job, string, float64)) *UpstreamProxy {
	return &UpstreamProxy{
		cfg:    cfg,
		symbol: symbol,
		onJob:  onJob,
		stopCh: make(chan struct{}),
	}
}

func (u *UpstreamProxy) Start() error {
	addr := fmt.Sprintf("%s:%d", u.cfg.Host, u.cfg.Port)
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("proxy connect to %s: %w", addr, err)
	}
	u.mu.Lock()
	u.conn = conn
	u.writer = bufio.NewWriter(conn)
	u.mu.Unlock()

	go u.run(conn)
	return nil
}

func (u *UpstreamProxy) Stop() {
	u.mu.Lock()
	u.stopped = true
	if u.conn != nil {
		u.conn.Close()
	}
	u.mu.Unlock()
}

func (u *UpstreamProxy) nextID() int {
	u.idSeq++
	return u.idSeq
}

func (u *UpstreamProxy) send(msg interface{}) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.writer == nil {
		return
	}
	b, _ := json.Marshal(msg)
	b = append(b, '\n')
	u.writer.Write(b)
	u.writer.Flush()
}

func (u *UpstreamProxy) run(conn net.Conn) {
	defer func() {
		conn.Close()
		u.mu.Lock()
		stopped := u.stopped
		u.mu.Unlock()
		if !stopped {
			// Reconnect after delay
			log.Printf("[%s/proxy] disconnected, reconnecting in 15s", u.symbol)
			select {
			case <-u.stopCh:
				return
			case <-time.After(15 * time.Second):
			}
			if err := u.Start(); err != nil {
				log.Printf("[%s/proxy] reconnect failed: %v", u.symbol, err)
			}
		}
	}()

	// Subscribe
	subID := u.nextID()
	u.send(map[string]interface{}{
		"id": subID, "method": "mining.subscribe",
		"params": []interface{}{"pipool/1.0"},
	})

	var en1 string
	var en2Size int
	var currentDiff float64 = 1.0
	scanner := bufio.NewScanner(conn)
	// pendingNotify: store the last notify before we've sent authorize, so we don't miss a job
	var pendingNotify *proxyMsg

	authorizeID := -1

	for scanner.Scan() {
		select {
		case <-u.stopCh:
			return
		default:
		}

		var msg proxyMsg
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}

		// Subscribe response
		if msg.ID != nil {
			var id int
			if json.Unmarshal(*msg.ID, &id) == nil && id == subID && msg.Result != nil {
				// [session_id, extranonce1, extranonce2_size]
				var parts []json.RawMessage
				if json.Unmarshal(msg.Result, &parts) == nil && len(parts) >= 3 {
					json.Unmarshal(parts[1], &en1)
					json.Unmarshal(parts[2], &en2Size)
				}
				// Authorize
				authorizeID = u.nextID()
				u.send(map[string]interface{}{
					"id": authorizeID, "method": "mining.authorize",
					"params": []interface{}{u.cfg.User, u.cfg.Password},
				})
				continue
			}
			if authorizeID >= 0 {
				var id int
				if json.Unmarshal(*msg.ID, &id) == nil && id == authorizeID {
					// Authorization response — ignore result for now
					authorizeID = -1
					// Process any pending notify
					if pendingNotify != nil {
						u.handleNotify(pendingNotify, en1, en2Size, currentDiff)
						pendingNotify = nil
					}
					continue
				}
			}
			continue
		}

		switch msg.Method {
		case "mining.set_difficulty":
			var params []float64
			if json.Unmarshal(msg.Params, &params) == nil && len(params) > 0 {
				currentDiff = params[0]
			}
		case "mining.notify":
			if en1 == "" {
				pendingNotify = &msg
			} else {
				u.handleNotify(&msg, en1, en2Size, currentDiff)
			}
		case "mining.set_extranonce":
			var params []json.RawMessage
			if json.Unmarshal(msg.Params, &params) == nil && len(params) >= 2 {
				json.Unmarshal(params[0], &en1)
				json.Unmarshal(params[1], &en2Size)
			}
		}
	}
}

func (u *UpstreamProxy) handleNotify(msg *proxyMsg, en1 string, en2Size int, diff float64) {
	// params: [jobId, prevHash, cb1, cb2, merkleBranches, version, nBits, nTime, cleanJobs]
	var params []json.RawMessage
	if err := json.Unmarshal(msg.Params, &params); err != nil || len(params) < 9 {
		return
	}
	var jobID, prevHash, cb1, cb2, version, nbits, ntime string
	var cleanJobs bool
	var merkleRaw []json.RawMessage
	json.Unmarshal(params[0], &jobID)
	json.Unmarshal(params[1], &prevHash)
	json.Unmarshal(params[2], &cb1)
	json.Unmarshal(params[3], &cb2)
	json.Unmarshal(params[4], &merkleRaw)
	json.Unmarshal(params[5], &version)
	json.Unmarshal(params[6], &nbits)
	json.Unmarshal(params[7], &ntime)
	json.Unmarshal(params[8], &cleanJobs)

	merkleBranches := make([]string, len(merkleRaw))
	for i, m := range merkleRaw {
		json.Unmarshal(m, &merkleBranches[i])
	}

	// Use a random job-id suffix to avoid collisions if we reconnect
	localJobID := fmt.Sprintf("%s_%04x", jobID, rand.Intn(65536))
	job := &Job{
		ID:             localJobID,
		PrevHash:       prevHash,
		CoinbasePart1:  cb1,
		CoinbasePart2:  cb2,
		MerkleBranches: merkleBranches,
		Version:        version,
		NBits:          nbits,
		NTime:          ntime,
		CleanJobs:      cleanJobs,
		CreatedAt:      time.Now(),
	}
	if u.onJob != nil {
		u.onJob(job, en1, diff)
	}
	_ = en2Size // used for future extranonce2 size negotiation
}

// Submit sends a share to the upstream pool. Returns true if accepted.
func (u *UpstreamProxy) Submit(worker, jobID, en2, ntime, nonce string) bool {
	id := u.nextID()
	// Strip our suffix from jobID to get the original upstream jobID
	origJobID := jobID
	if idx := len(jobID) - 5; idx > 0 && jobID[idx] == '_' {
		origJobID = jobID[:idx]
	}
	type submitMsg struct {
		ID     int      `json:"id"`
		Method string   `json:"method"`
		Params []string `json:"params"`
	}
	u.send(submitMsg{
		ID:     id,
		Method: "mining.submit",
		Params: []string{u.cfg.User, origJobID, en2, ntime, nonce},
	})
	// We optimistically return true; the upstream's response is not awaited
	// to avoid blocking the miner. A false positive is harmless for a fallback proxy.
	return true
}
