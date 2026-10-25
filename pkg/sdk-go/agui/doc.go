// Package agui is the AG-UI face of the Go SDK (master plan 15.6, ADR 0035, docs/integration/agui.md): the AG-UI event
// types of the pinned release (@ag-ui/core 1.0.1, protocol 1.0), an SSE encoder and decoder, and a net/http proxy that lets
// a host backend serve a CopilotKit or AG-UI frontend from its own origin.
//
// The package depends on the standard library and on package sdkgo (the context-token callback type); it does not import
// CopilotKit or any AG-UI library, and nothing in it contacts anything but the AlixAI service the host names.
//
//	proxy, err := agui.NewProxy(agui.Options{
//		BaseURL: "http://127.0.0.1:8080",
//		GetContextToken: func(ctx context.Context, req sdkgo.TokenRequest) (string, error) {
//			// authenticate the host's own user from ctx, check that they own req.SessionID (the thread),
//			// and mint a fresh single-use token whose session is that thread
//		},
//	})
//	mux.Handle("/agui/", http.StripPrefix("/agui", proxy.Handler())) // POST /agui/ runs; see Proxy.Handler for the rest
//
// # What the proxy does
//
// The browser (or CopilotKit's runtime) sends an AG-UI RunAgentInput to the host. The proxy validates the thread and run
// ids, asks the host's callback for a fresh single-use context token (session = thread), forwards the body unchanged to
// POST /v1/agui and streams the answer back frame by frame. It never buffers more than one frame, never keeps one after it
// was written and never logs a body, a token or a secret.
//
// # The confirmation secret
//
// A run that prepares an action ends in an interrupt, and the gateway sends the single-use confirmation secret in the
// X-AlixAI-Confirmation-Token response header. The proxy keeps it server-side, in a SecretStore, under (thread,
// preparation id): it is stored before the RUN_FINISHED frame that carries the interrupt is written, so the resume that
// follows always finds it. It is attached, as the same request header, only to a resume that explicitly approves that
// preparation ({"approved": true}); a reject or a cancelled resume never carries it and discards it; an approval for which
// no secret is stored goes without one and gets the gateway's 428. The browser never holds it: response headers of the
// upstream are copied by allowlist, request headers of the browser are never copied, and a frame that contains the secret
// is not forwarded. Run, retry, reconnect, cancel and capabilities are each forwarded with a token minted for that one request.
package agui
