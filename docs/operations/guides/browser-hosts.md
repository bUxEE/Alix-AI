# Guide: browser hosts

Status: written in task M5-13, 2026-10-03. ADR [0013](../../decisions/0013-application-context-token.md),
[0035](../../decisions/0035-copilotkit-agui-compatibility.md); SEC-092.

For the operator, and for the developer of a **host application** that has a browser front end. Read it before you open a listener to a browser origin. The short version: let the
host's own backend call AlixAI. A direct browser call is possible, and this page says exactly what it costs.

## 1. Why AlixAI is not called from a page by default

- **Tokens are single use and minted with a private key.** Every request, and every event-stream open, needs a fresh context token, and a second presentation is `replayed`. The token is
  signed by the host application's key, which must never reach a browser (`sdk/typescript/README.md`: "signing is your backend's job, never the browser's"). So even a browser that talks to
  AlixAI directly needs a round trip to the host's backend for every call.
- **The confirmation secret.** When a turn prepares a consequential action, the secret that proves a confirmation was issued for that preparation, principal, session and data versions travels
  in the `X-AlixAI-Confirmation-Token` response header of the stream. ADR 0035 keeps it out of the browser: the SDK agent in the host **backend** holds it per thread and attaches it to the resume.
- **The gateway sends no CORS headers, by design** (`internal/gateway/doc.go`: "No CORS headers are ever sent"; SEC-092 in `docs/security/test-backlog.md`: no response carries a wildcard
  `Access-Control-Allow-Origin` by default). A browser's preflight
  `OPTIONS` request is answered `405 Method Not Allowed` with no CORS headers; this was observed in M5-13 against `alixai serve`. There is no setting that turns CORS on, and no
  allowed-origins list in AlixAI.
- A browser `EventSource` cannot set the `Authorization` header, and a browser WebSocket cannot either, so the SDK reads streams with `fetch` and opens a WebSocket only through a factory you supply
  (`sdk/typescript/README.md`).

## 2. Recommended: the host's backend calls AlixAI, or proxies it

The browser talks only to your own origin. Your backend authenticates the user, mints the context token server-side for each call, calls AlixAI (with `@alixai/sdk`, the Go SDK or the Python SDK,
none of which depends on CopilotKit), and returns the result or relays the stream. Nothing about AlixAI is exposed to the browser: no AlixAI credential, no AlixAI origin, no CORS to configure.

| Pattern | The browser sees | Notes |
| --- | --- | --- |
| **Backend calls AlixAI** | your API only | Your backend uses the SDK; `getContextToken` runs in your backend, once per request. The confirmation secret stays in your backend (the SDK documents the obligation: keep it per thread and preparation, delete it on use, never put it in an event, state or log). |
| **Backend proxies AlixAI** | your origin, with AlixAI's paths behind it | Your backend (or an edge proxy you run) authenticates the user, attaches a freshly minted token to each forwarded request and forwards. It must not forward the browser's own `Authorization` header as if it were a context token. |
| **CopilotKit** | CopilotKit's runtime route on your origin | `@alixai/copilotkit-adapter` runs `AlixAIAgent` **in your backend**, inside CopilotKit's own runtime, against `POST /v1/agui`. Nothing needs CopilotKit Cloud, and the adapter sets the switches that keep CopilotKit's telemetry off. See below. |

### CopilotKit and AG-UI

AlixAI implements the AG-UI protocol as a second face of its streaming API, so CopilotKit's chat, sidebar, popup and headless components can use it (ADR 0035, `docs/integration/agui.md`). The
mapping lives in `internal/agui`; every event it emits is validated against schemas vendored from the pinned AG-UI release, **`@ag-ui/core` 1.0.1, protocol 1.0**. AlixAI stays the authority:
client-supplied tools, state, context and messages are untrusted data, never capabilities, and approval goes through interrupts and resume. The tested CopilotKit versions are in
`sdk/copilotkit-adapter/compat/matrix.json` (1.76.0 primary, 1.77.0), published with the feature matrix and its known limits in
[`docs/integration/copilotkit-compatibility.md`](../../integration/copilotkit-compatibility.md) (the gate is `tests/copilotkit`), and `GET /v1/agui/capabilities` states what a deployment supports.
The route in your backend, the confirmation card and the telemetry switches are in `sdk/copilotkit-adapter/README.md`. This pattern needs **no CORS configuration**, because the browser talks to your
CopilotKit route on your own origin.

**Authenticate the whole CopilotKit runtime route yourself.** The gate found that CopilotKit's runtime answers its own `connect`, `info` and `stop` routes without asking your token callback, so whoever
can reach the route and names a thread can read what that process recorded of it, including the confirmation card's effects. Runs and resumes are protected (the callback is asked for each and the gateway
checks the thread); put your own authentication in front of the whole route and treat the thread id as a secret (`docs/integration/copilotkit-compatibility.md`, "Limits the gate found", limit 1).

## 3. A browser that calls AlixAI directly from another origin

Not recommended. If your architecture insists, the browser can only read what AlixAI sends if the operator makes **both** of these true at the point where TLS ends (the reverse proxy of the
[private VPS guide](private-vps.md); AlixAI itself cannot do either):

1. **Allowed origins, configured by the operator.** The proxy answers the preflight itself (AlixAI would answer 405) and, for a request from an origin on your list, adds on the real response:
   `Access-Control-Allow-Origin` set to **that one origin**, never `*`, with `Vary: Origin`. The preflight allows the methods your calls use and these request headers, the ones the TypeScript SDK sends
   (`sdk/typescript/src/transport.ts`): `Authorization`, `Content-Type`, `Last-Event-ID`. The SDK sends no cookies (`credentials: "omit"`), so `Access-Control-Allow-Credentials` is neither needed nor
   wanted.
2. **Exposed response headers.** The proxy adds `Access-Control-Expose-Headers: X-AlixAI-Confirmation-Token, X-Request-ID` to the responses. A browser hides every response header that is not
   CORS-safelisted from script unless it is exposed; **without this, the SDK cannot read the confirmation secret from the stream header** (and cannot read the request id of an error). The
   TypeScript SDK says the same: "In a browser on another origin, the service must expose the header" (`sdk/typescript/README.md`). If you use file previews, `X-Alixai-Preview-Truncated` needs the same treatment.

Both are required: the first without the second lets the page call AlixAI and then fail to confirm anything.

What you accept by doing this:

- The page's scripts hold the context token for the length of a call **and the confirmation secret**, which ADR 0035 deliberately keeps away from the browser. Anything that can run script in your
  page, such as a cross-site scripting hole in your application, can then present what the user was shown as confirmed. Keep the page's own defences (a strict content security policy) in
  proportion.
- Your backend still has to mint a token per call, so the browser makes a request to your backend for each AlixAI request. Direct calls therefore save the data hop, not the round trip.
- The operator owns the list of origins. A wildcard, or an origin reflected from the request without a check, turns every site on the internet into a client of your deployment.

**Not run.** No task configured a proxy for CORS against AlixAI. The header names above come from the SDK, the OpenAPI fragments (`schemas/api/paths/sessions.yaml`, `agui.yaml`) and the
gateway; test the preflight and a real confirmation from a real second origin before you rely on it.
