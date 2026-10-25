package fuzzsec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/tests/security/secutil"
)

// schemaSet is the raw JSON Schema of the public documents, compiled once from
// schemas/ the way tools/schemacheck and the generated-type tests do: every
// *.schema.json registered by its $id, JSON Schema 2020-12, format assertions on.
// It is the independent oracle: nothing here goes through a production wrapper.
type schemaSet struct {
	c       *jsonschema.Compiler
	root    string
	compile map[string]*jsonschema.Schema
}

func loadSchemaSet(tb testing.TB) *schemaSet {
	tb.Helper()
	root := filepath.Join(secutil.RepoRoot(tb), "schemas")
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "examples" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".schema.json") {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		doc, err := jsonschema.UnmarshalJSON(f)
		if err != nil {
			return err
		}
		obj, _ := doc.(map[string]any)
		id, _ := obj["$id"].(string)
		if id == "" {
			return fmt.Errorf("%s has no $id", p)
		}
		n++
		return c.AddResource(id, doc)
	})
	if err != nil {
		tb.Fatal(err)
	}
	if n < 20 {
		tb.Fatalf("only %d schemas found under %s", n, root)
	}
	return &schemaSet{c: c, root: root, compile: map[string]*jsonschema.Schema{}}
}

func (s *schemaSet) schema(tb testing.TB, id string) *jsonschema.Schema {
	tb.Helper()
	if sc, ok := s.compile[id]; ok {
		return sc
	}
	sc, err := s.c.Compile(id)
	if err != nil {
		tb.Fatalf("compile %s: %v", id, err)
	}
	s.compile[id] = sc
	return sc
}

// valid reports whether the bytes are a JSON document the schema accepts.
func valid(sc *jsonschema.Schema, doc []byte) (ok bool, why string) {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return false, "not JSON: " + err.Error()
	}
	if err := sc.Validate(inst); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// schemaTarget is one public document and the production code that takes it.
type schemaTarget struct {
	name     string
	schemaID string
	// prod runs the PRODUCTION decode-and-validate path on the bytes. accepted is
	// whether it took them; canonical is what it returns, encoded again.
	prod func(which uint8, raw []byte) (accepted bool, canonical []byte)
	// inputValid: the path validates the bytes themselves against the published
	// schema, so an accepted input must satisfy it. (The deterministic result is
	// completed by the core before it is validated, so its input need not.)
	inputValid bool
	// gen: the generated types are shape-only (patterns, lengths and cross-field
	// rules stay with schema validation): they get no validity claim, only the
	// round-trip one. check returns a violation or "".
	gen func(sc *jsonschema.Schema, raw []byte) string
}

var deterministicOps = []adapters.Operation{adapters.OpRead, adapters.OpValidate, adapters.OpPrepare, adapters.OpCommit, adapters.OpStatus, adapters.OpCancel}

const fuzzIdemKey = "idem-key-0123456789abcdef"

// genRoundTrip is the property of a generated decoder: it never panics; what it
// accepts and can encode decodes again to the same encoding; and a document the
// raw schema accepts is not damaged by the round trip (the encoding of what was
// decoded is still valid). A value it accepts but cannot encode (an unset enum, an
// empty union) is fail-closed behaviour, not a violation.
func genRoundTrip[T any](name string) func(sc *jsonschema.Schema, raw []byte) string {
	return func(sc *jsonschema.Schema, raw []byte) string {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return ""
		}
		b1, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		var v2 T
		if err := json.Unmarshal(b1, &v2); err != nil {
			return fmt.Sprintf("%s: the encoding of an accepted document is not accepted: %v\n%s", name, err, b1)
		}
		b2, err := json.Marshal(v2)
		if err != nil || !bytes.Equal(b1, b2) {
			return fmt.Sprintf("%s: not stable:\n%s\n%s (%v)", name, b1, b2, err)
		}
		if ok, _ := valid(sc, raw); ok {
			if ok2, why := valid(sc, b1); !ok2 {
				return fmt.Sprintf("%s: a schema-valid document is damaged by decoding and encoding it (%s):\n%s", name, why, b1)
			}
		}
		return ""
	}
}

