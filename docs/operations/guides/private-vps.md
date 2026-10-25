# Guide: private VPS

Status: written in task M5-13, 2026-10-03.
Read the [guides index](README.md) first for the legend (**Run**, **Static**, **Not run**) and the known gaps.

A rented machine that the internet can reach, running AlixAI for your own applications. Everything in the [server guide](server.md) applies; do it first, with AlixAI on
loopback, and confirm `/healthz` answers. This guide adds what exposure needs: a **TLS listener**, a **firewall**, a **service identity** and an **explicit listener
configuration**. AlixAI is self-hosted and needs no AlixAI-operated service, so nothing here involves one.

No command in this guide configures a certificate authority, a reverse proxy or a firewall, because no earlier task ran one and the rules of this repository keep tasks out of
host-wide changes. Where the guide says what the proxy or the firewall must do, treat it as a requirement to implement and test with your own tools.

## 1. What exposure changes

| Surface | On a VPS |
| --- | --- |
| `/healthz`, `/readyz` | Anonymous. They reveal liveness and the status of the model roles, nothing more. Fine to expose, or to leave to the proxy to restrict. |
| `/v1/...` | Needs a single-use context token minted by the host application's **backend** and verified against an issuer key you registered. Without the host's private key nobody can mint one. Every other failure is the typed error envelope; nothing from an internal cause is sent (SEC-083). |
| LocalAI, the metrics listener, the admin interface | Never reachable from outside, and not made so by any setting: LocalAI has no published port, metrics refuse any non-loopback address, the admin interface binds loopback with no TLS exception (SEC-093). |
| CORS | The gateway sends **no CORS headers**, by design (SEC-092). A browser calling AlixAI from another origin is a separate decision: [browser hosts](browser-hosts.md). |

What exposure does not remove: the operating system. Whoever can log in as a user that reads the data directory is the operator ([operator commands](operator-commands.md), section 1).
Administer the machine with key-based SSH from addresses you name, keep it patched, and use a dedicated non-root administrator. None of that is AlixAI's to enforce.

## 2. Choose where TLS terminates

| | A. Reverse proxy on the host (recommended) | B. AlixAI's own TLS |
| --- | --- | --- |
| Works with | every shape; **the only option for the container stack** | the native service only |
| AlixAI listens on | `127.0.0.1:7470`, unchanged | a non-loopback address such as `0.0.0.0:7443`, with `gateway.tls` |
| Certificates | the proxy's tooling obtains and renews them | you supply a PEM chain and key; renewing means a restart |
| Hop between proxy and AlixAI | plaintext on the loopback of the same machine | none |
| Client certificates (mTLS) | the proxy's | `gateway.tls.client_ca_file` |

Choose A unless you want AlixAI to hold the certificate itself. In the container stack AlixAI listens on loopback inside its own network namespace, so B does not apply
there, and `TestPortsLoopbackOnly` refuses a published port that is not `127.0.0.1`.

### A. A reverse proxy on the host

The proxy is yours, with its own service identity (`deploy/container/stack/README.md`, "Reaching the API"). It must:

- terminate TLS 1.2 or later for your name and forward **only** to `127.0.0.1:7470` (in the container stack, the published `127.0.0.1:${ALIXAI_PORT:-7470}`); AlixAI will see the proxy as
  the peer address of every request;
- **not buffer or cut off streaming responses.** A turn answers as server-sent events (`/v1/sessions/...`, `/v1/agui`), and `/v1/agui` also speaks WebSocket, which needs the
  upgrade passed through;
- pass the headers the API uses unchanged: `Authorization`, `Content-Type`, `Last-Event-ID` and the request and response header `X-AlixAI-Confirmation-Token`, and let `X-Request-ID` and
  `Cache-Control: no-store` through on the way back;
- not set a body limit below what you allow AlixAI (`gateway.max_body_bytes`, 64 KiB by default; file uploads have their own limits);
- rate-limit by client address. at the time of writing AlixAI has no per-tenant request quota (ADR 0018 is provisional and task M5-06 has not landed), and it sees only the proxy's address;
- never add `Access-Control-Allow-Origin: *`. If a browser must call AlixAI directly, follow [browser hosts](browser-hosts.md) exactly.

**Not run.** No task configured a proxy against AlixAI; test the streaming and the headers end to end with a real turn before you rely on it.

### B. AlixAI's own TLS (native service)

```yaml
server:
  listen: 0.0.0.0:7443          # or one interface's address; never an empty host unless you mean every interface
gateway:
  tls:
    cert_file: /etc/alixai/tls/fullchain.pem     # server certificate chain, PEM
    key_file: /etc/alixai/tls/privkey.pem        # its private key, PEM
    # client_ca_file: /etc/alixai/tls/clients-ca.pem   # optional: every client must present a certificate this CA signed
```

