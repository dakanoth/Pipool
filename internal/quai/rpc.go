// Package quai implements a Quai Network stratum pool for SHA-256 and Scrypt ASICs.
//
// Architecture:
//   - NodeClient connects to the go-quai node via HTTP JSON-RPC
//   - Each Server polls quai_getBlockTemplate every second for fresh work
//   - Valid shares (workshares and full blocks) are submitted via
//     quai_submitShaBlock or quai_submitScryptBlock with the full serialized block
//   - The Server presents standard Stratum v1 to miners (same protocol as Bitcoin)
package quai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
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

// BlockTemplate is the work unit returned by quai_getBlockTemplate.
type BlockTemplate struct {
	Bits         string   `json:"bits"`
	Coinb1       string   `json:"coinb1"`
	Coinb2       string   `json:"coinb2"`
	MerkleBranch []string `json:"merklebranch"`
	PrevHash     string   `json:"previousblockhash"`
	Target       string   `json:"target"`
	Version      uint32   `json:"version"`
	Height       uint64   `json:"height"`
	CurTime      uint32   `json:"curtime"`
	QuaiRoot     string   `json:"quairoot"` // first 6 bytes of sealhash — change detector

	EN2Length int `json:"extranonce2Length"` // should be 8

	// Parsed target (big-endian big.Int for comparison). Filled by GetBlockTemplate.
	TargetInt *big.Int `json:"-"`
}

// getBlockTemplateParams is sent as the single element of the params array.
type getBlockTemplateParams struct {
	Rules       []string `json:"rules"`
	ExtraNonce1 string   `json:"extranonce1"`
	ExtraNonce2 string   `json:"extranonce2"`
	ExtraData   string   `json:"extradata"`
	Coinbase    string   `json:"coinbase"`
}

// SubmitResult is the response from quai_submitShaBlock / quai_submitScryptBlock.
type SubmitResult struct {
	Hash   string `json:"hash"`
	Number string `json:"number"`
	// "0x0" = workshare, "0x1" = valid block, "0x2" = canonical block
	Status string `json:"status"`
}

// ─── NodeClient ───────────────────────────────────────────────────────────────

// NodeClient sends HTTP JSON-RPC requests to a go-quai zone node.
type NodeClient struct {
	httpURL string
	client  *http.Client
	mu      sync.Mutex
	idSeq   int
}

// NewNodeClient creates a NodeClient pointed at host:httpPort.
// The HTTP port for zone-0-0 is 9200 (base 9001 + 199 + zone-index).
func NewNodeClient(host string, httpPort int) *NodeClient {
	return &NodeClient{
		httpURL: fmt.Sprintf("http://%s:%d", host, httpPort),
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// Connect is a no-op for HTTP; kept so callers compiled against the old API still build.
func (c *NodeClient) Connect(_ context.Context) error { return nil }

// Close is a no-op for HTTP.
func (c *NodeClient) Close() {}

func (c *NodeClient) call(method string, params []interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	c.idSeq++
	id := c.idSeq
	c.mu.Unlock()

	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params, ID: id}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Post(c.httpURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

// GetBlockTemplate fetches a fresh block template from the node.
// algo must be "sha" (SHA-256d) or "scrypt".
// coinbaseAddr is the Quai wallet address that receives the block reward.
func (c *NodeClient) GetBlockTemplate(algo, coinbaseAddr string) (*BlockTemplate, error) {
	params := getBlockTemplateParams{
		Rules:       []string{algo},
		ExtraNonce1: "00000000",
		ExtraNonce2: "0000000000000000",
		ExtraData:   "/PiPool/",
		Coinbase:    coinbaseAddr,
	}
	raw, err := c.call("quai_getBlockTemplate", []interface{}{params})
	if err != nil {
		return nil, err
	}
	var t BlockTemplate
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("unmarshal template: %w", err)
	}
	targetHex := strings.TrimPrefix(t.Target, "0x")
	t.TargetInt = new(big.Int)
	t.TargetInt.SetString(targetHex, 16)
	return &t, nil
}

// SubmitBlock submits a solved block or workshare to the node.
// rawHex is the full hex-encoded serialized block (80-byte header + varint + coinbase tx),
// without a 0x prefix. algo must be "sha" or "scrypt".
func (c *NodeClient) SubmitBlock(rawHex, algo string) (*SubmitResult, error) {
	method := "quai_submitShaBlock"
	if algo == "scrypt" {
		method = "quai_submitScryptBlock"
	}
	raw, err := c.call(method, []interface{}{"0x" + rawHex})
	if err != nil {
		return nil, err
	}
	var result SubmitResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("unmarshal submit result: %w", err)
	}
	return &result, nil
}
