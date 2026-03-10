package rpc

import (
	"errors"
	"strings"
)

// CashAddr is Bitcoin Cash's address format (BIP350-like but BCH-specific).
// Format: bitcoincash:<payload>  where payload uses the same charset as bech32.
// Reference: https://github.com/bitcoincashorg/bitcoincash.org/blob/master/spec/cashaddr.md

var cashAddrGen = [5]uint64{
	0x98f2bc8e61, 0x79b76d99e2, 0xf33e5fb3c4, 0xae2eabe2a8, 0x1e4f43e470,
}

func cashAddrPolymod(values []byte) uint64 {
	c := uint64(1)
	for _, d := range values {
		c0 := c >> 35
		c = ((c & 0x07ffffffff) << 5) ^ uint64(d)
		if c0&0x01 != 0 {
			c ^= cashAddrGen[0]
		}
		if c0&0x02 != 0 {
			c ^= cashAddrGen[1]
		}
		if c0&0x04 != 0 {
			c ^= cashAddrGen[2]
		}
		if c0&0x08 != 0 {
			c ^= cashAddrGen[3]
		}
		if c0&0x10 != 0 {
			c ^= cashAddrGen[4]
		}
	}
	return c ^ 1
}

func cashAddrVerifyChecksum(hrp string, data []byte) bool {
	// HRP expansion: each byte & 0x1f, then a zero separator
	payload := make([]byte, 0, len(hrp)+1+len(data))
	for i := 0; i < len(hrp); i++ {
		payload = append(payload, hrp[i]&0x1f)
	}
	payload = append(payload, 0)
	payload = append(payload, data...)
	return cashAddrPolymod(payload) == 0
}

// decodeCashAddr decodes a CashAddr string (with or without the "bitcoincash:" prefix).
// Returns the hash type (0=P2PKH, 1=P2SH) and the 20-byte hash.
func decodeCashAddr(addr string) (hashType byte, hash []byte, err error) {
	lower := strings.ToLower(strings.TrimSpace(addr))

	hrp := "bitcoincash"
	payload := lower
	if strings.Contains(lower, ":") {
		parts := strings.SplitN(lower, ":", 2)
		hrp = parts[0]
		payload = parts[1]
	}

	// Decode 5-bit groups using the bech32 charset (CashAddr shares it)
	values := make([]byte, len(payload))
	for i, c := range payload {
		d := strings.IndexRune(bech32Charset, c)
		if d < 0 {
			return 0, nil, errors.New("cashaddr: invalid character")
		}
		values[i] = byte(d)
	}

	if !cashAddrVerifyChecksum(hrp, values) {
		return 0, nil, errors.New("cashaddr: invalid checksum")
	}

	// Strip 8-character checksum
	data := values[:len(values)-8]
	if len(data) == 0 {
		return 0, nil, errors.New("cashaddr: empty payload")
	}

	// First 5-bit group is the version byte:
	//   bits 7-3 = type (0=P2PKH, 1=P2SH)
	//   bits 2-0 = hash size (0 = 160-bit / 20 bytes)
	version := data[0]
	hType := (version >> 3) & 0x1f
	if hType > 1 {
		return 0, nil, errors.New("cashaddr: unsupported hash type")
	}

	hashBytes, err := bech32ConvertBits(data[1:], 5, 8, false)
	if err != nil {
		return 0, nil, err
	}
	if len(hashBytes) != 20 {
		return 0, nil, errors.New("cashaddr: expected 20-byte hash")
	}

	return hType, hashBytes, nil
}

// BuildCashAddrScript builds a P2PKH or P2SH output script from a CashAddr address.
func BuildCashAddrScript(addr string) ([]byte, error) {
	hType, hash20, err := decodeCashAddr(addr)
	if err != nil {
		return nil, err
	}
	if hType == 1 {
		// P2SH: OP_HASH160 <20-byte-hash> OP_EQUAL
		script := make([]byte, 23)
		script[0] = 0xa9 // OP_HASH160
		script[1] = 0x14 // push 20 bytes
		copy(script[2:22], hash20)
		script[22] = 0x87 // OP_EQUAL
		return script, nil
	}
	// P2PKH: OP_DUP OP_HASH160 <20-byte-hash> OP_EQUALVERIFY OP_CHECKSIG
	script := make([]byte, 25)
	script[0] = 0x76 // OP_DUP
	script[1] = 0xa9 // OP_HASH160
	script[2] = 0x14 // push 20 bytes
	copy(script[3:23], hash20)
	script[23] = 0x88 // OP_EQUALVERIFY
	script[24] = 0xac // OP_CHECKSIG
	return script, nil
}
