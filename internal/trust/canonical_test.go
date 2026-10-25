package trust

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// SEC-057: the manifest is built over one canonical byte string.
//
// TestCanonicalJSONStable: the encoder is RFC 8785 for strings and integers,
// gives one output for one value however it is spelled or built, rejects what
// would make the output ambiguous, and produces exactly the bytes M1-01's
// example signature was made over.
func TestCanonicalJSONStable(t *testing.T) {
	t.Run("golden vectors", func(t *testing.T) {
		tests := []struct {
			name string
			in   string
			want string
		}{
			{"keys sorted", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
			{"whitespace removed", " {\n\t\"a\" : [ 1 , 2 ] ,\r\n \"b\" : null } ", `{"a":[1,2],"b":null}`},
			{"nested objects sorted, arrays keep order", `{"z":{"b":[3,1,2],"a":true},"a":false}`, `{"a":false,"z":{"a":true,"b":[3,1,2]}}`},
			{"html and separators are not escaped", q(bs + "u003c" + bs + "u003e" + bs + "u0026" + bs + "u2028" + bs + "u2029" + bs + "/"), q("<>&  /")},
			{"control characters", `"\u0000\u0008\u0009\u000a\u000c\u000d\u001f\u007f"`, "\"\\u0000\\b\\t\\n\\f\\r\\u001f\x7f\""},
			{"quote and backslash", `"a\"b\\c"`, `"a\"b\\c"`},
			{"escaped letters are unescaped", q(bs + "u0061" + bs + "u00e9" + bs + "u20ac"), q("aé€")},
			{"surrogate pair becomes the character", q(bs + "ud83d" + bs + "ude00"), q("😀")},
			{"integers", `[0,-1,9007199254740991,-9007199254740991]`, `[0,-1,9007199254740991,-9007199254740991]`},
			{"empty containers", `{"a":{},"b":[]}`, `{"a":{},"b":[]}`},
			{"keys sort by UTF-16 code unit, not by UTF-8 byte", "{\"￿\":2,\"😀\":1}", "{\"😀\":1,\"￿\":2}"},
			{"key escapes", "{\"b" + bs + "n\":1,\"a" + bs + "u0022\":2}", "{\"a" + bs + "\"\":2,\"b" + bs + "n\":1}"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, err := Canonicalize([]byte(tt.in))
				if err != nil {
					t.Fatalf("Canonicalize(%q): %v", tt.in, err)
				}
				if string(got) != tt.want {
					t.Errorf("Canonicalize(%q)\n got  %q\n want %q", tt.in, got, tt.want)
				}
				again, err := Canonicalize(got)
				if err != nil || string(again) != string(got) {
					t.Errorf("not idempotent: %q -> %q (%v)", got, again, err)
				}
			})
		}
	})

	t.Run("rejects what is ambiguous or not canonical", func(t *testing.T) {
		deep := strings.Repeat("[", maxCanonicalDepth+2) + strings.Repeat("]", maxCanonicalDepth+2)
		tests := []struct{ name, in string }{
			{"duplicate key", `{"a":1,"a":2}`},
			{"duplicate key in nested object", `{"x":{"k":1,"k":1}}`},
			{"float", `1.5`},
			{"integer written as float", `1.0`},
			{"exponent", `1e3`},
			{"negative zero", `-0`},
			{"leading zero", `01`},
			{"beyond 2^53-1", `9007199254740992`},
			{"below -(2^53-1)", `-9007199254740992`},
			{"data after the value", `{"a":1} {"b":2}`},
			{"garbage after the value", `{"a":1} x`},
			{"two values", `1 2`},
			{"empty input", ``},
			{"only whitespace", `  `},
			{"trailing comma", `[1,]`},
			{"single quotes", `{'a':1}`},
			{"NaN", `NaN`},
			{"unquoted key", `{a:1}`},
			{"invalid UTF-8", "\"a\xffb\""},
			{"byte order mark", "\xef\xbb\xbf{}"},
			{"too deep", deep},
			{"unterminated", `{"a":`},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, err := Canonicalize([]byte(tt.in))
				if err == nil {
					t.Fatalf("Canonicalize(%q) = %q, want an error", tt.in, got)
				}
				if contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
					t.Errorf("code = %s, want %s", contracts.CodeOf(err), contracts.ErrSchemaInvalid)
				}
			})
		}
	})

	t.Run("one value has one canonical form", func(t *testing.T) {
		type inner struct {
			Z int    `json:"z"`
			A string `json:"a"`
		}
		type doc struct {
			Name  string  `json:"name"`
			Items []inner `json:"items"`
			Flag  bool    `json:"flag"`
		}
		want := `{"flag":true,"items":[{"a":"x","z":1},{"a":"y","z":2}],"name":"n"}`
		fromStruct, err := CanonicalJSON(doc{Name: "n", Items: []inner{{Z: 1, A: "x"}, {Z: 2, A: "y"}}, Flag: true})
		if err != nil {
			t.Fatal(err)
		}
		if string(fromStruct) != want {
			t.Fatalf("struct form = %s, want %s", fromStruct, want)
		}
		spellings := []string{
			`{"name":"n","items":[{"z":1,"a":"x"},{"z":2,"a":"y"}],"flag":true}`,
			"{\n  \"flag\": true,\n  \"items\": [ {\"a\": \"x\", \"z\": 1}, {\"a\": \"y\", \"z\": 2} ],\n  \"name\": \"n\"\n}\n",
			`{"flag":true,"name":"n","items":[{"a":"x","z":1},{"a":"y","z":2}]}`,
		}
		for _, s := range spellings {
			got, err := Canonicalize([]byte(s))
			if err != nil || string(got) != want {
				t.Errorf("Canonicalize(%q) = %q, %v; want %s", s, got, err, want)
			}
		}
		// Go map iteration order is random: the output must not depend on it.
		m := map[string]any{"k0": 0, "k1": 1, "k2": 2, "k3": 3, "k4": 4, "k5": 5, "k6": 6, "k7": 7, "k8": 8, "k9": 9}
		first, err := CanonicalJSON(m)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 50; i++ {
			got, err := CanonicalJSON(m)
			if err != nil || string(got) != string(first) {
				t.Fatalf("run %d: %q, %v; first %q", i, got, err, first)
			}
		}
		if string(first) != `{"k0":0,"k1":1,"k2":2,"k3":3,"k4":4,"k5":5,"k6":6,"k7":7,"k8":8,"k9":9}` {
			t.Errorf("map form = %s", first)
		}
	})

	t.Run("CanonicalJSON refuses floats, huge integers and unmarshalable values", func(t *testing.T) {
		for name, v := range map[string]any{
			"float":     map[string]any{"a": 1.5},
			"huge":      map[string]any{"a": uint64(1) << 60},
			"channel":   make(chan int),
			"inf":       map[string]any{"a": inf()},
			"nested":    []any{map[string]any{"x": []any{0.25}}},
			"raw float": json.RawMessage(`{"a":2.0}`),
		} {
			if got, err := CanonicalJSON(v); err == nil {
				t.Errorf("%s: CanonicalJSON = %q, want an error", name, got)
			}
		}
		// An integer-valued float64 is an integer for JSON, and is accepted.
		if got, err := CanonicalJSON(map[string]any{"a": float64(3)}); err != nil || string(got) != `{"a":3}` {
			t.Errorf("float64(3) = %q, %v", got, err)
		}
	})

	t.Run("matches the M1-01 example signature byte for byte", func(t *testing.T) {
		// docs/integration/application-package.md section 13 publishes the public
		// key of the throwaway key that signed the example. The signature
		// verifies only if this encoder produces the same canonical bytes as the
		// tool that made it.
		raw, err := os.ReadFile("../../schemas/application-package/examples/signature-manifest/valid/demo-library-signed.json")
		if err != nil {
			t.Fatal(err)
		}
		pub, err := base64.StdEncoding.DecodeString(SchemaExamplePublicKeyB64)
		if err != nil {
			t.Fatal(err)
		}

		var generic struct {
			Signed     json.RawMessage `json:"signed"`
			Signatures []Signature     `json:"signatures"`
		}
		if err := json.Unmarshal(raw, &generic); err != nil {
			t.Fatal(err)
		}
		canon, err := Canonicalize(generic.Signed)
		if err != nil {
			t.Fatal(err)
		}
		sig, err := base64.StdEncoding.DecodeString(generic.Signatures[0].Signature)
		if err != nil {
			t.Fatal(err)
		}
		if !ed25519.Verify(pub, canon, sig) {
			t.Fatalf("the example signature does not verify over our canonical form of its signed part:\n%s", canon)
		}

		// The typed route (parse, then Canonical) yields the same bytes.
		sm, err := ParseSignatureManifest(raw)
		if err != nil {
			t.Fatalf("the valid example does not parse: %v", err)
		}
		typed, err := sm.Signed.Canonical()
		if err != nil {
			t.Fatal(err)
		}
		if string(typed) != string(canon) {
			t.Errorf("typed canonical form differs from the generic one:\n%s\n%s", typed, canon)
		}
		if strings.ContainsAny(string(canon), " \n\t") {
			t.Errorf("canonical form holds whitespace: %s", canon)
		}
	})
}

func inf() float64 {
	var zero float64
	return 1 / zero
}

// bs is a backslash. The escapes in the golden vectors are assembled from it
// so that no tool between the author and the compiler can rewrite them.
const bs = "\x5c"

// q wraps s in JSON string quotes.
func q(s string) string { return "\"" + s + "\"" }
