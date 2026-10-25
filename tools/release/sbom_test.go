package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testImageDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func mustReadBinary(t *testing.T, name string) binaryInfo {
	t.Helper()
	b, err := readBinary(name, testExe(t))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func repoDockerfile(t *testing.T) dockerfileImages {
	t.Helper()
	f, err := os.Open(filepath.Join(repoRoot(t), "deploy", "container", "alixai", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	imgs, err := parseDockerfileImages(f)
	if err != nil {
		t.Fatal(err)
	}
	return imgs
}

// SEC-090: the SBOM of a binary and the SBOM of the service image are valid CycloneDX 1.6 documents (checked
// against the official JSON schema), and they say what they claim: the binary's hash, the modules linked into it,
// the base image by digest.
func TestSBOMValidCycloneDX(t *testing.T) {
	bin := mustReadBinary(t, "alixai")
	bases := repoDockerfile(t)

	binDoc, err := binarySBOM(bin, "1.2.3", sbomOptions{})
	if err != nil {
		t.Fatal(err)
	}
	imgDoc, err := imageSBOM(imageSpec{Name: "alixai", Reference: "ghcr.io/example/alixai", Digest: testImageDigest, Version: "1.2.3"}, bases, []binaryInfo{bin}, sbomOptions{Timestamp: "2026-10-03T12:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}

	for name, doc := range map[string]*bom{"binary": binDoc, "image": imgDoc} {
		raw, err := marshalIndent(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateCycloneDX(raw); err != nil {
			t.Fatalf("%s SBOM is not valid CycloneDX: %v", name, err)
		}
		again, _ := marshalIndent(doc)
		if string(raw) != string(again) {
			t.Errorf("%s SBOM is not deterministic", name)
		}
		var generic map[string]any
		if err := json.Unmarshal(raw, &generic); err != nil {
			t.Fatal(err)
		}
		if generic["bomFormat"] != "CycloneDX" || generic["specVersion"] != "1.6" {
			t.Errorf("%s SBOM is not CycloneDX 1.6: %v %v", name, generic["bomFormat"], generic["specVersion"])
		}
		if !strings.HasPrefix(generic["serialNumber"].(string), "urn:uuid:") {
			t.Errorf("%s SBOM has no urn:uuid serial number", name)
		}
	}

	// Binary SBOM content: its own hash and the modules it was linked from, with the standard library.
	if got := binDoc.Metadata.Component.Hashes; len(got) != 1 || got[0].Alg != "SHA-256" || got[0].Content != bin.SHA256 {
		t.Errorf("binary component hash = %+v, want SHA-256 %s", got, bin.SHA256)
	}
	refs := map[string]bool{}
	for _, c := range binDoc.Components {
		refs[c.BOMRef] = true
	}
	var sawJSONSchema bool
	for ref := range refs {
		sawJSONSchema = sawJSONSchema || strings.HasPrefix(ref, "pkg:golang/github.com/santhosh-tekuri/jsonschema/v6@v")
	}
	if !sawJSONSchema {
		t.Errorf("the jsonschema module this tool links is not in the SBOM; components: %d", len(binDoc.Components))
	}
	if !strings.Contains(strings.Join(keysOf(refs), " "), "pkg:golang/std@") {
		t.Error("the Go standard library is not in the SBOM")
	}
	if len(binDoc.Components) != len(bin.Info.Deps)+1 {
		t.Errorf("%d components for %d modules plus the standard library", len(binDoc.Components), len(bin.Info.Deps))
	}

	// Image SBOM content: the base image of the Dockerfile by its digest, the binary, no builder in the image.
	runtimeDigest := strings.TrimPrefix(bases.Runtime.Digest, "sha256:")
	var sawBase, sawBuilderExcluded, sawBinary bool
	for _, c := range imgDoc.Components {
		switch {
		case c.Type == "container" && len(c.Hashes) == 1 && c.Hashes[0].Content == runtimeDigest:
			sawBase = c.Scope == "required"
		case c.Type == "container" && c.Scope == "excluded":
			sawBuilderExcluded = true
		case c.BOMRef == "bin/alixai":
			sawBinary = len(c.Hashes) == 1 && c.Hashes[0].Content == bin.SHA256
		}
	}
	if !sawBase || !sawBuilderExcluded || !sawBinary {
		t.Errorf("image SBOM misses a part: base=%v builder=%v binary=%v", sawBase, sawBuilderExcluded, sawBinary)
	}
	if got := imgDoc.Metadata.Component.Hashes; len(got) != 1 || "sha256:"+got[0].Content != testImageDigest {
		t.Errorf("image component hash = %+v, want %s", got, testImageDigest)
	}
	if !strings.Contains(imgDoc.Metadata.Component.PURL, "repository_url=ghcr.io/example/alixai") {
		t.Errorf("image purl = %q", imgDoc.Metadata.Component.PURL)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The validator is not vacuous: documents that break the schema are refused.
func TestSBOMValidatorRefusesInvalidDocuments(t *testing.T) {
	doc, err := binarySBOM(mustReadBinary(t, "alixai"), "1.2.3", sbomOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := marshalIndent(doc)
	if err := validateCycloneDX(raw); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(m map[string]any){
		"no specVersion":       func(m map[string]any) { delete(m, "specVersion") },
		"unknown spec version": func(m map[string]any) { m["specVersion"] = "9.9" },
		"unknown top property": func(m map[string]any) { m["surprise"] = true },
		"component type":       func(m map[string]any) { m["components"].([]any)[0].(map[string]any)["type"] = "gadget" },
		"short hash": func(m map[string]any) {
			m["metadata"].(map[string]any)["component"].(map[string]any)["hashes"] = []any{map[string]any{"alg": "SHA-256", "content": "abc"}}
		},
		"bad serial number": func(m map[string]any) { m["serialNumber"] = "not-a-uuid" },
		"unknown licence id": func(m map[string]any) {
			m["metadata"].(map[string]any)["component"].(map[string]any)["licenses"] = []any{map[string]any{"license": map[string]any{"id": "Not-A-Licence"}}}
		},
	}
	for name, mutate := range mutations {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		bad, _ := json.Marshal(m)
		if err := validateCycloneDX(bad); err == nil {
			t.Errorf("%s: an invalid document validated", name)
		}
	}
	if err := validateCycloneDX([]byte("{")); err == nil {
		t.Error("a document that is not JSON validated")
	}
}

func TestParseDockerfileImages(t *testing.T) {
	d := testImageDigest
	ok := "FROM x\nARG GO_IMAGE=docker.io/library/golang:1.27.0-alpine@" + d + "\r\nARG RUNTIME_IMAGE=gcr.io/distroless/static-debian13:nonroot@" + d + "\n"
	imgs, err := parseDockerfileImages(strings.NewReader(ok))
	if err != nil || imgs.Runtime.Repo != "gcr.io/distroless/static-debian13" || imgs.Builder.Tag != "1.27.0-alpine" {
		t.Fatalf("got %+v, %v", imgs, err)
	}
	for name, in := range map[string]string{
		"no runtime image": "ARG GO_IMAGE=docker.io/library/golang:1@" + d + "\n",
		"no builder image": "ARG RUNTIME_IMAGE=gcr.io/d/s:nonroot@" + d + "\n",
		"runtime unpinned": "ARG GO_IMAGE=a/b@" + d + "\nARG RUNTIME_IMAGE=gcr.io/distroless/static-debian13:nonroot\n",
		"empty":            "",
	} {
		if _, err := parseDockerfileImages(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestImageSBOMRefusesBadInput(t *testing.T) {
	bin := mustReadBinary(t, "alixai")
	bases := repoDockerfile(t)
	for name, c := range map[string]struct {
		img  imageSpec
		bins []binaryInfo
	}{
		"no name":    {imageSpec{Version: "1"}, []binaryInfo{bin}},
		"no binary":  {imageSpec{Name: "a", Version: "1"}, nil},
		"bad digest": {imageSpec{Name: "a", Version: "1", Digest: "sha256:zz"}, []binaryInfo{bin}},
	} {
		if _, err := imageSBOM(c.img, bases, c.bins, sbomOptions{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := binarySBOM(bin, "1", sbomOptions{Timestamp: "yesterday"}); err == nil {
		t.Error("a timestamp that is not RFC 3339 was accepted")
	}
}

func TestSBOMCommands(t *testing.T) {
	dir := t.TempDir()
	exe := testExe(t)

	binOut := filepath.Join(dir, "alixai.cdx.json")
	if code, out, errOut := runTool(t, "sbom", "binary", "-in", exe, "-name", "alixai", "-version", "1.2.3", "-out", binOut); code != 0 {
		t.Fatalf("sbom binary exit %d\n%s\n%s", code, out, errOut)
	}
	imgOut := filepath.Join(dir, "image.cdx.json")
	code, out, errOut := runTool(t, "sbom", "image", "-C", repoRoot(t), "-binary", "alixai="+exe, "-version", "1.2.3", "-reference", "ghcr.io/example/alixai", "-digest", testImageDigest, "-out", imgOut)
	if code != 0 {
		t.Fatalf("sbom image exit %d\n%s\n%s", code, out, errOut)
	}
	for _, f := range []string{binOut, imgOut} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateCycloneDX(raw); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}

	if code, _, _ := runTool(t, "sbom"); code != 2 {
		t.Errorf("sbom with no mode exits %d, want 2", code)
	}
	if code, _, _ := runTool(t, "sbom", "binary", "-version", "1"); code != 2 {
		t.Errorf("sbom binary without -in exits %d, want 2", code)
	}
	notGo := filepath.Join(dir, "notgo.bin")
	if err := os.WriteFile(notGo, []byte("not an executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runTool(t, "sbom", "binary", "-in", notGo, "-version", "1"); code != 1 {
		t.Errorf("sbom of a file with no Go build information exits %d, want 1", code)
	}
}
