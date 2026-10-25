package release_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/release"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/upgrade"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

var (
	ctx      = context.Background()
	signedAt = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	checkAt  = signedAt.Add(time.Hour)
)

const keyID = "alixai-release-test"

func hex64(b string) string { return strings.Repeat(b, 32) }

// fx is a release: the files it lists on disk, the signed object that describes them, and the key that signs it.
type fx struct {
	t      *testing.T
	dir    string
	signed map[string]any
	priv   ed25519.PrivateKey
	keys   release.StaticKeys
	clk    *clock.Fake
}

func newFx(t *testing.T) *fx {
	t.Helper()
	f := &fx{t: t, dir: t.TempDir(), clk: clock.NewFake(checkAt)}
	pub, priv, err := trust.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	f.priv = priv
	f.keys = release.StaticKeys{Keys: map[string]ed25519.PublicKey{keyID: pub}}

	files := map[string][]byte{
		"SHA256SUMS":                      []byte("sums"),
		"alixai":                          []byte("binary bytes"),
		"alixai_1.2.3_linux_amd64.tar.gz": []byte("archive bytes"),
	}
	var arts []release.Artifact
	for _, name := range []string{"SHA256SUMS", "alixai", "alixai_1.2.3_linux_amd64.tar.gz"} {
		if err := os.WriteFile(filepath.Join(f.dir, name), files[name], 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(files[name])
		a := release.Artifact{Name: name, Kind: release.KindArchive, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(files[name]))}
		switch name {
		case "SHA256SUMS":
			a.Kind = release.KindChecksums
		case "alixai":
			a.Kind, a.OS, a.Arch = release.KindBinary, "linux", "amd64"
		}
		arts = append(arts, a)
	}
	digest := "sha256:" + hex64("de")
	s := release.Signed{
		Kind: release.Kind, ApplicationID: trust.PlatformApplicationID, Version: "1.2.3", Commit: strings.Repeat("c0", 20),
		CreatedAt: "2026-10-03T12:00:00Z", HashAlgorithm: release.HashAlgorithm,
		Artifacts: arts,
		Images: []release.Image{
			{Name: "alixai", Source: release.SourceBuilt, Reference: "ghcr.io/example/alixai:1.2.3", Digest: "sha256:" + hex64("ad")},
			{Name: "localai", Source: release.SourceThirdParty, Reference: "docker.io/localai/localai:v4.10.0", Digest: digest},
		},
		Backends: []release.Backend{{
			Runtime: "localai", Name: "cpu-llama-cpp", Alias: "llama-cpp", Version: "b2000", Licence: "MIT",
			Reference: "quay.io/example/backend", Digest: digest, Mirrors: []string{"docker.io/example/backend@" + digest},
		}},
		Models: []release.Model{
			f.model("granite-embedding", "ab"),
			f.model("qwen3", "cd"),
		},
	}
	f.signed = toMap(t, s)
	return f
}

func (f *fx) model(id, sum string) release.Model {
	return release.Model{
		ID: id, Name: id, Version: "1", Status: "candidate",
		Upstream: release.ModelUpstream{RepoURL: "https://example.invalid/m", Revision: "r1"},
		Artifact: release.ModelArtifact{Format: "gguf", RepoURL: "https://example.invalid/m", Filename: id + ".gguf", Revision: "r1", SizeBytes: 1000, SHA256: hex64(sum), QuantizedBy: "x"},
		Licence:  release.ModelLicence{SPDX: "Apache-2.0", URL: "https://example.invalid/l", RedistributionNotes: "ok"},
		Backend:  release.ModelBackend{Runtime: "localai", Name: "cpu-llama-cpp", AllowedRuntimeVersions: []string{"v4.10.0"}},
	}
}

func toMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// clone is a deep copy of the fixture's signed object, so one case never leaks into another.
func (f *fx) clone() map[string]any { return toMap(f.t, f.signed) }

// first is the first element of the named list of the signed object.
func first(m map[string]any, list string) map[string]any { return m[list].([]any)[0].(map[string]any) }

