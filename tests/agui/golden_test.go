package aguigold

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/agui"
)

// writeEnv is the variable that makes TestGoldenStreamsAreCurrentAndValid rewrite the files (see doc.go).
const writeEnv = "AGUI_WRITE_GOLDEN"

const testdata = "testdata"

// The placeholders recorded for the confirmation secret, which is random and never written to a file.
const (
	secretRequestPlaceholder  = "<the confirmation secret the run that prepared the action returned in this header>"
	secretResponsePlaceholder = "<present: the single-use confirmation secret of the preparation>"
)

type goldenFile struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Protocol    string         `json:"protocol"`
	Source      string         `json:"source"`
	Request     goldenRequest  `json:"request"`
	Response    goldenResponse `json:"response"`
}

type goldenRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

type goldenResponse struct {
	Status      int               `json:"status"`
	ContentType string            `json:"content_type"`
	Headers     map[string]string `json:"headers,omitempty"`
	SSEFile     string            `json:"sse_file"`
	// IDs are the SSE ids (the resume cursors) in the order they appear, and EventTypes the type of every event.
	IDs        []string          `json:"ids"`
	EventTypes []string          `json:"event_types"`
	Events     []json.RawMessage `json:"events"`
}

// frames splits a recorded body into its frames: the id (or "") and the data line of each.
func frames(t *testing.T, body string) (ids, data []string) {
	t.Helper()
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		id, line := "", ""
		for _, l := range strings.Split(block, "\n") {
			k, v, ok := strings.Cut(l, ": ")
			if !ok {
				t.Fatalf("malformed SSE line %q", l)
			}
			switch k {
			case "id":
				id = v
			case "data":
				line = v
			default:
				t.Fatalf("unexpected SSE field %q", k)
			}
		}
		ids, data = append(ids, id), append(data, line)
	}
	return ids, data
}

// eventKeys names what an event is, for the expectations of a scenario: its type, and with a colon the activity type of an
// activity, the name of a custom event, the outcome of a finished run or the tool of a started call.
func eventKeys(ev map[string]any) []string {
	typ, _ := ev["type"].(string)
	keys := []string{typ}
	add := func(detail any) {
		if s, ok := detail.(string); ok && s != "" {
			keys = append(keys, typ+":"+s)
		}
	}
	switch typ {
	case "ACTIVITY_SNAPSHOT", "ACTIVITY_DELTA":
		add(ev["activityType"])
	case "CUSTOM":
		add(ev["name"])
	case "TOOL_CALL_START":
		add(ev["toolCallName"])
	case "RUN_FINISHED":
		if o, ok := ev["outcome"].(map[string]any); ok {
			add(o["type"])
		}
	}
	return keys
}

// verifyLifecycle is the ordering rules of @ag-ui/client's event verification: RUN_STARTED first, nothing after the end of
// the run, every step, text message and tool call opened before use and closed before the run ends, never opened twice.
func verifyLifecycle(evs []map[string]any) error {
	open := map[string]bool{}
	ended := false
	str := func(ev map[string]any, k string) string { s, _ := ev[k].(string); return s }
	for i, ev := range evs {
		typ := str(ev, "type")
		if ended {
			return fmt.Errorf("event %d (%s) follows the end of the run", i, typ)
		}
		if i == 0 && typ != "RUN_STARTED" {
			return fmt.Errorf("the first event is %s, want RUN_STARTED", typ)
		}
		start := func(kind, id string) error {
			if open[kind+":"+id] {
				return fmt.Errorf("event %d: %s %q is already open", i, kind, id)
			}
			open[kind+":"+id] = true
			return nil
		}
		finish := func(kind, id string) error {
			if !open[kind+":"+id] {
				return fmt.Errorf("event %d: %s %q is not open", i, kind, id)
			}
			delete(open, kind+":"+id)
			return nil
		}
		use := func(kind, id string) error {
			if !open[kind+":"+id] {
				return fmt.Errorf("event %d: %s %q is used before it is opened", i, kind, id)
			}
			return nil
		}
		var err error
		switch typ {
		case "RUN_STARTED":
			if i != 0 {
				err = fmt.Errorf("event %d: RUN_STARTED twice", i)
			}
		case "RUN_FINISHED", "RUN_ERROR":
			ended = true
			if len(open) != 0 {
				err = fmt.Errorf("event %d: the run ends with %v still open", i, open)
			}
		case "STEP_STARTED":
			err = start("step", str(ev, "stepName"))
		case "STEP_FINISHED":
			err = finish("step", str(ev, "stepName"))
		case "TEXT_MESSAGE_START":
			err = start("message", str(ev, "messageId"))
		case "TEXT_MESSAGE_CONTENT":
			err = use("message", str(ev, "messageId"))
		case "TEXT_MESSAGE_END":
			err = finish("message", str(ev, "messageId"))
		case "TOOL_CALL_START":
			err = start("call", str(ev, "toolCallId"))
		case "TOOL_CALL_ARGS":
			err = use("call", str(ev, "toolCallId"))
		case "TOOL_CALL_END":
			err = finish("call", str(ev, "toolCallId"))
		}
		if err != nil {
			return err
		}
	}
	if !ended {
		return fmt.Errorf("the run never ended")
	}
	return nil
}

