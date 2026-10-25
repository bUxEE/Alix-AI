package trust

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	schemasRoot     = "../../schemas"
	sigSchemaID     = "urn:alixai:schema:application-package:signature-manifest:v1"
	sigExamplesRoot = schemasRoot + "/application-package/examples/signature-manifest"
)

var urnRE = regexp.MustCompile(`^urn:alixai:schema:([a-z0-9-]+):([a-z0-9-]+):v[1-9][0-9]*$`)

// urnLoader resolves urn:alixai:schema:<area>:<name>:v<N> to the schema files
// in the repository, as tools/schemacheck does.
type urnLoader struct{}

func (urnLoader) Load(url string) (any, error) {
	m := urnRE.FindStringSubmatch(url)
	if m == nil {
		return nil, fmt.Errorf("cannot resolve %q", url)
	}
	f, err := os.Open(filepath.Join(schemasRoot, m[1], m[2]+".schema.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return jsonschema.UnmarshalJSON(f)
}

func compileSignatureSchema(t testing.TB) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	c.UseLoader(urnLoader{})
	sch, err := c.Compile(sigSchemaID)
	if err != nil {
		t.Fatalf("compile %s: %v", sigSchemaID, err)
	}
	return sch
}

func validateAgainstSchema(t testing.TB, sch *jsonschema.Schema, raw []byte) error {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	return sch.Validate(doc)
}

func jsonFiles(t testing.TB, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	if len(out) == 0 {
		t.Fatalf("no examples in %s", dir)
	}
	return out
}

// TestManifestMatchesTheSchema: what this package writes is valid against
// signature-manifest.schema.json, and what it reads agrees with the schema's
// own examples: every valid example parses, every invalid example is refused.
// A drift between the Go code and the M1-01 schema fails here.
func TestManifestMatchesTheSchema(t *testing.T) {
	sch := compileSignatureSchema(t)

	t.Run("the manifest we write validates", func(t *testing.T) {
		f := newFixture(t)
		raw := mustReadFile(t, filepath.Join(f.dir, filepath.FromSlash(SignatureManifestPath)))
		if err := validateAgainstSchema(t, sch, raw); err != nil {
			t.Fatalf("schema rejects our manifest: %v\n%s", err, raw)
		}
		if _, err := ParseSignatureManifest(raw); err != nil {
			t.Fatalf("we reject our own manifest: %v", err)
		}
	})

	t.Run("valid examples", func(t *testing.T) {
		for _, p := range jsonFiles(t, sigExamplesRoot+"/valid") {
			raw := mustReadFile(t, p)
			if err := validateAgainstSchema(t, sch, raw); err != nil {
				t.Errorf("%s: the schema rejects its own valid example: %v", p, err)
			}
			if _, err := ParseSignatureManifest(raw); err != nil {
				t.Errorf("%s: parser rejects a valid example: %v", p, err)
			}
		}
	})

	t.Run("invalid examples are refused by the parser", func(t *testing.T) {
		for _, p := range jsonFiles(t, sigExamplesRoot+"/invalid") {
			t.Run(filepath.Base(p), func(t *testing.T) {
				raw := mustReadFile(t, p)
				if err := validateAgainstSchema(t, sch, raw); err == nil {
					t.Fatalf("the schema accepts its own invalid example; fix the example, not the parser")
				}
				_, err := ParseSignatureManifest(raw)
				if err == nil {
					t.Fatalf("parser accepted an invalid example")
				}
				wantFail(t, err, contracts.ErrSchemaInvalid, ErrMalformedManifest)
			})
		}
	})
}

// TestParseSignatureManifestIsStrict covers what the schema examples do not:
// shapes encoding/json would accept silently.
func TestParseSignatureManifestIsStrict(t *testing.T) {
	f := newFixture(t)
	good := mustReadFile(t, filepath.Join(f.dir, filepath.FromSlash(SignatureManifestPath)))
	if _, err := ParseSignatureManifest(good); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(good, &doc); err != nil {
		t.Fatal(err)
	}
	clone := func() map[string]any {
		var c map[string]any
		_ = json.Unmarshal(good, &c)
		return c
	}
	sigOf := func(c map[string]any) map[string]any { return c["signatures"].([]any)[0].(map[string]any) }
	signedOf := func(c map[string]any) map[string]any { return c["signed"].(map[string]any) }
	fileOf := func(c map[string]any, i int) map[string]any { return signedOf(c)["files"].([]any)[i].(map[string]any) }

	mutations := map[string]func(c map[string]any){
		"unknown top-level property":      func(c map[string]any) { c["extra"] = 1 },
		"unknown property in signed":      func(c map[string]any) { signedOf(c)["extra"] = "x" },
		"unknown property in a file":      func(c map[string]any) { fileOf(c, 0)["mode"] = 420 },
		"unknown property in a signature": func(c map[string]any) { sigOf(c)["comment"] = "x" },
		"wrong format":                    func(c map[string]any) { c["format"] = "alixai.signature-manifest/v2" },
		"missing signed":                  func(c map[string]any) { delete(c, "signed") },
		"null signatures":                 func(c map[string]any) { c["signatures"] = nil },
		"empty signatures":                func(c map[string]any) { c["signatures"] = []any{} },
		"five signatures": func(c map[string]any) {
			s := c["signatures"].([]any)
			for i := 0; i < 4; i++ {
				cp := map[string]any{}
				for k, v := range s[0].(map[string]any) {
					cp[k] = v
				}
				cp["key_id"] = fmt.Sprintf("k%d", i)
				s = append(s, cp)
			}
			c["signatures"] = s
		},
		"same key twice": func(c map[string]any) {
			c["signatures"] = append(c["signatures"].([]any), sigOf(c))
		},
		"empty files":        func(c map[string]any) { signedOf(c)["files"] = []any{} },
		"files out of order": func(c map[string]any) { f := signedOf(c)["files"].([]any); f[0], f[1] = f[1], f[0] },
		"duplicate path": func(c map[string]any) {
			f := signedOf(c)["files"].([]any)
			signedOf(c)["files"] = append(f, f[len(f)-1])
		},
		"case-folded duplicate path": func(c map[string]any) {
			f := signedOf(c)["files"].([]any)
			e := map[string]any{"path": "zza.yaml", "sha256": fileOf(c, 0)["sha256"], "size": 1}
			e2 := map[string]any{"path": "zzA.yaml", "sha256": fileOf(c, 0)["sha256"], "size": 1}
			signedOf(c)["files"] = append(f, e2, e)
		},
		"uppercase digest":            func(c map[string]any) { fileOf(c, 0)["sha256"] = strings.ToUpper(fileOf(c, 0)["sha256"].(string)) },
		"short digest":                func(c map[string]any) { fileOf(c, 0)["sha256"] = "abc" },
		"negative size":               func(c map[string]any) { fileOf(c, 0)["size"] = -1 },
		"size as a string":            func(c map[string]any) { fileOf(c, 0)["size"] = "12" },
		"size as a float":             func(c map[string]any) { fileOf(c, 0)["size"] = 1.5 },
		"size beyond 1 TiB":           func(c map[string]any) { fileOf(c, 0)["size"] = int64(1)<<40 + 1 },
		"hash algorithm sha1":         func(c map[string]any) { signedOf(c)["hash_algorithm"] = "sha1" },
		"version is not semver":       func(c map[string]any) { signedOf(c)["package_version"] = "1.0" },
		"application id upper case":   func(c map[string]any) { signedOf(c)["application_id"] = "Demo-Library" },
		"created_at with an offset":   func(c map[string]any) { signedOf(c)["created_at"] = "2026-10-02T09:00:00+02:00" },
		"created_at not a date":       func(c map[string]any) { signedOf(c)["created_at"] = "2026-02-30T09:00:00Z" },
		"signed_at without Z":         func(c map[string]any) { sigOf(c)["signed_at"] = "2026-10-02T09:00:00" },
		"expires_at missing":          func(c map[string]any) { delete(sigOf(c), "expires_at") },
		"expires_at before signed_at": func(c map[string]any) { sigOf(c)["expires_at"] = "2020-01-01T00:00:00Z" },
		"algorithm rsa":               func(c map[string]any) { sigOf(c)["algorithm"] = "rsa" },
		"algorithm in upper case":     func(c map[string]any) { sigOf(c)["algorithm"] = "Ed25519" },
		"signature too short":         func(c map[string]any) { sigOf(c)["signature"] = "AAAA" },
		"signature not base64":        func(c map[string]any) { sigOf(c)["signature"] = strings.Repeat("!", 88) },
		"signature without padding":   func(c map[string]any) { sigOf(c)["signature"] = strings.TrimRight(sigOf(c)["signature"].(string), "=") },
		"signature url-safe alphabet": func(c map[string]any) {
			sigOf(c)["signature"] = strings.NewReplacer("+", "-", "/", "_").Replace(strings.Repeat("+/", 43) + "==")
		},
		"key_id with a space":      func(c map[string]any) { sigOf(c)["key_id"] = "has space" },
		"key_id wildcard":          func(c map[string]any) { sigOf(c)["key_id"] = "*" },
		"key_id empty":             func(c map[string]any) { sigOf(c)["key_id"] = "" },
		"signed is an array":       func(c map[string]any) { c["signed"] = []any{} },
		"a file entry is a string": func(c map[string]any) { signedOf(c)["files"] = []any{"alixai.yaml"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			c := clone()
			mutate(c)
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ParseSignatureManifest(raw)
			if err == nil {
				t.Fatalf("accepted: %s", raw)
			}
			wantFail(t, err, contracts.ErrSchemaInvalid, ErrMalformedManifest)
		})
	}

	t.Run("signature with non-zero trailing bits is a second spelling and is refused", func(t *testing.T) {
		c := clone()
		sig := sigOf(c)["signature"].(string)
		// The last base64 digit before "==" carries 4 unused bits. Changing
		// them leaves the decoded bytes alone but changes the text.
		alt := sig[:85] + string(altDigit(sig[85])) + "=="
		sigOf(c)["signature"] = alt
		raw, _ := json.Marshal(c)
		if _, err := ParseSignatureManifest(raw); err == nil {
			t.Error("a malleable base64 spelling of the signature was accepted")
		}
	})

	t.Run("text-level problems", func(t *testing.T) {
		dupKey := strings.Replace(string(good), `"format"`, `"format": "x", "format"`, 1)
		upperKey := strings.Replace(string(good), `"format"`, `"Format"`, 1)
		for name, raw := range map[string]string{
			"duplicate key":             dupKey,
			"differently cased key":     upperKey,
			"trailing data":             string(good) + "{}",
			"trailing garbage":          string(good) + " x",
			"not JSON":                  "format: yaml",
			"empty":                     "",
			"array":                     "[]",
			"null":                      "null",
			"byte order mark":           "\xef\xbb\xbf" + string(good),
			"invalid UTF-8 in a string": strings.Replace(string(good), "demo-library", "demo\xfflibrary", 1),
			"oversized":                 string(good) + strings.Repeat(" ", MaxSignatureManifestBytes),
		} {
			if _, err := ParseSignatureManifest([]byte(raw)); err == nil {
				t.Errorf("%s: accepted", name)
			} else if !errors.Is(err, ErrMalformedManifest) {
				t.Errorf("%s: error does not wrap ErrMalformedManifest: %v", name, err)
			}
		}
	})

	t.Run("ReadSignatureManifest", func(t *testing.T) {
		if _, err := ReadSignatureManifest(fstest.MapFS{}); !errors.Is(err, ErrUnsigned) || contracts.CodeOf(err) != contracts.ErrUnauthenticated {
			t.Errorf("missing file: %v", err)
		}
		dirFile := fstest.MapFS{SignatureManifestPath: &fstest.MapFile{Mode: fs.ModeDir}}
		if _, err := ReadSignatureManifest(dirFile); !errors.Is(err, ErrMalformedManifest) {
			t.Errorf("a directory where the file belongs: %v", err)
		}
		link := fstest.MapFS{SignatureManifestPath: &fstest.MapFile{Data: good, Mode: fs.ModeSymlink}}
		if _, err := ReadSignatureManifest(link); err == nil {
			t.Error("a symbolic link was read as the signature manifest")
		}
	})
}

// altDigit returns another base64 digit with the same high bits as c but
// different low four bits.
func altDigit(c byte) byte {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	i := strings.IndexByte(alphabet, c)
	return alphabet[(i&^0xf)|((i+1)&0xf)]
}