// sign signs the given signed object with the key under keyID, in the file form of a manifest. It does not go
// through release.Sign, which refuses an invalid manifest: a hostile or buggy signer would not.
func (f *fx) sign(signed map[string]any, id string, priv ed25519.PrivateKey) []byte {
	f.t.Helper()
	sig, err := trust.SignPayload(signed, id, priv, signedAt, signedAt.Add(30*24*time.Hour))
	if err != nil {
		f.t.Fatal(err)
	}
	raw, err := release.MarshalIndent(map[string]any{"format": release.Format, "signed": signed, "signatures": []any{sig}})
	if err != nil {
		f.t.Fatal(err)
	}
	return raw
}

func (f *fx) good() []byte { return f.sign(f.clone(), keyID, f.priv) }

func (f *fx) verify(raw []byte) (*release.Manifest, trust.Accepted, error) {
	return release.Verify(ctx, raw, f.keys, f.clk)
}

// SEC-064, SEC-090, SEC-091: a release manifest is accepted only when nothing under `signed` changed after it
// was signed and every input it lists is pinned. The unedited manifest is the control of every case below: it
// verifies, so each refusal is about the edit.
func TestVerifyRejectsATamperedManifestAndAnUnpinnedInput(t *testing.T) {
	f := newFx(t)
	good := f.good()
	m, acc, err := f.verify(good)
	if err != nil {
		t.Fatalf("the control manifest does not verify: %v", err)
	}
	if acc.KeyID != keyID || m.Signed.Version != "1.2.3" || len(m.Signed.Artifacts) != 3 || len(m.Signed.Models) != 2 {
		t.Fatalf("control: accepted %+v, manifest %+v", acc, m.Signed)
	}

	t.Run("a change after signing is a bad signature", func(t *testing.T) {
		edits := map[string]func(string) string{
			"a binary hash":   func(s string) string { return strings.Replace(s, m.Signed.Artifacts[1].SHA256, hex64("00"), 1) },
			"an image digest": func(s string) string { return strings.Replace(s, "sha256:"+hex64("ad"), "sha256:"+hex64("ee"), 1) },
			"a model hash":    func(s string) string { return strings.Replace(s, hex64("ab"), hex64("ba"), 1) },
			"a licence":       func(s string) string { return strings.Replace(s, `"spdx": "Apache-2.0"`, `"spdx": "MIT"`, 1) },
			"the version":     func(s string) string { return strings.Replace(s, `"version": "1.2.3"`, `"version": "1.2.4"`, 1) },
			"the signed instant": func(s string) string {
				return strings.Replace(s, `"created_at": "2026-10-03T12:00:00Z"`, `"created_at": "2026-10-03T12:00:01Z"`, 1)
			},
			"a runtime version": func(s string) string { return strings.Replace(s, `"v4.10.0"`, `"v9.9.9"`, 1) },
			"an artifact size":  func(s string) string { return strings.Replace(s, `"size": 12`, `"size": 13`, 1) },
		}
		for name, edit := range edits {
			bad := edit(string(good))
			if bad == string(good) {
				t.Errorf("%s: the edit changed nothing; the test is stale", name)
				continue
			}
			if _, _, err := f.verify([]byte(bad)); !errors.Is(err, trust.ErrBadSignature) {
				t.Errorf("%s: a manifest changed after signing gave %v, want a bad signature", name, err)
			}
		}
	})

	t.Run("a structural change after signing is a bad signature", func(t *testing.T) {
		edits := map[string]func(signed map[string]any){
			"an artifact dropped":     func(s map[string]any) { s["artifacts"] = s["artifacts"].([]any)[1:] },
			"an artifact added":       func(s map[string]any) { s["artifacts"] = append(s["artifacts"].([]any), first(s, "artifacts")) },
			"a model dropped":         func(s map[string]any) { s["models"] = s["models"].([]any)[:1] },
			"the application":         func(s map[string]any) { s["application_id"] = "demo-library" },
			"the purpose":             func(s map[string]any) { s["kind"] = "backup-manifest" },
			"an extra property":       func(s map[string]any) { s["extra"] = "x" },
			"a property removed":      func(s map[string]any) { delete(s, "commit") },
			"the artifacts reordered": func(s map[string]any) { a := s["artifacts"].([]any); a[0], a[1] = a[1], a[0] },
			"a mirror added to a backend": func(s map[string]any) {
				b := first(s, "backends")
				b["mirrors"] = append(b["mirrors"].([]any), "x/y@sha256:"+hex64("de"))
			},
			"a licence note removed":    func(s map[string]any) { first(s, "models")["licence"].(map[string]any)["redistribution_notes"] = "" },
			"a backend digest emptied":  func(s map[string]any) { first(s, "backends")["digest"] = "" },
			"a backend digest swapped":  func(s map[string]any) { first(s, "backends")["digest"] = "sha256:" + hex64("ee") },
			"an image's source flipped": func(s map[string]any) { first(s, "images")["source"] = release.SourceThirdParty },
			"a model's pinned size zeroed": func(s map[string]any) {
				first(s, "models")["artifact"].(map[string]any)["size_bytes"] = json.Number("0")
			},
		}
		for name, edit := range edits {
			// Take the signed manifest and edit it AFTER the signature was made: the envelope keeps the old signature.
			env := toMap(t, json.RawMessage(good))
			signed := env["signed"].(map[string]any)
			edit(signed)
			bad, err := release.MarshalIndent(env)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := f.verify(bad); !errors.Is(err, trust.ErrBadSignature) {
				t.Errorf("%s: gave %v, want a bad signature", name, err)
			}
		}
	})

	t.Run("a signature that is not the signer's is refused", func(t *testing.T) {
		env := toMap(t, json.RawMessage(good))
		sig := env["signatures"].([]any)[0].(map[string]any)
		raw, err := base64.StdEncoding.DecodeString(sig["signature"].(string))
		if err != nil {
			t.Fatal(err)
		}
		raw[0] ^= 0xff
		sig["signature"] = base64.StdEncoding.EncodeToString(raw)
		bad, _ := release.MarshalIndent(env)
		if _, _, err := f.verify(bad); !errors.Is(err, trust.ErrBadSignature) {
			t.Errorf("a flipped signature byte: %v", err)
		}
		env["signatures"] = []any{}
		unsigned, _ := release.MarshalIndent(env)
		if _, _, err := f.verify(unsigned); !errors.Is(err, trust.ErrUnsigned) {
			t.Errorf("no signature: %v", err)
		}
		delete(env, "signatures")
		missing, _ := release.MarshalIndent(env)
		if _, _, err := f.verify(missing); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("no signatures property: %v", err)
		}
	})

	// A manifest signed by the trusted key itself, carrying an input that is not pinned. The signature is
	// valid, so the refusal can only come from the pin rules: the code is schema_invalid, the message names the
	// input, and it is not a signature failure.
	t.Run("a signed manifest with an unpinned input is refused", func(t *testing.T) {
		cases := []struct {
			name string
			edit func(signed map[string]any)
			want string
		}{
			{"an artifact with no hash", func(s map[string]any) { first(s, "artifacts")["sha256"] = "" }, "artifacts[0]"},
			{"an artifact hash in upper case", func(s map[string]any) { first(s, "artifacts")["sha256"] = strings.ToUpper(hex64("ab")) }, "artifacts[0]"},
			{"an artifact hash too short", func(s map[string]any) { first(s, "artifacts")["sha256"] = "abcd" }, "artifacts[0]"},
			{"an artifact with a negative size", func(s map[string]any) { first(s, "artifacts")["size"] = json.Number("-1") }, "negative size"},
			{"an artifact named by a path", func(s map[string]any) { first(s, "artifacts")["name"] = "../SHA256SUMS" }, "not a plain file name"},
			{"a binary with no target", func(s map[string]any) { s["artifacts"].([]any)[1].(map[string]any)["os"] = "" }, "names its os and arch"},
			{"an artifact of an unknown kind", func(s map[string]any) { first(s, "artifacts")["kind"] = "firmware" }, "kind \"firmware\" is unknown"},
			{"no artifact at all", func(s map[string]any) { s["artifacts"] = []any{} }, "artifacts is empty"},
			{"an image with no digest", func(s map[string]any) { first(s, "images")["digest"] = "" }, "images[0]"},
			{"an image pinned by tag only", func(s map[string]any) { first(s, "images")["digest"] = "v1.2.3" }, "images[0]"},
			{"an image with another hash algorithm", func(s map[string]any) { first(s, "images")["digest"] = "sha512:" + strings.Repeat("a", 128) }, "images[0]"},
			{"a backend with no digest", func(s map[string]any) { first(s, "backends")["digest"] = "" }, "backends[0]"},
			{"a backend with no version", func(s map[string]any) { first(s, "backends")["version"] = "" }, "backends[0]"},
			{"a backend mirror pinned by another digest", func(s map[string]any) { first(s, "backends")["mirrors"] = []any{"docker.io/x/y@sha256:" + hex64("bb")} }, "mirror"},
			{"a backend mirror pinned by a tag", func(s map[string]any) { first(s, "backends")["mirrors"] = []any{"docker.io/x/y:latest"} }, "mirror"},
			{"a model with no hash", func(s map[string]any) { first(s, "models")["artifact"].(map[string]any)["sha256"] = "" }, "not fully pinned"},
			{"a model with no size", func(s map[string]any) {
				first(s, "models")["artifact"].(map[string]any)["size_bytes"] = json.Number("0")
			}, "not fully pinned"},
			{"a model with no revision", func(s map[string]any) { first(s, "models")["artifact"].(map[string]any)["revision"] = "" }, "not fully pinned"},
			{"a model with no licence record", func(s map[string]any) { first(s, "models")["licence"].(map[string]any)["spdx"] = "" }, "licence record is incomplete"},
			{"a model that names no backend", func(s map[string]any) { first(s, "models")["backend"].(map[string]any)["name"] = "" }, "backend is not named"},
			{"no model at all", func(s map[string]any) { s["models"] = []any{} }, "models is empty"},
			{"models out of order", func(s map[string]any) { a := s["models"].([]any); a[0], a[1] = a[1], a[0] }, "not sorted"},
			{"another purpose", func(s map[string]any) { s["kind"] = "backup-manifest" }, "kind is"},
			{"another application", func(s map[string]any) { s["application_id"] = "some.app" }, "application_id is"},
			{"a version that is not semantic", func(s map[string]any) { s["version"] = "two" }, "not a semantic version"},
			{"a version with a leading zero", func(s map[string]any) { s["version"] = "01.2.3" }, "not a semantic version"},
			{"a short commit", func(s map[string]any) { s["commit"] = "abc123" }, "commit"},
			{"a timestamp with an offset", func(s map[string]any) { s["created_at"] = "2026-10-03T12:00:00+02:00" }, "created_at"},
			{"another hash algorithm", func(s map[string]any) { s["hash_algorithm"] = "sha512" }, "hash_algorithm"},
			{"an unknown property", func(s map[string]any) { s["extra"] = "x" }, "does not match the format"},
			{"a missing property", func(s map[string]any) { delete(s, "images") }, "missing"},
			{"a null where a list goes", func(s map[string]any) { s["backends"] = nil }, "backends is null"},
			{"a null list of mirrors", func(s map[string]any) { first(s, "backends")["mirrors"] = nil }, "mirrors is null"},
			{"a null list of runtime versions", func(s map[string]any) {
				first(s, "models")["backend"].(map[string]any)["allowed_runtime_versions"] = nil
			}, "allowed_runtime_versions is null"},
		}
		for _, tc := range cases {
			s := f.clone()
			tc.edit(s)
			_, _, err := f.verify(f.sign(s, keyID, f.priv))
			switch {
			case err == nil:
				t.Errorf("%s: a manifest with an unpinned or malformed input verified", tc.name)
			case errors.Is(err, trust.ErrBadSignature) || contracts.CodeOf(err) != contracts.ErrSchemaInvalid:
				t.Errorf("%s: refused for the wrong reason (code %s): %v", tc.name, contracts.CodeOf(err), err)
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("%s: the refusal does not name the input (want %q): %v", tc.name, tc.want, err)
			}
		}
	})

	t.Run("keys and the clock", func(t *testing.T) {
		if _, _, err := release.Verify(ctx, good, nil, f.clk); contracts.CodeOf(err) != contracts.ErrUnauthenticated {
			t.Errorf("no keys: %v", err)
		}
		if _, _, err := release.Verify(ctx, good, release.StaticKeys{}, f.clk); !errors.Is(err, trust.ErrUnknownKey) {
			t.Errorf("no key is trusted: %v", err)
		}
		stranger := newFx(t)
		if _, _, err := f.verify(f.sign(f.clone(), "stranger", stranger.priv)); !errors.Is(err, trust.ErrUnknownKey) {
			t.Errorf("a key nobody registered: %v", err)
		}
		// The same key under another id is not the key that is registered.
		if _, _, err := f.verify(f.sign(f.clone(), "other-id", f.priv)); !errors.Is(err, trust.ErrUnknownKey) {
			t.Errorf("a key id that is not registered: %v", err)
		}
		late := clock.NewFake(signedAt.Add(31 * 24 * time.Hour))
		if _, _, err := release.Verify(ctx, good, f.keys, late); !errors.Is(err, trust.ErrSignatureExpired) || contracts.CodeOf(err) != contracts.ErrExpired {
			t.Errorf("an expired signature: %v", err)
		}
		early := clock.NewFake(signedAt.Add(-time.Hour))
		if _, _, err := release.Verify(ctx, good, f.keys, early); err == nil {
			t.Error("a signature dated in the future verified")
		}
		for name, raw := range map[string][]byte{"not JSON": []byte("{"), "empty": nil, "an array": []byte("[]")} {
			if _, _, err := f.verify(raw); err == nil {
				t.Errorf("%s verified", name)
			}
		}
		// A duplicate key is ambiguous JSON: refused before anything is trusted.
		dup := strings.Replace(string(good), `"format":`, `"format": "x", "format":`, 1)
		if _, _, err := f.verify([]byte(dup)); err == nil {
			t.Error("a manifest with a duplicate key verified")
		}
	})

	t.Run("the published TEST-ONLY key needs an explicit allow", func(t *testing.T) {
		dev := f.sign(f.clone(), devkeys.KeyID, devkeys.PrivateKey())
		keys := release.StaticKeys{Keys: map[string]ed25519.PublicKey{devkeys.KeyID: devkeys.PublicKey()}}
		if _, _, err := release.Verify(ctx, dev, keys, f.clk); !errors.Is(err, trust.ErrUnknownKey) {
			t.Fatalf("a TEST-ONLY key verified a release without the explicit allow: %v", err)
		}
		keys.AllowTestOnly = true
		if _, acc, err := release.Verify(ctx, dev, keys, f.clk); err != nil || acc.KeyID != devkeys.KeyID {
			t.Fatalf("with the explicit allow: %v", err)
		}
	})

	t.Run("key rotation: two signatures, either key is enough", func(t *testing.T) {
		raw := f.unsigned()
		oldPub, oldPriv, _ := trust.GenerateKey()
		_ = oldPub
		both, err := release.Sign(raw, trust.SigningKey{KeyID: "old-key", Key: oldPriv}, signedAt, 30*24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		both, err = release.Sign(both, trust.SigningKey{KeyID: keyID, Key: f.priv}, signedAt, 30*24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, acc, err := f.verify(both); err != nil || acc.KeyID != keyID {
			t.Errorf("signed by the old and the new key, only the new one trusted: %v, accepted %+v", err, acc)
		}
	})
}

// unsigned is the manifest in file form with no signature.
func (f *fx) unsigned() []byte {
	f.t.Helper()
	raw, err := release.MarshalIndent(map[string]any{"format": release.Format, "signed": f.clone(), "signatures": []any{}})
	if err != nil {
		f.t.Fatal(err)
	}
	return raw
}

func TestSignRefusesWhatVerifyWouldRefuse(t *testing.T) {
	f := newFx(t)
	key := trust.SigningKey{KeyID: keyID, Key: f.priv}
	signed, err := release.Sign(f.unsigned(), key, signedAt, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.verify(signed); err != nil {
		t.Fatalf("what Sign writes does not verify: %v", err)
	}
	if _, err := release.Sign(signed, key, signedAt, time.Hour); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("a key signed the same manifest twice: %v", err)
	}
	if _, err := release.Sign(f.unsigned(), key, signedAt, 0); err == nil {
		t.Error("a signature with no validity was written")
	}
	// A fifth signature does not fit.
	full := f.unsigned()
	for i := 0; i < trust.MaxSignatures; i++ {
		_, priv, _ := trust.GenerateKey()
		if full, err = release.Sign(full, trust.SigningKey{KeyID: "key-" + strconv.Itoa(i), Key: priv}, signedAt, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := release.Sign(full, key, signedAt, time.Hour); err == nil {
		t.Error("a fifth signature was written")
	}
	// An unpinned manifest is not signed at all.
	bad := f.clone()
	first(bad, "images")["digest"] = "v1"
	raw, _ := release.MarshalIndent(map[string]any{"format": release.Format, "signed": bad, "signatures": []any{}})
	if _, err := release.Sign(raw, key, signedAt, time.Hour); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
		t.Errorf("an unpinned manifest was signed: %v", err)
	}
}

// Parse is the strict reading of a manifest; it checks no signature.
func TestParseIsStrict(t *testing.T) {
	f := newFx(t)
	raw := string(f.good())
	if _, err := release.Parse([]byte(raw)); err != nil {
		t.Fatalf("the control: %v", err)
	}
	for name, bad := range map[string]string{
		"an unknown property":      strings.Replace(raw, `"kind": "release-manifest"`, `"kind": "release-manifest", "extra": "x"`, 1),
		"a miscased property":      strings.Replace(raw, `"hash_algorithm"`, `"Hash_Algorithm"`, 1),
		"a duplicate key":          strings.Replace(raw, `"kind": "release-manifest"`, `"kind": "release-manifest", "kind": "release-manifest"`, 1),
		"another format":           strings.Replace(raw, release.Format, "alixai.release-manifest.v2", 1),
		"a null where a list goes": strings.Replace(raw, `"backends": [`, `"backends": null, "x": [`, 1),
		"a float":                  strings.Replace(raw, `"size_bytes": 1000`, `"size_bytes": 1000.5`, 1),
		"not JSON":                 "{",
	} {
		if bad == raw {
			t.Errorf("%s: the edit changed nothing", name)
			continue
		}
		if _, err := release.Parse([]byte(bad)); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("%s: parsed or refused with %v", name, err)
		}
	}
}

// The hash half of the suite: the files of a release are re-hashed against the signed manifest.
func TestVerifyFilesChecksEveryHashAndRefusesUnlistedFiles(t *testing.T) {
	f := newFx(t)
	m, _, err := f.verify(f.good())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.VerifyFiles(f.dir, ""); err != nil {
		t.Fatalf("the files the manifest was built from: %v", err)
	}

	bin := filepath.Join(f.dir, "alixai")
	orig, _ := os.ReadFile(bin)

	t.Run("a replaced file of the same size", func(t *testing.T) {
		same := bytes.Repeat([]byte{'x'}, len(orig))
		if err := os.WriteFile(bin, same, 0o600); err != nil {
			t.Fatal(err)
		}
		err := m.VerifyFiles(f.dir, "")
		if contracts.CodeOf(err) != contracts.ErrValidationFailed || !strings.Contains(err.Error(), "does not match") {
			t.Errorf("a file with the right size and the wrong hash: %v", err)
		}
	})
	t.Run("a replaced file of another size", func(t *testing.T) {
		if err := os.WriteFile(bin, append(orig, '!'), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := m.VerifyFiles(f.dir, ""); err == nil {
			t.Error("a longer file went unnoticed")
		}
	})
	if err := os.WriteFile(bin, orig, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("an unlisted file is unsigned content, except the one named", func(t *testing.T) {
		extra := filepath.Join(f.dir, "release-manifest.json")
		if err := os.WriteFile(extra, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(extra)
		if err := m.VerifyFiles(f.dir, ""); err == nil || !strings.Contains(err.Error(), "not in the manifest") {
			t.Errorf("an unlisted file went unnoticed: %v", err)
		}
		if err := m.VerifyFiles(f.dir, "release-manifest.json"); err != nil {
			t.Errorf("the manifest itself was counted as unlisted: %v", err)
		}
		if err := m.VerifyFiles(f.dir, "something-else.json"); err == nil {
			t.Error("skipping another name hid an unlisted file")
		}
	})

	t.Run("an unlisted directory or link is unsigned content too", func(t *testing.T) {
		sub := filepath.Join(f.dir, "extra")
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(sub)
		if err := m.VerifyFiles(f.dir, ""); err == nil || !strings.Contains(err.Error(), "extra is in the directory but not in the manifest") {
			t.Errorf("an unlisted directory went unnoticed: %v", err)
		}
		os.Remove(sub)
		link := filepath.Join(f.dir, "alixai-link")
		if err := os.Symlink(bin, link); err != nil {
			t.Skipf("cannot create a symlink here: %v", err)
		}
		defer os.Remove(link)
		if err := m.VerifyFiles(f.dir, ""); err == nil || !strings.Contains(err.Error(), "alixai-link is in the directory but not in the manifest") {
			t.Errorf("an unlisted symbolic link went unnoticed: %v", err)
		}
	})

	t.Run("a listed file that is not a regular file", func(t *testing.T) {
		if err := os.Remove(bin); err != nil {
			t.Fatal(err)
		}
		defer os.WriteFile(bin, orig, 0o600)
		if err := m.VerifyFiles(f.dir, ""); err == nil {
			t.Error("a missing file went unnoticed")
		}
		if err := os.Mkdir(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(bin)
		if err := m.VerifyFiles(f.dir, ""); err == nil {
			t.Error("a directory stood in for a listed file")
		}
	})

	t.Run("a directory that cannot be read", func(t *testing.T) {
		err := m.VerifyFiles(filepath.Join(f.dir, "nope"), "")
		if err == nil {
			t.Error("a missing directory verified")
		}
	})
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, size, err := release.HashFile(p)
	if err != nil || size != 3 || sum != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("HashFile = %s, %d, %v", sum, size, err)
	}
	if _, _, err := release.HashFile(dir); err == nil {
		t.Error("a directory was hashed")
	}
	if _, _, err := release.HashFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file was hashed")
	}
}

func TestParseImageRef(t *testing.T) {
	d := "sha256:" + hex64("01")
	good := map[string]release.ImageRef{
		"docker.io/localai/localai:v4.10.0@" + d:         {Repo: "docker.io/localai/localai", Tag: "v4.10.0", Digest: d},
		"gcr.io/distroless/static-debian13:nonroot@" + d: {Repo: "gcr.io/distroless/static-debian13", Tag: "nonroot", Digest: d},
		"localhost:5000/team/app@" + d:                   {Repo: "localhost:5000/team/app", Digest: d}, // a port is not a tag
	}
	for in, want := range good {
		got, err := release.ParseImageRef(in)
		if err != nil || got != want {
			t.Errorf("ParseImageRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
		if got.String() != in {
			t.Errorf("String() = %q, want %q", got.String(), in)
		}
	}
	for _, in := range []string{
		"docker.io/localai/localai:v4.10.0",    // a tag is not a pin
		"docker.io/localai/localai:latest",     // neither is latest
		"x@sha256:ABCD",                        // short and upper case
		"@" + d,                                // no repository
		"a b@" + d,                             // whitespace
		"x@sha512:" + strings.Repeat("a", 128), // other algorithm
	} {
		if _, err := release.ParseImageRef(in); err == nil {
			t.Errorf("ParseImageRef(%q) accepted it", in)
		}
	}
}

// The upgrade and the release tool decide whether a manifest is genuine in one place, internal/release. Two
// halves: the code says so (neither carries a verifier of its own), and the behaviour says so (the same bytes
// get the same verdict and the same error from the library and from the upgrade).
func TestUpgradeAndReleaseToolShareOneVerifier(t *testing.T) {
	const releasePkg = "github.com/buxee/alix-ai/internal/release"
	t.Run("neither carries a verification of its own", func(t *testing.T) {
		forbidden := map[string]bool{"NewVerifier": true, "VerifyPayload": true, "SignPayload": true, "Canonicalize": true, "CanonicalJSON": true, "VerifyPackage": true}
		for _, dir := range []string{filepath.Join("..", "upgrade"), filepath.Join("..", "..", "tools", "release")} {
			files, err := filepath.Glob(filepath.Join(dir, "*.go"))
			if err != nil || len(files) < 3 {
				t.Fatalf("%s: found %d files (%v); the glob is not seeing the package", dir, len(files), err)
			}
			usesRelease := false
			for _, file := range files {
				if strings.HasSuffix(file, "_test.go") {
					continue
				}
				af, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
				if err != nil {
					t.Fatal(err)
				}
				for _, imp := range af.Imports {
					if p, _ := strconv.Unquote(imp.Path.Value); p == releasePkg {
						usesRelease = true
					}
				}
				ast.Inspect(af, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "trust" && forbidden[sel.Sel.Name] {
						t.Errorf("%s calls trust.%s: a manifest is verified and signed in internal/release only", file, sel.Sel.Name)
					}
					return true
				})
			}
			if !usesRelease {
				t.Errorf("%s does not import internal/release", dir)
			}
		}
	})

	t.Run("the same bytes get the same verdict and the same error", func(t *testing.T) {
		f := newFx(t)
		stranger := newFx(t)
		corpus := map[string][]byte{"the control": f.good()}
		corpus["unsigned"] = f.unsigned()
		corpus["an unknown key"] = f.sign(f.clone(), keyID, stranger.priv)
		corpus["the TEST-ONLY key, not allowed"] = f.sign(f.clone(), devkeys.KeyID, devkeys.PrivateKey())
		corpus["not JSON"] = []byte("{")
		corpus["tampered"] = []byte(strings.Replace(string(f.good()), `"version": "1.2.3"`, `"version": "1.2.4"`, 1))
		for name, edit := range map[string]func(map[string]any){
			"an image with no digest":   func(s map[string]any) { first(s, "images")["digest"] = "" },
			"a model with no hash":      func(s map[string]any) { first(s, "models")["artifact"].(map[string]any)["sha256"] = "" },
			"a backend with no version": func(s map[string]any) { first(s, "backends")["version"] = "" },
			"another purpose":           func(s map[string]any) { s["kind"] = "backup-manifest" },
		} {
			s := f.clone()
			edit(s)
			corpus[name] = f.sign(s, keyID, f.priv)
		}
		for name, raw := range corpus {
			m, acc, lerr := release.Verify(ctx, raw, f.keys, f.clk)
			rel, uerr := (upgrade.ReleaseVerifier{Keys: f.keys, Clock: f.clk}).VerifyRelease(ctx, raw)
			if (lerr == nil) != (uerr == nil) {
				t.Errorf("%s: the library says %v, the upgrade says %v", name, lerr, uerr)
				continue
			}
			if lerr != nil {
				if lerr.Error() != uerr.Error() || contracts.CodeOf(lerr) != contracts.CodeOf(uerr) {
					t.Errorf("%s: different refusals: %q (%s) and %q (%s)", name, lerr, contracts.CodeOf(lerr), uerr, contracts.CodeOf(uerr))
				}
				continue
			}
			if name != "the control" {
				t.Errorf("%s verified", name)
			}
			if rel.Version != m.Signed.Version || rel.Commit != m.Signed.Commit || len(rel.Artifacts) != len(m.Signed.Artifacts) ||
				len(rel.Backends) != len(m.Signed.Backends) || len(rel.Models) != len(m.Signed.Models) || rel.Accepted != acc {
				t.Errorf("%s: the upgrade's release %+v is not the library's manifest %+v", name, rel, m.Signed)
			}
			// And the files are checked by the same code.
			if err := rel.VerifyArtifacts(f.dir); err != nil {
				t.Errorf("%s: the upgrade refuses the release's own files: %v", name, err)
			}
		}
		// Clock and keys reach the shared verifier through the upgrade's wrapper.
		late := clock.NewFake(signedAt.Add(400 * 24 * time.Hour))
		if _, err := (upgrade.ReleaseVerifier{Keys: f.keys, Clock: late}).VerifyRelease(ctx, f.good()); !errors.Is(err, trust.ErrSignatureExpired) {
			t.Errorf("an expired signature through the upgrade: %v", err)
		}
		if _, err := (upgrade.ReleaseVerifier{Clock: f.clk}).VerifyRelease(ctx, f.good()); contracts.CodeOf(err) != contracts.ErrUnauthenticated {
			t.Errorf("no keys through the upgrade: %v", err)
		}
	})

	t.Run("what the tool writes verifies in the library and in the upgrade", func(t *testing.T) {
		// internal/upgrade/testdata/release-manifest.json was written by the real tool (go run ./tools/release
		// manifest, then sign -dev-key), not by these tests, and is valid for a year from 2026-10-03.
		raw, err := os.ReadFile(filepath.Join("..", "upgrade", "testdata", "release-manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		keys := release.StaticKeys{Keys: map[string]ed25519.PublicKey{devkeys.KeyID: devkeys.PublicKey()}}
		clk := clock.NewFake(signedAt)
		if _, _, err := release.Verify(ctx, raw, keys, clk); err == nil {
			t.Fatal("a release signed with the TEST-ONLY key verified without the explicit allow")
		}
		keys.AllowTestOnly = true
		m, _, err := release.Verify(ctx, raw, keys, clk)
		if err != nil {
			t.Fatalf("the tool's own manifest does not verify in the library: %v", err)
		}
		rel, err := (upgrade.ReleaseVerifier{Keys: keys, Clock: clk}).VerifyRelease(ctx, raw)
		if err != nil || rel.Version != m.Signed.Version {
			t.Fatalf("the tool's own manifest in the upgrade: %v", err)
		}
	})
}
