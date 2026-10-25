package providers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestChatRequestValidate(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	tests := []struct {
		name   string
		mutate func(r *providers.ChatRequest)
		ok     bool
	}{
		{"valid", func(r *providers.ChatRequest) {}, true},
		{"valid router role", func(r *providers.ChatRequest) { r.Role = providers.RoleRouter }, true},
		{"valid schema", func(r *providers.ChatRequest) { r.Output = providers.SchemaConstraint(schema) }, true},
		{"valid grammar", func(r *providers.ChatRequest) { r.Output = providers.GrammarConstraint(`root ::= "yes" | "no"`) }, true},
		{"embedder is not a text role", func(r *providers.ChatRequest) { r.Role = providers.RoleEmbedder }, false},
		{"unknown role", func(r *providers.ChatRequest) { r.Role = "wizard" }, false},
		{"no messages", func(r *providers.ChatRequest) { r.Messages = nil }, false},
		{"unknown message role", func(r *providers.ChatRequest) { r.Messages[1].Role = "root" }, false},
		{"untrusted system message", func(r *providers.ChatRequest) { r.Messages[0].Trusted = false }, false},
		{"schema and grammar together", func(r *providers.ChatRequest) {
			r.Output = providers.OutputConstraint{JSONSchema: schema, Grammar: "root ::= x"}
		}, false},
		{"schema that is not an object", func(r *providers.ChatRequest) {
			r.Output = providers.SchemaConstraint(json.RawMessage(`[1,2]`))
		}, false},
		{"schema that is not JSON", func(r *providers.ChatRequest) {
			r.Output = providers.SchemaConstraint(json.RawMessage(`{"type":`))
		}, false},
		{"blank grammar", func(r *providers.ChatRequest) { r.Output = providers.GrammarConstraint("   ") }, false},
		{"negative max tokens", func(r *providers.ChatRequest) { r.MaxTokens = -1 }, false},
		{"temperature too high", func(r *providers.ChatRequest) { r.Temperature = 2.5 }, false},
		{"negative temperature", func(r *providers.ChatRequest) { r.Temperature = -0.1 }, false},
		{"NaN temperature", func(r *providers.ChatRequest) { r.Temperature = math.NaN() }, false},
		{"five stop sequences", func(r *providers.ChatRequest) { r.Stop = []string{"a", "b", "c", "d", "e"} }, false},
		{"empty stop sequence", func(r *providers.ChatRequest) { r.Stop = []string{""} }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := chatReq("hello")
			tt.mutate(&r)
			err := r.Validate()
			if tt.ok != (err == nil) {
				t.Fatalf("Validate() = %v, want ok=%v", err, tt.ok)
			}
			if err != nil {
				wantCode(t, err, contracts.ErrValidationFailed)
			}
		})
	}
}

// Content is untrusted unless the prompt builder says otherwise.
func TestMessageTrustedDefaultsToUntrusted(t *testing.T) {
	var m providers.Message
	if m.Trusted {
		t.Fatal("the zero Message must be untrusted")
	}
	r := providers.ChatRequest{Messages: []providers.Message{{Role: providers.MsgSystem, Content: "template"}}}
	wantCode(t, r.Validate(), contracts.ErrValidationFailed)
	r.Messages[0].Trusted = true
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(providers.Message{Role: providers.MsgUser, Content: "x"})
	if !strings.Contains(string(b), `"trusted":false`) {
		t.Fatalf("the marker must always be serialised, got %s", b)
	}
}

func TestConstraintKinds(t *testing.T) {
	if (providers.OutputConstraint{}).Kind() != providers.ConstraintNone {
		t.Fatal("zero constraint should be none")
	}
	if providers.SchemaConstraint(json.RawMessage(`{}`)).Kind() != providers.ConstraintJSONSchema {
		t.Fatal("schema kind")
	}
	if providers.GrammarConstraint("root ::= a").Kind() != providers.ConstraintGrammar {
		t.Fatal("grammar kind")
	}
}

func TestChatRequestCloneIsDeep(t *testing.T) {
	r := chatReq("a")
	r.Stop = []string{"x"}
	r.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object"}`))
	c := r.Clone()
	r.Messages[1].Content = "changed"
	r.Stop[0] = "changed"
	r.Output.JSONSchema[2] = 'X'
	if c.Messages[1].Content != "a" || c.Stop[0] != "x" || strings.Contains(string(c.Output.JSONSchema), "X") {
		t.Fatalf("Clone aliases the original: %+v", c)
	}
}

func TestParseJSONOutput(t *testing.T) {
	good := []string{`{"a":1}`, "  {\"a\":[1,2]}\n", `[]`, `"s"`, `0`}
	for _, g := range good {
		j, err := providers.ParseJSONOutput(g)
		if err != nil || string(j) != strings.TrimSpace(g) {
			t.Fatalf("ParseJSONOutput(%q) = %q, %v", g, j, err)
		}
	}
	for _, bad := range []string{``, `   `, `{"a":`, `{"a":1} trailing`, `{"a":1}{"b":2}`, `not json`} {
		_, err := providers.ParseJSONOutput(bad)
		wantCode(t, err, contracts.ErrSchemaInvalid)
		if strings.Contains(err.Error(), "trailing") && bad != "" {
			t.Fatalf("the error must not echo model output: %v", err)
		}
	}
}

// scripted is a ChatStream that replays steps.
type scripted struct {
	steps  []step
	closed int
}

type step struct {
	chunk providers.ChatChunk
	err   error
}

func (s *scripted) Recv() (providers.ChatChunk, error) {
	if len(s.steps) == 0 {
		return providers.ChatChunk{}, io.EOF
	}
	st := s.steps[0]
	s.steps = s.steps[1:]
	return st.chunk, st.err
}

