// Package native is the native SDK transport of master plan 9.3: an
// adapters.AppAdapter that reaches an application through a bridge embedded in it
// (pkg/bridge is the Go one; the protocol below is what a bridge in another
// language implements), over local IPC only. It is a transport and nothing else:
// authorisation, risk, confirmation and idempotency belong to the policy and
// action layers, and the application reauthorises every request.
//
// # Wires
//
// A Unix domain socket (AF_UNIX, which Windows 10 and later has too), or the
// loopback TCP fallback (127.0.0.1, [::1] or localhost, the address a package
// declares and nothing else; the connection goes through an egress.Dialer that
// allows exactly that destination). A Windows named pipe is declared by the
// manifest schema and is not supported by this build. A connection serves one
// call: the adapter dials, authenticates, sends one request, reads one reply and
// closes. Nothing is pooled, so a failure never leaves a stale connection to guess
// about, and a Commit is never sent twice.
//
// # Frames
//
// A frame is a 4-byte big-endian length N and N bytes holding exactly one JSON-RPC
// 2.0 message (UTF-8, no unknown members), followed after the handshake by its
// 32-byte seal (below), which N counts. N is checked against the limit before
// any of the body is read: 0 and anything above the limit end the connection, so a
// hostile length costs four bytes. The limits, which are on the message and not the seal, are
// 4 KiB for the handshake, 1 MiB for a request (Config.MaxRequestBytes) and
// Config.MaxResponseBytes plus 4 KiB of envelope for a reply.
//
// # Peer authentication (SEC-038)
//
// Both ends prove they hold the application key, a secret of at least 32 bytes
// that the operator gives the bridge and the adapter out of band (the adapter
// configuration of a package holds only a reference to it: env:NAME or
// file:/absolute/path, owner-only on POSIX). The key never crosses the wire. A
// proof is HMAC-SHA256 over a role label and the transcript (protocol version,
// application ID, endpoint ID, key ID, the adapter's nonce and the bridge's
// nonce), each part length-prefixed, so a proof cannot be reflected to the other
// role, replayed into another session or moved to another application or
// endpoint. The HMAC input is the label, a zero byte, then each transcript part as
// a 4-byte big-endian length and the bytes: the version as decimal text, the
// application ID, the endpoint ID and the key ID as UTF-8, and the two nonces as
// the raw 32 bytes.
//
// The proofs authenticate the two ends; the seal binds everything after them to
// that session. The session key is HMAC-SHA256(key, "alixai-native/1 session" |
// transcript), and every frame after the handshake, the bridge's confirmation of
// the auth proof included, is the payload followed by the 32-byte tag
// HMAC-SHA256(session key, direction label | 0x00 | payload), with the labels
// "alixai-native/1 request" (adapter to bridge) and "alixai-native/1 reply"; the
// frame length counts the tag, and the tag is checked before any JSON is read. A
// process that holds no key and relays the handshake between the two ends (another
// user on a loopback port the package declares, say) can therefore not rewrite the
// tenant of a request or the content of a reply, and neither end accepts a frame it
// altered: the bridge drops the connection without running a handler, and the
// adapter reports schema_invalid (protocol_violation), uncertain_outcome for a
// Commit. Frames are authenticated, not encrypted: on a wire that file permissions
// do not protect (loopback TCP), another local process that can read the traffic
// can read the fields and results; it cannot alter or forge them.
//
//	adapter -> bridge  {"method":"session.hello","params":{"version":1,"application_id":...,
//	                    "endpoint_id":...,"key_id":...,"nonce":<32 bytes, base64url>}}
//	bridge  -> adapter {"result":{"version":1,"nonce":<32 bytes>,"proof":HMAC(key, "alixai-native/1 server" | transcript)}}
//	adapter -> bridge  {"method":"session.auth","params":{"proof":HMAC(key, "alixai-native/1 client" | transcript)}}
//	bridge  -> adapter {"result":{"authenticated":true}}   (the first sealed frame)
//
// The bridge proves itself first, so the adapter sends nothing of a call (no
// identity, no field, no prepared action) to a process that does not hold the key;
// a proof that does not verify, a refusal and a malformed handshake are all the
// same unavailable error (peer_authentication_failed) from which nothing about the
// key is learned, also for a Commit, because nothing was sent. The bridge answers
// every refusal with the same error (-32001) and serves nothing before the
// handshake is complete.
//
// Where the operating system reports the account of the process at the other end
// of a Unix socket (SO_PEERCRED, Linux), Endpoint.PeerCheck sees it and can refuse
// a bridge that runs as another user (the adapter configuration's peer_uid). It is
// a second control and never a replacement for the key.
//
// # The socket (SEC-081)
//
// The bridge creates its socket owner-only (0600) in a directory that is not
// writable by group or others; the adapter checks both before it connects and
// refuses a socket that is not (socket_permissions), because another user could
// then have connected to it or replaced it. Windows file modes carry no such bits,
// so nothing is checked there and the key is the control.
//
// # The calls
//
// After the handshake the adapter sends one request, with the minimum identity
// (the six members of adapters.Identity and nothing else: no deployment ID, no
// capability scopes, no credential) and the call's own members:
//
//	<native_method> read, validate, prepare   {"identity": {...}, "fields": {...}}
//	<native_method> commit                    {"identity": {...}, "idempotency_key": "...", "prepared": {PreparedAction}}
//	<native_method> status, cancel            {"identity": {...}, "idempotency_key": "..."}
//	alixai.describe                           {}
//	alixai.health                             {}
//	alixai.inspect                            {"identity": {...}, "aspects": [...]}   (aspects omitted: all)
//	alixai.manage                             {"name": "...", "args": {...}}
//
// The method of a capability operation is the native_method of the package, a name
// matching ^[A-Za-z_][A-Za-z0-9_.-]{0,127}$ that does not start with session. or
// alixai. (those are the protocol's). The reply is the host's members of a
// DeterministicResult (status, identifiers, values, errors, allowed_next_actions,
// freshness and, for a prepare, the preparation); operation and capability_id are
// the core's and the host does not state them. A reply to commit, status or cancel
// may carry {"meta":{"idempotency_key":...}} beside "result", which must be the key
// of the call. A host refusal is a result with status rejected, never an error.
//
// alixai.describe answers {"contract_versions":[1],"application_version":"...",
// "methods":["every method the host declares"]}. alixai.health answers
// {"status":"ok"|"degraded"|"unavailable","reason":...}. alixai.inspect answers
// {"reports":[...]}. A bridge that does not declare one of them answers -32601 (the
// adapter then treats the aspect as unsupported, which the audit records as
// unverified). alixai.manage reaches only the management operations the host
// declared and the adapter configuration approves (management.provisioning): the
// integration audit's create_tenant, seed and cleanup, which no capability maps.
//
// # Validate and the version Prepare stored
//
// A host must make Validate, on unchanged data, report in freshness.version the
// version that Prepare reported: the snapshot version of the records the action
// relies on, not a per-response ETag and not a tenant-wide counter. A host that
// breaks this makes every commit stale or invalidates every open preparation. The
// adapter passes the version through; the integration audit's probes check it.
//
// # What is reachable
//
// Config maps capability operations to methods, as the package declares them. Those
// methods, the protocol's own and the approved provisioning operations are the only
// ones the adapter ever calls. alixai.describe lists what the host declares; one
// that no capability maps is reported to the audit in Description.Unmapped (by name
// when it is a valid name, by length and a hash prefix otherwise) and is not
// callable by any request. Describe's capabilities are the package's mapping, never
// the host's claim.
//
// # Failures
//
// The adapter never retries.
//
//	nothing was sent (socket refused, connection refused, egress refusal,
//	handshake failed, request too large)         unavailable, validation_failed; also for Commit
//	the method is not declared by the bridge     unknown_capability (operation_not_offered); nothing ran
//	the caller's deadline or cancellation        the context's own error
//	a reply too large, empty, not JSON-RPC,
//	not a DeterministicResult                    schema_invalid; Commit: uncertain_outcome
//	possibly delivered (reset, EOF, host error)  Commit: uncertain_outcome; others: unavailable
//
// Errors carry codes and fixed reasons, never a path, a key, a proof or text the
// host chose.
//
// # Identity seam
//
// PeerAuthenticator is the identity.Authenticator of mutually authenticated local
// IPC (ADR 0013): it turns the context an authenticated peer asserts into a Scope
// through identity.NewScope, the constructor the context-token verifier uses, so
// every transport yields the same Scope. Nothing serves it yet: a gateway listener
// for hosts that call AlixAI over IPC is a later task.
package native
