// Package credentials is the local credential service of the external provider mode (additive plan
// section 11.1): the only place a provider key or token lives. Clients, host applications, prompts and
// models never receive a reusable provider credential; the host sees a connection status, a masked
// fingerprint and a safe account identifier, nothing more.
//
// # What it keeps, and where
//
// A credential reference is an opaque ID ("cred_" and a ULID) with a typed record
// (schemas/providers/credential-ref, providers.CredentialRef) in SQLite: kind (oauth or api_key), label,
// the binding to one provider, deployment, application and tenant plus the permitted provider scopes, the
// status, times, warnings. That record has no member that can hold a secret. The value itself is in a
// Backend, chosen when the reference is made and stored with it:
//
//   - BackendOSStore: the operating system's secret store (Windows Credential Manager, macOS Keychain, Linux
//     Secret Service) behind the OSStore interface. The implementation over go-keyring is the subpackage
//     oskeyring, imported by a composition root; this package links none of it, and the tests run on
//     MemoryOSStore.
//   - BackendEncryptedFile: one JSON file, every value sealed with AES-256-GCM (authenticated encryption, fresh
//     random nonce per write, the slot and the binding as additional authenticated data). The 32-byte master
//     key comes from the OS store (OSStoreKeySource) or from a key file that only the owner can read
//     (KeyFileSource). It is always available, so a headless host without an OS store still works.
//   - BackendEnv and BackendFile: read-only references for headless installs and mounted secrets. The value is
//     read at every use, so replacing the variable or file rotates the credential. Names are confined (the
//     ALIXAI_SECRET_ prefix; a plain file name inside one directory, no links, owner-only) and only an operator's
//     composition root registers them (AddExternalRef).
//
// Nothing is cached: every Resolve reads the record and the backend again, so a rotation or a revocation is
// effective on the next call with no restart of any host application.
//
// # Using a credential
//
// Resolve (or a Resolver bound to one scope and provider, which adapters adopt through AsSecretProvider) turns the
// opaque reference into the secret for the duration of the call. It checks, in order: the scope, the deployment, the
// reference (the same refusal for "no such reference" and "another tenant's", so existence does not leak), the
// provider, the requested provider scopes against the permitted ones, the status (active only; expiry is judged by
// the injected clock), the backend, and that the use could be recorded in the audit log: a use that cannot be audited
// releases nothing. Every use, allowed or denied, is an audit event (credentials.use) with the reference, provider,
// purpose and outcome and no value. An invalid, revoked, expired or quarantined credential fails closed with a
// typed error; this package has no API that picks another reference, and the provider registry never selects an
// alternate provider on invalid credentials.
//
// # Manual API keys
//
// AddAPIKey is the fallback when OAuth is unavailable. The injected KeyValidator makes the minimally scoped provider
// request and nothing is stored if the provider does not accept the key. Afterwards only a masked fingerprint
// ("sha256:" and 8 hex digits of the key's SHA-256) is shown; the record always warns that a manual key may grant broader
// access than OAuth scopes, and a provider report of excessive permissions or an unsupported account policy stores the
// reference quarantined: it is never released. Rotate, Revoke and RevokeTenant implement rotation and immediate
// revocation; AddOAuth and Rotate are what internal/oauth uses to store and rotate tokens. AddOAuthIssuedKey stores
// a provider-issued API key from browser login (kind oauth_issued_key: fingerprint like a manual key, no
// broader_than_oauth_scopes warning, no refresh).
//
// # Secrets in text
//
// Secret redacts itself under every formatting verb, slog and JSON. Every exported field that can carry a key has that
// type. Errors carry only a fixed message and a Reason code: the text of a backend, an operating system, a validator or a
// database error is never passed on, because it can echo a value. Audit and log keys avoid the substrings the redactors
// blank (token, secret, credential), so the trail names the reference.
//
// # Dependencies
//
// Authenticated encryption is the standard library's AES-256-GCM: authenticated, hardware accelerated and no new module
// (golang.org/x/crypto's chacha20poly1305 was considered; a master key seals a few hundred records at most, far below
// GCM's 2^32 random-nonce limit). The one added module is github.com/zalando/go-keyring (MIT) with wincred (MIT) and
// godbus (BSD-2-Clause), all on the ADR-0008 allowlist, pure Go and built with CGO_ENABLED=0 for windows, darwin and linux.
package credentials
