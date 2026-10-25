package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/release"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/models/registry"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

var (
	testNow    = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	testCommit = strings.Repeat("c0", 20)
)

// releaseFiles writes the small files of a release into a new directory: an archive, an SBOM, a checksum file.
func releaseFiles(t *testing.T) (dir string, inputs []artifactInput) {
	t.Helper()
	dir = t.TempDir()
	doc, err := binarySBOM(mustReadBinary(t, "alixai"), "1.2.3", sbomOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sbom, _ := marshalIndent(doc)
	for name, body := range map[string][]byte{
		"alixai_1.2.3_linux_amd64.tar.gz": []byte("pretend archive"),
		"alixai_linux_amd64.cdx.json":     sbom,
		"SHA256SUMS":                      []byte(hex64("ab") + "  alixai_1.2.3_linux_amd64.tar.gz\n"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inputs = []artifactInput{
		{release.KindArchive, filepath.Join(dir, "alixai_1.2.3_linux_amd64.tar.gz")},
		{release.KindSBOM, filepath.Join(dir, "alixai_linux_amd64.cdx.json")},
		{release.KindChecksums, filepath.Join(dir, "SHA256SUMS")},
	}
	return dir, inputs
}

// testInputs is a complete release: the test executable stands in for the binary, the real registry, compose files
// and backend catalogue of the checkout supply the rest.
func testInputs(t *testing.T) manifestInputs {
	t.Helper()
	root := repoRoot(t)
	reg, err := models.LoadFS(registry.FS)
	if err != nil {
		t.Fatal(err)
	}
	_, files := releaseFiles(t)
	return manifestInputs{
		Version:   "1.2.3",
		Commit:    testCommit,
		CreatedAt: testNow,
		Artifacts: append(files, artifactInput{release.KindBinary, testExe(t)}),
		BuiltImages: []string{
			"alixai=ghcr.io/example/alixai:1.2.3@" + testImageDigest,
			"alixai-netctl=ghcr.io/example/alixai-netctl:1.2.3@sha256:" + hex64("de"),
		},
		ComposeFiles: []string{
			filepath.Join(root, "deploy", "container", "stack", "compose.yaml"),
			filepath.Join(root, "deploy", "container", "localai", "compose.yaml"),
		},
		QuadletFiles:   quadletFiles(t, root),
		BackendGallery: filepath.Join(root, "deploy", "container", "localai", "config", "backend-gallery.yaml"),
		Registry:       reg,
	}
}

func quadletFiles(t *testing.T, root string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "deploy", "container", "quadlet", "*.container"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Quadlet units found: %v", err)
	}
	return files
}

func testManifestBytes(t *testing.T) []byte {
	t.Helper()
	m, err := buildReleaseManifest(testInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := marshalIndent(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newStore(t *testing.T, clk clock.Clock, opts ...trust.StoreOption) *trust.Store {
	t.Helper()
	return trust.NewStore(storetest.Open(t, nil), clk, opts...)
}

func mustSign(t *testing.T, raw []byte, key trust.SigningKey) []byte {
	t.Helper()
	signed, err := release.Sign(raw, key, testNow, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func generatedKey(t *testing.T, id string) trust.SigningKey {
	t.Helper()
	_, priv, err := trust.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return trust.SigningKey{KeyID: id, Key: priv}
}

func pubOf(k trust.SigningKey) ed25519.PublicKey { return k.Key.Public().(ed25519.PublicKey) }

// SEC-064, SEC-090: the release manifest lists the binary, image and backend hashes, carries a signature made
// with the internal/trust signer, and verifies against the trust store only while nothing under `signed` has
// changed and the key is a trusted, unexpired platform key.
func TestReleaseManifestSignedAndVerifiable(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow.Add(time.Hour))
	raw := testManifestBytes(t)
	dev := devkeys.SigningKey()
	signed := mustSign(t, raw, dev)

	// A store that opted in to test keys trusts the dev key, as a developer machine does.
	devStore := newStore(t, clk, trust.AllowTestOnlyKeys())
	if _, err := devStore.RegisterKey(ctx, trust.PlatformApplicationID, dev.KeyID, pubOf(dev), "dev", trust.WithPurpose(trust.PurposeRelease)); err != nil {
		t.Fatal(err)
	}
	m, acc, err := release.Verify(ctx, signed, devStore, clk)
	if err != nil {
		t.Fatalf("a freshly signed manifest does not verify: %v", err)
	}
	if acc.KeyID != dev.KeyID || !acc.ExpiresAt.After(testNow) {
		t.Errorf("accepted = %+v", acc)
	}

	// What it lists: the binary's own hash and target, the images by digest, the backend by digest.
	exeSum, exeSize, err := release.HashFile(testExe(t))
	if err != nil {
		t.Fatal(err)
	}
	var sawBinary bool
	for _, a := range m.Signed.Artifacts {
		if a.Kind == release.KindBinary {
			sawBinary = a.SHA256 == exeSum && a.Size == exeSize && a.OS != "" && a.Arch != ""
		}
	}
	if !sawBinary {
		t.Error("the binary is not listed with its hash, size and target")
	}
	images := map[string]release.Image{}
	for _, im := range m.Signed.Images {
		images[im.Name] = im
	}
	if im := images["alixai"]; im.Source != release.SourceBuilt || im.Digest != testImageDigest || im.Reference != "ghcr.io/example/alixai:1.2.3" {
		t.Errorf("built image not listed as given: %+v", im)
	}
	third := 0
	for _, im := range m.Signed.Images {
		if im.Source == release.SourceThirdParty {
			third++
		}
	}
	if third != 1 {
		t.Errorf("%d third-party images listed; the compose files and the Quadlet units pin one and the same LocalAI image", third)
	}
	if im := images["localai"]; im.Source != release.SourceThirdParty || im.Digest != "sha256:0632c21ddbe440bb71bc7aa744796c227a51658641b216290e86d6894e1c705b" {
		t.Errorf("the pinned LocalAI image is not listed with its digest: %+v", im)
	}
	if len(m.Signed.Backends) != 1 || m.Signed.Backends[0].Name != "cpu-llama-cpp" || m.Signed.Backends[0].Digest != "sha256:d7b0d9c69ecce93c2e7f7500ac3d99aba28648dab1ab29b698e0cc4538662579" || len(m.Signed.Backends[0].Mirrors) != 1 {
		t.Errorf("backends = %+v", m.Signed.Backends)
	}

	// The same bytes, parsed and signed again, are a second signature, not a second manifest.
	if _, err := release.Sign(signed, dev, testNow, time.Hour); err == nil {
		t.Error("a key signed the same manifest twice")
	}

	t.Run("unsigned", func(t *testing.T) {
		_, _, err := release.Verify(ctx, raw, devStore, clk)
		if !errors.Is(err, trust.ErrUnsigned) {
			t.Errorf("an unsigned manifest: %v", err)
		}
	})

	t.Run("tampered after signing", func(t *testing.T) {
		for name, edit := range map[string]func(string) string{
			"a binary hash":    func(s string) string { return strings.Replace(s, exeSum, hex64("00"), 1) },
			"an image digest":  func(s string) string { return strings.Replace(s, "sha256:"+hex64("de"), "sha256:"+hex64("ee"), 1) },
			"a backend digest": func(s string) string { return strings.Replace(s, "sha256:d7b0d9c69ecce", "sha256:e7b0d9c69ecce", 1) },
			"a model hash": func(s string) string {
				return strings.Replace(s, "aaf42c8b7c3cab2bf3d69c355048d4a0ee9973d48f16c731c0520ee914699223", hex64("ab"), 1)
			},
			"a licence":   func(s string) string { return strings.Replace(s, `"spdx": "Apache-2.0"`, `"spdx": "MIT"`, 1) },
			"the version": func(s string) string { return strings.Replace(s, `"version": "1.2.3"`, `"version": "1.2.4"`, 1) },
			"the signed instant": func(s string) string {
				return strings.Replace(s, `"created_at": "2026-10-03T12:00:00Z"`, `"created_at": "2026-10-03T12:00:01Z"`, 1)
			},
		} {
			bad := edit(string(signed))
			if bad == string(signed) {
				t.Fatalf("%s: the edit changed nothing; the test is stale", name)
			}
			if _, _, err := release.Verify(ctx, []byte(bad), devStore, clk); err == nil {
				t.Errorf("%s: a manifest changed after signing still verifies", name)
			}
		}
	})

	t.Run("key not trusted", func(t *testing.T) {
		// A normal store refuses to register the published TEST-ONLY key at all, so a dev signature is never a release.
		strict := newStore(t, clk)
		if _, err := strict.RegisterKey(ctx, trust.PlatformApplicationID, dev.KeyID, pubOf(dev), "dev"); !errors.Is(err, trust.ErrTestOnlyKey) {
			t.Fatalf("a normal store registered the TEST-ONLY key: %v", err)
		}
		if _, _, err := release.Verify(ctx, signed, strict, clk); !errors.Is(err, trust.ErrUnknownKey) {
			t.Errorf("a dev-signed manifest verified without the key being trusted: %v", err)
		}
		// Signed by a key nobody registered.
		stranger := generatedKey(t, "stranger-2026")
		if _, _, err := release.Verify(ctx, mustSign(t, raw, stranger), devStore, clk); !errors.Is(err, trust.ErrUnknownKey) {
			t.Errorf("an unregistered key: %v", err)
		}
		// Registered, but for another application: the platform's keys are its own.
		other := newStore(t, clk)
		real := generatedKey(t, "alixai-release-test")
		if _, err := other.RegisterKey(ctx, "some.other.app", real.KeyID, pubOf(real), "x"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := release.Verify(ctx, mustSign(t, raw, real), other, clk); !errors.Is(err, trust.ErrUnknownKey) {
			t.Errorf("a key registered for another application: %v", err)
		}
	})

	t.Run("real key in a normal store, then revoked, then expired", func(t *testing.T) {
		real := generatedKey(t, "alixai-release-test")
		st := newStore(t, clk)
		if _, err := st.RegisterKey(ctx, trust.PlatformApplicationID, real.KeyID, pubOf(real), "release", trust.WithPurpose(trust.PurposeRelease)); err != nil {
			t.Fatal(err)
		}
		good := mustSign(t, raw, real)
		if _, _, err := release.Verify(ctx, good, st, clk); err != nil {
			t.Fatalf("a manifest signed with a real key in a normal store: %v", err)
		}
		late := clock.NewFake(testNow.Add(31 * 24 * time.Hour))
		if _, _, err := release.Verify(ctx, good, st, late); !errors.Is(err, trust.ErrSignatureExpired) || !contracts.HasCode(err, contracts.ErrExpired) {
			t.Errorf("an expired signature: %v", err)
		}
		if err := st.RevokeKey(ctx, trust.PlatformApplicationID, real.KeyID, "test"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := release.Verify(ctx, good, st, clk); !errors.Is(err, trust.ErrRevokedKey) {
			t.Errorf("a revoked key: %v", err)
		}
	})

	t.Run("key rotation: two signatures, either key is enough", func(t *testing.T) {
		oldKey, newKey := generatedKey(t, "alixai-release-old"), generatedKey(t, "alixai-release-new")
		both := mustSign(t, mustSign(t, raw, oldKey), newKey)
		onlyNew := newStore(t, clk)
		if _, err := onlyNew.RegisterKey(ctx, trust.PlatformApplicationID, newKey.KeyID, pubOf(newKey), "new", trust.WithPurpose(trust.PurposeRelease)); err != nil {
			t.Fatal(err)
		}
		if _, acc, err := release.Verify(ctx, both, onlyNew, clk); err != nil || acc.KeyID != newKey.KeyID {
			t.Errorf("a manifest signed by the old and the new key: %v, accepted %+v", err, acc)
		}
	})
}

// SEC-091: every model registry entry is in the manifest with its licence record and its pinned file, and a
// licence that is missing or altered does not survive.
func TestManifestIncludesModelLicences(t *testing.T) {
	reg, err := models.LoadFS(registry.FS)
	if err != nil {
		t.Fatal(err)
	}
	entries := reg.Entries()
	if len(entries) == 0 {
		t.Fatal("the embedded registry is empty")
	}
	m, err := buildReleaseManifest(testInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Signed.Models) != len(entries) {
		t.Fatalf("manifest lists %d models, the registry has %d", len(m.Signed.Models), len(entries))
	}
	byID := map[string]release.Model{}
	for _, me := range m.Signed.Models {
		byID[me.ID] = me
	}
	for _, e := range entries {
		got, ok := byID[e.ID]
		if !ok {
			t.Errorf("registry entry %s is not in the manifest", e.ID)
			continue
		}
		if got.Licence.SPDX != e.Licence.SPDX || got.Licence.URL != e.Licence.URL || got.Licence.RedistributionNotes != e.Licence.RedistributionNotes {
			t.Errorf("%s: licence record differs: %+v vs %+v", e.ID, got.Licence, e.Licence)
		}
		if got.Licence.SPDX == "" || got.Licence.URL == "" || got.Licence.RedistributionNotes == "" {
			t.Errorf("%s: the licence record is incomplete in the manifest: %+v", e.ID, got.Licence)
		}
		if got.Artifact.SHA256 != *e.Artifact.SHA256 || got.Artifact.SizeBytes != *e.Artifact.SizeBytes || got.Artifact.Filename != *e.Artifact.Filename || got.Artifact.Revision != *e.Artifact.Revision {
			t.Errorf("%s: the pinned artifact differs: %+v", e.ID, got.Artifact)
		}
		if got.Backend.Name != e.Backend.Name || got.Status != string(e.Status) {
			t.Errorf("%s: backend or status differs: %+v %s", e.ID, got.Backend, got.Status)
		}
	}

	raw, _ := marshalIndent(m)
	t.Run("a licence field emptied in the file is refused", func(t *testing.T) {
		emptied := strings.Replace(string(raw), `"spdx": "Apache-2.0"`, `"spdx": ""`, 1)
		if _, err := release.Parse([]byte(emptied)); err == nil || !strings.Contains(err.Error(), "licence record is incomplete") {
			t.Errorf("a manifest with an empty SPDX id parsed: %v", err)
		}
	})

	t.Run("a registry entry that pins no hash stops the manifest", func(t *testing.T) {
		src, err := os.ReadFile(filepath.Join(repoRoot(t), "models", "registry", "qwen3.5-2b-q4_k_m.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		const pin = "sha256: aaf42c8b7c3cab2bf3d69c355048d4a0ee9973d48f16c731c0520ee914699223"
		if !strings.Contains(string(src), pin) {
			t.Fatal("the registry entry no longer has the pin this test removes")
		}
		unpinned := strings.Replace(string(src), pin, "sha256: null", 1)
		weak, err := models.LoadFS(fstest.MapFS{"qwen3.5-2b-q4_k_m.yaml": {Data: []byte(unpinned)}})
		if err != nil {
			t.Skipf("the registry loader itself refuses an unpinned entry (%v); nothing is left for the manifest to refuse", err)
		}
		in := testInputs(t)
		in.Registry = weak
		if _, err := buildReleaseManifest(in); err == nil || !strings.Contains(err.Error(), "no pinned artifact") {
			t.Errorf("a registry entry with no hash went into a manifest: %v", err)
		}
	})
}

func TestBuildManifestRefusesUnpinnedOrMissingInputs(t *testing.T) {
	good := testInputs(t)
	root := repoRoot(t)
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	notGo := write("tool.bin", "not an executable")
	cases := map[string]func(in *manifestInputs){
		"a compose image with a tag only": func(in *manifestInputs) {
			in.ComposeFiles = []string{write("c.yaml", "services:\n  a:\n    image: docker.io/localai/localai:v4.10.0\n")}
		},
		"a Quadlet image with a tag only": func(in *manifestInputs) {
			in.QuadletFiles = []string{write("u.container", "[Container]\nImage=docker.io/localai/localai:v4.10.0\n")}
		},
		"a compose file that is not YAML": func(in *manifestInputs) { in.ComposeFiles = []string{write("c2.yaml", "services: [")} },
		"a compose file that is missing":  func(in *manifestInputs) { in.ComposeFiles = []string{filepath.Join(dir, "nope.yaml")} },
		"a backend with a tag only": func(in *manifestInputs) {
			in.BackendGallery = write("g.yaml", "- name: b\n  version: v1\n  license: mit\n  uri: quay.io/x/y:latest\n")
		},
		"a backend mirror with another digest": func(in *manifestInputs) {
			in.BackendGallery = write("g2.yaml", "- name: b\n  version: v1\n  license: mit\n  uri: quay.io/x/y@sha256:"+hex64("aa")+"\n  mirrors:\n    - docker.io/x/y@sha256:"+hex64("bb")+"\n")
		},
		"a built image with a tag only": func(in *manifestInputs) { in.BuiltImages = []string{"alixai=ghcr.io/example/alixai:1.2.3"} },
		"a built image with no name":    func(in *manifestInputs) { in.BuiltImages = []string{"ghcr.io/example/alixai@" + testImageDigest} },
		"a binary with no build info": func(in *manifestInputs) {
			in.Artifacts = append(in.Artifacts, artifactInput{release.KindBinary, notGo})
		},
		"an unknown artifact kind": func(in *manifestInputs) { in.Artifacts = append(in.Artifacts, artifactInput{"firmware", notGo}) },
		"a missing artifact file": func(in *manifestInputs) {
			in.Artifacts = append(in.Artifacts, artifactInput{release.KindArchive, filepath.Join(dir, "gone.tgz")})
		},
		"two artifacts of one name": func(in *manifestInputs) {
			in.Artifacts = append(in.Artifacts, artifactInput{release.KindChecksums, in.Artifacts[0].Path})
		},
		"no artifacts":   func(in *manifestInputs) { in.Artifacts = nil },
		"a bad version":  func(in *manifestInputs) { in.Version = "v1" },
		"a short commit": func(in *manifestInputs) { in.Commit = "abc123" },
		"no registry":    func(in *manifestInputs) { in.Registry = nil },
	}
	for name, mutate := range cases {
		in := good
		in.Artifacts = append([]artifactInput(nil), good.Artifacts...)
		mutate(&in)
		if _, err := buildReleaseManifest(in); err == nil {
			t.Errorf("%s: a manifest was built", name)
		}
	}
	// The unmodified inputs are fine, so each refusal above is about the change.
	if _, err := buildReleaseManifest(good); err != nil {
		t.Fatal(err)
	}
	// A compose service that is built from source has no image to pin, and is not an error.
	in := good
	in.ComposeFiles = []string{filepath.Join(root, "deploy", "container", "stack", "compose.noexternal.yaml")}
	if _, err := buildReleaseManifest(in); err != nil {
		t.Errorf("a compose file with only build services: %v", err)
	}
}

func TestParseReleaseManifestIsStrict(t *testing.T) {
	raw := testManifestBytes(t)
	for name, bad := range map[string]string{
		"an unknown property":       strings.Replace(string(raw), `"kind": "release-manifest"`, `"kind": "release-manifest", "extra": "x"`, 1),
		"a miscased property":       strings.Replace(string(raw), `"hash_algorithm"`, `"Hash_Algorithm"`, 1),
		"a duplicate key":           strings.Replace(string(raw), `"kind": "release-manifest"`, `"kind": "release-manifest", "kind": "release-manifest"`, 1),
		"another format":            strings.Replace(string(raw), release.Format, "alixai.release-manifest.v2", 1),
		"another purpose":           strings.Replace(string(raw), `"kind": "release-manifest"`, `"kind": "package-manifest"`, 1),
		"another application":       strings.Replace(string(raw), `"application_id": "alixai.platform"`, `"application_id": "demo-library"`, 1),
		"a null where a list goes":  strings.Replace(string(raw), `"backends": [`, `"backends": null, "x": [`, 1),
		"a float":                   strings.Replace(string(raw), `"size_bytes": 1280835840`, `"size_bytes": 1280835840.5`, 1),
		"not JSON":                  "{",
		"an unsorted artifact list": swapFirstTwoArtifacts(t, raw),
	} {
		if bad == string(raw) {
			t.Errorf("%s: the edit changed nothing", name)
			continue
		}
		if _, err := release.Parse([]byte(bad)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := release.Parse(raw); err != nil {
		t.Errorf("the unedited manifest: %v", err)
	}
}

func swapFirstTwoArtifacts(t *testing.T, raw []byte) string {
	t.Helper()
	m, err := release.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	m.Signed.Artifacts[0], m.Signed.Artifacts[1] = m.Signed.Artifacts[1], m.Signed.Artifacts[0]
	out, err := marshalIndent(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestVerifyArtifactFiles(t *testing.T) {
	dir, inputs := releaseFiles(t)
	m, err := buildReleaseManifest(manifestInputs{Version: "1.2.3", Commit: testCommit, CreatedAt: testNow, Artifacts: inputs, Registry: testInputs(t).Registry})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.VerifyFiles(dir, ""); err != nil {
		t.Fatalf("the files the manifest was built from: %v", err)
	}
	// A manifest sitting in the directory is not an unlisted file.
	if err := os.WriteFile(filepath.Join(dir, "release-manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.VerifyFiles(dir, "release-manifest.json"); err != nil {
		t.Errorf("the manifest itself was counted as unlisted: %v", err)
	}
	if err := m.VerifyFiles(dir, ""); err == nil || !strings.Contains(err.Error(), "not in the manifest") {
		t.Errorf("an unlisted file went unnoticed: %v", err)
	}
	os.Remove(filepath.Join(dir, "release-manifest.json"))

	if err := os.WriteFile(inputs[0].Path, []byte("replaced archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.VerifyFiles(dir, ""); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("a replaced file went unnoticed: %v", err)
	}
	os.Remove(inputs[0].Path)
	if err := m.VerifyFiles(dir, ""); err == nil {
		t.Error("a missing file went unnoticed")
	}
}

// The command line, end to end: build the unsigned manifest, sign it, verify it. The dev key signs and verifies
// only with the explicit flags; a real key file goes through -key and -pubkey, and the TEST-ONLY key file is
// refused where a real key is expected.
func TestReleaseCommandsEndToEnd(t *testing.T) {
	root := repoRoot(t)
	dir, files := releaseFiles(t)
	work := t.TempDir()
	manifest := filepath.Join(work, "release-manifest.json")

	args := []string{"manifest", "-C", root, "-version", "1.2.3", "-commit", testCommit, "-created-at", "2026-10-03T12:00:00Z", "-out", manifest,
		"-image", "alixai=ghcr.io/example/alixai:1.2.3@" + testImageDigest}
	for _, f := range files {
		args = append(args, "-artifact", f.Kind+"="+f.Path)
	}
	if code, out, errOut := runTool(t, args...); code != 0 {
		t.Fatalf("manifest exit %d\n%s\n%s", code, out, errOut)
	}

	if code, _, errOut := runTool(t, "verify", "-manifest", manifest, "-allow-dev-key"); code != 1 || !strings.Contains(errOut, "no signature") {
		t.Errorf("verifying an unsigned manifest: exit %d, %q", code, errOut)
	}
	if code, _, _ := runTool(t, "verify", "-manifest", manifest); code != 2 {
		t.Errorf("verify with no trusted key exits %d, want 2", code)
	}

	// -dev-key
	devSigned := filepath.Join(work, "dev-signed.json")
	code, out, errOut := runTool(t, "sign", "-manifest", manifest, "-out", devSigned, "-dev-key")
	if code != 0 || !strings.Contains(errOut, "TEST-ONLY") {
		t.Fatalf("sign -dev-key: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, _, errOut := runTool(t, "verify", "-manifest", devSigned, "-pubkey", devkeys.KeyID+"="+writePub(t, work, devkeys.PublicKey())); code != 1 || !strings.Contains(errOut, "TEST-ONLY") {
		t.Errorf("dev-signed manifest verified without -allow-dev-key: exit %d, %q", code, errOut)
	}
	code, out, errOut = runTool(t, "verify", "-manifest", devSigned, "-allow-dev-key", "-dir", dir)
	if code != 0 || !strings.HasPrefix(out, "VERIFIED release 1.2.3") || !strings.Contains(errOut, "TEST-ONLY") {
		t.Errorf("dev-signed manifest with -allow-dev-key: exit %d, %q, %q", code, out, errOut)
	}
	if err := os.WriteFile(files[0].Path, []byte("replaced"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runTool(t, "verify", "-manifest", devSigned, "-allow-dev-key", "-dir", dir); code != 1 || !strings.Contains(errOut, "do not match") {
		t.Errorf("a replaced release file: exit %d, %q", code, errOut)
	}

	// a real key file
	_, priv, err := trust.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyDir := filepath.Join(work, "keys")
	privPath, pubPath, err := trust.WriteKeyFiles(keyDir, "alixai-release-test", priv)
	if err != nil {
		t.Fatal(err)
	}
	realSigned := filepath.Join(work, "real-signed.json")
	if code, out, errOut := runTool(t, "sign", "-manifest", manifest, "-out", realSigned, "-key", privPath, "-key-id", "alixai-release-test"); code != 0 {
		t.Fatalf("sign -key: exit %d, %q, %q", code, out, errOut)
	}
	code, out, errOut = runTool(t, "verify", "-manifest", realSigned, "-pubkey", "alixai-release-test="+pubPath)
	if code != 0 || strings.Contains(errOut, "TEST-ONLY") {
		t.Errorf("real-signed manifest: exit %d, %q, %q", code, out, errOut)
	}
	if code, _, _ := runTool(t, "verify", "-manifest", realSigned, "-pubkey", "another-key="+pubPath); code != 1 {
		t.Errorf("a manifest verified under a key id it was not signed with: exit %d", code)
	}

	// refusals
	devKeyFile, err := filepath.Abs(filepath.Join(root, "tools", "devkeys", "test-only-publisher.key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runTool(t, "sign", "-manifest", manifest, "-out", filepath.Join(work, "x.json"), "-key", devKeyFile, "-key-id", "alixai-release-2026"); code != 1 || !strings.Contains(errOut, "TEST-ONLY") {
		t.Errorf("the TEST-ONLY key file was accepted as a real key: exit %d, %q", code, errOut)
	}
	if code, _, _ := runTool(t, "sign", "-manifest", manifest); code != 2 {
		t.Errorf("sign with no key exits %d, want 2", code)
	}
	if code, _, _ := runTool(t, "sign", "-manifest", manifest, "-dev-key", "-key-id", "x"); code != 2 {
		t.Errorf("sign -dev-key with -key-id exits %d, want 2", code)
	}
	if code, _, _ := runTool(t, "manifest", "-C", root, "-version", "1.2.3"); code != 2 {
		t.Errorf("manifest with no commit exits %d, want 2", code)
	}
	if code, _, _ := runTool(t, "no-such-command"); code != 2 {
		t.Errorf("an unknown command exits %d, want 2", code)
	}
}

func writePub(t *testing.T, dir string, pub ed25519.PublicKey) string {
	t.Helper()
	p := filepath.Join(dir, "pub-"+trust.Fingerprint(pub)[:8]+".pub")
	if err := os.WriteFile(p, []byte(trust.EncodePublicKey(pub)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
