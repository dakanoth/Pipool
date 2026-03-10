package rpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
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
			Timeout: 3 * time.Second,
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
	rules := []string{"segwit"}
	if strings.EqualFold(c.Symbol, "LTC") {
		rules = append(rules, "mweb")
	}
	params := map[string]any{"capabilities": capabilities, "rules": rules}
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


// BlockchainInfo holds sync state from getblockchaininfo
type BlockchainInfo struct {
	Chain                string  `json:"chain"`
	Blocks               int64   `json:"blocks"`
	Headers              int64   `json:"headers"`
	VerificationProgress float64 `json:"verificationprogress"` // 0.0 → 1.0
	InitialBlockDownload bool    `json:"initialblockdownload"`
	SizeOnDisk           int64   `json:"size_on_disk"`
}

// GetBlockchainInfo returns chain sync state including verificationprogress (0.0→1.0).
// Use this to show sync progress on the dashboard.
func (c *Client) GetBlockchainInfo() (*BlockchainInfo, error) {
	var info BlockchainInfo
	err := c.Call("getblockchaininfo", nil, &info)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// GetBlockCount returns the current block height
func (c *Client) GetBlockCount() (int64, error) {
	var count int64
	err := c.Call("getblockcount", nil, &count)
	return count, err
}

// CreateCoinbaseTx builds a coinbase transaction split around the extranonce space.
// The coinbase script sig is structured as:
//   [block height (BIP34)] [coinbase tag bytes] [extranonce placeholder]
//
// Returns two halves — part1 ends just before extranonce, part2 starts after.
// The miner fills in extranonce1 + extranonce2 between the two parts.
// The coinbaseTag (e.g. "/PiPool/") is permanently visible on block explorers.
func CreateCoinbaseTx(walletAddr string, value int64, height int64, extranonceSize int, coinbaseTag string) (part1, part2 []byte) {
	heightScript := encodeHeight(height)

	// Sanitize tag — ASCII only, max 20 bytes, ensure it won't blow the 100-byte script limit
	tagBytes := []byte(coinbaseTag)
	if len(tagBytes) > 20 {
		tagBytes = tagBytes[:20]
	}

	// Total coinbase script length:
	// height script + tag + extranonce (extranonceSize bytes)
	scriptLen := len(heightScript) + len(tagBytes) + extranonceSize
	if scriptLen > 100 {
		// Trim tag if we'd exceed the protocol limit
		trim := scriptLen - 100
		if trim < len(tagBytes) {
			tagBytes = tagBytes[:len(tagBytes)-trim]
		}
		scriptLen = len(heightScript) + len(tagBytes) + extranonceSize
	}

	// ── Part 1: version + input + script up to (not including) extranonce ────
	var buf1 bytes.Buffer

	// Version (int32 LE)
	buf1.Write([]byte{0x01, 0x00, 0x00, 0x00})

	// Input count: 1
	buf1.WriteByte(0x01)

	// Prev hash: 32 zero bytes (coinbase)
	buf1.Write(make([]byte, 32))

	// Prev index: 0xFFFFFFFF
	buf1.Write([]byte{0xff, 0xff, 0xff, 0xff})

	// Script length varint
	buf1.WriteByte(byte(scriptLen))

	// Height (BIP34)
	buf1.Write(heightScript)

	// Coinbase tag — this is what shows up on block explorers
	buf1.Write(tagBytes)

	// ── Part 2: after extranonce — sequence + output + locktime ──────────────
	var buf2 bytes.Buffer

	// Sequence
	buf2.Write([]byte{0xff, 0xff, 0xff, 0xff})

	// Output count: 1
	buf2.WriteByte(0x01)

	// Value (int64 LE) — satoshis
	for i := 0; i < 8; i++ {
		buf2.WriteByte(byte(value >> (i * 8)))
	}

	// Output script: supports legacy P2PKH (Base58Check) and native SegWit P2WPKH (bech32).
	outputScript, err := BuildOutputScript(walletAddr)
	if err != nil {
		// Fallback: OP_RETURN — provably unspendable. Should never happen with a valid address.
		log.Printf("[coinbase] WARN: cannot decode wallet address %q: %v — block reward will be UNSPENDABLE. Fix the wallet address in your config.", walletAddr, err)
		buf2.WriteByte(0x01)
		buf2.WriteByte(0x6a) // OP_RETURN
	} else {
		buf2.WriteByte(byte(len(outputScript)))
		buf2.Write(outputScript)
	}

	// Locktime
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

var _ = bytes.NewBuffer  // ensure bytes stays imported
var _ = log.Printf       // ensure log stays imported
