package merge

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/dakota/pipool/internal/config"
	"github.com/dakota/pipool/internal/rpc"
)

// MagicBytes is the AuxPoW commitment marker embedded in the coinbase:
// "Merged Mining Marker" = fabe6d6d followed by the aux merkle root.
var MagicBytes = []byte{0xfa, 0xbe, 0x6d, 0x6d}

// AuxWork holds a snapshot of work from an auxiliary chain (e.g. DOGE, BCH).
type AuxWork struct {
	Symbol    string
	ChainID   uint32
	Hash      []byte // commitment hash bytes (little-endian)
	HashHex   string // original hash string as returned by getauxblock (for submitauxblock)
	Target    []byte // packed target
	Height    int64
	FetchedAt time.Time
}

// AuxPoWCommitment is embedded in the parent coinbase to commit to aux chain work
type AuxPoWCommitment struct {
	AuxHash      [32]byte
	MerkleBranch [][]byte
	Index        uint32
	ChainID      uint32
}

// Coordinator manages merge mining across multiple aux chains
type Coordinator struct {
	mu      sync.RWMutex
	clients map[string]*rpc.Client
	auxWork map[string]*AuxWork // symbol -> current work
	config  map[string]config.CoinConfig
	stopCh  chan struct{}
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

// pollAuxChain calls getauxblock on the aux daemon every 500ms to keep work fresh.
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
			ab, err := cli.GetAuxBlock(c.config[symbol].Wallet)
			if err != nil {
				// daemon unreachable — back off
				time.Sleep(5 * time.Second)
				continue
			}

			if ab.Hash == lastHash {
				continue
			}
			lastHash = ab.Hash

			auxWork := buildAuxWork(symbol, ab)
			c.mu.Lock()
			c.auxWork[symbol] = auxWork
			c.mu.Unlock()
		}
	}
}

