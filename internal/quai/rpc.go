// Package quai implements a Quai Network stratum pool for SHA-256 and Scrypt ASICs.
//
// Architecture:
//   - NodeClient connects to the go-quai node via WebSocket JSON-RPC
//   - It polls quai_getPendingHeader every second for fresh work
//   - Valid workshares are submitted via quai_receiveMinedHeader
//   - The Server presents standard Stratum v1 to miners (same protocol as Bitcoin)
package quai

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// ─── RPC types ────────────────────────────────────────────────────────────────

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      int           `json:"id"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// PendingHeader is the work unit returned by quai_getPendingHeader.
// Fields follow go-quai's JSON encoding of a Quai block header.
type PendingHeader struct {
	// WorkShare fields
	ParentHash    []string `json:"parentHash"`    // [prime, region, zone]
	Number        []string `json:"number"`        // [prime, region, zone] hex
	Difficulty    string   `json:"difficulty"`    // hex big.Int
	PrimeTerminus string   `json:"primeTerminus"` // hash hex
	// Seal fields (what miners work on)
	MixHash string `json:"mixHash"` // 32-byte hex (seed for the job)
	Nonce   string `json:"nonce"`   // 8-byte hex
	// Workshare difficulty (may differ from block difficulty)
	WorkShareThreshold string `json:"workShareThreshold"` // hex big.Int
	// Raw header for resubmission
	raw json.RawMessage
}

// SealHash returns the 32-byte hash miners should work on.
// For SHA-256 and Scrypt miners we encode this into a Bitcoin-style 80-byte header.
func (p *PendingHeader) SealHash() []byte {
	h, _ := hex.DecodeString(strings.TrimPrefix(p.MixHash, "0x"))
	return h
}

// WorkDifficulty returns the workshare difficulty as a big.Int.
func (p *PendingHeader) WorkDifficulty() *big.Int {
	d := new(big.Int)
	if p.WorkShareThreshold != "" {
		d.SetString(strings.TrimPrefix(p.WorkShareThreshold, "0x"), 16)
	} else if p.Difficulty != "" {
		d.SetString(strings.TrimPrefix(p.Difficulty, "0x"), 16)
	}
	return d
}

// BlockDifficulty returns the full block difficulty.
func (p *PendingHeader) BlockDifficulty() *big.Int {
	d := new(big.Int)
	d.SetString(strings.TrimPrefix(p.Difficulty, "0x"), 16)
	return d
}

// ZoneNumber returns the zone block number (index 2).
func (p *PendingHeader) ZoneNumber() uint64 {
	if len(p.Number) < 3 {
		return 0
	}
	n := new(big.Int)
	n.SetString(strings.TrimPrefix(p.Number[2], "0x"), 16)
	return n.Uint64()
}

// ─── NodeClient ───────────────────────────────────────────────────────────────

// NodeClient maintains a persistent WebSocket connection to a go-quai node
// and provides methods for fetching work and submitting solutions.
type NodeClient struct {
	wsURL  string
	mu     sync.Mutex
	ws     *websocket.Conn
	idSeq  int
	closed bool

	// Pending header polling
	onNewWork func(*PendingHeader)
	pollStop  chan struct{}
}

// NewNodeClient creates a NodeClient connected to the Quai node at host:wsPort.
// The WebSocket port for a zone node is typically 8546 (zone WS) or a configured zone WS port.
func NewNodeClient(host string, wsPort int) *NodeClient {
	u := url.URL{Scheme: "ws", Host: fmt.Sprintf("%s:%d", host, wsPort), Path: "/"}
	return &NodeClient{
		wsURL:    u.String(),
		pollStop: make(chan struct{}),
	}
}

// Connect establishes the WebSocket connection. Retries indefinitely on failure.
func (c *NodeClient) Connect(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		ws, err := websocket.Dial(c.wsURL, "", "http://localhost/")
		if err != nil {
			log.Printf("[quai-rpc] connect failed (%s): %v — retrying in 5s", c.wsURL, err)
			time.Sleep(5 * time.Second)
			continue
		}
		c.mu.Lock()
		c.ws = ws
		c.mu.Unlock()
		log.Printf("[quai-rpc] connected to %s", c.wsURL)
		return nil
	}
}

// Close shuts down the client.
func (c *NodeClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.ws != nil {
		c.ws.Close()
	}
}

func (c *NodeClient) call(method string, params []interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	if c.ws == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("not connected")
	}
	c.idSeq++
	id := c.idSeq
	ws := c.ws
	c.mu.Unlock()

	req := rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      id,
	}
	if err := websocket.JSON.Send(ws, req); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	var resp rpcResponse
	if err := websocket.JSON.Receive(ws, &resp); err != nil {
		return nil, fmt.Errorf("receive: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// GetPendingHeader fetches the current pending header from the Quai node.
func (c *NodeClient) GetPendingHeader() (*PendingHeader, error) {
	raw, err := c.call("quai_getPendingHeader", nil)
	if err != nil {
		return nil, err
	}
	var h PendingHeader
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("unmarshal header: %w", err)
	}
	h.raw = raw
	return &h, nil
}

// ReceiveMinedHeader submits a solved header (workshare) to the Quai node.
// header is the original PendingHeader with Nonce filled in by the miner.
func (c *NodeClient) ReceiveMinedHeader(header *PendingHeader, nonce string, mixHash string) error {
	// Build a copy of the header with the miner's nonce and mixHash
	var obj map[string]interface{}
	if err := json.Unmarshal(header.raw, &obj); err != nil {
		return fmt.Errorf("unmarshal header for submission: %w", err)
	}
	obj["nonce"] = nonce
	obj["mixHash"] = mixHash

	_, err := c.call("quai_receiveMinedHeader", []interface{}{obj})
	return err
}

// StartPolling begins polling for new work every interval, calling onWork on each new header.
// It reconnects automatically if the WebSocket drops.
func (c *NodeClient) StartPolling(ctx context.Context, interval time.Duration, onWork func(*PendingHeader)) {
	c.onNewWork = onWork
	go func() {
		var lastMixHash string
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
			h, err := c.GetPendingHeader()
			if err != nil {
				log.Printf("[quai-rpc] getPendingHeader: %v", err)
				// Try to reconnect
				_ = c.Connect(ctx)
				continue
			}
			// Only push if work changed
			if h.MixHash != lastMixHash {
				lastMixHash = h.MixHash
				if c.onNewWork != nil {
					c.onNewWork(h)
				}
			}
		}
	}()
}
