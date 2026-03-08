package merge

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/dakota/pipool/internal/config"
	"github.com/dakota/pipool/internal/rpc"
)

// MagicHeader is the AuxPoW commitment marker embedded in the coinbase
// "Merged Mining Marker" = fabe6d6d (big-endian) followed by aux hash
var MagicBytes = []byte{0xfa, 0xbe, 0x6d, 0x6d}

// AuxWork holds a snapshot of work from an auxiliary chain (e.g. DOGE, BCH)
type AuxWork struct {
	Symbol     string
	ChainID    uint32
	Hash       []byte // merkle root of aux block header
	Target     []byte // packed target
	Height     int64
	FetchedAt  time.Time
	BlockTemplate interface{} // raw block template from aux daemon
}

// AuxPoWCommitment is embedded in the parent coinbase to commit to aux chain work
type AuxPoWCommitment struct {
	AuxHash  [32]byte // hash of the aux block header
	MerkleBranch [][]byte
	Index    uint32
	ChainID  uint32
}

// Coordinator manages merge mining across multiple aux chains
type Coordinator struct {
	mu       sync.RWMutex
	clients  map[string]*rpc.Client
	auxWork  map[string]*AuxWork // symbol -> current work
	config   map[string]config.CoinConfig
	stopCh   chan struct{}
}

// NewCoordinator creates a merge mining coordinator
func NewCoordinator(cfgs map[string]config.CoinConfig) *Coordinator {
	c := &Coordinator{
		clients: make(map[string]*rpc.Client),
		auxWork: make(map[string]*AuxWork),
		config:  cfgs,
		stopCh:  make(chan struct{}),
	}

	for sym, cfg := range cfgs {
		if cfg.Enabled && cfg.MergeParent != "" {
			c.clients[sym] = rpc.NewClient(
				cfg.Node.Host, cfg.Node.Port,
				cfg.Node.User, cfg.Node.Password, sym,
			)
		}
	}
	return c
}

// Start begins polling aux chains for new block templates
func (c *Coordinator) Start() {
	for sym := range c.clients {
		go c.pollAuxChain(sym)
	}
}

// Stop halts all aux chain polling
func (c *Coordinator) Stop() {
	close(c.stopCh)
}

// pollAuxChain fetches new work from an aux daemon every ~500ms
func (c *Coordinator) pollAuxChain(symbol string) {
	cli := c.clients[symbol]
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var lastHash string

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			hash, err := cli.GetBestBlockHash()
			if err != nil {
				// daemon unreachable — back off
				time.Sleep(5 * time.Second)
				continue
			}
			if hash == lastHash {
				continue
			}
			lastHash = hash

			bt, err := cli.GetBlockTemplate([]string{"coinbasetxn", "workid", "coinbase/append"})
			if err != nil {
				continue
			}

			auxWork := c.buildAuxWork(symbol, bt)
			c.mu.Lock()
			c.auxWork[symbol] = auxWork
			c.mu.Unlock()
		}
	}
}

// buildAuxWork constructs an AuxWork from a block template
func (c *Coordinator) buildAuxWork(symbol string, bt *rpc.BlockTemplate) *AuxWork {
	cfg := c.config[symbol]
	_ = cfg

	// Compute a pseudo aux hash from block template data for commitment
	h := sha256.Sum256([]byte(bt.PreviousBlockHash + bt.Bits))

	target, _ := hex.DecodeString(bt.Target)

	return &AuxWork{
		Symbol:        symbol,
		ChainID:       chainID(symbol),
		Hash:          h[:],
		Target:        target,
		Height:        bt.Height,
		FetchedAt:     time.Now(),
		BlockTemplate: bt,
	}
}

// GetAuxWork returns current work for a given aux symbol
func (c *Coordinator) GetAuxWork(symbol string) (*AuxWork, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	w, ok := c.auxWork[symbol]
	return w, ok
}

// GetAllAuxWork returns all current aux work keyed by symbol
func (c *Coordinator) GetAllAuxWork() map[string]*AuxWork {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]*AuxWork, len(c.auxWork))
	for k, v := range c.auxWork {
		out[k] = v
	}
	return out
}

// BuildCoinbaseCommitment creates the AuxPoW data to embed in the parent coinbase.
// The commitment format follows the Namecoin/merged mining standard:
//   MAGIC (4 bytes) | aux_hash (32 bytes) | aux_merkle_size (4 bytes) | aux_merkle_nonce (4 bytes)
func BuildCoinbaseCommitment(works map[string]*AuxWork) []byte {
	if len(works) == 0 {
		return nil
	}

	// For single aux: use its hash directly.
	// For multiple aux: compute the aux chain merkle tree.
	var auxHashes [][32]byte
	for _, w := range works {
		var h [32]byte
		copy(h[:], w.Hash)
		auxHashes = append(auxHashes, h)
	}

	merkleRoot := computeAuxMerkleRoot(auxHashes)

	out := make([]byte, 0, 4+32+4+4)
	out = append(out, MagicBytes...)
	out = append(out, merkleRoot[:]...)
	// aux_merkle_size: number of aux chains (little-endian uint32)
	out = append(out, uint32LE(uint32(len(works)))...)
	// nonce placeholder
	out = append(out, 0x00, 0x00, 0x00, 0x00)
	return out
}

// SubmitAuxBlock submits a solved aux block to the aux chain daemon.
// parentCoinbase, parentHash, and merkle branch are extracted from the
// solved parent block and packaged as an AuxPoW proof.
func (c *Coordinator) SubmitAuxBlock(symbol string, parentBlockHex string) error {
	cli, ok := c.clients[symbol]
	if !ok {
		return fmt.Errorf("no rpc client for aux coin %s", symbol)
	}

	// In a full implementation, we'd construct the complete AuxPoW structure here:
	// - parent block header (80 bytes)
	// - coinbase transaction
	// - coinbase merkle branch
	// - aux chain merkle branch
	// For now we pass the raw parent block and let the daemon validate AuxPoW
	result, err := cli.SubmitBlock(parentBlockHex)
	if err != nil {
		return fmt.Errorf("[%s] submitblock: %w", symbol, err)
	}
	if result != "" {
		return fmt.Errorf("[%s] block rejected: %s", symbol, result)
	}
	return nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// chainID returns a unique 16-bit chain ID for each supported aux coin.
// These are conventional values used by merge mining.
func chainID(symbol string) uint32 {
	switch symbol {
	case "DOGE":
		return 0x0062 // 98
	case "BCH":
		return 0x0051
	case "LCC":
		return 0x0157 // Litecoin Cash
	case "PEP":
		return 0x0101
	default:
		return 0x0001
	}
}

func computeAuxMerkleRoot(hashes [][32]byte) [32]byte {
	if len(hashes) == 0 {
		return [32]byte{}
	}
	if len(hashes) == 1 {
		return hashes[0]
	}
	for len(hashes) > 1 {
		if len(hashes)%2 != 0 {
			hashes = append(hashes, hashes[len(hashes)-1])
		}
		var next [][32]byte
		for i := 0; i < len(hashes); i += 2 {
			combined := append(hashes[i][:], hashes[i+1][:]...)
			h := sha256.Sum256(combined)
			h = sha256.Sum256(h[:]) // double-sha256
			next = append(next, h)
		}
		hashes = next
	}
	return hashes[0]
}

func uint32LE(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}
