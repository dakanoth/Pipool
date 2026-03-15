// Package zmq provides a minimal ZMTP 3.0 ZMQ subscriber for receiving
// hashblock notifications from Bitcoin/Litecoin/Dogecoin nodes.
// No external dependencies — implements just enough of the protocol to
// connect a SUB socket to a node's PUB endpoint.
package zmq

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Subscribe connects to a ZMTP 3.0 ZMQ PUB endpoint (e.g. "tcp://127.0.0.1:28332"),
// subscribes to topic (e.g. "hashblock"), and calls fn for each received multi-frame message.
// Blocks until the connection is lost or an error occurs — the caller should reconnect.
func Subscribe(endpoint, topic string, fn func(frames [][]byte)) error {
	addr := strings.TrimPrefix(endpoint, "tcp://")
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	if err := handshake(conn, topic); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	for {
		frames, err := readMsg(conn)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if len(frames) > 0 {
			fn(frames)
		}
	}
}

// ─── ZMTP 3.0 handshake ───────────────────────────────────────────────────────

func handshake(conn net.Conn, topic string) error {
	// Both sides send the 64-byte greeting simultaneously.
	// Signature: 0xFF, 8 zero bytes, 0x7F
	// Version:   3 (major), 1 (minor)
	// Mechanism: "NULL" padded to 20 bytes
	// As-server: 0 (we are the client)
	// Padding:   31 zero bytes
	var g [64]byte
	g[0] = 0xFF
	g[9] = 0x7F
	g[10] = 3 // ZMTP major
	g[11] = 1 // ZMTP minor
	copy(g[12:], "NULL")
	// g[32] = 0 (as-server = false, already zero)
	if _, err := conn.Write(g[:]); err != nil {
		return fmt.Errorf("send greeting: %w", err)
	}

	// Read server greeting
	var sg [64]byte
	if _, err := io.ReadFull(conn, sg[:]); err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}

	// Send our READY command with Socket-Type = "SUB"
	if _, err := conn.Write(buildReadyCmd()); err != nil {
		return fmt.Errorf("send READY: %w", err)
	}

	// Consume server's READY command (we don't need its contents)
	if err := consumeFrame(conn); err != nil {
		return fmt.Errorf("read server READY: %w", err)
	}

	// Send SUBSCRIBE command for the topic
	if _, err := conn.Write(buildSubscribeCmd(topic)); err != nil {
		return fmt.Errorf("send SUBSCRIBE: %w", err)
	}

	return nil
}

// buildReadyCmd builds a ZMTP READY command frame with Socket-Type=SUB.
//
//	Frame: [0x04][size][0x05]["READY"][0x0B]["Socket-Type"][0x00 0x00 0x00 0x03]["SUB"]
func buildReadyCmd() []byte {
	body := []byte{
		0x05, 'R', 'E', 'A', 'D', 'Y', // name-size + "READY"
		0x0B, // property name length: 11
		'S', 'o', 'c', 'k', 'e', 't', '-', 'T', 'y', 'p', 'e', // "Socket-Type"
		0x00, 0x00, 0x00, 0x03, // property value length: 3 (big-endian)
		'S', 'U', 'B', // "SUB"
	}
	return append([]byte{0x04, byte(len(body))}, body...)
}

// buildSubscribeCmd builds a ZMTP SUBSCRIBE command frame for the given topic.
//
//	Frame: [0x04][size][0x09]["SUBSCRIBE"][topic]
func buildSubscribeCmd(topic string) []byte {
	name := "SUBSCRIBE"
	body := make([]byte, 0, 1+len(name)+len(topic))
	body = append(body, byte(len(name)))
	body = append(body, name...)
	body = append(body, topic...)
	return append([]byte{0x04, byte(len(body))}, body...)
}

// ─── Frame reading ────────────────────────────────────────────────────────────

// consumeFrame reads and discards a single ZMTP frame (command or message).
func consumeFrame(conn net.Conn) error {
	_, _, err := readFrameRaw(conn)
	return err
}

// readMsg reads a complete multi-frame ZMQ message, skipping any interleaved
// command frames (e.g. HEARTBEAT). Returns when a full message (all frames
// up to and including the last one without MORE flag) is assembled.
func readMsg(conn net.Conn) ([][]byte, error) {
	var frames [][]byte
	for {
		flags, data, err := readFrameRaw(conn)
		if err != nil {
			return nil, err
		}
		isCommand := flags&0x04 != 0
		if isCommand {
			// Skip commands (HEARTBEAT, PING, etc.) that may appear at any time
			continue
		}
		frames = append(frames, data)
		if flags&0x01 == 0 { // MORE flag not set → last frame of this message
			return frames, nil
		}
	}
}

// readFrameRaw reads one ZMTP 3.0 frame and returns (flags, body, err).
// Supports both short (1-byte size) and long (8-byte size) frames.
func readFrameRaw(conn net.Conn) (byte, []byte, error) {
	var hdr [1]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return 0, nil, err
	}
	flags := hdr[0]

	var size uint64
	if flags&0x02 != 0 {
		// Long frame: 8-byte big-endian size
		var s [8]byte
		if _, err := io.ReadFull(conn, s[:]); err != nil {
			return 0, nil, err
		}
		size = binary.BigEndian.Uint64(s[:])
	} else {
		// Short frame: 1-byte size
		var s [1]byte
		if _, err := io.ReadFull(conn, s[:]); err != nil {
			return 0, nil, err
		}
		size = uint64(s[0])
	}

	if size > 1<<20 { // sanity cap: 1 MB
		return 0, nil, fmt.Errorf("frame too large: %d bytes", size)
	}

	body := make([]byte, size)
	if _, err := io.ReadFull(conn, body); err != nil {
		return 0, nil, err
	}
	return flags, body, nil
}