// buildAuxWork constructs an AuxWork from a getauxblock result.
func buildAuxWork(symbol string, ab *rpc.AuxBlockResult) *AuxWork {
	// The hash from getauxblock is in display (reversed) byte order.
	// Convert to internal little-endian byte order for the commitment.
	hashBytes, err := hex.DecodeString(ab.Hash)
	if err != nil {
		// fallback — use zero hash
		hashBytes = make([]byte, 32)
	}
	// Reverse bytes: getauxblock returns display order, commitment needs LE
	for i, j := 0, len(hashBytes)-1; i < j; i, j = i+1, j-1 {
		hashBytes[i], hashBytes[j] = hashBytes[j], hashBytes[i]
	}

	target, _ := hex.DecodeString(ab.Target)

	return &AuxWork{
		Symbol:    symbol,
		ChainID:   ab.ChainID,
		Hash:      hashBytes,
		HashHex:   ab.Hash, // keep original for submitauxblock
		Target:    target,
		Height:    ab.Height,
		FetchedAt: time.Now(),
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
// works is the set of aux chains to commit to.
// sortedSymbols returns the symbol order used to build the merkle tree — this MUST be
// passed to BuildAuxPoWHex when constructing the proof so the same ordering is used.
func BuildCoinbaseCommitment(works map[string]*AuxWork) (commitment []byte, sortedSymbols []string) {
	if len(works) == 0 {
		return nil, nil
	}

	// Sort symbols for deterministic merkle tree construction
	sortedSymbols = make([]string, 0, len(works))
	for sym := range works {
		sortedSymbols = append(sortedSymbols, sym)
	}
	sort.Strings(sortedSymbols)

	var auxHashes [][32]byte
	for _, sym := range sortedSymbols {
		var h [32]byte
		copy(h[:], works[sym].Hash)
		auxHashes = append(auxHashes, h)
	}

	merkleRoot := computeAuxMerkleRoot(auxHashes)

	out := make([]byte, 0, 4+32+4+4)
	out = append(out, MagicBytes...)
	out = append(out, merkleRoot[:]...)
	out = append(out, uint32LE(uint32(len(works)))...)
	out = append(out, 0x00, 0x00, 0x00, 0x00) // nonce placeholder
	return out, sortedSymbols
}

// BuildAuxPoWHex serializes the AuxPoW proof required by submitauxblock.
// coinbaseMerkleBranch: branch from coinbase tx to the block merkle root (raw bytes per element).
// chainIndex: index of this aux chain in the sorted symbol list (0 for single chain).
// allHashes: all aux chain hashes in sorted order (for computing aux merkle branch).
func BuildAuxPoWHex(coinbaseTx []byte, coinbaseMerkleBranch [][]byte, parentHeader []byte, allHashes [][32]byte, chainIndex int) string {
	var buf bytes.Buffer

	// Coinbase transaction
	buf.Write(coinbaseTx)

	// Coinbase merkle branch (from coinbase to block merkle root)
	buf.Write(varInt(uint64(len(coinbaseMerkleBranch))))
	for _, b := range coinbaseMerkleBranch {
		buf.Write(b)
	}
	// Coinbase index = 0 (coinbase is always the first transaction)
	buf.Write([]byte{0x00, 0x00, 0x00, 0x00})

	// Aux chain merkle branch (from this chain's hash to the aux merkle root)
	auxBranch, auxIndex := computeAuxMerkleBranch(allHashes, chainIndex)
	buf.Write(varInt(uint64(len(auxBranch))))
	for _, b := range auxBranch {
		buf.Write(b[:])
	}
	// Write aux index as int32 LE
	buf.Write([]byte{
		byte(auxIndex),
		byte(auxIndex >> 8),
		byte(auxIndex >> 16),
		byte(auxIndex >> 24),
	})

	// Parent block header (80 bytes)
	buf.Write(parentHeader)

	return hex.EncodeToString(buf.Bytes())
}

// computeAuxMerkleBranch computes the merkle branch (proof path) for element at chainIndex
// in the aux merkle tree. Returns the branch hashes and the index used to reconstruct the root.
func computeAuxMerkleBranch(hashes [][32]byte, chainIndex int) ([][32]byte, int) {
	if len(hashes) <= 1 {
		// Single chain: empty branch, index 0
		return nil, 0
	}

	var branch [][32]byte
	idx := chainIndex
	work := make([][32]byte, len(hashes))
	copy(work, hashes)

	for len(work) > 1 {
		if len(work)%2 != 0 {
			work = append(work, work[len(work)-1])
		}
		// Sibling of idx
		sibling := idx ^ 1
		if sibling < len(work) {
			branch = append(branch, work[sibling])
		}
		// Move to next level
		var next [][32]byte
		for i := 0; i < len(work); i += 2 {
			combined := append(work[i][:], work[i+1][:]...)
			h := sha256.Sum256(combined)
			h = sha256.Sum256(h[:])
			next = append(next, h)
		}
		work = next
		idx /= 2
	}
	return branch, chainIndex
}

// SubmitAuxBlock submits a solved aux block to the aux chain daemon using submitauxblock.
// auxHash is the work hash from GetAuxBlock (must match what was committed to in the parent coinbase).
// auxPoWHex is the serialized AuxPoW proof from BuildAuxPoWHex.
func (c *Coordinator) SubmitAuxBlock(symbol, auxHash, auxPoWHex string) error {
	cli, ok := c.clients[symbol]
	if !ok {
		return fmt.Errorf("no rpc client for aux coin %s", symbol)
	}

	result, err := cli.SubmitAuxBlockRPC(auxHash, auxPoWHex)
	if err != nil {
		return fmt.Errorf("[%s] submitauxblock: %w", symbol, err)
	}
	if result != "" {
		return fmt.Errorf("[%s] block rejected: %s", symbol, result)
	}
	return nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// chainID returns a unique chain ID for each supported aux coin.
func chainID(symbol string) uint32 {
	switch symbol {
	case "DOGE":
		return 0x0062 // 98
	case "BCH":
		return 0x0051
	case "LCC":
		return 0x0157
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
			h = sha256.Sum256(h[:])
			next = append(next, h)
		}
		hashes = next
	}
	return hashes[0]
}

func varInt(n uint64) []byte {
	switch {
	case n < 0xfd:
		return []byte{byte(n)}
	case n <= 0xffff:
		return []byte{0xfd, byte(n), byte(n >> 8)}
	case n <= 0xffffffff:
		return []byte{0xfe, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
	default:
		return []byte{0xff,
			byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24),
			byte(n >> 32), byte(n >> 40), byte(n >> 48), byte(n >> 56),
		}
	}
}

func uint32LE(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

// keep chainID referenced so the compiler doesn't complain
var _ = chainID
