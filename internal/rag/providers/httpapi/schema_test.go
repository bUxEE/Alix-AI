package httpapi

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const listSchemaID = "urn:alixai:schema:rag:httpapi-list-page:v1"

func compileListSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile("testdata/list-page.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(listSchemaID, doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile(listSchemaID)
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

func schemaAccepts(t *testing.T, sch *jsonschema.Schema, body string) bool {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(body))
	if err != nil {
		return false // not even JSON
	}
	return sch.Validate(inst) == nil
}

// TestListPageSchema: the JSON Schema under testdata is the machine-checkable
// half of the API contract, and the provider's decoder agrees with it on every
// structural case: what the schema calls a page is accepted, and what it calls
// invalid is refused (as schema_invalid) and never half-read.
func TestListPageSchema(t *testing.T) {
	sch := compileListSchema(t)
	long := strings.Repeat("v", maxVersionLen+1)
	cases := []struct {
		name  string
		body  string
		valid bool
	}{
		{"an empty page", `{"items":[]}`, true},
		{"a full page", `{"items":[{"key":"a/b.md","version":"7","size":0,"modified_at":"2026-02-01T09:30:00Z"},{"key":"c","version":"sha256:` + strings.Repeat("a", 64) + `"}],"next_cursor":"abc"}`, true},
		{"a deletion marker", `{"items":[{"key":"gone.md","version":"3","deleted":true}]}`, true},
		{"a null cursor", `{"items":[],"next_cursor":null}`, true},
		{"an empty cursor", `{"items":[],"next_cursor":""}`, true},
		{"a fractional-second time with an offset", `{"items":[{"key":"a","version":"1","modified_at":"2026-02-01T10:30:00.25+01:00"}]}`, true},
		{"extra members", `{"items":[{"key":"a","version":"1","acl":["x"]}],"total":9}`, true},
		{"the longest version", `{"items":[{"key":"a","version":"` + long[:maxVersionLen] + `"}]}`, true},
		{"the longest key", `{"items":[{"key":"` + strings.Repeat("k", maxKeyChars) + `","version":"1"}]}`, true},
		{"a key too long", `{"items":[{"key":"` + strings.Repeat("k", maxKeyChars+1) + `","version":"1"}]}`, false},
		{"a key of multi-byte characters within the limit", `{"items":[{"key":"` + strings.Repeat("é", maxKeyChars) + `","version":"1"}]}`, true},
		{"items missing", `{"next_cursor":null}`, false},
		{"items null", `{"items":null}`, false},
		{"items not an array", `{"items":"a.md"}`, false},
		{"a key missing", `{"items":[{"version":"1"}]}`, false},
		{"an empty key", `{"items":[{"key":"","version":"1"}]}`, false},
		{"a version missing", `{"items":[{"key":"a"}]}`, false},
		{"an empty version", `{"items":[{"key":"a","version":""}]}`, false},
		{"a version with a space", `{"items":[{"key":"a","version":"a b"}]}`, false},
		{"a version with a control character", `{"items":[{"key":"a","version":"a\u0001b"}]}`, false},
		{"a version too long", `{"items":[{"key":"a","version":"` + long + `"}]}`, false},
		{"a numeric version", `{"items":[{"key":"a","version":7}]}`, false},
		{"a negative size", `{"items":[{"key":"a","version":"1","size":-1}]}`, false},
		{"a fractional size", `{"items":[{"key":"a","version":"1","size":1.5}]}`, false},
		{"a string size", `{"items":[{"key":"a","version":"1","size":"9"}]}`, false},
		{"a time that is not a time", `{"items":[{"key":"a","version":"1","modified_at":"yesterday"}]}`, false},
		{"a date without a time", `{"items":[{"key":"a","version":"1","modified_at":"2026-02-01"}]}`, false},
		{"a non-boolean deleted", `{"items":[{"key":"a","version":"1","deleted":"yes"}]}`, false},
		{"a numeric cursor", `{"items":[],"next_cursor":5}`, false},
		{"a cursor with a control character", `{"items":[],"next_cursor":"a\u0007b"}`, false},
		{"a cursor too long", `{"items":[],"next_cursor":"` + strings.Repeat("c", maxCursorBytes+1) + `"}`, false},
		{"not an object", `[]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := schemaAccepts(t, sch, tc.body); got != tc.valid {
				t.Errorf("the schema says valid = %v, the case says %v", got, tc.valid)
			}
			_, err := decodePage([]byte(tc.body))
			if (err == nil) != tc.valid {
				t.Errorf("decodePage err = %v, want valid = %v", err, tc.valid)
			}
		})
	}

	t.Run("the example of the package comment is a valid page", func(t *testing.T) {
		raw, err := os.ReadFile("testdata/list-page.example.json")
		if err != nil {
			t.Fatal(err)
		}
		if !schemaAccepts(t, sch, string(raw)) {
			t.Error("the schema rejects the example")
		}
		pg, err := decodePage(raw)
		if err != nil || len(pg.entries) != 2 || pg.next != "opaque-cursor-or-null" {
			t.Fatalf("decodePage = %+v, %v", pg, err)
		}
		if e := pg.entries[0]; e.key != "policies/refund.md" || e.version != "7" || e.size != 1280 || e.modTime.IsZero() || e.deleted {
			t.Errorf("entry 0 = %+v", e)
		}
		if e := pg.entries[1]; !e.deleted || e.size != -1 || !e.modTime.IsZero() {
			t.Errorf("entry 1 = %+v", e)
		}
	})

	t.Run("what the fake application sends follows the schema", func(t *testing.T) {
		h := newHarness(t, "/api/docs")
		a := h.api.put("a.md", "a\n")
		a.hideMod = true
		b := h.api.put("b.md", "b\n")
		b.hideSize = true
		h.api.put("c.md", "c\n")
		h.api.put("d.md", "d\n")
		h.api.mark("d.md")
		pages := 0
		for cursor := ""; ; {
			u := h.api.rootURL() + "?limit=2"
			if cursor != "" {
				u += "&cursor=" + cursor
			}
			resp, err := http.Get(u)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if !schemaAccepts(t, sch, string(body)) {
				t.Fatalf("the schema rejects a reply of the fake application: %s", body)
			}
			pg, err := decodePage(body)
			if err != nil {
				t.Fatal(err)
			}
			pages++
			if cursor = pg.next; cursor == "" {
				break
			}
		}
		if pages != 2 {
			t.Errorf("pages = %d", pages)
		}
	})
}
