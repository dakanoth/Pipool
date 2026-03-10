package rpc

import (
	"crypto/sha256"
	"errors"
	"math/big"
)

const b58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// b58Decode decodes a Base58-encoded string to raw bytes.
func b58Decode(s string) ([]byte, error) {
	n := new(big.Int)
	for _, c := range s {
		idx := -1
		for i, a := range b58Alphabet {
			if a == c {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, errors.New("base58: invalid character")
		}
		n.Mul(n, big.NewInt(58))
		n.Add(n, big.NewInt(int64(idx)))
	}

	// Count leading '1's — each represents a zero byte.
	var leading int
	for leading < len(s) && s[leading] == '1' {
		leading++
	}

	b := n.Bytes()
	result := make([]byte, leading+len(b))
	copy(result[leading:], b)
	return result, nil
}

// B58CheckDecode decodes a Base58Check address and returns the version byte
// and 20-byte payload (the pubkey/script hash).
func B58CheckDecode(addr string) (version byte, hash20 []byte, err error) {
	decoded, err := b58Decode(addr)
	if err != nil {
		return 0, nil, err
	}
	// Minimum: 1 version byte + 20-byte hash + 4-byte checksum = 25 bytes.
	if len(decoded) < 25 {
		return 0, nil, errors.New("base58check: decoded data too short")
	}

	data := decoded[:len(decoded)-4]
	checksum := decoded[len(decoded)-4:]

	h1 := sha256.Sum256(data)
	h2 := sha256.Sum256(h1[:])
	for i := 0; i < 4; i++ {
		if h2[i] != checksum[i] {
			return 0, nil, errors.New("base58check: invalid checksum")
		}
	}

	if len(data) < 21 {
		return 0, nil, errors.New("base58check: payload too short")
	}
	return data[0], data[1:21], nil
}

// BuildP2PKHScript builds a standard P2PKH locking script from a Base58Check
// wallet address (legacy 1xxx / Lxxx / Dxxx addresses).
//
// The script format is: OP_DUP OP_HASH160 <20-byte-pubkey-hash> OP_EQUALVERIFY OP_CHECKSIG
func BuildP2PKHScript(addr string) ([]byte, error) {
	_, hash20, err := B58CheckDecode(addr)
	if err != nil {
		return nil, err
	}
	if len(hash20) != 20 {
		return nil, errors.New("buildP2PKH: expected 20-byte hash")
	}
	script := make([]byte, 25)
	script[0] = 0x76 // OP_DUP
	script[1] = 0xa9 // OP_HASH160
	script[2] = 0x14 // push 20 bytes
	copy(script[3:23], hash20)
	script[23] = 0x88 // OP_EQUALVERIFY
	script[24] = 0xac // OP_CHECKSIG
	return script, nil
}
