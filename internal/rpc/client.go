package rpc

import (
	"bytes"
	"encoding/hex"
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
	// SegWit witness commitment (BTC/BCH). When present, the coinbase must
	// include an OP_RETURN output with this commitment as the second output.
	DefaultWitnessCommitment string `json:"default_witness_commitment,omitempty"`
	// DGB MultiAlgo: the algorithm the node expects for this block.
	// Values: "sha256d", "scrypt", "skein", "qubit", "odo". Empty for non-DGB coins.
	PowAlgo string `json:"pow_algo,omitempty"`
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
	} else if strings.EqualFold(c.Symbol, "BCH") {
		rules = []string{} // Bitcoin Cash does not support the segwit rule
	}
	params := map[string]any{"capabilities": capabilities, "rules": rules}
	// DGB MultiAlgo: the algo is a separate second RPC parameter (not inside the
	// template_request object). Without it, digibyted defaults to "scrypt".
	var rpcParams []any
	if strings.EqualFold(c.Symbol, "DGB") || strings.EqualFold(c.Symbol, "DGBS") {
		algo := "sha256d"
		if strings.EqualFold(c.Symbol, "DGBS") {
			algo = "scrypt"
		}
		rpcParams = []any{params, algo}
	} else {
		rpcParams = []any{params}
	}
	var bt BlockTemplate
	if err := c.Call("getblocktemplate", rpcParams, &bt); err != nil {
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
	Blocks           int64              `json:"blocks"`
	Difficulty       float64            `json:"difficulty"`
	Difficulties     map[string]float64 `json:"difficulties"` // DGB multi-algo: per-algo difficulties
	NetworkHashPS    float64            `json:"networkhashps"`
	PooledTx         int                `json:"pooledtx"`
	Chain            string             `json:"chain"`
	Warnings         string             `json:"warnings"`
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

// AuxBlockResult holds the result of getauxblock (for merge mining)
type AuxBlockResult struct {
	Hash    string `json:"hash"`
	ChainID uint32 `json:"chainid"`
	Target  string `json:"target"`
	Height  int64  `json:"height"`
}

// GetAuxBlock fetches current merged mining work from an aux chain daemon.
// walletAddr is the address where the aux block reward should be paid.
// Passing an empty string uses the daemon's default wallet address.
func (c *Client) GetAuxBlock(walletAddr string) (*AuxBlockResult, error) {
	var result AuxBlockResult
	var params []any
	if walletAddr != "" {
		params = []any{walletAddr}
	}
	err := c.Call("getauxblock", params, &result)
	return &result, err
}

// SubmitAuxBlockRPC submits a solved aux block.
// hash is the work hash from GetAuxBlock.
// auxpow is the serialized AuxPoW proof hex (parent coinbase + merkle branch + parent header).
func (c *Client) SubmitAuxBlockRPC(hash, auxpow string) (string, error) {
	var result string
	err := c.Call("submitauxblock", []any{hash, auxpow}, &result)
	return result, err
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

// BlockInfo holds the data returned by getblock
type BlockInfo struct {
	Hash          string  `json:"hash"`
	Confirmations int64   `json:"confirmations"` // -1 if block is on a side chain (orphaned)
	Height        int64   `json:"height"`
	Time          int64   `json:"time"`
}

// GetBlock fetches block info by hash. Returns (nil, nil) if block not found.
// Confirmations < 0 means the block is on a side chain (orphaned).
func (c *Client) GetBlock(hash string) (*BlockInfo, error) {
	var info BlockInfo
	// verbosity=1 returns full block info as JSON
	if err := c.Call("getblock", []any{hash, 1}, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// WalletInfo holds balance information from getwalletinfo
type WalletInfo struct {
	Balance            float64 `json:"balance"`
	UnconfirmedBalance float64 `json:"unconfirmed_balance"`
	ImmatureBalance    float64 `json:"immature_balance"`
}

// GetWalletInfo returns current wallet balance info
func (c *Client) GetWalletInfo() (*WalletInfo, error) {
	var wi WalletInfo
	err := c.Call("getwalletinfo", nil, &wi)
	return &wi, err
}

// SendToAddress sends coins from the daemon wallet to the given address.
// Returns the transaction ID on success.
func (c *Client) SendToAddress(addr string, amount float64) (string, error) {
	var txid string
	err := c.Call("sendtoaddress", []any{addr, amount}, &txid)
	return txid, err
}

// GetBlockHash returns the block hash at the given height.
func (c *Client) GetBlockHash(height int64) (string, error) {
	var hash string
	err := c.Call("getblockhash", []any{height}, &hash)
	return hash, err
}

// BlockHeaderInfo holds the result of getblockheader
type BlockHeaderInfo struct {
	Hash       string  `json:"hash"`
	Height     int64   `json:"height"`
	Time       int64   `json:"time"`
	Difficulty float64 `json:"difficulty"`
}

// GetBlockHeader returns header info for the block with the given hash
func (c *Client) GetBlockHeader(hash string) (*BlockHeaderInfo, error) {
	var bh BlockHeaderInfo
	err := c.Call("getblockheader", []any{hash, true}, &bh)
	return &bh, err
}

// CreateCoinbaseTx builds a coinbase transaction split around the extranonce space.
// The coinbase script sig is structured as:
//   [block height (BIP34)] [coinbase tag bytes] [extranonce placeholder]
//
// Returns two halves — part1 ends just before extranonce, part2 starts after.
// The miner fills in extranonce1 + extranonce2 between the two parts.
// The coinbaseTag (e.g. "/PiPool/") is permanently visible on block explorers.
//
// witnessCommitment (hex, from getblocktemplate's default_witness_commitment) is
// required for SegWit blocks (BTC height ≥ 481824). When non-empty, a second output
// is added: value=0, script=OP_RETURN OP_PUSHDATA(36) 0xaa21a9ed <commitment>.
func CreateCoinbaseTx(walletAddr string, value int64, height int64, extranonceSize int, coinbaseTag, witnessCommitment string, auxCommitment []byte) (part1, part2 []byte) {
	heightScript := encodeHeight(height)

	// Sanitize tag — ASCII only, max 20 bytes, ensure it won't blow the 100-byte script limit
	tagBytes := []byte(coinbaseTag)
	if len(tagBytes) > 20 {
		tagBytes = tagBytes[:20]
	}

	// Total coinbase script length:
	// height script + tag + extranonce (extranonceSize bytes) + auxCommitment
	scriptLen := len(heightScript) + len(tagBytes) + extranonceSize + len(auxCommitment)
	if scriptLen > 100 {
		// Trim tag if we'd exceed the protocol limit
		trim := scriptLen - 100
		if trim < len(tagBytes) {
			tagBytes = tagBytes[:len(tagBytes)-trim]
		}
		scriptLen = len(heightScript) + len(tagBytes) + extranonceSize + len(auxCommitment)
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

	// ── Part 2: after extranonce — auxCommitment + sequence + output + locktime ──────────────
	var buf2 bytes.Buffer

	// AuxPoW commitment — still inside the coinbase scriptSig, after extranonce
	if len(auxCommitment) > 0 {
		buf2.Write(auxCommitment)
	}
	// Sequence
	buf2.Write([]byte{0xff, 0xff, 0xff, 0xff})

	// Output count: 1 normally, 2 when a witness commitment is present.
	// Bitcoin Core's getblocktemplate returns default_witness_commitment as the
	// full OP_RETURN scriptPubKey (typically 38 bytes: 6a24aa21a9ed<32-byte-hash>).
	// If only a bare 32-byte hash is provided, wrap it in the standard OP_RETURN script.
	var witnessCommBytes []byte
	if witnessCommitment != "" {
		decoded, err := hex.DecodeString(witnessCommitment)
		if err == nil && len(decoded) > 0 {
			if len(decoded) == 32 {
				// Bare 32-byte hash — wrap in standard OP_RETURN script
				witnessCommBytes = append([]byte{0x6a, 0x24, 0xaa, 0x21, 0xa9, 0xed}, decoded...)
			} else {
				// Full script as returned by Bitcoin Core (38 bytes)
				witnessCommBytes = decoded
			}
		}
	}
	if witnessCommBytes != nil {
		buf2.WriteByte(0x02)
	} else {
		buf2.WriteByte(0x01)
	}

	// Output 0: block reward → wallet
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

	// Output 1 (SegWit only): OP_RETURN witness commitment
	// Bitcoin Core's getblocktemplate returns default_witness_commitment as the
	// full scriptPubKey hex (38 bytes: 6a 24 aa21a9ed <32-byte hash>).
	// We use it directly as the output script.
	if witnessCommBytes != nil {
		// Value: 0 satoshis
		buf2.Write([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
		buf2.WriteByte(byte(len(witnessCommBytes))) // script length
		buf2.Write(witnessCommBytes)
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
	// BIP34 uses CScript signed integer encoding.
	// If the high bit of the last byte is set, the value would be interpreted
	// as negative. Append a zero byte to preserve the positive sign.
	if b[len(b)-1]&0x80 != 0 {
		b = append(b, 0x00)
	}
	return append([]byte{byte(len(b))}, b...)
}

var _ = bytes.NewBuffer  // ensure bytes stays imported
var _ = log.Printf       // ensure log stays imported