func schemaTargets() []schemaTarget {
	blk := blocks.Default()
	var facts []actions.Fact
	for i := int64(0); i < 40; i++ {
		facts = append(facts, actions.Fact{
			Field: common.FieldName("f_n"),
			Value: actions.CanonicalValue{Value: actions.IntegerValue{Kind: "integer", Value: i}},
		})
	}
	bctx := blocks.Context{Facts: blocks.NewFactSet(facts)}

	return []schemaTarget{
		{
			name: "rich-block", schemaID: "urn:alixai:schema:conversation:rich-block:v1", inputValid: true,
			prod: func(_ uint8, raw []byte) (bool, []byte) {
				b, err := blk.JSON(raw, bctx)
				if err != nil {
					return false, nil
				}
				enc, err := json.Marshal(b)
				if err != nil {
					return true, nil
				}
				return true, enc
			},
		},
		{
			name: "deterministic-result", schemaID: "urn:alixai:schema:actions:deterministic-result:v1",
			prod: func(which uint8, raw []byte) (bool, []byte) {
				exp := adapters.Expect{Operation: deterministicOps[int(which>>4)%len(deterministicOps)], CapabilityID: "demo.lookup"}
				if exp.Operation == adapters.OpCommit || exp.Operation == adapters.OpStatus || exp.Operation == adapters.OpCancel {
					exp.IdempotencyKey = fuzzIdemKey
				}
				res, err := adapters.Normalise(raw, exp)
				if err != nil {
					return false, nil
				}
				enc, err := json.Marshal(res)
				if err != nil {
					return true, nil
				}
				return true, enc
			},
		},
		{
			name: "registry-entry", schemaID: "urn:alixai:schema:models:registry-entry:v1", inputValid: true,
			prod: func(_ uint8, raw []byte) (bool, []byte) {
				// The registry reads YAML; a JSON document is YAML, so it is the one
				// form the raw schema can be applied to byte for byte.
				if !json.Valid(raw) {
					return false, nil
				}
				e, vs := models.ParseEntry("fuzz.yaml", raw)
				if len(vs) != 0 {
					return false, nil
				}
				enc, err := json.Marshal(e)
				if err != nil {
					return true, nil
				}
				return true, enc
			},
		},
		{name: "turn-event", schemaID: "urn:alixai:schema:conversation:turn-event:v1", gen: genRoundTrip[conversation.TurnEvent]("TurnEvent")},
		{name: "task-proposal", schemaID: "urn:alixai:schema:conversation:task-proposal:v1", gen: genRoundTrip[conversation.TaskProposal]("TaskProposal")},
		{name: "dialogue-directive", schemaID: "urn:alixai:schema:conversation:dialogue-directive:v1", gen: genRoundTrip[conversation.DialogueDirective]("DialogueDirective")},
		{name: "citation", schemaID: "urn:alixai:schema:conversation:citation:v1", gen: genRoundTrip[conversation.Citation]("Citation")},
		{name: "file-ref", schemaID: "urn:alixai:schema:conversation:file-ref:v1", gen: genRoundTrip[conversation.FileRef]("FileRef")},
		{name: "rich-block (generated)", schemaID: "urn:alixai:schema:conversation:rich-block:v1", gen: genRoundTrip[conversation.RichBlock]("RichBlock")},
		{name: "prepared-action", schemaID: "urn:alixai:schema:actions:prepared-action:v1", gen: genRoundTrip[actions.PreparedAction]("PreparedAction")},
		{name: "action-receipt", schemaID: "urn:alixai:schema:actions:action-receipt:v1", gen: genRoundTrip[actions.ActionReceipt]("ActionReceipt")},
		{name: "deterministic-result (generated)", schemaID: "urn:alixai:schema:actions:deterministic-result:v1", gen: genRoundTrip[actions.DeterministicResult]("DeterministicResult")},
		{name: "scope", schemaID: "urn:alixai:schema:common:scope:v1", gen: genRoundTrip[common.Scope]("Scope")},
		{name: "error", schemaID: "urn:alixai:schema:common:error:v1", gen: genRoundTrip[common.Error]("Error")},
	}
}

