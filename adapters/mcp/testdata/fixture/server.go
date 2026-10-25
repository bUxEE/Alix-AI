// Package fixture is a small, scriptable MCP server for the tests of
// adapters/mcp. It speaks JSON-RPC 2.0 with the 2025-11-25 revision of the MCP
// protocol over streamable HTTP (one JSON response per POST) and over lines of
// JSON on a stream (stdio), and it puts internal/adapters/fake.App behind MCP
// tools the way the clinic application puts its service behind them:
//
//	identity + fields                  -> items_get, items_create_validate, items_create_prepare
//	identity + idempotency_key + prepared -> items_create_commit
//	identity + idempotency_key         -> items_create_status, items_create_cancel
//	describe, health, inspect          -> the management tools
//
// Every tool answers with the host members of a DeterministicResult as
// structuredContent and mirrors the JSON as text, exactly as a real server does.
//
// The application behind it keeps the contract of task M1-12: Validate, on
// unchanged data, reports in freshness.version the version Prepare reported, which
// is the snapshot version of the records the action relies on (here, the version of
// the item collection of the tenant and workspace), not a per-response ETag and not
// a counter that moves with every response. A server that broke it would make every
// commit stale; the adapter passes the version through and the audit probes check it.
//
// It is also an adversary: tools can be added, removed and redefined while it
// runs (a rug pull), results can be text only, a connection can be dropped or
// held open, and the process can exit in the middle of a call. Nothing here is
// production code.
package fixture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The names of the tools that put fake.App behind MCP.
const (
	ToolGet      = "items_get"
	ToolValidate = "items_create_validate"
	ToolPrepare  = "items_create_prepare"
	ToolCommit   = "items_create_commit"
	ToolStatus   = "items_create_status"
	ToolCancel   = "items_create_cancel"
	ToolDescribe = "describe"
	ToolHealth   = "health"
	ToolInspect  = "inspect"
)

// MetaKey is the member of a tool result's _meta that echoes the idempotency key.
const MetaKey = "alixai/idempotency_key"

// Tool is one tool as the server lists it.
type Tool struct {
	Name        string
	Description string
	Input       map[string]any
	Output      map[string]any
}

// Result is what a tool call answers.
type Result struct {
	// Structured is encoded as structuredContent; nil leaves it out.
	Structured any
	// Text is the text content; "" leaves the content empty.
	Text    string
	IsError bool
	Meta    map[string]any
	// Abort drops the connection without an answer (the stream is closed for a
	// stdio server).
	Abort bool
	// Hang holds the call open until the client gives up.
	Hang bool
	// Crash ends the process (stdio subprocess) without an answer.
	Crash bool
	// Raw, when set, is the whole result object, sent as it is.
	Raw any
	// RPCError, when set, answers the call with a JSON-RPC error.
	RPCError *RPCError
	// Preamble is sent before the answer, on a stream only: requests a hostile
	// server makes of its client.
	Preamble []any
}

// RPCError is a JSON-RPC error answer.
type RPCError struct {
	Code    int
	Message string
}

// Handler answers one tool call. args is the call's arguments object.
type Handler func(ctx context.Context, args json.RawMessage) Result

// Entry is one request the server received.
type Entry struct {
	Method string
	Tool   string
	// Args is the arguments of a tool call.
	Args json.RawMessage
	// Authorization is the Authorization header of an HTTP request.
	Authorization string
}

// Server is the MCP server.
type Server struct {
	// App is the host application behind the standard tools.
	App *fake.App

	mu       sync.Mutex
	tools    []Tool
	handlers map[string]Handler
	log      []Entry
	pageSize int
	crashFn  func()
	cancels  map[string]context.CancelFunc
	version  string
}

