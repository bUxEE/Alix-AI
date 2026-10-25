package providers_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// legacyRuntime implements only the pre-X-06 contract, as adapters/localai
// does. It must keep registering and working: the rich contract is an
// extension, not a requirement.
type legacyRuntime struct{}

func (legacyRuntime) Name() string { return "legacy" }
func (legacyRuntime) Health(context.Context) (providers.HealthReport, error) {
	return providers.HealthReport{Status: providers.HealthOK}, nil
}
func (legacyRuntime) Chat(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatResult, error) {
	return providers.ChatResult{Text: "legacy"}, nil
}
func (legacyRuntime) ChatStream(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, nil
}

var (
	_ providers.TextGenerator     = legacyRuntime{}
	_ providers.AIRuntimeProvider = legacyRuntime{}

	_ providers.TextGenerator     = (*fake.Provider)(nil)
	_ providers.Embedder          = (*fake.Provider)(nil)
	_ providers.Reranker          = (*fake.Provider)(nil)
	_ providers.AIRuntimeProvider = (*fake.Provider)(nil)
)

func TestExistingProvidersUnchanged(t *testing.T) {
	t.Run("the existing interfaces have not grown", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			typ  reflect.Type
			want int
		}{
			{"TextGenerator", reflect.TypeOf((*providers.TextGenerator)(nil)).Elem(), 2},
			{"Embedder", reflect.TypeOf((*providers.Embedder)(nil)).Elem(), 1},
			{"Reranker", reflect.TypeOf((*providers.Reranker)(nil)).Elem(), 1},
			{"AIRuntimeProvider", reflect.TypeOf((*providers.AIRuntimeProvider)(nil)).Elem(), 2},
			{"ChatStream", reflect.TypeOf((*providers.ChatStream)(nil)).Elem(), 2},
		} {
			if got := tc.typ.NumMethod(); got != tc.want {
				t.Errorf("%s has %d methods, want %d: an extension must not widen an existing contract", tc.name, got, tc.want)
			}
		}
	})

	t.Run("a legacy-only runtime still registers; a rich-only one cannot take a text role", func(t *testing.T) {
		reg := providers.NewRegistry(clock.Real{})
		if err := reg.Register(providers.RoleController, legacyRuntime{}); err != nil {
			t.Fatal(err)
		}
		reg2 := providers.NewRegistry(clock.Real{})
		if err := reg2.Register(providers.RoleController, scriptGen{}); err == nil {
			t.Fatal("a MultimodalGenerator without TextGenerator took the controller role")
		}
		reg3 := providers.NewRegistry(clock.Real{})
		for _, role := range []providers.Role{providers.RoleController, providers.RoleRouter, providers.RoleEmbedder, providers.RoleReranker} {
			if err := reg3.Register(role, fake.New()); err != nil {
				t.Fatalf("fake no longer serves %s: %v", role, err)
			}
		}
	})

	t.Run("the wire shapes are frozen", func(t *testing.T) {
		req := providers.ChatRequest{
			Messages:    []providers.Message{{Role: providers.MsgSystem, Content: "s", Trusted: true}, {Role: providers.MsgUser, Content: "u"}},
			MaxTokens:   5,
			Temperature: 0.5,
			Stop:        []string{"x"},
		}
		res := providers.ChatResult{Text: "t", FinishReason: providers.FinishStop, Usage: providers.Usage{InputTokens: 1, OutputTokens: 2}, TraceID: "x", Model: "m"}
		chunk := providers.ChatChunk{Delta: "d"}
		for _, tc := range []struct {
			name string
			v    any
			want string
		}{
			{"ChatRequest", req, `{"messages":[{"role":"system","content":"s","trusted":true},{"role":"user","content":"u","trusted":false}],"output":{},"max_toks":5,"temperature":0.5,"stop":["x"]}`},
			{"ChatResult", res, `{"text":"t","finish_reason":"stop","usage":{"tok_in":1,"tok_out":2},"timings":{"ttft_ns":0,"total_ns":0},"trace_id":"x","model":"m"}`},
			{"ChatChunk", chunk, `{"delta":"d","usage":{"tok_in":0,"tok_out":0},"timings":{"ttft_ns":0,"total_ns":0}}`},
			{"Usage", providers.Usage{InputTokens: 3, OutputTokens: 4}, `{"tok_in":3,"tok_out":4}`},
		} {
			b, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.want {
				t.Errorf("%s JSON changed:\n got %s\nwant %s", tc.name, b, tc.want)
			}
		}
	})

	t.Run("text behaviour is unchanged", func(t *testing.T) {
		ctx, scope := context.Background(), testScope("ten_a")
		// a system message must still be trusted, in both shapes
		bad := chatReq("hi")
		bad.Messages[0].Trusted = false
		wantCode(t, bad.Validate(), contracts.ErrValidationFailed)
		wantCode(t, providers.FromChat(bad).Validate(), contracts.ErrValidationFailed)

		f := fake.New()
		f.On(fake.Contains("hi")).Reply("hello").Usage(3, 4)
		g, err := providers.WithBudget(f, providers.Budget{MaxPromptTokens: 100, MaxOutputTokens: 50, MaxBatchItems: 3, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		res, err := g.Chat(ctx, scope, chatReq("hi"))
		if err != nil || res.Text != "hello" || res.Usage != (providers.Usage{InputTokens: 3, OutputTokens: 4}) {
			t.Fatalf("Chat = %+v, %v", res, err)
		}
		s, err := g.ChatStream(ctx, scope, chatReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := providers.Collect(s, providers.OutputConstraint{})
		if err != nil || got.Text != "hello" {
			t.Fatalf("Collect = %+v, %v", got, err)
		}
		// a plain chat call records no rich request
		p := f.Prompts()[0]
		if p.Op != fake.OpChat || len(p.Rich.Messages) != 0 || len(p.Rich.Tools) != 0 {
			t.Fatalf("a text call recorded rich data: %+v", p)
		}
		if f.OpenStreams() != 0 {
			t.Fatal("stream leaked")
		}
	})
}
