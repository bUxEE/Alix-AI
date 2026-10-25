# Release keys

`alixai-release-2026.pub` is the public half of the Ed25519 key that signs AlixAI release manifests
(`release-manifest.json`). The private half is held by the maintainer, never in CI or in the repository.

| Key id | Public key (base64) |
| --- | --- |
| `alixai-release-2026` | `ok5oQ98ihECPbbWYuXWyfAlVbK6kZV97vQliBsYfzOU=` |

Verify a downloaded release:

```sh
go run ./tools/release verify -manifest release-manifest.json -pubkey alixai-release-2026=keys/alixai-release-2026.pub -dir .
```

The key is also built into the `alixai` binary (`internal/release`), so `alixai upgrade` accepts a release it signed with no setup. Register
it only to revoke it (`alixai trust add-key -app alixai.platform -key-id alixai-release-2026 -public-key-file keys/alixai-release-2026.pub -purpose release`,
then `alixai trust revoke-key`). Another release key must be registered with `-purpose release`: platform keys of other purposes, such as the
audit-checkpoint key, never verify a release.

The container images are signed separately with cosign keyless signing by the release workflow; see
[docs/operations/release.md](../docs/operations/release.md).
