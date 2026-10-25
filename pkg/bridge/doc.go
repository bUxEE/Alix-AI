// Package bridge is the Go library a host application embeds to expose its
// operations to AlixAI's native adapter (master plan 9.3, adapters/native). It
// imports only pkg/contracts and the standard library, so a host depends on it
// without pulling in the service.
//
//	srv, _ := bridge.New(bridge.Config{
//	    ApplicationID: "ledger",
//	    Keys:          map[string][]byte{"k1": applicationKey}, // 32+ random bytes, shared with AlixAI
//	})
//	srv.Handle("invoices_find", func(ctx context.Context, c bridge.Call) (bridge.Reply, error) {
//	    // reauthorise c.Identity, then answer with the host's members of a DeterministicResult
//	    return bridge.Reply{Result: result}, nil
//	})
//	l, _ := bridge.ListenUnix("/run/ledger/alixai.sock") // owner-only; or bridge.ListenLoopback(7001)
//	go srv.Serve(l)
//
// The bridge serves exactly what the host declared with Handle (and
// HandleManagement), to a peer that proved it holds the application key. It
// authenticates AlixAI, not the principal: the handler reauthorises every request
// from the identity it receives, as master plan 10.1 requires. The wire protocol is
// specified in the documentation of adapters/native. Every frame after the
// handshake is sealed under a session key, so a process that holds no key and
// relays the connection can neither change the identity of a request nor forge a
// reply; frames are authenticated, not encrypted, so keep the socket owner-only
// (ListenUnix does) where reading matters.
//
// Method names are the native_method names of the application package; the
// prefixes session. and alixai. are the protocol's and cannot be declared. A
// handler's error is reported to AlixAI without its text; return a result with
// status rejected for a refusal, never an error. A handler's context ends when the
// caller gives up and when the Server closes.
package bridge
