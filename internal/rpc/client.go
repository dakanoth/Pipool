package rpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// Client is a JSON-RPC client for a coin daemon
type Client struct {
	url     string
	user    string
	pass    string
	idSeq   atomic.Uint64
	httpCli *http.Client
	Symbol  string
}

// NewClient creates a new RPC client. Timeouts are tuned for Pi 5 latency on loopback.
func NewClient(host string, port int, user, pass, symbol string) *Client {
	return &Client{
		url:    fmt.Sprintf("http://%s:%d/", host, port),
		user:   user,
		pass:   pass,
		Symbol: symbol,
		httpCli: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// rpcRequest is a JSON-RPC 1.0 request envelope
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

// rpcResponse is a JSON-RPC 1.0 response envelope
type rpcResponse struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// Call executes a raw RPC call and decodes the result into dest
func (c *Client) Call(method string, params []any, dest any) error {
	id := c.idSeq.Add(1)
	req := rpcRequest{JSONRPC: "1.0", ID: id, Method: method, Params: params}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal rpc request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.SetBasicAuth(c.user, c.pass)

	resp, err := c.httpCli.Do(httpReq)
	if err != nil {
		return fmt.Errorf("[%s] rpc call %s: %w", c.Symbol, method, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read rpc response: %w", err)
	}

	var rpcResp rpcResponse
	if err := json.Unmarshal(raw, &rpcResp); err != nil {
		return fmt.Errorf("unmarshal rpc response: %w", err)
	}
	if rpcResp.Error != nil {
		return rpcResp.Error
	}
	if dest != nil {
		return json.Unmarshal(rpcResp.Result, dest)
	}
	return nil
}

// ─── High-level helpers ────────────────────────────────────────────────────────

// BlockTemplate fetches a getblocktemplate for mining
type BlockTemplate struct {
	Version           int32    `json:"version"`
	PreviousBlockHash string   `json:"previousblockhash"`
	Transactions      []TxData `json:"transactions"`
	CoinbaseAux       struct {
		Flags string `json:"flags"`
	} `json:"coinbaseaux"`
	CoinbaseValue  int64    `json:"coinbasevalue"`
	LongPollID     string   `json:"longpollid"`
	Target         string   `json:"target"`
	MinTime        int64    `json:"mintime"`
	Mutable        []string `json:"mutable"`
	NonceRange     string   `json:"noncerange"`
	SigOpLimit     int      `json:"sigoplimit"`
	SizeLimit      int      `json:"sizelimit"`
	WeightLimit    int      `json:"weightlimit"`
	CurTime        int64    `json:"curtime"`
	Bits           string   `json:"bits"`
	Height         int64    `json:"height"`
	WorkID         string   `json:"workid"`
	// AuxPoW fields (for merge mining)
	AuxTarget      string   `json:"auxtarget,omitempty"`
	AuxHash        string   `json:"auxhash,omitempty"`
}

type TxData struct {
	Data    string `json:"data"`
	TxID    string `json:"txid"`
	Hash    string `json:"hash"`
	Fee     int64  `json:"fee"`
	SigOps  int    `json:"sigops"`
	Weight  int    `json:"weight"`
}

func (c *Client) GetBlockTemplate(capabilities []string) (*BlockTemplate, error) {
	params := map[string]any{"capabilities": capabilities, "rules": []string{"segwit"}}
	var bt BlockTemplate
	if err := c.Call("getblocktemplate", []any{params}, &bt); err != nil {
		return nil, err
	}
	return &bt, nil
}

// SubmitBlock submits a solved block. Returns "" on success, rejection reason otherwise.
func (c *Client) SubmitBlock(hexData string) (string, error) {
	var result string
	err := c.Call("submitblock", []any{hexData}, &result)
	return result, err
}

// GetBestBlockHash returns the hash of the chain tip
func (c *Client) GetBestBlockHash() (string, error) {
	var hash string
	err := c.Call("getbestblockhash", nil, &hash)
	return hash, err
}

// GetNetworkHashPS returns the current network hashrate estimate
func (c *Client) GetNetworkHashPS() (float64, error) {
	var h float64
	err := c.Call("getnetworkhashps", nil, &h)
	return h, err
}

// GetMiningInfo returns daemon mining info
type MiningInfo struct {
	Blocks           int64   `json:"blocks"`
	Difficulty       float64 `json:"difficulty"`
	NetworkHashPS    float64 `json:"networkhashps"`
	PooledTx         int     `json:"pooledtx"`
	Chain            string  `json:"chain"`
	Warnings         string  `json:"warnings"`
}

func (c *Client) GetMiningInfo() (*MiningInfo, error) {
	var mi MiningInfo
	err := c.Call("getmininginfo", nil, &mi)
	return &mi, err
}

// ValidateAddress checks that a wallet address is valid for this coin
func (c *Client) ValidateAddress(addr string) (bool, error) {
	var result struct {
		IsValid bool `json:"isvalid"`
	}
	if err := c.Call("validateaddress", []any{addr}, &result); err != nil {
		return false, err
	}
	return result.IsValid, nil
}

// Ping checks that the daemon is reachable
func (c *Client) Ping() error {
	return c.Call("ping", nil, nil)
}

// GetBlockCount returns the current block height
func (c *Client) GetBlockCount() (int64, error) {
	var count int64
	err := c.Call("getblockcount", nil, &count)
	return count, err
}

// CreateCoinbaseTx builds a minimal coinbase transaction paying to walletAddr.
// Returns hex-encoded transaction bytes split into two parts (before & after extranonce).
func CreateCoinbaseTx(walletAddr string, value int64, height int64, extranonceSize int) (part1, part2 []byte) {
	// Encode block height in script (BIP34)
	heightScript := encodeHeight(height)

	// part1: version(4) + input count(1) + prev hash(32) + prev idx(4) + script len varint + height script + extranonce placeholder start
	var buf1 bytes.Buffer
	// version
	buf1.Write([]byte{0x01, 0x00, 0x00, 0x00})
	// input count
	buf1.WriteByte(0x01)
	// prev hash (zeroes for coinbase)
	buf1.Write(make([]byte, 32))
	// prev index (0xFFFFFFFF)
	buf1.Write([]byte{0xff, 0xff, 0xff, 0xff})
	// script: height + extranonce space (we split here)
	scriptLen := len(heightScript) + extranonceSize + 4 // 4 bytes for nonce placeholder
	buf1.WriteByte(byte(scriptLen))
	buf1.Write(heightScript)

	// part2: sequence + output count + output value + output script + locktime
	var buf2 bytes.Buffer
	// sequence
	buf2.Write([]byte{0xff, 0xff, 0xff, 0xff})
	// output count
	buf2.WriteByte(0x01)
	// value (little-endian int64)
	for i := 0; i < 8; i++ {
		buf2.WriteByte(byte(value >> (i * 8)))
	}
	// output script (OP_DUP OP_HASH160 <addr> OP_EQUALVERIFY OP_CHECKSIG)
	// For simplicity we encode a p2pkh placeholder; real impl uses btcutil
	addrBytes := []byte(walletAddr)
	buf2.WriteByte(byte(len(addrBytes) + 5))
	buf2.WriteByte(0x76) // OP_DUP
	buf2.WriteByte(0xa9) // OP_HASH160
	buf2.WriteByte(byte(len(addrBytes)))
	buf2.Write(addrBytes)
	buf2.WriteByte(0x88) // OP_EQUALVERIFY
	buf2.WriteByte(0xac) // OP_CHECKSIG
	// locktime
	buf2.Write([]byte{0x00, 0x00, 0x00, 0x00})

	return buf1.Bytes(), buf2.Bytes()
}

func encodeHeight(h int64) []byte {
	if h == 0 {
		return []byte{0x51} // OP_1
	}
	var b []byte
	for h > 0 {
		b = append(b, byte(h&0xff))
		h >>= 8
	}
	return append([]byte{byte(len(b))}, b...)
}

var _ = bytes.NewBuffer // suppress unused import warning
