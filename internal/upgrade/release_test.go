package upgrade

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/release"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

func (f *fixture) verify(raw []byte) (*Release, error) {
	return ReleaseVerifier{Keys: f.relKeys, Clock: f.clk}.VerifyRelease(ctx, raw)
}

// SEC-064: the release manifest is verified with internal/trust under the
// platform application, and anything unpinned is refused even when signed.
func TestVerifyRelease(t *testing.T) {
	f := newFixture(t)
	good := f.release("2.0.0", []string{"v3.6.0"}, "b2000")

	rel, err := f.verify(good)
	if err != nil {
		t.Fatalf("a good manifest: %v", err)
	}
	if rel.Version != "2.0.0" || len(rel.Backends) != 1 || rel.Backends[0].Name != "llama-cpp" || len(rel.Models) != 1 || rel.Accepted.KeyID != releaseKeyID {
		t.Errorf("release = %+v", rel)
	}

	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	bad := map[string]func() []byte{
		"changed byte under signed": func() []byte {
			return []byte(strings.Replace(string(good), `"version": "2.0.0"`, `"version": "2.0.1"`, 1))
		},
		"no signature": func() []byte {
			var env map[string]any
			if err := json.Unmarshal(good, &env); err != nil {
				t.Fatal(err)
			}
			env["signatures"] = []any{}
			raw, _ := json.Marshal(env)
			return raw
		},
		"unregistered key": func() []byte {
			return f.sign(map[string]any{"kind": "release-manifest"}, "other-key", otherPriv)
		},
		"wrong kind": func() []byte {
			return f.releaseWith("2.0.0", []string{"v3.6.0"}, "b2000", func(s map[string]any) { s["kind"] = "backup-manifest" })
		},
		"wrong application": func() []byte {
			return f.releaseWith("2.0.0", []string{"v3.6.0"}, "b2000", func(s map[string]any) { s["application_id"] = "some.app" })
		},
		"version not semantic": func() []byte { return f.releaseWith("two", []string{"v3.6.0"}, "b2000", nil) },
		"commit malformed": func() []byte {
			return f.releaseWith("2.0.0", []string{"v3.6.0"}, "b2000", func(s map[string]any) { s["commit"] = "abc" })
		},
		"no artifacts": func() []byte {
			return f.releaseWith("2.0.0", []string{"v3.6.0"}, "b2000", func(s map[string]any) { s["artifacts"] = []any{} })
		},
		"artifact without hash": func() []byte {
			return f.releaseWith("2.0.0", []string{"v3.6.0"}, "b2000", func(s map[string]any) { s["artifacts"].([]any)[0].(map[string]any)["sha256"] = "" })
		},
		"image without digest": func() []byte {
			return f.releaseWith("2.0.0", []string{"v3.6.0"}, "b2000", func(s map[string]any) { s["images"].([]any)[0].(map[string]any)["digest"] = "" })
		},
		"image pinned by tag only": func() []byte {
			return f.releaseWith("2.0.0", []string{"v3.6.0"}, "b2000", func(s map[string]any) { s["images"].([]any)[0].(map[string]any)["digest"] = "v3.6.0" })
		},
		"backend without digest": func() []byte {
			return f.releaseWith("2.0.0", []string{"v3.6.0"}, "b2000", func(s map[string]any) { s["backends"].([]any)[0].(map[string]any)["digest"] = "" })
		},
		"backend without version": func() []byte {
			return f.releaseWith("2.0.0", []string{"v3.6.0"}, "", nil)
		},
		"model without hash": func() []byte {
			return f.releaseWith("2.0.0", []string{"v3.6.0"}, "b2000", func(s map[string]any) {
				s["models"].([]any)[0].(map[string]any)["artifact"].(map[string]any)["sha256"] = ""
			})
		},
		"no models": func() []byte {
			return f.releaseWith("2.0.0", []string{"v3.6.0"}, "b2000", func(s map[string]any) { s["models"] = []any{} })
		},
		"unknown top-level field": func() []byte {
			return []byte(strings.Replace(string(good), `"format":`, `"extra": 1, "format":`, 1))
		},
		"duplicate key": func() []byte {
			return []byte(strings.Replace(string(good), `"kind": "release-manifest"`, `"kind": "release-manifest", "kind": "release-manifest"`, 1))
		},
		"not json": func() []byte { return []byte("not json") },
	}
	for name, mk := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := f.verify(mk()); err == nil {
				t.Fatal("a manifest that must be refused verified")
			} else {
				t.Logf("refused: %v", err)
			}
		})
	}

	t.Run("expired signature", func(t *testing.T) {
		late := ReleaseVerifier{Keys: f.relKeys, Clock: clock.NewFake(t0.Add(400 * 24 * time.Hour))}
		if _, err := late.VerifyRelease(ctx, good); contracts.CodeOf(err) != contracts.ErrExpired {
			t.Fatalf("err = %v (code %s), want %s", err, contracts.CodeOf(err), contracts.ErrExpired)
		}
	})
	t.Run("no keys", func(t *testing.T) {
		if _, err := (ReleaseVerifier{}).VerifyRelease(ctx, good); err == nil {
			t.Fatal("verified without keys")
		}
	})
	t.Run("the published TEST-ONLY key needs an explicit allow", func(t *testing.T) {
		dev := f.signWith(devkeys.KeyID, devkeys.PrivateKey(), "2.0.0")
		keys := release.StaticKeys{Keys: map[string]ed25519.PublicKey{devkeys.KeyID: devkeys.PublicKey()}}
		if _, err := (ReleaseVerifier{Keys: keys, Clock: f.clk}).VerifyRelease(ctx, dev); err == nil {
			t.Fatal("a TEST-ONLY key verified a release without the explicit allow")
		}
		keys.AllowTestOnly = true
		if _, err := (ReleaseVerifier{Keys: keys, Clock: f.clk}).VerifyRelease(ctx, dev); err != nil {
			t.Fatalf("with the explicit allow: %v", err)
		}
	})
}

