package agui

import (
	"bytes"
	"encoding/json"
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// TestConfirmationSecretNeverAppearsInAnEvent: the secret is not a TurnEvent member, so no mapping can carry it; the
// events are checked for it by value besides; and a producer that put it into a data member (a summary, a fact, a
// message) by mistake has the run stopped, not the secret sent.
func TestConfirmationSecretNeverAppearsInAnEvent(t *testing.T) {
	env, _ := testEnv(t)
	req, err := intake(t, resumeInput("resolved", `{"approved":true}`), secretToken, env)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("it is in the request for the engine and nowhere an encoder can reach", func(t *testing.T) {
		if req.Input.Confirm.Token.Reveal() != secretToken {
			t.Fatal("the engine is not given the secret")
		}
		// The token prints and marshals as [redacted], so a request logged or encoded does not carry it.
		if strings.Contains(string(mustJSON(t, req)), "SECRETCANARY") {
			t.Fatalf("the request marshals the secret: %s", mustJSON(t, req))
		}
		rc := newRC()
		rc.Withhold = []string{secretToken}
		if strings.Contains(string(mustJSON(t, rc)), "SECRETCANARY") {
			t.Fatal("the run context, which the caller keeps with the stream, holds the secret")
		}
	})

	t.Run("no event of any scenario carries it, whether or not it is withheld", func(t *testing.T) {
		for _, sc := range scenarios(t) {
			sc.rc.Withhold = []string{secretToken}
			for _, e := range runAll(t, sc.rc, sc.events) {
				if bytes.Contains(e.Data, []byte("SECRETCANARY")) {
					t.Fatalf("%s: an event carries the secret: %s", sc.name, e.Data)
				}
				if key := credentialMember(e.Data); key != "" {
					t.Fatalf("%s: an event has a credential-shaped member %q: %s", sc.name, key, e.Data)
				}
			}
		}
	})

	t.Run("the interrupt carries the preparation id, never the secret that confirms it", func(t *testing.T) {
		rc := newRC()
		rc.Withhold = []string{secretToken}
		em := runAll(t, rc, scenarios(t)[3].events)
		fin := em[len(em)-1].Event.(RunFinishedEvent)
		blob := string(mustJSON(t, fin))
		if !strings.Contains(blob, testPrep) || strings.Contains(blob, "cfm_") {
			t.Fatalf("%s", blob)
		}
	})

	// The mutation: the same secret, put where a careless producer could put it. With the secret withheld the event is
	// refused and the run ends cleanly; the control (nothing withheld) shows the guard is what stops it.
	poisons := map[string]func(d map[string]any){
		"a prepared action summary": func(d map[string]any) {
			d["prepared_action"].(map[string]any)["summary"] = "Book it. Code: " + secretToken
		},
	}
	for name, mutate := range poisons {
		t.Run("poisoned: "+name, func(t *testing.T) {
			events := []conversation.TurnEvent{
				tev(t, "directive", 0), tev(t, "prepared-action", 1, mutate), token(t, 2, "Please confirm."), done(t, 3, "awaiting_confirmation", testPrep),
			}
			// Control: without the guard the secret goes out, so the guard below is what is being tested.
			control := runAll(t, newRC(), events)
			leaked := false
			for _, e := range control {
				leaked = leaked || bytes.Contains(e.Data, []byte(secretToken))
			}
			if !leaked {
				t.Fatal("the control did not leak: the poison is not in the stream, so the test proves nothing")
			}
			rc := newRC()
			rc.Withhold = []string{secretToken}
			m, _ := NewMapper(rc)
			var em []Emitted
			var mapErr error
			for _, ev := range events {
				got, err := m.Map(ev)
				em = append(em, got...)
				if err != nil {
					mapErr = err
					break
				}
			}
			if mapErr == nil {
				t.Fatal("the poisoned event was mapped")
			}
			for _, e := range em {
				if bytes.Contains(e.Data, []byte(secretToken)) {
					t.Fatalf("the secret was sent: %s", e.Data)
				}
			}
			if !m.Finished() {
				t.Fatal("the run was not ended")
			}
			validateAll(t, em)
			if _, ok := em[len(em)-1].Event.(RunErrorEvent); !ok {
				t.Fatalf("the run does not end with RUN_ERROR: %v", types(em))
			}
		})
	}
	t.Run("poisoned: a token of the response, an error and a block", func(t *testing.T) {
		for label, ev := range map[string]conversation.TurnEvent{
			"text":  token(t, 1, "your code is "+secretToken),
			"error": tev(t, "error-unavailable", 1, func(d map[string]any) { d["error"].(map[string]any)["message"] = secretToken }),
			"block": tev(t, "block-markdown", 1, func(d map[string]any) { d["block"].(map[string]any)["text"] = secretToken }),
		} {
			rc := newRC()
			rc.Withhold = []string{secretToken}
			m, _ := NewMapper(rc)
			if _, err := m.Map(tev(t, "directive", 0)); err != nil {
				t.Fatal(err)
			}
			got, err := m.Map(ev)
			if label == "error" {
				// An error event is held until the done event; it is refused there.
				if err != nil {
					t.Fatal(err)
				}
				got, err = m.Map(done(t, 2, "failed", ""))
			}
			if err == nil {
				t.Fatalf("%s: the secret was mapped", label)
			}
			for _, e := range got {
				if bytes.Contains(e.Data, []byte(secretToken)) {
					t.Fatalf("%s: the secret was sent", label)
				}
			}
		}
	})
}

// TestReasoningSubagentAndRawAreNeverEmitted: reasoning (and encrypted reasoning), sub-agent and RAW events, and the
// chunk forms, are accepted when read and never produced; there is no path from a TurnEvent to one.
func TestReasoningSubagentAndRawAreNeverEmitted(t *testing.T) {
	never := []EventType{
		Raw, ReasoningStart, ReasoningMessageStart, ReasoningMessageContent, ReasoningMessageEnd, ReasoningMessageChunk,
		ReasoningEnd, ReasoningEncryptedValue, SubagentStarted, SubagentFinished, SubagentError,
		TextMessageChunk, ToolCallChunk, "SOMETHING_NEW",
	}
	for _, ty := range never {
		if Emittable(ty) {
			t.Errorf("%s is Emittable", ty)
		}
		if _, err := Encode(fakeEvent{base{ty}}); err == nil {
			t.Errorf("a %s event was encoded", ty)
		}
	}
	for _, ty := range EmittedTypes() {
		for _, bad := range never {
			if ty == bad {
				t.Errorf("%s is both emitted and never emitted", ty)
			}
		}
	}

	// Whatever a turn contains, the events that come out are of the emitted types and carry none of the members that
	// belong to those events (raw payloads, encrypted values, sub-agent ids, usage, timestamps).
	forbiddenMembers := []string{`"rawEvent"`, `"encryptedValue"`, `"subagentRunId"`, `"usage"`, `"timestamp"`, `"parentRunId"`, `"input"`}
	check := func(label string, em []Emitted) {
		for _, e := range em {
			if !Emittable(e.Event.Kind()) {
				t.Fatalf("%s: a %s event was produced", label, e.Event.Kind())
			}
			s := string(e.Data)
			for _, banned := range []string{"REASONING", "SUBAGENT", `"RAW"`, "_CHUNK"} {
				if strings.Contains(s, banned) {
					t.Fatalf("%s: %s in %s", label, banned, s)
				}
			}
			for _, m := range forbiddenMembers {
				if strings.Contains(s, m) {
					t.Fatalf("%s: member %s in %s", label, m, s)
				}
			}
		}
	}
	for _, sc := range scenarios(t) {
		check(sc.name, runAll(t, sc.rc, sc.events))
	}
	// Turn text that looks like model reasoning is text like any other: the mapper has no notion of it.
	think := runAll(t, newRC(), []conversation.TurnEvent{token(t, 0, "<think>secret plan</think>done"), done(t, 1, "completed", "")})
	check("think", think)

	// Run input never turns into reasoning, sub-agent or raw output either: a client's messages of those roles are
	// dropped, not forwarded.
	env, _ := testEnv(t)
	doc := `{"threadId":"ses1","runId":"r","messages":[
		{"id":"a","role":"reasoning","content":"hidden chain of thought","encryptedValue":"abc"},
		{"id":"b","role":"activity","activityType":"x","content":{"k":"v"}},
		{"id":"c","role":"user","content":"hi","subagentRunId":"sub1"}]}`
	req, err := intake(t, doc, "", env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(req.Context.Text, "chain of thought") || strings.Contains(req.Context.Text, "abc") {
		t.Fatalf("a reasoning message was forwarded: %q", req.Context.Text)
	}
}

type fakeEvent struct{ base }

// TestTheMappingHasNoPathToAModel: suggestions, state, interrupts and the whole mapping are computed; the package
// imports nothing that could call a model, a provider, an adapter or the network.
func TestTheMappingHasNoPathToAModel(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{
		"internal/providers", "adapters/", "net/http", "internal/rag", "internal/gateway", "internal/stream",
		"internal/egress", "internal/decision",
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(gotoken.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, b := range banned {
				if strings.Contains(path, b) {
					t.Errorf("%s imports %s: the mapping must not be able to call a model or the network", f, path)
				}
			}
		}
	}
	_ = json.Marshal
}