func (s *scripted) Close() error { s.closed++; return nil }

func TestCollect(t *testing.T) {
	delta := func(s string) step { return step{chunk: providers.ChatChunk{Delta: s}} }
	done := step{chunk: providers.ChatChunk{
		Done: true, FinishReason: providers.FinishLength, TraceID: "tr-1", Model: "m",
		Usage: providers.Usage{InputTokens: 3, OutputTokens: 4},
	}}
	jsonOut := providers.SchemaConstraint(json.RawMessage(`{"type":"object"}`))

	t.Run("complete stream", func(t *testing.T) {
		s := &scripted{steps: []step{delta("ab"), delta("cd"), done}}
		res, err := providers.Collect(s, providers.OutputConstraint{})
		if err != nil || res.Text != "abcd" || res.TraceID != "tr-1" || res.FinishReason != providers.FinishLength || res.Usage.Total() != 7 {
			t.Fatalf("Collect = %+v, %v", res, err)
		}
		if s.closed != 1 {
			t.Fatalf("Close called %d times", s.closed)
		}
	})
	t.Run("constrained output is parsed", func(t *testing.T) {
		res, err := providers.Collect(&scripted{steps: []step{delta(`{"a"`), delta(`:1}`), done}}, jsonOut)
		if err != nil || string(res.JSON) != `{"a":1}` {
			t.Fatalf("Collect = %+v, %v", res, err)
		}
	})
	t.Run("constrained output that does not parse is schema_invalid", func(t *testing.T) {
		res, err := providers.Collect(&scripted{steps: []step{delta(`{"a"`), done}}, jsonOut)
		wantCode(t, err, contracts.ErrSchemaInvalid)
		if res.Text != "" {
			t.Fatalf("no text may come back with the error: %+v", res)
		}
	})
	t.Run("EOF before the final chunk is unavailable", func(t *testing.T) {
		res, err := providers.Collect(&scripted{steps: []step{delta("ab"), delta("cd")}}, providers.OutputConstraint{})
		wantCode(t, err, contracts.ErrUnavailable)
		if res.Text != "" {
			t.Fatalf("a truncated stream returned text %q", res.Text)
		}
	})
	t.Run("an error mid-stream records the partial length but not the text", func(t *testing.T) {
		boom := step{err: errors.New("connection reset")}
		res, err := providers.Collect(&scripted{steps: []step{delta("abc"), boom}}, providers.OutputConstraint{})
		wantCode(t, err, contracts.ErrUnavailable)
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce.Details["partial_len"] != 3 || res.Text != "" {
			t.Fatalf("got %+v / %+v", ce, res)
		}
		if strings.Contains(err.Error(), "abc") {
			t.Fatalf("partial text leaked into the error: %v", err)
		}
	})
	t.Run("a typed adapter error keeps its code and is not mutated", func(t *testing.T) {
		orig := contracts.New(contracts.ErrResourceExhausted, "out of memory")
		_, err := providers.Collect(&scripted{steps: []step{delta("abc"), {err: orig}}}, providers.OutputConstraint{})
		wantCode(t, err, contracts.ErrResourceExhausted)
		if orig.Details != nil {
			t.Fatal("Collect mutated the adapter's error")
		}
	})
	t.Run("data after the final chunk is internal", func(t *testing.T) {
		_, err := providers.Collect(&scripted{steps: []step{done, delta("late")}}, providers.OutputConstraint{})
		wantCode(t, err, contracts.ErrInternal)
	})
	t.Run("a canceled stream keeps context.Canceled", func(t *testing.T) {
		_, err := providers.Collect(&scripted{steps: []step{delta("ab"), {err: fmt.Errorf("x: %w", context.Canceled)}}}, providers.OutputConstraint{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestContextError(t *testing.T) {
	if providers.ContextError(nil) != nil {
		t.Fatal("nil in, nil out")
	}
	dl := providers.ContextError(context.DeadlineExceeded)
	wantCode(t, dl, contracts.ErrTimeout)
	if !errors.Is(dl, context.DeadlineExceeded) {
		t.Fatal("timeout must keep DeadlineExceeded")
	}
	if again := providers.ContextError(dl); again != dl {
		t.Fatal("an already-mapped error is returned unchanged")
	}
	cn := providers.ContextError(context.Canceled)
	if !errors.Is(cn, context.Canceled) || contracts.HasCode(cn, contracts.ErrTimeout) {
		t.Fatalf("cancel mapped wrongly: %v", cn)
	}
	other := errors.New("other")
	if providers.ContextError(other) != other {
		t.Fatal("other errors pass through")
	}
}

func TestUsageAndEstimates(t *testing.T) {
	if (providers.Usage{InputTokens: 2, OutputTokens: 5}).Total() != 7 {
		t.Fatal("Total")
	}
	b, _ := json.Marshal(providers.Usage{InputTokens: 2, OutputTokens: 5})
	if string(b) != `{"tok_in":2,"tok_out":5}` {
		t.Fatalf("usage JSON keys must avoid the redacted word: %s", b)
	}
	if providers.EstimateTokens("") != 0 || providers.EstimateTokens("a") != 1 || providers.EstimateTokens("abcd") != 2 {
		t.Fatal("EstimateTokens rounding")
	}
	// Deliberately pessimistic: never fewer tokens than bytes/4.
	s := strings.Repeat("hello world ", 100)
	if providers.EstimateTokens(s) < len(s)/4 {
		t.Fatal("estimator underestimates")
	}
	for _, h := range []providers.Health{providers.HealthOK, providers.HealthDegraded, providers.HealthUnavailable} {
		if !h.Valid() {
			t.Fatalf("%s should be valid", h)
		}
	}
	if providers.Health("fine").Valid() {
		t.Fatal("unknown health accepted")
	}
}
