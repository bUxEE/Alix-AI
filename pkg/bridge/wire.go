package bridge

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
)

// The wire protocol of the native adapter, version 1. adapters/native speaks the
// same bytes; docs of that package are the specification a bridge in another
// language implements.
//
// A frame is a 4-byte big-endian length N followed by N bytes holding exactly one
// JSON-RPC 2.0 message. N is checked against the limit before anything of the body
// is read, so a hostile length costs four bytes of memory.
const (
	protocolVersion = 1
	frameHeaderLen  = 4

	// DefaultMaxRequestBytes bounds a frame the bridge accepts: 1 MiB.
	DefaultMaxRequestBytes = 1 << 20
	// DefaultMaxResponseBytes bounds a frame the bridge sends: 1 MiB, the size
	// the adapter accepts by default (adapters.DefaultMaxResponseBytes) once its
	// 4 KiB of envelope is added.
	DefaultMaxResponseBytes = 1 << 20
	// maxHandshakeFrame bounds the frames of the handshake: a few hundred bytes
	// are legitimate.
	maxHandshakeFrame = 4 << 10

	nonceLen = 32
	// writeTimeout bounds a write to a peer that stopped reading.
	writeTimeout = 10 * time.Second
	// MinKeyBytes is the shortest application key the bridge accepts.
	MinKeyBytes = 32
)

// Reserved method names. A host cannot register a method with one of these
// prefixes.
const (
	methodHello    = "session.hello"
	methodAuth     = "session.auth"
	methodDescribe = "alixai.describe"
	methodHealth   = "alixai.health"
	methodInspect  = "alixai.inspect"
	methodManage   = "alixai.manage"

	labelServer = "alixai-native/1 server"
	labelClient = "alixai-native/1 client"
)

// JSON-RPC error codes the bridge answers with.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeAuthFailed     = -32001
	codePeerRefused    = -32002
)

var (
	errFrameEmpty    = errors.New("bridge: empty frame")
	errFrameTooLarge = errors.New("bridge: frame larger than the limit")
)

// readFrame reads one frame of at most limit bytes. A length above the limit or
// zero is an error and nothing of the body is read.
func readFrame(r io.Reader, limit int) ([]byte, error) {
	var hdr [frameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err // io.EOF when the peer closed between frames
	}
	n := binary.BigEndian.Uint32(hdr[:])
	switch {
	case n == 0:
		return nil, errFrameEmpty
	case uint64(n) > uint64(limit):
		return nil, errFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}

// writeFrame writes payload as one frame in one Write call.
func writeFrame(w io.Writer, payload []byte, limit int) error {
	if len(payload) == 0 || len(payload) > limit {
		return errFrameTooLarge
	}
	buf := make([]byte, frameHeaderLen+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)))
	copy(buf[frameHeaderLen:], payload)
	_, err := w.Write(buf)
	return err
}

// rpcRequest is a JSON-RPC request. Params stay raw until the method is known.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcMeta struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// rpcResponse is a JSON-RPC response. Meta is the one member this protocol adds:
// the idempotency key a result echoes.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	Meta    *rpcMeta        `json:"meta,omitempty"`
}

// decodeRequest decodes one request frame strictly: one object, no unknown
// members, jsonrpc "2.0", a method, no trailing data.
func decodeRequest(b []byte) (rpcRequest, error) {
	var req rpcRequest
	if !utf8.Valid(b) {
		return req, errors.New("not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return rpcRequest{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return rpcRequest{}, errors.New("trailing data")
	}
	if req.JSONRPC != "2.0" || req.Method == "" || len(req.ID) == 0 || string(req.ID) == "null" {
		return rpcRequest{}, errors.New("not a JSON-RPC 2.0 request")
	}
	return req, nil
}

// proof is the HMAC-SHA256 one side of the handshake sends: it covers a role
// label and the whole transcript (version, application, endpoint, key id and both
// nonces), each part length-prefixed, so a proof cannot be reflected to the other
// role, replayed into another session, or moved to another application.
func proof(key []byte, label string, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(label))
	m.Write([]byte{0})
	var n [4]byte
	for _, p := range parts {
		binary.BigEndian.PutUint32(n[:], uint32(len(p)))
		m.Write(n[:])
		m.Write(p)
	}
	return m.Sum(nil)
}

func transcript(application, endpoint, keyID string, clientNonce, serverNonce []byte) [][]byte {
	return [][]byte{
		[]byte(fmt.Sprint(protocolVersion)), []byte(application), []byte(endpoint), []byte(keyID), clientNonce, serverNonce,
	}
}

// After the handshake every frame is sealed: the payload is followed by a 32-byte
// HMAC-SHA256 under a session key derived from the handshake transcript, and the
// frame length counts both. The handshake proves that the two ends hold the key;
// the seal binds each request and reply to that session, so a process that holds
// no key and relays the handshake between the two ends cannot alter a request (the
// host would serve an identity the caller never had) or a reply (the adapter would
// take forged data for a result), and cannot have either end accept a frame it
// changed. The direction is part of what is sealed, so a request cannot be
// reflected as a reply. Frames are authenticated, not encrypted.
const (
	macLen       = sha256.Size
	labelSession = "alixai-native/1 session"
	labelRequest = "alixai-native/1 request"
	labelReply   = "alixai-native/1 reply"
)

var errBadMAC = errors.New("bridge: a frame failed its authentication code")

func sessionKey(key []byte, t [][]byte) []byte { return proof(key, labelSession, t...) }

func frameMAC(ks []byte, label string, payload []byte) []byte {
	m := hmac.New(sha256.New, ks)
	m.Write([]byte(label))
	m.Write([]byte{0})
	m.Write(payload)
	return m.Sum(nil)
}

// writeSealed writes payload (at most limit bytes) and its seal as one frame.
func writeSealed(w io.Writer, ks []byte, label string, payload []byte, limit int) error {
	if len(payload) == 0 || len(payload) > limit {
		return errFrameTooLarge
	}
	buf := make([]byte, 0, len(payload)+macLen)
	buf = append(append(buf, payload...), frameMAC(ks, label, payload)...)
	return writeFrame(w, buf, limit+macLen)
}

// readSealed reads a frame whose payload is at most limit bytes and verifies its
// seal before anything of it is used.
func readSealed(r io.Reader, ks []byte, label string, limit int) ([]byte, error) {
	b, err := readFrame(r, limit+macLen)
	if err != nil {
		return nil, err
	}
	if len(b) <= macLen {
		return nil, errBadMAC
	}
	payload, tag := b[:len(b)-macLen], b[len(b)-macLen:]
	if !hmac.Equal(tag, frameMAC(ks, label, payload)) {
		return nil, errBadMAC
	}
	return payload, nil
}