// signWith signs a good release body with another key.
func (f *fixture) signWith(keyID string, priv ed25519.PrivateKey, version string) []byte {
	f.t.Helper()
	var signed map[string]any
	f.releaseWith(version, []string{"v3.6.0"}, "b2000", func(s map[string]any) { signed = s })
	return f.sign(signed, keyID, priv)
}

func TestVerifyArtifacts(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	body := []byte("binary 2.0.0")
	if err := os.WriteFile(filepath.Join(dir, "alixai"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	rel, err := f.verify(f.release("2.0.0", []string{"v3.6.0"}, "b2000"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != rel.Artifacts[0].SHA256 || int64(len(body)) != rel.Artifacts[0].Size {
		t.Fatalf("test setup: artifact pin %+v does not describe the body", rel.Artifacts[0])
	}
	if err := rel.VerifyArtifacts(dir); err != nil {
		t.Fatalf("matching files: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "extra"), []byte("x"), 0o600)
	if err := rel.VerifyArtifacts(dir); err == nil {
		t.Error("an unlisted file was accepted")
	}
	os.Remove(filepath.Join(dir, "extra"))
	os.WriteFile(filepath.Join(dir, "alixai"), []byte("binary 6.6.6"), 0o600)
	if err := rel.VerifyArtifacts(dir); err == nil {
		t.Error("a modified file was accepted")
	}
	os.Remove(filepath.Join(dir, "alixai"))
	if err := rel.VerifyArtifacts(dir); err == nil {
		t.Error("a missing file was accepted")
	}
}

// SEC-072: the qualified set is the release's, compared the way the adapter
// compares versions (case and a leading "v" are cosmetic).
func TestQualify(t *testing.T) {
	f := newFixture(t)
	rel, err := f.verify(f.release("2.0.0", []string{"v3.6.0", "V3.7.1"}, "b2000"))
	if err != nil {
		t.Fatal(err)
	}
	digest := rel.Backends[0].Digest
	for _, tc := range []struct {
		name string
		rs   RuntimeSet
		ok   bool
	}{
		{"listed, as spelt", RuntimeSet{Runtime: "localai", Version: "v3.6.0"}, true},
		{"v is cosmetic", RuntimeSet{Runtime: "localai", Version: "3.6.0"}, true},
		{"case is cosmetic", RuntimeSet{Runtime: "localai", Version: "v3.7.1"}, true},
		{"unlisted", RuntimeSet{Runtime: "localai", Version: "3.8.0"}, false},
		{"prefix is not a match", RuntimeSet{Runtime: "localai", Version: "3.6"}, false},
		{"other runtime", RuntimeSet{Runtime: "ollama", Version: "3.6.0"}, false},
		{"backend by version", RuntimeSet{Runtime: "localai", Version: "3.6.0", Backends: map[string]string{"llama-cpp": "B2000"}}, true},
		{"backend by digest", RuntimeSet{Runtime: "localai", Version: "3.6.0", Backends: map[string]string{"llama-cpp": digest}}, true},
		{"backend other version", RuntimeSet{Runtime: "localai", Version: "3.6.0", Backends: map[string]string{"llama-cpp": "b1999"}}, false},
		{"backend unknown", RuntimeSet{Runtime: "localai", Version: "3.6.0", Backends: map[string]string{"whisper": "b2000"}}, false},
		{"no version", RuntimeSet{Runtime: "localai"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := rel.Qualify(tc.rs)
			if (err == nil) != tc.ok {
				t.Fatalf("Qualify(%+v) = %v, want ok=%v", tc.rs, err, tc.ok)
			}
		})
	}
	if err := rel.Qualify(RuntimeSet{Runtime: "localai", Version: "3.6.0"}, "no-such-model"); err == nil {
		t.Error("a model that is not in the release qualified")
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.9.0", "1.10.0", -1},
		{"2.0.0", "1.99.99", 1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"1.0.0-rc.2", "1.0.0-rc.10", -1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-1", "1.0.0-alpha", -1},
		{"1.0.0+build.5", "1.0.0+build.9", 0},
	} {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// release-manifest.json in testdata was written by the real tool, not by these
// tests, so the verifier is checked against the shape tools/release produces:
//
//	go build -o alixai ./cmd/alixai
//	go run ./tools/release manifest -version 2.0.0 -commit <sha> -artifact binary=alixai -out m.json
//	go run ./tools/release sign -manifest m.json -dev-key
//
// It is signed with the published TEST-ONLY key, valid for a year from 2026-10-03.
func TestVerifyManifestWrittenByReleaseTool(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "release-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	keys := release.StaticKeys{Keys: map[string]ed25519.PublicKey{devkeys.KeyID: devkeys.PublicKey()}}
	v := ReleaseVerifier{Keys: keys, Clock: clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))}
	if _, err := v.VerifyRelease(ctx, raw); err == nil {
		t.Fatal("a release signed with the TEST-ONLY key verified without the explicit allow")
	}
	keys.AllowTestOnly = true
	v.Keys = keys
	rel, err := v.VerifyRelease(ctx, raw)
	if err != nil {
		t.Fatalf("the tool's own manifest does not verify: %v", err)
	}
	if rel.Version != "2.0.0" || len(rel.Artifacts) != 1 || len(rel.Backends) != 1 || len(rel.Models) == 0 {
		t.Fatalf("release = %+v", rel)
	}
	bk := rel.Backends[0]
	rs := RuntimeSet{Runtime: bk.Runtime, Version: "v4.10.0", Backends: map[string]string{bk.Name: bk.Version}}
	// The registry lists several candidate models with no qualified runtime
	// version; only the embedding model is qualified. Asked about every model the
	// release lists, the runtime is NOT qualified (fail closed); asked about the
	// models the installation serves, it is.
	if err := rel.Qualify(rs); err == nil {
		t.Error("a runtime was qualified for models the release lists no qualified version for")
	}
	if err := rel.Qualify(rs, "granite-embedding-97m-multilingual-r2-q4_k_m"); err != nil {
		t.Errorf("the qualified model and runtime: %v", err)
	}
	rs.Version = "v4.11.0"
	if err := rel.Qualify(rs, "granite-embedding-97m-multilingual-r2-q4_k_m"); err == nil {
		t.Error("an unlisted runtime version was qualified")
	}
}
