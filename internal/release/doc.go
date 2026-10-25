// Package release is the one implementation of the signed release manifest
// (master plan 16.3, 23 gate 9; SEC-064, SEC-090, SEC-091): its types, the rules
// of its format, signing it, and verifying it.
//
// The release tool (tools/release, a main package) builds and signs the manifest
// and the upgrade (internal/upgrade) verifies it before it changes anything.
// Both import this package, so there is exactly one place where "this manifest
// is genuine" is decided:
//
//  1. Verify authenticates first. The signature covers trust.CanonicalJSON of the
//     "signed" object, so it is checked over those bytes under the release keys
//     of the platform application (trust.PlatformApplicationID with
//     trust.PurposeRelease, or the built-in PinnedKeyID), before the content is
//     interpreted. Other platform keys, such as the audit-checkpoint key serve
//     keeps online, never sign a release. No key, no signature, an unknown, revoked or expired key, a
//     published TEST-ONLY key that was not explicitly allowed, or one changed byte
//     under "signed" is refused.
//  2. Then the content is parsed as strictly as a package signature manifest:
//     the bytes are acceptable canonical JSON (no duplicate key, no float, valid
//     UTF-8), no property is unknown, missing, differently cased or null, and the
//     rules of Signed.Validate hold. Every pin a consumer relies on must be
//     present and well formed: an artifact without a SHA-256 and a size, an image
//     or backend without a sha256 digest, a model without a pinned file, is
//     refused even when it is signed, because a consumer could not tell what it
//     was being asked to trust.
//
// Errors are *contracts.Error values; the cause (a trust error, for a signature)
// is kept in the chain, so errors.Is(err, trust.ErrUnknownKey) works.
//
// The published TEST-ONLY key (tools/devkeys) verifies only through a key lookup
// that was opened with an explicit allow (StaticKeys.AllowTestOnly,
// trust.AllowTestOnlyKeys): nothing in this package widens it.
package release
