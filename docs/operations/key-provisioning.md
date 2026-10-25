# Key provisioning

Two kinds of keys have to be provisioned by the operator before an `alixai serve` deployment is
useful. Neither is created implicitly, and `serve` never makes one up.

| Key | What it does | Who holds the private half | Where the public half lives |
| --- | --- | --- | --- |
| **Issuer key** (one or more per host application) | Verifies the context tokens a host application mints for its users ([ADR 0013](../decisions/0013-application-context-token.md)). | The host application. AlixAI holds only the public key and never stores or sees anything that can mint a token. | `identity_issuer_keys` in the SQLite store, module `identity`. |
| **Platform checkpoint signing key** (one per deployment) | Signs the audit chain-head checkpoints that make tail truncation and chain rewrites detectable (SEC-086). | This machine: a file under the data directory, readable by the service identity only. | `trust_publisher_keys` in the same store, under the application `alixai.platform`. |

Security backlog ids: SEC-031 and SEC-032 (token forgery and
substitution), SEC-037 (issuer key compromise and rotation), SEC-081 (key files owner-only),
SEC-086 (audit tail truncation).

## 1. Context-token issuer keys

### 1.1 What the verifier needs

A context token is a JWS signed by the host application with Ed25519; the header names a key id
(`kid`), and AlixAI looks the key up by that id alone before it reads anything else in the token.
So an issuer key has four parts:

- a **key id**, unique across the whole deployment (the same id cannot be used by two applications,
  and an id is never reused, not even after revocation);
- the **application** it speaks for: a token signed by the key can only ever carry that application;
- the **issuer**: the exact `iss` the tokens carry;
- the **public key**: the raw 32-byte Ed25519 key, in standard base64.

The key id and the issuer must be the ones the application's package declares in the
`context_exchange` block of its manifest
([`docs/integration/application-package.md`](../integration/application-package.md), section
"Context exchange"):

```yaml
context_exchange:
  mode: context_token
  issuer: https://library.example.test
  audience: alixai
  key_ids: [demo-library-ctx-2026]
```

`alixai identity add-issuer-key` reads that block from the package and refuses a key id that is not
in `key_ids` or an issuer that is not `issuer`. The `audience` is not a key property: tokens must
carry the audience this deployment requires, which is `identity.audience` or, when that is empty,
`identity.deployment_id`. The command warns when the package's audience differs, because every token
the host mints would then be refused with `wrong_audience`.

### 1.2 Provisioning, step by step

1. **Configure the deployment identity.** Without a `deployment_id` the `/v1` API refuses every
   request.

   ```yaml
   identity:
     deployment_id: dep_main      # the deployment level of every scope; never taken from a token
     audience: alixai             # what tokens must carry as aud; empty means deployment_id
     max_token_lifetime: 15m      # never above 15m
   ```

2. **The host generates its key pair** with its own key management and keeps the private half.
   Any Ed25519 key works. For development, `alixai package keygen -key-id demo-library-ctx-2026 -out
   ./keys` writes `ID.key` (private, mode 0600) and `ID.pub`; the `.pub` file is exactly the format
   `-public-key-file` takes. A production host exports the raw public key from its KMS; with OpenSSL:

   ```sh
   openssl pkey -in host-ed25519.pem -pubout -outform DER | tail -c 32 | base64
   ```