**Run:** `alixai config check` accepts this block (M5-13), and the listener rules are tested: `TestListenerRefusesNonLoopbackWithoutTLS` and `TestServeTLS`
(`internal/gateway/server_test.go`), `TestSecServeRefusesNonLoopbackWithoutTLS` (`internal/cli/cmd_serve_test.go`). In M5-13, `alixai serve` with this block and no certificate files exited
with `gateway: invalid TLS configuration: gateway: tls certificate: open ...`, and with a non-loopback address and no TLS block exited with `gateway: refusing to listen on 0.0.0.0:7443: a
listener beyond loopback needs tls.cert_file and tls.key_file`. **Not run:** a real certificate, a real client over the internet.

What `serve` does with it (`internal/gateway/listen.go`):

- `cert_file` and `key_file` go together, `client_ca_file` needs both, and the files are read when the server is built, so a missing or unreadable file stops the start, and **a renewed
  certificate takes effect at the next restart**.
- TLS 1.2 is the floor, with Go's default AEAD cipher suites. With `client_ca_file`, every client, health probes included, must present a certificate that CA signed.
- The decision is made before a port is bound and repeated on the bound address. A non-loopback listener without a certificate is refused, and **`dev.spike_endpoint` is refused on any
  non-loopback listener even with TLS** (SEC-092): leave it off.
- Keep the key file readable by the service identity only. Under the systemd unit, supply it the way the unit supplies the LocalAI key, through `LoadCredential=` and a path under
  `/run/credentials/alixai.service/`; the unit as shipped does not do this for TLS, so that is a drop-in you write and test.
- The shipped unit allows loopback traffic only (`IPAddressDeny=any`, `IPAddressAllow=localhost`) and grants no capability. **As shipped it drops packets from other hosts, and, with Linux's default
  unprivileged-port range, it cannot bind a port below 1024.** Use a high port and widen `IPAddressAllow=` in a drop-in (`systemctl edit alixai`, the mechanism `deploy/native/README.md` names for outbound access). Both follow from the
  unit file; no task ran the unit on a Linux host.

## 3. Firewall

Requirements, not commands. Apply them in your provider's firewall and, if you run one, the host's.

| Direction | Allow | Everything else |
| --- | --- | --- |
| Inbound | TCP to the TLS port (443 for the proxy, or the high port of option B), and SSH from your administration addresses | deny |
| Inbound, never | `7470` as the plaintext API port (it is bound to loopback; with option B the TLS port replaces it), LocalAI's `8080`, the metrics port `9464`, the admin interface's random port | these are loopback by design; a rule that opens them is a mistake |
| Outbound | what the **host** needs: operating-system updates, certificate renewal, your own backup transfer | AlixAI in Local mode needs nothing outbound |

In the container stack the networks the services share are `internal: true`, so even a mistaken host rule cannot give LocalAI a route out. External provider mode is the one thing that
opens an outbound path, and only on purpose (`compose.external.yaml`, `deploy/container/quadlet/external`). If you enable it, add host rules for the provider addresses as well:
compose and Podman cannot filter by host name, so the name allow-list is enforced by AlixAI itself (`internal/egress`), and a firewall rule is the second line
(`deploy/container/stack/README.md`, "External provider mode").

## 4. Service identity

- **Operating-system identity.** The service runs as its own non-root identity per shape ([server guide](server.md), section 4), and LocalAI as another. Do not run either as root, and do not
  widen the unit's capability set.
- **Caller identity.** A host application proves who it is to AlixAI with its context tokens, verified against the issuer key you registered for it (`alixai identity add-issuer-key`). The
  token is single use, carries the application, tenant, workspace, principal and session, and lives at most `identity.max_token_lifetime` (15 minutes at most). Rotate and revoke issuer keys
  as in [key provisioning](../key-provisioning.md).
- **Optional transport identity.** With option B you can additionally require a client certificate (`client_ca_file`), so only your hosts' backends can open a connection at all. With option A
  that is the proxy's job.
- **Operator identity.** There is no remote administration API. Operator commands run on the machine, as the identity that may open the data directory, over SSH. See
  [operator commands](operator-commands.md), section 1, and the open point about packaged installs in the [server guide](server.md), section 7.

## 5. Connect your application

Your application's **backend** calls AlixAI: it mints the single-use token for each call server-side and sends it as `Authorization: Bearer ...`. A browser should talk to your backend, not to
AlixAI; [browser hosts](browser-hosts.md) says why and what a direct browser call needs.

## 6. Backups and keys

A VPS disk is not a backup. `alixai backup create` writes a signed backup while the service runs; copy the backup directory **and** `backup.pub` to storage you control off the machine, and
keep the checkpoint key and the release key offline ([backup and restore](backup-restore.md)). Remote backup is outside the core and needs a provider you choose.

## 7. Check it from outside

- From the VPS: the `/healthz` probe of the server guide on `127.0.0.1:7470` (option A) or the TLS address (option B).
- From another network: the same `/healthz` probe against your public name over HTTPS answers 200, and a plain-HTTP request to that name or to the TLS port returns no API content.
- From another network: `8080`, `9464` and the loopback-only `7470` accept no connection.
- A request to a `/v1` route without a token, or with one that was already used, is refused with the typed error envelope; a reused token answers `replayed`.
- `alixai identity list` shows no `TEST-ONLY` key, and `alixai serve` logs no warning that names one.
