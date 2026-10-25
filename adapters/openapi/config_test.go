package openapi

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/adapters/openapi/testdata/fixture"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// adapters/openapi is tier 4 of docs/architecture/module-map.md: it may import
// the base packages, internal/adapters and internal/egress. It never imports an
// application package, another concrete adapter or LocalAI.
func TestImportsStayWithinTheModuleMap(t *testing.T) {
	const modulePath = "github.com/buxee/alix-ai/"
	allowed := map[string]bool{
		"pkg/contracts": true, "internal/clock": true, "internal/ids": true, "internal/config": true, "internal/log": true,
		"internal/version": true, "internal/egress": true, "internal/adapters": true,
	}
	files := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		isTest := strings.HasSuffix(path, "_test.go") || strings.Contains(filepath.ToSlash(path), "testdata/")
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			rel := strings.TrimPrefix(p, modulePath)
			if strings.HasPrefix(rel, "pkg/contracts/gen/") || rel == "adapters/openapi" || strings.HasPrefix(rel, "adapters/openapi/testdata/") {
				continue
			}
			if isTest && (strings.HasPrefix(rel, "internal/adapters/") || strings.HasPrefix(rel, "tests/conformance/")) {
				continue // the fake application and the shared suite
			}
			if !allowed[rel] {
				t.Errorf("%s imports %s, which module-map.md does not allow for adapters/openapi", path, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 10 {
		t.Fatalf("scanned only %d files", files)
	}
}

func TestParseAdapterConfig(t *testing.T) {
	ok, err := ParseAdapterConfig([]byte(`# comment
server: fixture-api
auth: {header: X-Api-Key, scheme: ""}
identity: {header: X-Who}
idempotency: {header: X-Key}
management:
  describe: describeApplication
  health: getHealth
  inspect: inspectApplication
  provisioning: [createTenant, cleanupTenant]
`), "fixture-api")
	if err != nil {
		t.Fatal(err)
	}
	if ok.Auth == nil || ok.Auth.Header != "X-Api-Key" || ok.Auth.Scheme == nil || *ok.Auth.Scheme != "" ||
		ok.Identity.Header != "X-Who" || ok.Idempotency.Header != "X-Key" || ok.Management.Describe != "describeApplication" ||
		len(ok.Management.Provisioning) != 2 {
		t.Errorf("parsed = %+v", ok)
	}
	if empty, err := ParseAdapterConfig([]byte("server: fixture-api\n"), "fixture-api"); err != nil || empty.Auth != nil {
		t.Errorf("an empty configuration: %+v, %v", empty, err)
	}
	if c, err := ParseAdapterConfig(nil, "any-endpoint"); err != nil || c.Server != "" {
		t.Errorf("no file content: %+v, %v", c, err)
	}
	for name, bad := range map[string]string{
		"an unknown member":                   "server: fixture-api\nextra: 1\n",
		"an unknown nested member":            "server: fixture-api\nmanagement:\n  reboot: now\n",
		"another server":                      "server: other-api\n",
		"a second document":                   "server: fixture-api\n---\nserver: x\n",
		"not yaml":                            "management: [unterminated",
		"a header with a space":               "identity: {header: 'X Who'}\n",
		"a reserved identity header":          "identity: {header: Content-Type}\n",
		"a reserved idempotency header":       "idempotency: {header: Host}\n",
		"a reserved auth header":              "auth: {header: Content-Length}\n",
		"a scheme with a space":               "auth: {scheme: 'Bearer token'}\n",
		"a malformed operationId":             "management: {describe: 'has space'}\n",
		"one operation in two roles":          "management: {describe: x, health: x}\n",
		"a provisioning repeat":               "management: {provisioning: [a, a]}\n",
		"a provisioning that is a management": "management: {health: h, provisioning: [h]}\n",
	} {
		if _, err := ParseAdapterConfig([]byte(bad), "fixture-api"); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: %v, want validation_failed", name, err)
		}
	}
}

// A configuration the adapter cannot trust is a build error, never a silently
// wider or weaker adapter.
func TestNewRefusesWhatItCannotTrust(t *testing.T) {
	h := fixture.Start(t, fake.NewApp())
	base := func() Config { return fixtureConfig(t, h) }
	hash := strings.Repeat("0", 64)

	// A one-operation document, to be broken in one way at a time.
	doc := func(op string) []byte {
		return []byte("openapi: 3.0.3\ninfo: {title: t, version: '1'}\npaths:\n  /items/{id}:\n    get:\n      operationId: getItem\n" + op)
	}
	okOp := "      parameters:\n        - {name: id, in: path, required: true, schema: {type: string}}\n" +
		"      responses:\n        '200': {description: ok, content: {application/json: {schema: {type: object}}}}\n"
	withRead := func(spec []byte) Config {
		c := base()
		c.Endpoints[0].Spec = spec
		c.Endpoints[0].CredentialRef = ""
		c.Endpoints[0].Settings = mustSettings(t, "server: fixture-api\n")
		c.Capabilities = c.Capabilities[:1]
		c.Capabilities[0].Operations = map[adapters.Operation]Binding{adapters.OpRead: {Endpoint: testEP, OperationID: "getItem", Arguments: map[string]string{"item_id": "id"}}}
		return c
	}
	if a, err := New(withRead(doc(okOp))); err != nil {
		t.Fatalf("the baseline document is refused: %v", err)
	} else {
		_ = a.Close()
	}

	for name, mutate := range map[string]func() Config{
		"no application ID":            func() Config { c := base(); c.ApplicationID = ""; return c },
		"no endpoints":                 func() Config { c := base(); c.Endpoints = nil; return c },
		"an endpoint declared twice":   func() Config { c := base(); c.Endpoints = append(c.Endpoints, c.Endpoints[0]); return c },
		"a malformed endpoint ID":      func() Config { c := base(); c.Endpoints[0].ID = "Has Space"; return c },
		"a non-http base URL":          func() Config { c := base(); c.Endpoints[0].BaseURL = "ftp://127.0.0.1/api"; return c },
		"a base URL with credentials":  func() Config { c := base(); c.Endpoints[0].BaseURL = "http://user:pw@127.0.0.1/api"; return c },
		"a base URL with a query":      func() Config { c := base(); c.Endpoints[0].BaseURL = "http://127.0.0.1/api?key=1"; return c },
		"a base URL with no host":      func() Config { c := base(); c.Endpoints[0].BaseURL = "http:///api"; return c },
		"no document":                  func() Config { c := base(); c.Endpoints[0].Spec = nil; return c },
		"a document larger than read":  func() Config { c := base(); c.Endpoints[0].Spec = make([]byte, maxSpecBytes+1); return c },
		"a malformed contract pin":     func() Config { c := base(); c.Endpoints[0].ContractSHA256 = "ABC"; return c },
		"a pin that does not match":    func() Config { c := base(); c.Endpoints[0].ContractSHA256 = hash; return c },
		"a malformed credential ref":   func() Config { c := base(); c.Endpoints[0].CredentialRef = "my-password-hunter2"; return c },
		"a credential and no provider": func() Config { c := base(); c.Secrets = nil; return c },
		"auth without a credential": func() Config {
			c := base()
			c.Endpoints[0].CredentialRef = ""
			return c // the fixture's adapter configuration sets auth
		},
		"two headers that are one": func() Config {
			c := base()
			c.Endpoints[0].Settings.Identity.Header = "idempotency-key"
			return c
		},
		"a negative timeout": func() Config {
			c := base()
			c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: testEP, OperationID: fixture.OpGet, Timeout: -1}
			return c
		},
		"renames that collide": func() Config {
			c := base()
			c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: testEP, OperationID: fixture.OpGet, Arguments: map[string]string{"a": "id", "b": "id"}}
			return c
		},
		"a capability declared twice": func() Config { c := base(); c.Capabilities = append(c.Capabilities, c.Capabilities[0]); return c },
		"an operation of no result": func() Config {
			c := base()
			c.Capabilities[0].Operations[adapters.OpDescribe] = Binding{Endpoint: testEP, OperationID: fixture.OpGet}
			return c
		},
		"a resolver of no type": func() Config {
			c := base()
			c.Capabilities[0].Resolve = map[string]Binding{"": {Endpoint: testEP, OperationID: fixture.OpGet}}
			return c
		},

		// The document.
		"swagger 2.0": func() Config {
			return withRead([]byte("swagger: '2.0'\ninfo: {title: t, version: '1'}\npaths: {}\n"))
		},
		"not a document": func() Config { return withRead([]byte("just: [text")) },
		"an invalid document": func() Config {
			return withRead(doc("      parameters:\n        - {name: id, in: path, required: true, schema: {type: array}}\n      responses:\n        '200': {description: ok}\n"))
		},
		"an external $ref to a file": func() Config {
			return withRead(doc("      parameters:\n        - {name: id, in: path, required: true, schema: {type: string}}\n      responses:\n        '200': {$ref: './responses.yaml#/Ok'}\n"))
		},
		"an external $ref to a URL": func() Config {
			return withRead(doc("      parameters:\n        - {name: id, in: path, required: true, schema: {type: string}}\n      responses:\n        '200': {$ref: 'http://169.254.169.254/latest/meta-data#/Ok'}\n"))
		},
		"a duplicate operationId": func() Config {
			return withRead([]byte("openapi: 3.0.3\ninfo: {title: t, version: '1'}\npaths:\n  /a:\n    get:\n      operationId: getItem\n      responses: {'200': {description: ok}}\n  /b:\n    get:\n      operationId: getItem\n      responses: {'200': {description: ok}}\n"))
		},
		"no JSON success response": func() Config {
			return withRead(doc("      parameters:\n        - {name: id, in: path, required: true, schema: {type: string}}\n      responses:\n        '200': {description: ok}\n"))
		},
		"a success response in XML": func() Config {
			return withRead(doc("      parameters:\n        - {name: id, in: path, required: true, schema: {type: string}}\n      responses:\n        '200': {description: ok, content: {application/xml: {schema: {type: object}}}}\n"))
		},
		"a JSON response with no schema": func() Config {
			return withRead(doc("      parameters:\n        - {name: id, in: path, required: true, schema: {type: string}}\n      responses:\n        '200': {description: ok, content: {application/json: {}}}\n"))
		},
		"a cookie parameter": func() Config {
			return withRead(doc("      parameters:\n        - {name: id, in: path, required: true, schema: {type: string}}\n        - {name: sid, in: cookie, schema: {type: string}}\n      responses:\n        '200': {description: ok, content: {application/json: {schema: {type: object}}}}\n"))
		},
		"a deepObject parameter": func() Config {
			return withRead(doc("      parameters:\n        - {name: id, in: path, required: true, schema: {type: string}}\n        - {name: f, in: query, style: deepObject, schema: {type: object}}\n      responses:\n        '200': {description: ok, content: {application/json: {schema: {type: object}}}}\n"))
		},
		"an object parameter": func() Config {
			return withRead(doc("      parameters:\n        - {name: id, in: path, required: true, schema: {type: string}}\n        - {name: f, in: query, schema: {type: object}}\n      responses:\n        '200': {description: ok, content: {application/json: {schema: {type: object}}}}\n"))
		},
		"a parameter encoded as a media type": func() Config {
			return withRead(doc("      parameters:\n        - {name: id, in: path, required: true, schema: {type: string}}\n        - {name: f, in: query, content: {application/json: {schema: {type: object}}}}\n      responses:\n        '200': {description: ok, content: {application/json: {schema: {type: object}}}}\n"))
		},
		"a request body in a form": func() Config {
			return withRead(doc("      parameters:\n        - {name: id, in: path, required: true, schema: {type: string}}\n      requestBody:\n        content:\n          application/x-www-form-urlencoded:\n            schema: {type: object}\n      responses:\n        '200': {description: ok, content: {application/json: {schema: {type: object}}}}\n"))
		},
		"an inspect operation with no aspects parameter": func() Config {
			c := withRead(doc(okOp))
			c.Endpoints[0].Settings.Management.Inspect = "getItem"
			return c
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, err := New(mutate())
			if err == nil {
				_ = a.Close()
				t.Fatal("an adapter was built")
			}
			if contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Errorf("error = %v, want validation_failed", err)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "pw@") {
				t.Errorf("the error quotes a secret: %v", err)
			}
		})
	}
}