// checkSchemaTarget is every property of one body against one target. It returns
// the first violation, "" when none.
func checkSchemaTarget(set *schemaSet, tb testing.TB, t schemaTarget, which uint8, raw []byte) string {
	sc := set.schema(tb, t.schemaID)
	if t.gen != nil {
		return t.gen(sc, raw)
	}
	accepted, canonical := t.prod(which, raw)
	if !accepted {
		return ""
	}
	// Accepted: the schema must accept it too, and what the path returns must be a
	// valid document again that the path accepts a second time, unchanged.
	if t.inputValid {
		if ok, why := valid(sc, raw); !ok {
			return fmt.Sprintf("%s: the production path accepted a body the published schema rejects (%s)", t.name, why)
		}
	}
	if canonical == nil {
		return ""
	}
	if ok, why := valid(sc, canonical); !ok {
		return fmt.Sprintf("%s: the production path returned a document the published schema rejects (%s):\n%s", t.name, why, canonical)
	}
	if t.name == "registry-entry" {
		return "" // the typed entry is a different shape from the YAML document
	}
	again, canonical2 := t.prod(which, canonical)
	if !again {
		return fmt.Sprintf("%s: what the production path returned is refused when it is given again:\n%s", t.name, canonical)
	}
	if !bytes.Equal(canonical, canonical2) {
		return fmt.Sprintf("%s: not stable:\n%s\n%s", t.name, canonical, canonical2)
	}
	return ""
}

