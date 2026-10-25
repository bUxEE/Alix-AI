package agui

import (
	"bytes"
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	iagui "github.com/buxee/alix-ai/internal/agui"
)

func TestFrameReaderAndEncoderRoundTripTheGoldenFraming(t *testing.T) {
	for _, g := range loadGoldens(t) {
		fr := NewFrameReader(bytes.NewReader(g.sse))
		var out bytes.Buffer
		enc := NewEncoder(&out)
		n := 0
		for {
			f, err := fr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%s: %v", g.Name, err)
			}
			if err := enc.WriteFrame(f); err != nil {
				t.Fatalf("%s: %v", g.Name, err)
			}
			n++
		}
		if n == 0 || !bytes.Equal(out.Bytes(), g.sse) {
			t.Errorf("%s: %d frames re-encoded to different bytes", g.Name, n)
		}
	}
}

func TestSSEIDBelongsToTheFrameThatCarriedIt(t *testing.T) {
	// The HTML standard keeps the last event id across frames; AG-UI's cursor does not (it is on the last event of a TurnEvent only).
	wire := "id: 3\ndata: a\n\ndata: b\n\n: comment\n\nid: 4\r\ndata: c\r\n\r\ndata: d1\ndata: d2\n\n"
	fr := NewFrameReader(strings.NewReader(wire))
	var got []Frame
	for {
		f, err := fr.Next()
		if err != nil {
			break
		}
		got = append(got, f)
	}
	if len(got) != 4 || !got[0].HasID || got[0].ID != "3" || got[1].HasID || !got[2].HasID || got[2].ID != "4" || got[3].HasID {
		t.Fatalf("frames = %+v", got)
	}
	if string(got[3].Data) != "d1\nd2" {
		t.Errorf("multi-line data = %q", got[3].Data)
	}
	// A frame cut off before its blank line is never delivered.
	fr = NewFrameReader(strings.NewReader("data: whole\n\ndata: half"))
	if f, err := fr.Next(); err != nil || string(f.Data) != "whole" {
		t.Fatal("the whole frame was not read")
	}
	if _, err := fr.Next(); err != io.EOF {
		t.Fatalf("a half frame was delivered (%v)", err)
	}
	// An event that never ends is refused.
	huge := "data: " + strings.Repeat("x", MaxFrameBytes+10) + "\n\n"
	if _, err := NewFrameReader(strings.NewReader(huge)).Next(); err == nil {
		t.Error("an oversize frame was accepted")
	}
}

func TestEncoderRefusesWhatItCannotFrame(t *testing.T) {
	var b bytes.Buffer
	enc := NewEncoder(&b)
	for _, f := range []Frame{
		{Data: []byte("a\rb")}, {ID: "1\n2", HasID: true, Data: []byte("x")}, {Event: "e\nf", Data: []byte("x")}, {Data: []byte{0xff, 0xfe}},
	} {
		if err := enc.WriteFrame(f); err == nil {
			t.Errorf("frame %+v was framed", f)
		}
	}
	if b.Len() != 0 {
		t.Errorf("a refused frame wrote %q", b.String())
	}
	rec := httptest.NewRecorder()
	if err := NewEncoder(rec).WriteEvent("7", NewRunError("boom", "x")); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != "id: 7\ndata: {\"type\":\"RUN_ERROR\",\"message\":\"boom\",\"code\":\"x\"}\n\n" || !rec.Flushed {
		t.Errorf("WriteEvent = %q (flushed %v)", rec.Body.String(), rec.Flushed)
	}
}