// New returns a server with the standard tools over app.
func New(app *fake.App) *Server {
	s := &Server{App: app, handlers: map[string]Handler{}, cancels: map[string]context.CancelFunc{}, version: "1.0.0", crashFn: func() { os.Exit(3) }}
	obj := func(props map[string]any, required ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		return m
	}
	identity := map[string]any{"type": "object"}
	fields := map[string]any{"type": "object"}
	key := map[string]any{"type": "string"}
	out := obj(map[string]any{"status": map[string]any{"type": "string"}}, "status")
	std := []struct {
		name, desc string
		op         adapters.Operation
		capability string
		in         map[string]any
	}{
		{ToolGet, "Read one item.", adapters.OpRead, fake.ReadCapability, obj(map[string]any{"identity": identity, "fields": fields}, "identity", "fields")},
		{ToolValidate, "Validate an item name.", adapters.OpValidate, fake.ActionCapability, obj(map[string]any{"identity": identity, "fields": fields}, "identity", "fields")},
		{ToolPrepare, "Prepare the creation of an item.", adapters.OpPrepare, fake.ActionCapability, obj(map[string]any{"identity": identity, "fields": fields}, "identity", "fields")},
		{ToolCommit, "Create the prepared item.", adapters.OpCommit, fake.ActionCapability, obj(map[string]any{"identity": identity, "idempotency_key": key, "prepared": map[string]any{"type": "object"}}, "identity", "idempotency_key", "prepared")},
		{ToolStatus, "What was recorded for a key.", adapters.OpStatus, fake.ActionCapability, obj(map[string]any{"identity": identity, "idempotency_key": key}, "identity", "idempotency_key")},
		{ToolCancel, "Withdraw pending work for a key.", adapters.OpCancel, fake.ActionCapability, obj(map[string]any{"identity": identity, "idempotency_key": key}, "identity", "idempotency_key")},
	}
	for _, t := range std {
		s.Add(Tool{Name: t.name, Description: t.desc, Input: t.in, Output: out}, s.appHandler(t.op, t.capability))
	}
	empty := obj(map[string]any{})
	s.Add(Tool{Name: ToolDescribe, Description: "The contract versions and operations of the application.", Input: empty, Output: out}, s.appHandler(adapters.OpDescribe, ""))
	s.Add(Tool{Name: ToolHealth, Description: "Whether the application can serve requests.", Input: empty, Output: out}, s.appHandler(adapters.OpHealth, ""))
	s.Add(Tool{Name: ToolInspect, Description: "Runtime facts the application exposes.", Input: obj(map[string]any{"identity": identity, "aspects": map[string]any{"type": "array"}}), Output: out}, s.appHandler(adapters.OpInspect, ""))
	return s
}

// Add registers a tool, replacing one of the same name.
func (s *Server) Add(t Tool, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.tools {
		if s.tools[i].Name == t.Name {
			s.tools[i] = t
			s.handlers[t.Name] = h
			return
		}
	}
	s.tools = append(s.tools, t)
	s.handlers[t.Name] = h
}

// AddDuplicate lists one more tool of an existing name, with its own definition.
func (s *Server) AddDuplicate(t Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = append(s.tools, t)
}

// Remove drops a tool from the list and from what can be called.
func (s *Server) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.tools[:0]
	for _, t := range s.tools {
		if t.Name != name {
			out = append(out, t)
		}
	}
	s.tools = out
	delete(s.handlers, name)
}

// Redefine changes a listed tool: the rug pull.
func (s *Server) Redefine(name string, f func(*Tool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.tools {
		if s.tools[i].Name == name {
			f(&s.tools[i])
		}
	}
}

// Handle replaces what a tool does, leaving its listing alone.
func (s *Server) Handle(name string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[name] = h
}

// SetPageSize makes tools/list return that many tools per page (0: all).
func (s *Server) SetPageSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSize = n
}

// SetVersion sets the version label of the MCP handshake.
func (s *Server) SetVersion(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = v
}

// OnCrash replaces what a Crash result does (the default exits the process).
func (s *Server) OnCrash(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.crashFn = f
}

// Log returns every request received, oldest first.
func (s *Server) Log() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Entry(nil), s.log...)
}

// ToolCalls counts the tools/call requests for a tool ("" counts all).
func (s *Server) ToolCalls(name string) int {
	n := 0
	for _, e := range s.Log() {
		if e.Method == "tools/call" && (name == "" || e.Tool == name) {
			n++
		}
	}
	return n
}

// Requests counts every request received (HTTP POSTs or stream lines).
func (s *Server) Requests() int { return len(s.Log()) }

// ---- the standard tools -----------------------------------------------------

