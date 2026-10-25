package native

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"unicode/utf8"
)

// The wire protocol, version 1; doc.go is its specification. pkg/bridge speaks
// the same bytes from the other side. The two share no code on purpose: a bridge
// in another language implements the specification, and the tests run this
// adapter against the Go bridge to keep both honest.
const (
	protocolVersion = 1
	frameHeaderLen  = 4
	nonceLen        = 32
	// maxHandshakeFrame bounds the frames of the handshake: a few hundred bytes
	// are legitimate.
	maxHandshakeFrame = 4 << 10

	methodHello    = "session.hello"
	methodAuth     = "session.auth"
	methodDescribe = "alixai.describe"
	methodHealth   = "alixai.health"
	methodInspect  = "alixai.inspect"
	methodManage   = "alixai.manage"

	labelServer = "alixai-native/1 server"
	labelClient = "alixai-native/1 client"

	idHello = 1
	idAuth  = 2
	idCall  = 3
)

// JSON-RPC codes the adapter reacts to.
const (
	codeMethodNotFound = -32601
	codeAuthFailed     = -32001
	codePeerRefused    = -32002
)

var (
	errFrameEmpty    = errors.New("native: empty frame")
	errFrameTooLarge = errors.New("native: frame larger than the limit")
	b64              = base64.RawURLEncoding
)

// readFrame reads one frame of at most limit bytes: a 4-byte big-endian length
// and that many bytes of JSON. A length above the limit or zero is an error and
// nothing of the body is read, so a hostile length costs four bytes of memory.
func readFrame(r io.Reader, limit int) ([]byte, error) {
	var hdr [frameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
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

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
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

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	Meta    *rpcMeta        `json:"meta,omitempty"`
}

// decodeResponse reads one response strictly: UTF-8, one object, no unknown
// members, jsonrpc "2.0", the id of the request, and exactly one of result and
// error. Anything else is not a reply to this call.
func decodeResponse(b []byte, id int) (rpcResponse, error) {
	var r rpcResponse
	if !utf8.Valid(b) {
		return r, errors.New("not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return rpcResponse{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return rpcResponse{}, errors.New("trailing data")
	}
	switch {
	case r.JSONRPC != "2.0":
		return rpcResponse{}, errors.New("not a JSON-RPC 2.0 response")
	case string(r.ID) != fmt.Sprint(id):
		return rpcResponse{}, errors.New("the response answers another request")
	case (len(r.Result) == 0) == (r.Error == nil):
		return rpcResponse{}, errors.New("a response carries exactly one of result and error")
	}
	return r, nil
}

// proof is the HMAC-SHA256 one side of the handshake sends: it covers a role
// label and the whole transcript (version, application, endpoint, key id and both
// nonces), each part length-prefixed, so a proof cannot be reflected to the other
// role, replayed into another session, or moved to another application or
// endpoint.
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

// errPeerAuth is a bridge that did not authenticate, or that refused to
// authenticate this adapter: nothing of the call was sent.
var errPeerAuth = errors.New("native: peer authentication failed")

type helloParams struct {
	Version       int    `json:"version"`
	ApplicationID string `json:"application_id"`
	EndpointID    string `json:"endpoint_id"`
	KeyID         string `json:"key_id"`
	Nonce         string `json:"nonce"`
}

type helloResult struct {
	Version int    `json:"version"`
	Nonce   string `json:"nonce"`
	Proof   string `json:"proof"`
}

// handshake is the client half of the mutual authentication. Both sides prove
// they hold the application key over a transcript of both nonces: the bridge
// proves it first, so this adapter sends nothing of a call (no identity, no
// fields) to a process that is not the application, and then proves itself in
// turn. Every refusal and every proof that does not verify is errPeerAuth, from
// which nothing is learned about the key.
func (a *Adapter) handshake(c net.Conn, ep *endpoint) ([]byte, error) {
	send := func(id int, method string, params any) error {
		p, err := json.Marshal(params)
		if err != nil {
			return err
		}
		b, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: p})
		if err != nil {
			return err
		}
		return writeFrame(c, b, maxHandshakeFrame)
	}
	// recv reads one handshake response; once the session key exists (ks not nil) it
	// must be sealed, which only a bridge that holds the key can do.
	recv := func(id int, ks []byte) (rpcResponse, error) {
		var b []byte
		var err error
		if ks != nil {
			b, err = readSealed(c, ks, labelReply, maxHandshakeFrame)
		} else {
			b, err = readFrame(c, maxHandshakeFrame)
		}
		if err != nil {
			if errors.Is(err, errFrameTooLarge) || errors.Is(err, errFrameEmpty) || errors.Is(err, errBadMAC) {
				return rpcResponse{}, errPeerAuth
			}
			return rpcResponse{}, err
		}
		r, err := decodeResponse(b, id)
		if err != nil {
			return rpcResponse{}, errPeerAuth
		}
		if r.Error != nil {
			return rpcResponse{}, errPeerAuth
		}
		return r, nil
	}

	cn := make([]byte, nonceLen)
	if _, err := rand.Read(cn); err != nil {
		return nil, err
	}
	if err := send(idHello, methodHello, helloParams{
		Version: protocolVersion, ApplicationID: a.cfg.ApplicationID, EndpointID: ep.cfg.ID, KeyID: ep.cfg.KeyID, Nonce: b64.EncodeToString(cn),
	}); err != nil {
		return nil, err
	}
	r, err := recv(idHello, nil)
	if err != nil {
		return nil, err
	}
	var hello helloResult
	dec := json.NewDecoder(bytes.NewReader(r.Result))
	dec.DisallowUnknownFields()
	if dec.Decode(&hello) != nil || hello.Version != protocolVersion {
		return nil, errPeerAuth
	}
	sn, err1 := b64.DecodeString(hello.Nonce)
	got, err2 := b64.DecodeString(hello.Proof)
	if err1 != nil || err2 != nil || len(sn) != nonceLen {
		return nil, errPeerAuth
	}
	t := transcript(a.cfg.ApplicationID, ep.cfg.ID, ep.cfg.KeyID, cn, sn)
	key := ep.cfg.Key.value()
	if !hmac.Equal(got, proof(key, labelServer, t...)) {
		return nil, errPeerAuth // the process on the other end does not hold the key
	}
	if err := send(idAuth, methodAuth, map[string]string{"proof": b64.EncodeToString(proof(key, labelClient, t...))}); err != nil {
		return nil, err
	}
	// The confirmation is the first sealed frame: a relay that holds no key and
	// answers for the bridge cannot make it.
	ks := sessionKey(key, t)
	if _, err := recv(idAuth, ks); err != nil {
		return nil, err
	}
	return ks, nil
}

// After the handshake every frame is sealed: the payload is followed by a 32-byte
// HMAC-SHA256 under a session key derived from the handshake transcript, and the
// frame length counts both. The handshake proves that the two ends hold the key;
// the seal binds each request and reply to that session, so a process that holds
// no key and relays the handshake between the two ends cannot alter a request (the
// host would serve an identity the caller never had) or a reply (this adapter
// would take forged data for a result), and cannot have either end accept a frame
// it changed. The direction is part of what is sealed, so a request cannot be
// reflected as a reply. Frames are authenticated, not encrypted.
const (
	macLen       = sha256.Size
	labelSession = "alixai-native/1 session"
	labelRequest = "alixai-native/1 request"
	labelReply   = "alixai-native/1 reply"
)

var errBadMAC = errors.New("native: a frame failed its authentication code")

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