3. **Register the public key**, naming the package the key is declared in:

   ```sh
   alixai identity add-issuer-key -config alixai.yaml \
     -package ./demo-library-package \
     -key-id demo-library-ctx-2026 \
     -public-key-file ./keys/demo-library-ctx-2026.pub \
     -label "primary signing key"
   ```

   The package is named on the command line, not looked up among the installed packages, on purpose:
   registering a package through `POST /v1/applications` needs a context token, and a token needs an
   issuer key, so the first key has to be added before anything is installed. The package must load
   with no critical finding (`alixai package validate`); its signature is checked against its own
   files, and who may publish it is decided where it is installed, not here. `-app` and `-issuer`
   default to the package's values and may only repeat them. `-not-after 2027-01-01T00:00:00Z` makes
   a key stop verifying at that instant, which bounds a rotation overlap.

   The key is stored in one transaction with its audit event (`identity.issuer_key_added`, on the
   application's chain, carrying the id, issuer and fingerprint, never key bytes). If the audit log
   cannot take the event the key is not added.

4. **Check it.**

   ```sh
   alixai identity list -config alixai.yaml            # or -app demo-library, or -json
   ```

   and restart nothing: `alixai serve` reads the table on every token, so a key added or revoked
   from another process applies to the very next request. At startup `serve` logs how many issuer
   keys are active and warns when there are none.

### 1.3 Rotation and revocation

- **Rotation.** Declare the new key id in the package's `key_ids` (a new package version), add the
  new key with `add-issuer-key` while the old one is still active, let the host switch, then revoke
  the old one. Give the old key a `-not-after` if the overlap should end by itself.
- **Revocation.** `alixai identity revoke-issuer-key -app demo-library -key-id demo-library-ctx-2026
  -reason "key compromised"` makes tokens signed by the key fail from the next request on, even a
  token minted a moment earlier that has not been used yet. The row is kept as the record; the key
  id and the key material can never be registered again. The revocation is audited
  (`identity.issuer_key_revoked`, with the reason). A key is revoked as the application it speaks
  for: naming another application finds nothing.
- Every context token is single use. After a revocation the host has to be given a new key before it
  can mint anything that is accepted.

### 1.4 What is refused, and why

| Refusal | Reason |
| --- | --- |
| A key id the package does not list in `context_exchange.key_ids`, or an issuer other than its `issuer` | What the operator provisions must be what the package declares. |
| A key id that is already registered (any application, any status) | A lookup knows only the id; two applications sharing one would make the lookup ambiguous. |
| Key material that was revoked, under any new name | A key revoked as compromised does not come back. |
| Anything that is not a raw 32-byte Ed25519 public key: PEM text, a 64-byte private key, junk | Public keys only. A pasted private key is refused without being echoed, stored or logged. (A 32-byte value is indistinguishable from a public key by size; a seed pasted by mistake would verify nothing and costs nothing, but rotate it.) |
| A published TEST-ONLY key, without `-allow-test-key` | See below. |
| A package that has critical findings, is for another application, or declares `context_exchange.mode: local_ipc` | The package is the source of the declaration; a package that cannot be loaded declares nothing, and local IPC has no issuer keys. |

### 1.5 The published TEST-ONLY keys

Two key pairs are published in this repository on purpose (`tools/devkeys`, and the example in
`schemas/application-package`). Their private keys are public, so anyone can mint a token or sign a
package with them. `add-issuer-key` refuses them unless the developer flag `-allow-test-key` is
given, and even then the row is marked as a developer key, `identity list` shows it as `TEST-ONLY`,
`add-issuer-key` warns on stderr, and `serve` warns at every start while one is active. A published
key found in the table without that mark (put there by editing the database) is never served. The
flag exists for the reference apps and for tests; never use it in production.

## 2. The platform checkpoint signing key

### 2.1 Create it

```sh
alixai keys init -config alixai.yaml          # optional: -key-id audit-checkpoint-2027
```

This creates, under `<data_dir>/keys/`:

- `audit-checkpoint.key`: the private key, PKCS#8 PEM, **mode 0600**, in a directory of **mode
  0700**; both are created that way and never widened afterwards;
- `audit-checkpoint.pub`: the public key, one line of base64 (0644, it is public);

and registers the public key in the trust store under the application `alixai.platform`, which is
what lets `serve` and an audit verification believe a checkpoint it signs. The private key is written
to its file and nowhere else: it is not printed, not logged and not stored in the database.

`keys init` never overwrites. Run again, it checks the key file, and registers the key if the
database does not have it yet (a fresh database next to a restored key file); it fails on a key file
that other users can read.

**Windows.** File modes are advisory there: the key file inherits the access rules of its directory.
Restrict `<data_dir>\keys` to the service identity yourself (`keys init` says so). The owner-only
behaviour is tested on POSIX.

**Back it up** like any signing key, offline and separately from the database. A lost key does not
break the audit log (the existing checkpoints stay verifiable from the public key in the trust
store); it means a new key, below.

### 2.2 What `serve` does with it

`alixai serve` looks for the key before it opens the store:

| Configuration | Behaviour |
| --- | --- |
| Nothing set, and `<data_dir>/keys/audit-checkpoint.key` does not exist | Starts, and logs `audit checkpoints are not signed` (events are still hash-chained; only the signed anchor is missing). |
| Nothing set, and that file exists | The key is **configured**: it must be usable. |
| `keys.checkpoint_key_file` set | The key is **configured**: it must be usable. |

A configured key that cannot be used stops `serve` before it listens: the file is missing, is not a
regular file, is not an Ed25519 PKCS#8 key, is readable by other users (POSIX), is not registered in
the trust store, is revoked or retired there, or does not match the registered public key. The error
names the cause and never contains key material.

When the key is usable, `serve` passes it to the audit log and signs checkpoints three ways: after
every `keys.checkpoint_every` events of a chain (default 100); in a periodic sweep every
`keys.checkpoint_interval` (default 5 minutes) that signs every chain whose head moved, which also
covers the events written inside another component's transaction, like the write-ahead action
events; and once more at a clean shutdown, so a stopped service has no unsigned tail.

```yaml
keys:
  checkpoint_key_file: ""    # PKCS#8 PEM; empty means <data_dir>/keys/audit-checkpoint.key if it exists
  checkpoint_key_id: ""      # its id in the trust store; empty means the file name without .key
  checkpoint_every: 100      # 0 leaves it to the sweeps
  checkpoint_interval: 5m
```

### 2.3 Limits, stated plainly

- Events appended after the newest checkpoint are not protected until the next one is signed; the
  window is `checkpoint_every` events or `checkpoint_interval`, whichever comes first.
- Someone who can write the database can delete checkpoint rows as well as events. The defence is a
  checkpoint held elsewhere: export one with the audit log's `ExportCheckpoint` and keep it with the
  integration-audit reports or the backup manifest.
- Revoking the platform key makes every checkpoint it signed fail verification, and `serve` refuses to
  start with a revoked key. To replace a key: `alixai keys init -key-id audit-checkpoint-2`, then set
  `keys.checkpoint_key_file` to the new file (the old public key stays registered so earlier
  checkpoints keep verifying).

## 3. Troubleshooting

| Symptom | Cause |
| --- | --- |
| Every `/v1` request answers 503 `the /v1 API is not enabled` | `identity.deployment_id` is not configured. |
| Every token answers 401 `unknown_key` | No issuer key is provisioned for that `kid`, or it was revoked. `alixai identity list`. |
| 401 `wrong_audience` | The token's `aud` is not `identity.audience` (or the deployment id). The package's declared audience and the deployment's differ. |
| 401 `wrong_issuer` / `wrong_application` | The token's `iss` or `application_id` is not the one the key was registered for. |
| `serve` exits with `the checkpoint signing key ...` | See 2.2; fix the file, or run `alixai keys init`. |
| `serve` exits with `route areas without their services` | A route area is registered in the binary but nothing provided its services: a wiring bug, found at start instead of as a 404. |

## 4. What this document does not cover

The **publisher** keys that sign application packages are registered, revoked and listed with
`alixai trust add-key`, `alixai trust revoke-key` and `alixai trust list`. The platform key above is
registered by `keys init`, and issuer keys by `identity add-issuer-key`.