// schemaSeeds are the published valid and invalid examples of every target, and
// the hostile bodies: deep nesting, huge and odd numbers, duplicate keys, escapes
// and surrogates, null for what is required, a byte order mark, trailing data,
// oversized strings.
func schemaSeeds(f *testing.F, set *schemaSet, targets []schemaTarget) (seeds []struct {
	which uint8
	data  []byte
}) {
	byStem := map[string][]int{}
	for i, t := range targets {
		parts := strings.Split(t.schemaID, ":")
		stem := parts[len(parts)-2]
		byStem[stem] = append(byStem[stem], i)
	}
	add := func(i int, data []byte, op uint8) {
		seeds = append(seeds, struct {
			which uint8
			data  []byte
		}{uint8(i) | op<<4, data})
	}
	areas, err := os.ReadDir(set.root)
	if err != nil {
		f.Fatal(err)
	}
	for _, a := range areas {
		stems, _ := os.ReadDir(filepath.Join(set.root, a.Name(), "examples"))
		for _, s := range stems {
			idx := byStem[s.Name()]
			if len(idx) == 0 {
				continue
			}
			for _, kind := range []string{"valid", "invalid"} {
				files, _ := filepath.Glob(filepath.Join(set.root, a.Name(), "examples", s.Name(), kind, "*.json"))
				sort.Strings(files)
				for _, file := range files {
					data, err := os.ReadFile(file)
					if err != nil {
						f.Fatal(err)
					}
					var op uint8
					var probe struct {
						Operation string `json:"operation"`
					}
					if json.Unmarshal(data, &probe) == nil {
						for i, o := range deterministicOps {
							if string(o) == probe.Operation {
								op = uint8(i)
							}
						}
					}
					for _, i := range idx {
						add(i, data, op)
					}
				}
			}
		}
	}
	// Hostile bodies, offered to every target.
	hostile := [][]byte{
		nil, []byte(""), []byte("null"), []byte("true"), []byte("0"), []byte(`""`), []byte("[]"), []byte("{}"),
		[]byte(`{"type":null}`), []byte(`{"type":"markdown","text":null}`), []byte(`{"type":"markdown"}`),
		[]byte(`{"type":"markdown","text":"a","text":"<script>alert(1)</script>"}`),
		[]byte(`{"type":"markdown","type":"code","text":"a"}`),
		[]byte(`{"type":"markdown","text":"\ud800"}`), []byte(`{"type":"markdown","text":"\u0000"}`),
		[]byte(`{"type":"markdown","text":"a‮b"}`), []byte(`{"type":"markdown","text":"<script>alert(1)</script>"}`),
		[]byte(`{"type":"markdown","text":"x","extra":1}`), []byte(`{"type":"unknown"}`),
		[]byte(`{"type":"token","turn_id":"trn_01J0000000000000000000000A","seq":1e999,"text":"a"}`),
		[]byte(`{"type":"token","turn_id":"trn_01J0000000000000000000000A","seq":123456789012345678901234567890,"text":"a"}`),
		[]byte(`{"type":"token","turn_id":"trn_01J0000000000000000000000A","seq":-0,"text":"a"}`),
		[]byte(`{"type":"token","turn_id":"trn_01J0000000000000000000000A","seq":1.5,"text":"a"}`),
		[]byte(`{"type":"token","turn_id":"trn_01J0000000000000000000000A","seq":"1","text":"a"}`),
		[]byte(`{"type":"token","turn_id":null,"seq":0,"text":"a"}`),
		[]byte(`{"status":"ok","errors":null}`), []byte(`{"status":"OK"}`), []byte(`{"status":"ok","status":"rejected"}`),
		[]byte(`{"deployment_id":"d","application_id":"a","tenant_id":"*","workspace_id":"w","principal_id":"p","roles":[],"capability_scopes":["x"],"session_id":"s"}`),
		[]byte(`{"code":"nope","message":"x","retryable":false}`), []byte(`{"code":null,"message":"x"}`),
		append([]byte("\xef\xbb\xbf"), `{"type":"markdown","text":"a"}`...),
		[]byte(`{"type":"markdown","text":"a"} {"type":"markdown","text":"b"}`),
		[]byte(`{"type":"markdown","text":"a"}garbage`),
		[]byte(`{"type":"markdown","text":"` + strings.Repeat("a", 70000) + `"}`),
		[]byte(strings.Repeat("[", 5000) + strings.Repeat("]", 5000)),
		[]byte(strings.Repeat(`{"a":`, 3000) + "1" + strings.Repeat("}", 3000)),
		[]byte(`{"a":` + strings.Repeat("9", 5000) + `}`),
		[]byte("{" + strings.Repeat(`"k":1,`, 2000) + `"z":2}`),
	}
	// Every hostile body goes to the three production paths and to one generated
	// decoder in turn: the fuzzer carries it to the others.
	for n, h := range hostile {
		for _, i := range []int{0, 1, 2, 3 + n%(len(targets)-3)} {
			add(i, h, 0)
		}
	}
	return seeds
}