// render is the files of one recording: name -> bytes. The same canon is applied to the request and the response.
func render(t *testing.T, sc scenario, rec *recorded) map[string][]byte {
	t.Helper()
	c := newCanon()
	body := rec.sse
	var reqBody json.RawMessage
	if rec.body != nil {
		raw, err := json.Marshal(rec.body)
		if err != nil {
			t.Fatal(err)
		}
		reqBody = json.RawMessage(c.apply(string(raw)))
	}
	sse := c.apply(body)
	ids, data := frames(t, sse)
	g := goldenFile{
		Name: sc.name, Description: sc.description, Protocol: "AG-UI " + agui.ProtocolVersion + " (" + agui.PinnedPackage + ")", Source: rec.source,
		Request:  goldenRequest{Method: rec.method, Path: c.apply(rec.path), Body: reqBody},
		Response: goldenResponse{Status: rec.status, ContentType: rec.contentType, SSEFile: sc.name + ".sse"},
	}
	if rec.secretRequest {
		g.Request.Headers = map[string]string{secretHeader: secretRequestPlaceholder}
	}
	if rec.secretResponse {
		g.Response.Headers = map[string]string{secretHeader: secretResponsePlaceholder}
	}
	for i, d := range data {
		if ids[i] != "" {
			g.Response.IDs = append(g.Response.IDs, ids[i])
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(d), &head); err != nil {
			t.Fatalf("%s: frame %d is not JSON: %v", sc.name, i, err)
		}
		g.Response.EventTypes = append(g.Response.EventTypes, head.Type)
		g.Response.Events = append(g.Response.Events, json.RawMessage(d))
	}
	var js bytes.Buffer
	enc := json.NewEncoder(&js)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(g); err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{sc.name + ".json": js.Bytes(), sc.name + ".sse": []byte(sse)}
}

// check holds a recording to everything the golden streams promise (doc.go).
func check(t *testing.T, sc scenario, rec *recorded, files map[string][]byte, schemas *agui.Schemas) {
	t.Helper()
	if rec.status != http.StatusOK || !strings.HasPrefix(rec.contentType, "text/event-stream") {
		t.Fatalf("status %d, content type %q: %s", rec.status, rec.contentType, rec.sse)
	}
	_, data := frames(t, string(files[sc.name+".sse"]))
	if len(data) == 0 {
		t.Fatal("an empty stream")
	}
	evs := make([]map[string]any, len(data))
	keys := map[string]bool{}
	for i, d := range data {
		if err := schemas.ValidateEvent([]byte(d)); err != nil {
			t.Fatalf("event %d does not validate against the vendored AG-UI schemas: %v\n%s", i, err, d)
		}
		p, err := agui.ParseEvent([]byte(d))
		if err != nil || !agui.Emittable(agui.EventType(p.Type)) {
			t.Fatalf("event %d does not parse as an emittable event: %v\n%s", i, err, d)
		}
		if err := json.Unmarshal([]byte(d), &evs[i]); err != nil {
			t.Fatal(err)
		}
		for _, k := range eventKeys(evs[i]) {
			keys[k] = true
		}
	}
	if err := verifyLifecycle(evs); err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	for _, want := range sc.expect {
		if !keys[want] {
			var have []string
			for k := range keys {
				have = append(have, k)
			}
			sort.Strings(have)
			t.Errorf("the stream has no %s event (it has %v)", want, have)
		}
	}
	if rec.body != nil {
		raw, _ := json.Marshal(rec.body)
		if err := schemas.ValidateInput(raw); err != nil {
			t.Errorf("the request is not a valid RunAgentInput: %v\n%s", err, raw)
		}
	}
	// The confirmation secret is a header: it is in no event and no file.
	for name, b := range files {
		for _, s := range rec.secrets {
			if s != "" && bytes.Contains(b, []byte(s)) {
				t.Errorf("%s carries a confirmation secret", name)
			}
		}
		if bytes.Contains(b, []byte("cfm_")) {
			t.Errorf("%s carries something shaped like a confirmation secret", name)
		}
	}
	if (rec.secretResponse) != (sc.name == "prepare_interrupt" || sc.name == "choice_answer") {
		t.Errorf("the response carried a secret header = %v: only a run that ended in the confirmation interrupt does", rec.secretResponse)
	}
}