func TestEventTypesAreTheReleaseAndRoundTripTheGoldenEvents(t *testing.T) {
	// The set of types is the one internal/agui takes from the vendored schemas of the pinned release.
	var want []string
	for _, e := range iagui.EmittedTypes() {
		want = append(want, string(e))
	}
	for _, e := range iagui.NotEmittedTypes() {
		want = append(want, string(e))
	}
	var got []string
	for _, e := range AllTypes() {
		got = append(got, string(e))
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		t.Fatalf("the event types differ from internal/agui:\n got %v\nwant %v", got, want)
	}
	if ProtocolVersion != iagui.ProtocolVersion || PinnedPackage != iagui.PinnedPackage {
		t.Errorf("pinned release %s %s, internal/agui has %s %s", ProtocolVersion, PinnedPackage, iagui.ProtocolVersion, iagui.PinnedPackage)
	}
	dir, err := iagui.FindSchemaDir()
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := iagui.LoadSchemas(dir)
	if err != nil {
		t.Fatal(err)
	}
	typed := map[EventType]bool{}
	for _, g := range loadGoldens(t) {
		fr := NewFrameReader(bytes.NewReader(g.sse))
		for {
			f, err := fr.Next()
			if err != nil {
				break
			}
			if err := schemas.ValidateEvent(f.Data); err != nil {
				t.Errorf("%s: %v", g.Name, err)
			}
			ev, err := ParseEvent(f.Data)
			if err != nil {
				t.Fatalf("%s: %v\n%s", g.Name, err, f.Data)
			}
			if _, unknown := ev.(UnknownEvent); unknown {
				t.Errorf("%s: %s has no struct", g.Name, ev.Kind())
			}
			typed[ev.Kind()] = true
			// Open members (a directive, a block) come back in sorted key order, so the round trip is compared as JSON values.
			back, err := MarshalEvent(ev)
			var gotV, wantV any
			if err != nil || json.Unmarshal(back, &gotV) != nil || json.Unmarshal(f.Data, &wantV) != nil || !reflect.DeepEqual(gotV, wantV) {
				t.Errorf("%s: %s does not marshal back to the same event: got %s, want %s (%v)", g.Name, ev.Kind(), back, f.Data, err)
			}
		}
	}
	if len(typed) < 12 {
		t.Errorf("the golden streams exercised only %d event types", len(typed))
	}
	for _, e := range []string{`{"type":"REASONING_START"}`, `{"type":"SOMETHING_NEWER","x":1}`} {
		ev, err := ParseEvent([]byte(e))
		if u, ok := ev.(UnknownEvent); err != nil || !ok || string(u.Data) != e {
			t.Errorf("ParseEvent(%s) = %#v, %v", e, ev, err)
		}
	}
	for _, bad := range []string{``, `[]`, `{}`, `{"type":""}`, `{"type":5}`, `nope`} {
		if _, err := ParseEvent([]byte(bad)); err == nil {
			t.Errorf("ParseEvent(%q) accepted it", bad)
		}
	}
}

func TestMemorySecretStoreExpiresAndIsBounded(t *testing.T) {
	now := testNow
	s := NewMemorySecretStore(func() time.Time { return now }, 2)
	ctx := context.Background()
	a, b, c := SecretKey{"t", "a"}, SecretKey{"t", "b"}, SecretKey{"t", "c"}
	_ = s.Put(ctx, a, "sa", now.Add(time.Minute))
	_ = s.Put(ctx, b, "sb", now.Add(time.Minute))
	_ = s.Put(ctx, c, "sc", now.Add(time.Minute)) // evicts the oldest
	if _, ok, _ := s.Get(ctx, a); ok {
		t.Error("the oldest entry survived a full store")
	}
	if v, ok, _ := s.Get(ctx, c); !ok || v != "sc" {
		t.Error("the newest entry is missing")
	}
	if _, ok, _ := s.Get(ctx, SecretKey{"other", "c"}); ok {
		t.Error("a secret was found under another thread")
	}
	now = now.Add(2 * time.Minute)
	if _, ok, _ := s.Get(ctx, c); ok {
		t.Error("an expired secret was returned")
	}
	_ = s.Put(ctx, a, "sa", now.Add(-time.Second)) // already expired: not kept
	if s.Len() > 1 {
		t.Errorf("store holds %d entries", s.Len())
	}
}

// Neither SDK depends on CopilotKit or an AG-UI library (ADR 0035).
func TestPackageImportsNoCopilotKitOrAGUILibrary(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p := strings.ToLower(imp.Path.Value)
			if strings.Contains(p, "copilotkit") || strings.Contains(p, "ag-ui") || strings.Contains(p, "/internal/") {
				t.Errorf("%s imports %s", f, imp.Path.Value)
			}
		}
		src, _ := os.ReadFile(f)
		if strings.Contains(string(src), "slog.Default") || strings.Contains(string(src), "log.Printf") {
			t.Errorf("%s writes to a global logger", f)
		}
	}
}