// SEC-097
// FuzzSchemaValidate: hostile JSON bodies against the public schemas
// (schemas/**), through the production decode and validation paths, with the raw
// JSON Schema (santhosh-tekuri/jsonschema/v6, compiled from schemas/ as
// tools/schemacheck does) as the independent oracle.
//
//   - rich-block (blocks.Validator.JSON), deterministic-result
//     (adapters.Normalise) and registry-entry (models.ParseEntry) validate against
//     the schema inside the production path: an ACCEPTED body must satisfy the raw
//     schema (the body itself where the path validates it as given, always what the
//     path returns), and what it returns must be accepted again, unchanged. An
//     accepted-but-invalid result is a violation.
//   - the generated types (pkg/contracts/gen) are shape-only by design, so no
//     validity claim is made for them: they never panic, what they accept and can
//     encode round-trips stably, and a schema-valid body is not damaged by it.
//
// The control, run first, is the published invalid examples: each one the raw
// schema rejects must be refused by the production paths that claim to validate
// (so the oracle and the paths can both say no), and a deliberately lax stand-in
// (the generated decoder alone) accepts at least one of them (so "accepted but
// invalid" is something this target can see).
//
//	go test -run '^$' -fuzz '^FuzzSchemaValidate$' -fuzztime 5s ./tests/security/fuzz/
func FuzzSchemaValidate(f *testing.F) {
	set := loadSchemaSet(f)
	targets := schemaTargets()
	seeds := schemaSeeds(f, set, targets)
	if len(seeds) < 100 {
		f.Fatalf("only %d seeds: the published examples were not found", len(seeds))
	}

	// Control 1: the production paths that validate refuse the published invalid
	// examples, and accept the valid ones.
	var validAccepted, invalidRefused, laxAccepts int
	for _, t := range targets {
		if t.prod == nil {
			continue
		}
		stem := strings.Split(t.schemaID, ":")
		area, name := stem[len(stem)-3], stem[len(stem)-2]
		for _, kind := range []string{"valid", "invalid"} {
			files, _ := filepath.Glob(filepath.Join(set.root, area, "examples", name, kind, "*.json"))
			for _, file := range files {
				data, err := os.ReadFile(file)
				if err != nil {
					f.Fatal(err)
				}
				var probe struct {
					Operation string `json:"operation"`
				}
				var op uint8
				if json.Unmarshal(data, &probe) == nil {
					for i, o := range deterministicOps {
						if string(o) == probe.Operation {
							op = uint8(i)
						}
					}
				}
				accepted, _ := t.prod(op<<4, data)
				switch {
				case kind == "valid" && accepted:
					validAccepted++
				case kind == "invalid" && !accepted:
					invalidRefused++
				case kind == "invalid" && accepted:
					f.Fatalf("control: %s accepted the published invalid example %s", t.name, filepath.Base(file))
				}
			}
		}
	}
	if validAccepted < 10 || invalidRefused < 10 {
		f.Fatalf("control: the production paths accepted %d valid and refused %d invalid examples: the oracle has too little to compare", validAccepted, invalidRefused)
	}
	// Control 2: a lax stand-in (strict decoding of the generated type alone)
	// accepts invalid examples, and the oracle says they are invalid.
	for _, files := range [][]string{
		globAll(set.root, "conversation", "turn-event", "invalid"),
		globAll(set.root, "conversation", "rich-block", "invalid"),
		globAll(set.root, "actions", "deterministic-result", "invalid"),
	} {
		for _, file := range files {
			data, _ := os.ReadFile(file)
			var v any
			switch {
			case strings.Contains(file, "turn-event"):
				v = new(conversation.TurnEvent)
			case strings.Contains(file, "rich-block"):
				v = new(conversation.RichBlock)
			default:
				v = new(actions.DeterministicResult)
			}
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.DisallowUnknownFields()
			if dec.Decode(v) == nil {
				laxAccepts++
			}
		}
	}
	if laxAccepts == 0 {
		f.Fatal("control: the shape-only decoders refuse every invalid example: this target would never see an accepted-but-invalid body")
	}

	for _, s := range seeds {
		f.Add(s.which, s.data)
	}
	f.Fuzz(func(t *testing.T, which uint8, raw []byte) {
		target := targets[int(which&0x0f)%len(targets)]
		var msg string
		within(t, 30*time.Second, "the validation", func() { msg = checkSchemaTarget(set, t, target, which, raw) })
		if msg != "" {
			t.Fatalf("body %s: %s", quoteShort(raw), msg)
		}
	})
}

func globAll(root, area, stem, kind string) []string {
	files, _ := filepath.Glob(filepath.Join(root, area, "examples", stem, kind, "*.json"))
	sort.Strings(files)
	return files
}