// Every golden stream is current, valid and complete: a fresh recording of each scenario is byte for byte the file that is
// checked in, every event validates against the vendored AG-UI schemas and the lifecycle rules, and the files carry no
// secret. AGUI_WRITE_GOLDEN=1 rewrites them (doc.go).
func TestGoldenStreamsAreCurrentAndValid(t *testing.T) {
	dir, err := agui.FindSchemaDir()
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := agui.LoadSchemas(dir)
	if err != nil {
		t.Fatal(err)
	}
	write := os.Getenv(writeEnv) == "1"
	if write {
		if err := os.MkdirAll(testdata, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	produced := map[string]bool{}
	allKeys := map[string]bool{}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			rec := sc.run(t)
			files := render(t, sc, rec)
			check(t, sc, rec, files, schemas)
			for name, b := range files {
				produced[name] = true
				path := filepath.Join(testdata, name)
				if write {
					if err := os.WriteFile(path, b, 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				have, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("%s is missing (%v): regenerate with %s=1 go test ./tests/agui -run TestGoldenStreamsAreCurrentAndValid", path, err, writeEnv)
				}
				if !bytes.Equal(have, b) {
					t.Fatalf("%s is out of date: regenerate with %s=1 go test ./tests/agui -run TestGoldenStreamsAreCurrentAndValid and review the diff\n--- checked in\n%s\n--- recorded now\n%s", path, writeEnv, have, b)
				}
			}
			_, data := frames(t, string(files[sc.name+".sse"]))
			for _, d := range data {
				var ev map[string]any
				_ = json.Unmarshal([]byte(d), &ev)
				for _, k := range eventKeys(ev) {
					allKeys[k] = true
				}
			}
		})
	}
	if t.Failed() {
		return
	}
	// No file in testdata that no scenario produces.
	entries, err := os.ReadDir(testdata)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !produced[e.Name()] {
			t.Errorf("testdata/%s is produced by no scenario: delete it", e.Name())
		}
	}
	// The set as a whole shows what the protocol mapping promises.
	for _, want := range []string{
		"RUN_STARTED", "RUN_FINISHED:success", "RUN_FINISHED:interrupt", "RUN_FINISHED:cancelled", "RUN_ERROR", "STEP_STARTED", "STEP_FINISHED",
		"TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_END", "TOOL_CALL_START:alixai_confirm_action", "TOOL_CALL_ARGS", "TOOL_CALL_END", "TOOL_CALL_RESULT",
		"STATE_SNAPSHOT", "STATE_DELTA", "ACTIVITY_SNAPSHOT:alixai.attachments", "ACTIVITY_SNAPSHOT:alixai.citations", "ACTIVITY_DELTA:alixai.citations",
		"ACTIVITY_SNAPSHOT:alixai.block.chart", "ACTIVITY_SNAPSHOT:alixai.block.table", "ACTIVITY_SNAPSHOT:alixai.block.clarification_choices",
		"CUSTOM:alixai.directive", "CUSTOM:alixai.suggestions", "TOOL_CALL_START:highlightRow",
	} {
		if !allKeys[want] {
			t.Errorf("no golden stream shows %s", want)
		}
	}
	// The scenarios the plan names are all there.
	names := map[string]bool{}
	for _, sc := range scenarios {
		names[sc.name] = true
	}
	for _, want := range []string{"plain_answer", "clarification", "read_with_citations", "prepare_interrupt", "resume_approve", "resume_reject", "stale", "error",
		"state_snapshot", "state_delta", "suggestions", "attachments_preview", "chart", "table", "forms",
		"client_context", "ui_tool_call", "ui_tool_result", "ui_tool_undeclared", "ui_tool_unregistered", "ui_tool_capability_named"} {
		if !names[want] {
			t.Errorf("the scenario %s is missing", want)
		}
	}
}
