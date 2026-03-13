package rpc

import (
	"errors"
	"strings"
)

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var bech32Gen = [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}

func bech32Polymod(values []byte) uint32 {
	chk := uint32(1)
	for _, v := range values {
		b := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (b>>uint(i))&1 != 0 {
				chk ^= bech32Gen[i]
			}
		}
	}
	return chk
}

func bech32HRPExpand(hrp string) []byte {
	ret := make([]byte, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		ret[i] = hrp[i] >> 5
		ret[i+len(hrp)+1] = hrp[i] & 31
	}
	ret[len(hrp)] = 0
	return ret
}

func bech32VerifyChecksum(hrp string, data []byte) bool {
	exp := bech32HRPExpand(hrp)
	combined := make([]byte, len(exp)+len(data))
	copy(combined, exp)
	copy(combined[len(exp):], data)
	return bech32Polymod(combined) == 1
}

func bech32ConvertBits(data []byte, fromBits, toBits uint, pad bool) ([]byte, error) {
	acc := 0
	bits := uint(0)
	var result []byte
	maxv := (1 << toBits) - 1
	for _, v := range data {
		if int(v) < 0 || int(v) >= (1<<fromBits) {
			return nil, errors.New("bech32: invalid data value")
		}
		acc = (acc << fromBits) | int(v)
		bits += fromBits
		for bits >= toBits {
			bits -= toBits
			result = append(result, byte((acc>>bits)&maxv))
		}
	}
	if pad {
		if bits > 0 {
			result = append(result, byte((acc<<(toBits-bits))&maxv))
		}
	} else if bits >= fromBits || ((acc<<(toBits-bits))&maxv) != 0 {
		return nil, errors.New("bech32: invalid padding in data")
	}
	return result, nil
}

// decodeBech32 splits a bech32 string into HRP and 5-bit data groups (checksum stripped).
func decodeBech32(addr string) (hrp string, data []byte, err error) {
	if strings.ToLower(addr) != addr && strings.ToUpper(addr) != addr {
		return "", nil, errors.New("bech32: mixed case")
	}
	lower := strings.ToLower(addr)

	pos := strings.LastIndex(lower, "1")
	if pos < 1 || pos+7 > len(lower) {
		return "", nil, errors.New("bech32: missing or misplaced separator")
	}
	hrp = lower[:pos]

	decoded := make([]byte, len(lower)-pos-1)
	for i, c := range lower[pos+1:] {
		d := strings.IndexRune(bech32Charset, c)
		if d < 0 {
			return "", nil, errors.New("bech32: invalid character")
		}
		decoded[i] = byte(d)
	}

	if !bech32VerifyChecksum(hrp, decoded) {
		return "", nil, errors.New("bech32: invalid checksum")
	}
	// Strip 6-char checksum
	return hrp, decoded[:len(decoded)-6], nil
}

// BuildSegwitScript builds a P2WPKH (20-byte program) or P2WSH (32-byte program)
// output script from a native SegWit bech32 address (bc1q..., ltc1q..., etc.).
//
// Script format for version 0:
//
//	OP_0 (0x00)  PUSH20/PUSH32 (0x14 or 0x20)  <witness-program>
func BuildSegwitScript(addr string) ([]byte, error) {
	_, data, err := decodeBech32(addr)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("bech32: empty witness data")
	}

	witVer := data[0]
	if witVer > 16 {
		return nil, errors.New("bech32: invalid witness version")
	}

	witProg, err := bech32ConvertBits(data[1:], 5, 8, false)
	if err != nil {
		return nil, err
	}
	// P2WPKH: 20 bytes; P2WSH: 32 bytes
	if len(witProg) != 20 && len(witProg) != 32 {
		return nil, errors.New("bech32: witness program must be 20 or 32 bytes")
	}

	// OP_0 = 0x00; OP_1..OP_16 = 0x51..0x60
	var opcode byte
	if witVer == 0 {
		opcode = 0x00
	} else {
		opcode = 0x50 + witVer
	}

	script := make([]byte, 2+len(witProg))
	script[0] = opcode
	script[1] = byte(len(witProg))
	copy(script[2:], witProg)
	return script, nil
}

// BuildOutputScript builds the correct locking script for any supported address type:
//   - Legacy P2PKH / P2SH (Base58Check): 1... 3... L... M... D... ...
//   - Native SegWit P2WPKH / P2WSH (bech32): bc1q... ltc1q... doge1q... ...
//   - Bitcoin Cash CashAddr: bitcoincash:q... (P2PKH) or bitcoincash:p... (P2SH)
func BuildOutputScript(addr string) ([]byte, error) {
	lower := strings.ToLower(strings.TrimSpace(addr))

	// CashAddr: contains a colon separator (bitcoincash:q...)
	if strings.Contains(lower, ":") {
		return BuildCashAddrScript(addr)
	}

	// Bech32 native SegWit: known HRP prefixes
	if strings.HasPrefix(lower, "bc1") ||
		strings.HasPrefix(lower, "ltc1") ||
		strings.HasPrefix(lower, "doge1") ||
		strings.HasPrefix(lower, "dgb1") ||
		strings.HasPrefix(lower, "tb1") ||
		strings.HasPrefix(lower, "tltc1") {
		return BuildSegwitScript(addr)
	}

	// Legacy Base58Check (P2PKH or P2SH)
	return BuildP2PKHScript(addr)
}
