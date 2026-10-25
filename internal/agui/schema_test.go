package agui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func TestEveryEmittedEventValidatesAgainstTheVendoredAGUISchema(t *testing.T) {
	s := vendored(t)
	seen := map[EventType]int{}
	check := func(label string, em []Emitted) {
		t.Helper()
		validateAll(t, em)
		for _, e := range em {
			seen[e.Event.Kind()]++
			// The encoded bytes are the event: what is validated is what is sent.
			if err := s.ValidateEvent(e.Data); err != nil {
				t.Fatalf("%s: %v", label, err)
			}
		}
	}
	for _, sc := range scenarios(t) {
		check(sc.name, runAll(t, sc.rc, sc.events))
	}
	// A connect run and an aborted run.
	rc := newRC()
	rc.StateBefore = clientState(goalState("open"))
	em, err := Connect(rc, []HistoryTurn{{TurnID: "trn_1", Role: "user", Text: "hi"}, {TurnID: "trn_1", Role: "assistant", Text: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	check("connect", em)
	m, _ := NewMapper(newRC())
	got, _ := m.Map(token(t, 0, "partial"))
	check("aborted", append(got, m.Abort(0, "aborted", "the turn was abandoned")...))

	// Every event type the package may emit was produced and validated at least once.
	for _, ty := range EmittedTypes() {
		if seen[ty] == 0 {
			t.Errorf("no scenario produced a %s event, so it is not validated", ty)
		}
	}
	for ty := range seen {
		if !Emittable(ty) {
			t.Errorf("a %s event was produced but is not Emittable", ty)
		}
	}

	// The same schemas reject what is wrong, so the check above means something.
	reject := func(label, doc string) {
		t.Helper()
		if err := s.ValidateEvent([]byte(doc)); err == nil {
			t.Errorf("%s: the vendored schema accepted %s", label, doc)
		}
	}
	reject("a tool call without a name", `{"type":"TOOL_CALL_START","toolCallId":"c1"}`)
	reject("a misspelled member", `{"type":"TOOL_CALL_START","toolCallId":"c1","toolCallName":"x","toolCallID":"c1"}`)
	reject("an unknown type", `{"type":"SOMETHING_NEW","x":1}`)
	reject("a run finished with an unknown outcome", `{"type":"RUN_FINISHED","threadId":"t","runId":"r","outcome":{"type":"maybe"}}`)
	reject("an interrupt with no interrupts", `{"type":"RUN_FINISHED","threadId":"t","runId":"r","outcome":{"type":"interrupt","interrupts":[]}}`)
	reject("a patch without a value", `{"type":"STATE_DELTA","delta":[{"op":"add","path":"/a"}]}`)
	reject("a patch with a bad pointer", `{"type":"STATE_DELTA","delta":[{"op":"remove","path":"a"}]}`)
	reject("a message with a bad role", `{"type":"TEXT_MESSAGE_START","messageId":"m","role":"robot"}`)
	if err := s.ValidateEvent([]byte(`{"type":"TEXT_MESSAGE_START","messageId":"m","role":"assistant"}`)); err != nil {
		t.Errorf("a valid event is rejected: %v", err)
	}
}

// TestVendoredSchemasCoverTheWholeProtocol keeps the two lists of this package (what is emitted and what is not) in step
// with the release: a new event type in a newer @ag-ui/core fails here until it is classified.
func TestVendoredSchemasCoverTheWholeProtocol(t *testing.T) {
	s := vendored(t)
	var bundle struct {
		Defs map[string]struct {
			OneOf []struct {
				Ref string `json:"$ref"`
			} `json:"oneOf"`
		} `json:"$defs"`
	}
	dir, _ := FindSchemaDir()
	raw, err := os.ReadFile(filepath.Join(dir, BundleFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatal(err)
	}
	var fromSchema []string
	for _, alt := range bundle.Defs["Event"].OneOf {
		var def struct {
			Properties struct {
				Type struct {
					Const string `json:"const"`
				} `json:"type"`
			} `json:"properties"`
		}
		name := strings.TrimPrefix(alt.Ref, "#/$defs/")
		d, _ := json.Marshal(mustDef(t, raw, name))
		if err := json.Unmarshal(d, &def); err != nil {
			t.Fatal(err)
		}
		fromSchema = append(fromSchema, def.Properties.Type.Const)
	}
	var ours []string
	for _, ty := range allTypesInOrder {
		ours = append(ours, string(ty))
	}
	sort.Strings(fromSchema)
	sort.Strings(ours)
	if strings.Join(fromSchema, ",") != strings.Join(ours, ",") {
		t.Fatalf("the release's event types differ from this package's:\nschema %v\nours   %v", fromSchema, ours)
	}
	for _, name := range []string{"Event", "RunAgentInput", "Message", "Tool", "Context", "JsonPatchOperation", "ResumeEntry", "Interrupt"} {
		if !s.Has(name) {
			t.Errorf("the bundle has no %s", name)
		}
	}
	if len(EmittedTypes())+len(NotEmittedTypes()) != len(allTypesInOrder) {
		t.Fatal("emitted and not emitted do not partition the event types")
	}
}

func mustDef(t *testing.T, raw []byte, name string) any {
	t.Helper()
	var doc struct {
		Defs map[string]any `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	d, ok := doc.Defs[name]
	if !ok {
		t.Fatalf("no definition %s", name)
	}
	return d
}

func TestVendoredSchemasAreThePinnedRelease(t *testing.T) {
	s := vendored(t)
	v := s.Version
	if v.Package != "@ag-ui/core" || v.ProtocolVersion != ProtocolVersion || !strings.HasPrefix(v.Integrity, "sha512-") {
		t.Fatalf("VERSION: %+v", v)
	}
	if PinnedPackage != v.Package+" "+v.Version {
		t.Fatalf("PinnedPackage %q is not %s %s", PinnedPackage, v.Package, v.Version)
	}
	// The tool's own pin is the same release.
	dir, _ := FindSchemaDir()
	pkg, err := os.ReadFile(filepath.Join(dir, "..", "..", "tools", "aguischema", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(pkg, &p); err != nil {
		t.Fatal(err)
	}
	if p.Dependencies["@ag-ui/core"] != v.Version {
		t.Fatalf("tools/aguischema pins @ag-ui/core %s, VERSION says %s", p.Dependencies["@ag-ui/core"], v.Version)
	}
}

// TestEditedBundleIsRefused: a hand-edited bundle no longer matches the hash VERSION records, so it does not load.
func TestEditedBundleIsRefused(t *testing.T) {
	dir, err := FindSchemaDir()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	for _, name := range []string{VersionFile, BundleFile} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if name == BundleFile {
			b = []byte(strings.Replace(string(b), `"TOOL_CALL_START"`, `"TOOL_CALL_BEGIN"`, 1))
		}
		if err := os.WriteFile(filepath.Join(tmp, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadSchemas(tmp); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("an edited bundle loaded: %v", err)
	}
}

func TestInputSchemaAcceptsWhatTheClientsSend(t *testing.T) {
	s := vendored(t)
	ok := `{"threadId":"ses1","runId":"r1","state":{"todos":[]},"messages":[{"id":"m1","role":"user","content":"hi"}],
	"tools":[{"name":"addTodo","description":"adds one","parameters":{"type":"object"}}],
	"context":[{"description":"page","value":"/home"}],"forwardedProps":{"x":1},
	"resume":[{"interruptId":"prep_1","status":"resolved","payload":{"approved":true}}]}`
	if err := s.ValidateInput([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateInput([]byte(`{"threadId":"t","runId":"r"}`)); err == nil {
		t.Fatal("a run input without messages was accepted")
	}
	if _, err := ParseRunAgentInput([]byte(ok)); err != nil {
		t.Fatal(err)
	}
}

func TestParseEventAcceptsChunksAndUnknownEventsOnInputOnly(t *testing.T) {
	for _, tc := range []struct {
		doc   string
		known bool
		chunk bool
	}{
		{`{"type":"TEXT_MESSAGE_CHUNK","messageId":"m","delta":"x"}`, true, true},
		{`{"type":"TOOL_CALL_CHUNK","toolCallId":"c","delta":"{}"}`, true, true},
		{`{"type":"RAW","event":{}}`, true, false},
		{`{"type":"SOMETHING_FROM_A_NEWER_RELEASE"}`, false, false},
	} {
		p, err := ParseEvent([]byte(tc.doc))
		if err != nil || p.Known != tc.known || p.Chunk != tc.chunk {
			t.Errorf("%s: %+v, %v", tc.doc, p, err)
		}
		if Emittable(p.Type) {
			t.Errorf("%s: a type that is only accepted on input is Emittable", p.Type)
		}
	}
	for _, bad := range []string{`not json`, `{"type":""}`, `{}`, `[]`} {
		if _, err := ParseEvent([]byte(bad)); err == nil {
			t.Errorf("%q was accepted as an event", bad)
		}
	}
}

func TestEncodeRefusesWhatMustNotBeSent(t *testing.T) {
	if _, err := Encode(NewCustom("alixai.x", map[string]any{"ok": 1})); err != nil {
		t.Fatal(err)
	}
	// A credential-shaped member, at any depth.
	for _, key := range []string{"token", "Authorization", "api_key", "confirmationToken", "client-secret", "password"} {
		if _, err := Encode(NewCustom("alixai.x", map[string]any{"a": []any{map[string]any{key: "v"}}})); err == nil {
			t.Errorf("a %q member was sent", key)
		}
	}
	// A member that merely mentions a token count is not a credential.
	if _, err := Encode(NewCustom("alixai.x", map[string]any{"token_count": 3})); err != nil {
		t.Errorf("token_count was refused: %v", err)
	}
	// A withheld value, wherever it sits.
	if _, err := Encode(NewTextMessageContent("m", "the code is cfm_SECRETVALUE1"), "cfm_SECRETVALUE1"); err == nil {
		t.Error("a withheld value was sent")
	}
	// An oversized event.
	if _, err := Encode(NewTextMessageContent("m", strings.Repeat("x", MaxEventBytes))); err == nil {
		t.Error("an oversized event was sent")
	}
	// A type that is never emitted cannot be encoded even if someone builds one.
	if _, err := Encode(rawEvent{base{Raw}}); err == nil {
		t.Error("a RAW event was encoded")
	}
	if _, err := Encode(nil); err == nil {
		t.Error("no event was encoded")
	}
	// The credential list is the stream hub's: a name the hub refuses is refused here (see internal/stream.credentialKeys).
	for _, name := range []string{"token", "secret", "password", "authorization", "apikey", "cookie", "privatekey", "credential"} {
		if !credentialKeys[normalizeKey(name)] {
			t.Errorf("%s is not in the credential list", name)
		}
	}
}

type rawEvent struct{ base }

var _ = conversation.TurnEvent{}

func TestCapabilitiesAreValidAndNeverOverclaim(t *testing.T) {
	doc := Capabilities("0.0.0-test")
	agent, err := json.Marshal(doc.Agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := vendored(t).Validate("AgentCapabilities", agent); err != nil {
		t.Fatalf("the capabilities do not validate against the vendored schema: %v", err)
	}
	if doc.ProtocolVersion != ProtocolVersion || len(doc.EventsEmitted) != len(EmittedTypes()) || len(doc.EventsNotEmitted) != len(NotEmittedTypes()) {
		t.Fatalf("%+v", doc)
	}
	claims := func(path ...string) any {
		var cur any = doc.Agent
		for _, p := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("no %v", path)
			}
			cur = m[p]
		}
		return cur
	}
	// What is not emitted is not claimed.
	for _, path := range [][]string{{"multiAgent", "supported"}, {"reasoning", "supported"}, {"reasoning", "encrypted"}, {"reasoning", "streaming"},
		{"tools", "clientProvided"}, {"multimodal", "input", "audio"}, {"multimodal", "input", "video"}, {"multimodal", "output", "audio"}, {"execution", "codeExecution"}} {
		if claims(path...) != false {
			t.Errorf("%v is claimed", path)
		}
	}
	for _, id := range []string{"reasoning_events", "subagent_events", "raw_events", "voice_full_duplex", "client_tool_calls"} {
		found := false
		for _, n := range doc.NotClaimed {
			found = found || n == id
		}
		if !found {
			t.Errorf("%s is not listed as not claimed", id)
		}
	}
	if blob := strings.ToLower(string(agent)); strings.Contains(blob, "duplex") || strings.Contains(blob, "realtime") {
		t.Errorf("the capabilities mention full duplex or realtime voice: %s", blob)
	}
}