// OpenAPI 3.0, 3.1 and later are all documents the adapter reads.
func TestOpenAPIVersions(t *testing.T) {
	app := fake.NewApp()
	app.Seed("ten_a", "wks_a", "item_1", "Widget")
	h := fixture.Start(t, app)
	for _, version := range []string{"3.0.3", "3.1.0"} {
		t.Run(version, func(t *testing.T) {
			spec := "openapi: " + version + "\ninfo: {title: t, version: '1'}\npaths:\n  /items/{id}:\n    get:\n      operationId: getItem\n" +
				"      parameters:\n        - {name: id, in: path, required: true, schema: {type: string}}\n" +
				"      responses:\n        '200':\n          description: ok\n          content:\n            application/json:\n              schema:\n                type: object\n                required: [status]\n                properties:\n                  status: {type: string}\n"
			cfg := fixtureConfig(t, h)
			cfg.Endpoints[0].Spec = []byte(spec)
			cfg.Endpoints[0].CredentialRef = ""
			cfg.Endpoints[0].Settings = mustSettings(t, "server: fixture-api\n")
			cfg.Capabilities = cfg.Capabilities[:1]
			cfg.Capabilities[0].Operations = map[adapters.Operation]Binding{adapters.OpRead: {Endpoint: testEP, OperationID: "getItem", Arguments: map[string]string{"item_id": "id"}}}
			a := newAdapter(t, cfg)
			if _, err := read(a, fake.ReadCapability, map[string]any{"item_id": "item_1"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
