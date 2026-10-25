// Package trust is package authenticity (master plan 4.5 check 1, 9.1, 16.3):
// the integrity manifest of an application package, detached Ed25519
// signatures over it, and the SQLite trust store of publisher keys. The same
// signer signs integration-audit reports and release manifests.
//
// # The signature manifest
//
// A package carries signatures/signature-manifest.json, defined by
// schemas/application-package/signature-manifest.schema.json and
// docs/integration/application-package.md section 13. This package builds and
// checks exactly that file:
//
//	signed      application_id, package_version, created_at, hash_algorithm,
//	            files: every file of the package outside signatures/, sorted
//	            by path, with its SHA-256 and size
//	signatures  one to four detached Ed25519 signatures over the canonical
//	            JSON of signed, each with key_id, signed_at and a mandatory
//	            expires_at
//
// Canonical JSON (Canonicalize, CanonicalJSON) is RFC 8785 restricted to
// strings and integers: sorted keys, no whitespace, minimal escapes, duplicate
// keys and floats refused. Signatures cover those bytes and nothing else.
//
// # Building the file list
//
// BuildSigned walks the package and refuses anything it cannot list: symbolic
// links, junctions and other non-regular entries are errors (never followed),
// every path must satisfy the safe path grammar (so ".." segments, absolute
// paths, backslashes, drive letters and hidden entries such as .git cannot
// appear), and there is no skip list. The top-level signatures/ directory is
// excluded from the list by a fixed rule, and it may hold nothing but
// signature-manifest.json: an unlisted file there would be unsigned content.
//
// # Verifying
//
// Verifier.VerifyPackage checks the signature first and the content second.
// A file that is not in the manifest, a missing file, a changed byte, a
// symbolic link, a stray file in signatures/: each is a TamperError. A package
// with no manifest is ErrUnsigned. Every failure is a *contracts.Error with
// code contracts.ErrUnauthenticated (contracts.ErrExpired when expiry is the
// only reason, contracts.ErrSchemaInvalid for a malformed manifest) that also
// wraps one of the sentinel errors in errors.go, so errors.Is names the
// reason.
//
// A signature is accepted only when its key is registered for the package's
// application, named in the package's integrity.publisher_key_ids, not
// revoked (a retired key counts only for signatures made before it was
// retired), the Ed25519 signature verifies, the signature has not expired and
// is not dated in the future.
//
// # The trust store
//
// Store keeps publisher keys per application in table trust_publisher_keys
// (module "trust"). It starts empty and trusts nothing. Registration is an
// operator action. Keys are retired (RetireKey, RotateKey: earlier signatures
// stay valid until they expire) or revoked (RevokeKey: nothing verifies,
// permanently; revoked key material cannot be registered again under another
// name). The published TEST-ONLY keys (tools/devkeys, the schema example) are
// refused unless the store is opened with AllowTestOnlyKeys.
//
// The table has no tenant or workspace column: a publisher key is per
// application and shared by all its tenants. Every query filters on
// application_id, except the revocation checks of RegisterKey and LookupKey,
// which ask by fingerprint whether the key material was revoked for any
// application: revoked material verifies nothing for any of them.
//
// # Limits
//
// The package reads files once and cannot protect a directory that an
// attacker can write to while it is being signed or verified; callers sign
// and verify a private copy, or a directory only the service can write.
package trust
