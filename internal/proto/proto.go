// Package proto defines the ADSBNG contributor ingest wire protocol (v1).
//
// This file is intentionally duplicated between adsbng-feeder and adsbng-ingest.
// The two repositories must not import each other: the feeder is contributor-
// facing and public, the gateway is private. Duplicating ~150 lines of framing
// is the price of that boundary. Keep both copies wire-compatible; bump Version
// on any breaking change.
//
// # Connection lifecycle
//
//  1. Feeder dials the gateway over TLS and verifies the server certificate.
//  2. Feeder sends exactly one Hello (length-prefixed JSON).
//  3. Gateway replies with one Ack (length-prefixed JSON).
//  4. On Ack.OK, the feeder streams typed frames (MsgData / MsgHeartbeat).
//     The gateway may reply with MsgPong. Any other direction/type is a
//     protocol error and closes the connection.
//
// # Identity rule (the reason this protocol exists)
//
// The Hello carries a bearer token and NOTHING that names a receiver
// authoritatively. StationIDHint is free-text supplied by an untrusted client;
// it exists so operators can spot a misconfigured box in the logs. The gateway
// resolves the true receiver_id by hashing the token and looking it up in its
// own registry, and echoes the resolved value back in Ack.ReceiverID. A client
// cannot select, suggest, or override its own identity.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Version is the protocol version carried in Hello.Proto.
const Version = 1

// Message types for the post-handshake typed-frame phase.
const (
	MsgData      = 0x01 // feeder -> gateway: opaque Beast bytes
	MsgHeartbeat = 0x02 // feeder -> gateway: JSON HeartbeatPayload
	MsgPong      = 0x03 // gateway -> feeder: JSON PongPayload
)

// Size limits. These are enforced on BOTH sides: the gateway because its peer
// is untrusted, and the feeder so a hostile or broken endpoint cannot make it
// allocate without bound.
const (
	MaxControlLen = 4 << 10  // 4 KiB — Hello/Ack/Heartbeat/Pong JSON
	MaxDataLen    = 64 << 10 // 64 KiB — one MsgData payload
)

// ErrTooLarge is returned when a declared length exceeds the limit for its type.
var ErrTooLarge = errors.New("proto: declared length exceeds maximum")

// Hello is the first and only message the feeder sends before authentication.
type Hello struct {
	Proto int    `json:"proto"`
	Token string `json:"token"`

	// StationIDHint is UNTRUSTED and advisory only. The gateway logs it for
	// diagnostics and MUST NOT use it to determine receiver identity.
	StationIDHint string `json:"station_id_hint,omitempty"`

	FeederVersion string `json:"feeder_version,omitempty"`
}

// Ack is the gateway's reply to Hello.
type Ack struct {
	OK bool `json:"ok"`

	// ReceiverID is the authoritative identity the gateway resolved from the
	// token. Present only when OK. Informational for the feeder (logging).
	ReceiverID string `json:"receiver_id,omitempty"`

	Server        string `json:"server,omitempty"`
	HeartbeatSecs int    `json:"heartbeat_secs,omitempty"`

	// Error is a deliberately coarse reason ("unauthorized", "rate_limited").
	// It never distinguishes "no such token" from "revoked token": that would
	// let a caller probe which tokens exist.
	Error string `json:"error,omitempty"`
}

// HeartbeatPayload reports liveness plus counters the contributor can also see
// in their own logs. Counters are cumulative for the current connection.
type HeartbeatPayload struct {
	Frames     uint64 `json:"frames"`
	Bytes      uint64 `json:"bytes"`
	Discarded  uint64 `json:"discarded"`
	UptimeSecs int64  `json:"uptime_secs"`
}

// PongPayload is the gateway's heartbeat reply.
type PongPayload struct {
	OK bool `json:"ok"`
}

// WriteControl writes a length-prefixed JSON control message (Hello/Ack).
// Framing: 4-byte big-endian length, then the JSON body.
func WriteControl(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("proto: marshal control: %w", err)
	}
	if len(body) > MaxControlLen {
		return ErrTooLarge
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// ReadControl reads a length-prefixed JSON control message into v.
func ReadControl(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxControlLen {
		return ErrTooLarge
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// WriteMsg writes one typed frame: 1-byte type, 3-byte big-endian length, body.
// A 3-byte length caps any single frame at 16 MiB on the wire; the per-type
// limits above are what actually apply.
func WriteMsg(w io.Writer, typ byte, body []byte) error {
	if err := checkLen(typ, len(body)); err != nil {
		return err
	}
	hdr := [4]byte{typ, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	_, err := w.Write(body)
	return err
}

// WriteJSONMsg writes a typed frame whose body is JSON.
func WriteJSONMsg(w io.Writer, typ byte, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("proto: marshal msg: %w", err)
	}
	return WriteMsg(w, typ, body)
}

// ReadMsg reads one typed frame. buf, if large enough, is reused for the body,
// which lets a hot read loop run allocation-free.
//
// The returned slice aliases buf; copy it if you retain it across calls.
func ReadMsg(r io.Reader, buf []byte) (typ byte, body []byte, err error) {
	var hdr [4]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	typ = hdr[0]
	n := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if err = checkLen(typ, n); err != nil {
		return typ, nil, err
	}
	if n == 0 {
		return typ, nil, nil
	}
	if cap(buf) < n {
		buf = make([]byte, n)
	}
	body = buf[:n]
	if _, err = io.ReadFull(r, body); err != nil {
		return typ, nil, err
	}
	return typ, body, nil
}

// checkLen enforces the per-type size ceiling. Unknown types get the small
// control limit so a bogus type byte cannot induce a large allocation.
func checkLen(typ byte, n int) error {
	limit := MaxControlLen
	if typ == MsgData {
		limit = MaxDataLen
	}
	if n > limit {
		return fmt.Errorf("%w: type 0x%02x declared %d > %d", ErrTooLarge, typ, n, limit)
	}
	return nil
}
