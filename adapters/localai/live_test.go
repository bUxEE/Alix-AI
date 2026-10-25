//go:build live

package localai

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Live tests run against a real LocalAI, for example the container of
// deploy/container/localai (task M0-13 runs them):
//
//	ALIXAI_LOCALAI_URL=http://127.0.0.1:8080 ALIXAI_LOCALAI_KEY=... \
//	  go test -tags live -count=1 -v ./adapters/localai/
//
// Optional: ALIXAI_LOCALAI_MODEL (chat model, default alixai-smoke) and
// ALIXAI_LOCALAI_EMBED_MODEL (embedding model; the embedding test is skipped
// without it). Without URL and key every test here is skipped.

func liveClient(t *testing.T, mutate ...func(*Config)) *Client {
	t.Helper()
	url, key := os.Getenv("ALIXAI_LOCALAI_URL"), os.Getenv("ALIXAI_LOCALAI_KEY")
	if url == "" || key == "" {
		t.Skip("ALIXAI_LOCALAI_URL and ALIXAI_LOCALAI_KEY are not set")
	}
	model := os.Getenv("ALIXAI_LOCALAI_MODEL")
	if model == "" {
		model = "alixai-smoke"
	}
	cfg := Config{
		Endpoint:       url,
		Models:         ModelSet{Controller: model, Embedder: os.Getenv("ALIXAI_LOCALAI_EMBED_MODEL")},
		RequestTimeout: 5 * time.Minute, // the first call loads the backend and the model
	}
	for _, m := range mutate {
		m(&cfg)
	}
	c, err := New(cfg, WithAPIKey(NewSecret(key)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestLiveHealth(t *testing.T) {
	c := liveClient(t)
	rep, err := c.Health(bg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("health = %+v", rep)
	if rep.Status != providers.HealthOK || rep.Version == "" {
		t.Fatalf("report = %+v, want ok with a version", rep)
	}
	ids, err := c.ListModels(bg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("models = %v", ids)
}

func TestLiveChatPlain(t *testing.T) {
	c := liveClient(t)
	req := chatReq("Reply with the single word: ready")
	req.MaxTokens = 16
	res, err := c.Chat(bg, testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("res = %+v", res)
	if strings.TrimSpace(res.Text) == "" {
		t.Error("empty completion")
	}
}

// The tool role is sent as role "tool" (data, never promoted to user). Whether
// the pinned model's chat template accepts it without a tool call before it is
// unknown until a real run: a failure here is a finding for M0-13, and the
// prompt builder must then use a different carrier for retrieved text.
func TestLiveChatToolRoleMessage(t *testing.T) {
	c := liveClient(t)
	req := providers.ChatRequest{Messages: []providers.Message{
		{Role: providers.MsgSystem, Content: "Answer using only the document.", Trusted: true},
		{Role: providers.MsgTool, Content: "Document: the office opens at nine."},
		{Role: providers.MsgUser, Content: "When does the office open?"},
	}, MaxTokens: 32}
	res, err := c.Chat(bg, testScope(), req)
	if err != nil {
		t.Fatalf("a tool-role message was refused: %v (code %s)", err, contracts.CodeOf(err))
	}
	t.Logf("res = %+v", res)
}

func TestLiveChatGrammar(t *testing.T) {
	c := liveClient(t)
	req := chatReq("Is the sky blue? Answer yes or no.")
	req.Output = providers.GrammarConstraint(`root ::= "yes" | "no"`)
	req.MaxTokens = 8
	res, err := c.Chat(bg, testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "yes" && res.Text != "no" {
		t.Fatalf("grammar-constrained output = %q, want yes or no", res.Text)
	}
}

// LocalAI's json_schema constraint is best effort (package documentation), so
// this reports what the pinned build does with a typical schema. A
// schema_invalid failure here is a finding for M0-13, not noise.
func TestLiveChatJSONSchema(t *testing.T) {
	c := liveClient(t)
	schema := json.RawMessage(`{"type":"object","properties":{"intent":{"type":"string"},"urgent":{"type":"boolean"}},"required":["intent","urgent"]}`)
	req := chatReq("Classify: 'please book a room for tomorrow, it is urgent'. Answer as JSON.")
	req.Output = providers.SchemaConstraint(schema)
	req.MaxTokens = 64
	res, err := c.Chat(bg, testScope(), req)
	if err != nil {
		t.Fatalf("Chat: %v (code %s)", err, contracts.CodeOf(err))
	}
	var out map[string]any
	if err := json.Unmarshal(res.JSON, &out); err != nil {
		t.Fatal(err)
	}
	t.Logf("json = %s", res.JSON)
	if _, ok := out["intent"]; !ok {
		t.Errorf("output lacks the required field intent: %s", res.JSON)
	}
}

func TestLiveChatStream(t *testing.T) {
	c := liveClient(t)
	req := chatReq("Count from one to five.")
	req.MaxTokens = 48
	s, err := c.ChatStream(bg, testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	res, err := providers.Collect(s, req.Output)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("res = %+v", res)
	if res.Text == "" || res.Usage.OutputTokens == 0 {
		t.Errorf("stream result = %+v", res)
	}
	// A second call proves the connection is reusable after a stream.
	s, err = c.ChatStream(bg, testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := s.Recv(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
}

func TestLiveEmbed(t *testing.T) {
	if os.Getenv("ALIXAI_LOCALAI_EMBED_MODEL") == "" {
		t.Skip("ALIXAI_LOCALAI_EMBED_MODEL is not set")
	}
	c := liveClient(t, func(cfg *Config) { cfg.EmbedBatchSize = 2 })
	req := providers.EmbedRequest{Inputs: []string{"first", "second", "third"}, Purpose: providers.EmbedDocument}
	res, err := c.Embed(bg, testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dims = %d, model = %s", res.Dimensions, res.Model)
	if err := res.CheckResult(req); err != nil {
		t.Fatal(err)
	}
}

// Only readiness needs no key; the rest of the allowlist must demand it.
func TestLiveWrongKeyIsRejected(t *testing.T) {
	url := os.Getenv("ALIXAI_LOCALAI_URL")
	if url == "" || os.Getenv("ALIXAI_LOCALAI_KEY") == "" {
		t.Skip("ALIXAI_LOCALAI_URL and ALIXAI_LOCALAI_KEY are not set")
	}
	c, err := New(Config{Endpoint: url, Models: ModelSet{Controller: "alixai-smoke"}}, WithAPIKey(NewSecret(newTestKey(t))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListModels(bg); err == nil {
		t.Fatal("the runtime accepted an unknown key")
	}
	rep, err := c.Health(bg)
	if err != nil || rep.Status != providers.HealthUnavailable {
		t.Fatalf("health with a wrong key = %+v, %v", rep, err)
	}
}