// appHandler wires fake.App behind a tool.
func (s *Server) appHandler(op adapters.Operation, capability string) Handler {
	return func(ctx context.Context, args json.RawMessage) Result {
		var in struct {
			Identity       adapters.Identity       `json:"identity"`
			Fields         map[string]any          `json:"fields"`
			IdempotencyKey string                  `json:"idempotency_key"`
			Prepared       *actions.PreparedAction `json:"prepared"`
			Aspects        []adapters.Aspect       `json:"aspects"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &in); err != nil {
				return Result{IsError: true, Text: "bad arguments"}
			}
		}
		reply := s.App.Handle(ctx, fake.Request{
			Operation: op, Identity: in.Identity, CapabilityID: capability, Fields: in.Fields,
			Prepared: in.Prepared, IdempotencyKey: in.IdempotencyKey, Aspects: in.Aspects,
		})
		switch reply.Action {
		case fake.Hang:
			return Result{Hang: true}
		case fake.Drop, fake.Refuse:
			return Result{Abort: true}
		}
		return FromBody(reply.Body, reply.EchoedKey)
	}
}

// FromBody turns the body fake.App produced into a tool result: a JSON object is
// structured content (with its JSON mirrored as text); anything else is text
// only. Padding past the object (the oversize fault pads with spaces, which JSON
// encoding would drop) is carried by the text, as a real server's mirror would.
func FromBody(body []byte, echoedKey string) Result {
	r := Result{}
	if echoedKey != "" {
		r.Meta = map[string]any{MetaKey: echoedKey}
	}
	trimmed := bytes.TrimRight(body, " ")
	var obj map[string]any
	if json.Unmarshal(trimmed, &obj) != nil || obj == nil {
		r.Text = string(body)
		return r
	}
	r.Structured = obj
	r.Text = string(trimmed)
	if pad := len(body) - len(trimmed); pad > 1024 {
		r.Text += strings.Repeat("x", pad)
	}
	return r
}

// ---- JSON-RPC ---------------------------------------------------------------

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// outcome is what handling one request produces.
type outcome struct {
	response *rpcResponse // nil: nothing to send (a notification)
	abort    bool
	hang     bool
	preamble []any
}

func (s *Server) record(e Entry) {
	s.mu.Lock()
	s.log = append(s.log, e)
	s.mu.Unlock()
}

func rpcID(id json.RawMessage) string { return string(id) }

// handle processes one decoded request. ctx is cancelled when the client
// withdraws the call or goes away.
func (s *Server) handle(ctx context.Context, req rpcRequest, authorization string) outcome {
	entry := Entry{Method: req.Method, Authorization: authorization}
	respond := func(result any) outcome {
		return outcome{response: &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}}
	}
	fail := func(code int, msg string) outcome {
		return outcome{response: &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: msg}}}
	}
	isNotification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		s.record(entry)
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := "2025-11-25"
		for _, v := range []string{"2025-11-25", "2025-06-18", "2025-03-26"} {
			if p.ProtocolVersion == v {
				version = v
			}
		}
		s.mu.Lock()
		ver := s.version
		s.mu.Unlock()
		return respond(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "fixture", "version": ver},
		})
	case "notifications/initialized":
		s.record(entry)
		return outcome{}
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		_ = json.Unmarshal(req.Params, &p)
		s.mu.Lock()
		if c := s.cancels[rpcID(p.RequestID)]; c != nil {
			c()
		}
		s.mu.Unlock()
		return outcome{}
	case "ping":
		s.record(entry)
		return respond(map[string]any{})
	case "tools/list":
		s.record(entry)
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(req.Params, &p)
		return respond(s.list(p.Cursor))
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(-32602, "bad params")
		}
		entry.Tool, entry.Args = p.Name, p.Arguments
		s.record(entry)
		s.mu.Lock()
		h := s.handlers[p.Name]
		s.mu.Unlock()
		if h == nil {
			return fail(-32602, "unknown tool")
		}
		res := h(ctx, p.Arguments)
		switch {
		case res.Crash:
			s.mu.Lock()
			f := s.crashFn
			s.mu.Unlock()
			f()
			return outcome{abort: true}
		case res.Abort:
			return outcome{abort: true}
		case res.Hang:
			return outcome{hang: true}
		}
		if res.RPCError != nil {
			o := fail(res.RPCError.Code, res.RPCError.Message)
			o.preamble = res.Preamble
			return o
		}
		if res.Raw != nil {
			o := respond(res.Raw)
			o.preamble = res.Preamble
			return o
		}
		out := map[string]any{"content": []any{}}
		if res.Text != "" {
			out["content"] = []any{map[string]any{"type": "text", "text": res.Text}}
		}
		if res.Structured != nil {
			out["structuredContent"] = res.Structured
		}
		if res.IsError {
			out["isError"] = true
		}
		if res.Meta != nil {
			out["_meta"] = res.Meta
		}
		o := respond(out)
		o.preamble = res.Preamble
		return o
	}
	if isNotification {
		return outcome{}
	}
	return fail(-32601, "method not found")
}

// list answers tools/list, with pages when a page size is set.
func (s *Server) list(cursor string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := 0
	if cursor != "" {
		fmt.Sscanf(cursor, "p%d", &start)
	}
	end := len(s.tools)
	next := ""
	if s.pageSize > 0 && start+s.pageSize < end {
		end = start + s.pageSize
		next = fmt.Sprintf("p%d", end)
	}
	if start > end {
		start = end
	}
	tools := make([]any, 0, end-start)
	for _, t := range s.tools[start:end] {
		m := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Input}
		if t.Output != nil {
			m["outputSchema"] = t.Output
		}
		tools = append(tools, m)
	}
	res := map[string]any{"tools": tools}
	if next != "" {
		res["nextCursor"] = next
	}
	return res
}

// Names returns the names of the tools listed now, sorted.
func (s *Server) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, t := range s.tools {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// ---- streamable HTTP --------------------------------------------------------

// maxBody bounds a request body.
const maxBody = 4 << 20

// ServeHTTP implements http.Handler for the endpoint (any path): POST carries one
// JSON-RPC message and is answered with one JSON document; everything else is
// refused.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		http.Error(w, "not a request", http.StatusBadRequest)
		return
	}
	out := s.handle(r.Context(), req, r.Header.Get("Authorization"))
	switch {
	case out.hang:
		<-r.Context().Done()
		return
	case out.abort:
		abortConnection(w)
		return
	case out.response == nil:
		w.WriteHeader(http.StatusAccepted)
		return
	}
	b, _ := json.Marshal(out.response)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// abortConnection closes the connection without a response.
func abortConnection(w http.ResponseWriter) {
	if h, ok := w.(http.Hijacker); ok {
		if conn, _, err := h.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

// ---- streams (stdio) --------------------------------------------------------

// ServeStream serves newline-delimited JSON-RPC on rwc until it is closed. Calls
// run concurrently; a cancellation notification ends the call it names.
func (s *Server) ServeStream(rwc io.ReadWriteCloser) {
	var wmu sync.Mutex
	write := func(v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		defer wmu.Unlock()
		_, _ = rwc.Write(append(b, '\n'))
	}
	defer rwc.Close()
	dec := json.NewDecoder(rwc)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return
		}
		var req rpcRequest
		if json.Unmarshal(raw, &req) != nil {
			continue
		}
		if req.Method == "" {
			// An answer to a request the server made of its client.
			s.record(Entry{Method: "<response>", Args: raw})
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.mu.Lock()
		if len(req.ID) > 0 {
			s.cancels[rpcID(req.ID)] = cancel
		}
		s.mu.Unlock()
		go func() {
			defer cancel()
			out := s.handle(ctx, req, "")
			switch {
			case out.hang:
				<-ctx.Done()
			case out.abort:
				_ = rwc.Close()
			case out.response != nil:
				for _, m := range out.preamble {
					write(m)
				}
				write(out.response)
			}
		}()
	}
}

// RunStdio serves the process's own stdin and stdout, for the subprocess
// fixture, and returns when stdin closes.
func (s *Server) RunStdio() {
	s.ServeStream(stdio{os.Stdin, os.Stdout})
}

type stdio struct {
	io.Reader
	io.Writer
}

func (stdio) Close() error { return os.Stdout.Close() }

// ---- helpers for tests ------------------------------------------------------

// Wait polls cond until it holds or the timeout passes.
func Wait(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
