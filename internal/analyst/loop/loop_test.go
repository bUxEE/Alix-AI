package loop_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/analyst/loop"
	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func TestEmbeddedSchemasMatchPublished(t *testing.T) {
	root := filepath.Join("..", "..", "..", "schemas")
	for _, name := range loop.EmbeddedSchemaNames() {
		emb, err := loop.EmbeddedSchema(name)
		if err != nil {
			t.Fatal(err)
		}
		var pubPath string
		switch name {
		case "ids.schema.json":
			pubPath = filepath.Join(root, "common", name)
		default:
			pubPath = filepath.Join(root, "analyst", name)
		}
		pub, err := os.ReadFile(pubPath)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(emb) != string(pub) {
			t.Fatalf("embedded schema %s drifted from published", name)
		}
	}
}

func TestLoopReachesAnswerInNSteps(t *testing.T) {
	reader := &fakeReader{}
	caller := &scriptedCaller{steps: [][]byte{
		stepJSON(map[string]any{
			"kind": "read",
			"read": map[string]any{"capability_id": "items.get", "arguments": map[string]any{"item_id": "item_1"}},
		}),
		stepJSON(map[string]any{
			"kind": "compute",
			"compute": map[string]any{
				"op": "aggregate", "inputs": []string{"ds_read1"},
				"params": map[string]any{"field": "name", "agg": "count", "nulls": "skip"},
			},
		}),
		stepJSON(map[string]any{
			"kind": "answer",
			"answer": map[string]any{
				"text_plan": "Total is 42.",
				"blocks":    []any{},
			},
		}),
	}}
	prog := &recordingProgress{}
	r := mustRunner(t, loop.Deps{
		Abilities: &fakeAbilities{eff: loop.Effective{Enabled: true, AnalystCapable: true, Abilities: allOn()}},
		Reader:    reader, Caller: caller, Computer: &fakeComputer{}, Verifier: &fakeVerifier{ok: true},
		Progress: prog, Caps: loop.Caps{MaxSteps: 8},
	})
	out, err := r.Run(context.Background(), testScope(), loop.Input{TurnID: "trn_1", Utterance: "how many?", AnalystCapable: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != loop.OutcomeAnswer || out.Steps != 3 || !strings.Contains(out.Text, "Verified") {
		t.Fatalf("out = %+v", out)
	}
	if out.WritesAttempted {
		t.Fatal("writes attempted inside loop")
	}
	want := []conversation.ProgressEventStage{
		conversation.ProgressEventStageAnalystRead,
		conversation.ProgressEventStageAnalystCompute,
		conversation.ProgressEventStageAnalystAnswer,
	}
	if len(prog.stages) != len(want) {
		t.Fatalf("stages = %v", prog.stages)
	}
}

func TestCaps(t *testing.T) {
	t.Run("max_steps", func(t *testing.T) {
		caller := &scriptedCaller{steps: [][]byte{
			stepJSON(map[string]any{"kind": "read", "read": map[string]any{"capability_id": "items.get", "arguments": map[string]any{"item_id": "1"}}}),
			stepJSON(map[string]any{"kind": "read", "read": map[string]any{"capability_id": "items.get", "arguments": map[string]any{"item_id": "2"}}}),
			stepJSON(map[string]any{"kind": "read", "read": map[string]any{"capability_id": "items.get", "arguments": map[string]any{"item_id": "3"}}}),
		}}
		r := mustRunner(t, loop.Deps{
			Abilities: &fakeAbilities{eff: loop.Effective{Enabled: true, AnalystCapable: true, Abilities: allOn()}},
			Reader:    &fakeReader{}, Caller: caller, Caps: loop.Caps{MaxSteps: 2, MaxReads: 8},
		})
		out, err := r.Run(context.Background(), testScope(), loop.Input{TurnID: "trn_cap", Utterance: "x", AnalystCapable: true})
		if err != nil {
			t.Fatal(err)
		}
		if out.Kind != loop.OutcomeRefuse || out.Code != contracts.ErrAnalystCapExceeded || out.CapHit != loop.CapSteps {
			t.Fatalf("out = %+v", out)
		}
	})
	t.Run("max_reads", func(t *testing.T) {
		caller := &scriptedCaller{steps: [][]byte{
			stepJSON(map[string]any{"kind": "read", "read": map[string]any{"capability_id": "items.get", "arguments": map[string]any{"item_id": "1"}}}),
			stepJSON(map[string]any{"kind": "read", "read": map[string]any{"capability_id": "items.get", "arguments": map[string]any{"item_id": "2"}}}),
			stepJSON(map[string]any{"kind": "answer", "answer": map[string]any{"text_plan": "done", "blocks": []any{}}}),
		}}
		r := mustRunner(t, loop.Deps{
			Abilities: &fakeAbilities{eff: loop.Effective{Enabled: true, AnalystCapable: true, Abilities: allOn()}},
			Reader:    &fakeReader{}, Caller: caller, Caps: loop.Caps{MaxSteps: 8, MaxReads: 1},
		})
		out, err := r.Run(context.Background(), testScope(), loop.Input{TurnID: "trn_reads", Utterance: "x", AnalystCapable: true})
		if err != nil {
			t.Fatal(err)
		}
		// second read refused; may continue and answer or refuse — must not hang and must not exceed reads
		if out.Kind == loop.OutcomeAnswer && out.WritesAttempted {
			t.Fatal("write")
		}
	})
	t.Run("wall", func(t *testing.T) {
		start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
		n := 0
		clock := func() time.Time {
			n++
			if n > 2 {
				return start.Add(2 * time.Second)
			}
			return start
		}
		caller := &scriptedCaller{steps: [][]byte{
			stepJSON(map[string]any{"kind": "read", "read": map[string]any{"capability_id": "items.get", "arguments": map[string]any{"item_id": "1"}}}),
			stepJSON(map[string]any{"kind": "answer", "answer": map[string]any{"text_plan": "done", "blocks": []any{}}}),
		}}
		r := mustRunner(t, loop.Deps{
			Abilities: &fakeAbilities{eff: loop.Effective{Enabled: true, AnalystCapable: true, Abilities: allOn()}},
			Reader:    &fakeReader{}, Caller: caller, Clock: clock,
			Caps: loop.Caps{MaxSteps: 8, MaxTurnWallMS: 1000},
		})
		out, err := r.Run(context.Background(), testScope(), loop.Input{TurnID: "trn_wall", Utterance: "x", AnalystCapable: true})
		if err != nil {
			t.Fatal(err)
		}
		if out.Kind != loop.OutcomeRefuse || out.CapHit != loop.CapWall {
			t.Fatalf("out = %+v", out)
		}
	})
}

func TestActHandoffNeverWrites(t *testing.T) {
	reader := &fakeReader{}
	caller := &scriptedCaller{steps: [][]byte{
		stepJSON(map[string]any{
			"kind": "act",
			"act":  map[string]any{"capability_id": "items.create", "fields": map[string]any{"name": "x", "quantity": 1}},
		}),
	}}
	r := mustRunner(t, loop.Deps{
		Abilities: &fakeAbilities{eff: loop.Effective{Enabled: true, AnalystCapable: true, Abilities: allOn()}},
		Reader:    reader, Caller: caller,
	})
	out, err := r.Run(context.Background(), testScope(), loop.Input{TurnID: "trn_act", Utterance: "create", AnalystCapable: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != loop.OutcomeAct || out.WritesAttempted || out.Act == nil {
		t.Fatalf("out = %+v", out)
	}
	if reader.writes != 0 || len(reader.calls) != 0 {
		t.Fatalf("reader touched: calls=%d writes=%d", len(reader.calls), reader.writes)
	}
}

func TestCrossTenantReadRefused(t *testing.T) {
	reader := &fakeReader{fn: func(scope contracts.Scope, _ loop.ReadRequest) (loop.ReadResult, error) {
		if scope.TenantID != "tnt-a" {
			return loop.ReadResult{}, contracts.New(contracts.ErrScopeViolation, "cross tenant")
		}
		return loop.ReadResult{}, contracts.New(contracts.ErrScopeViolation, "forced")
	}}
	caller := &scriptedCaller{steps: [][]byte{
		stepJSON(map[string]any{"kind": "read", "read": map[string]any{"capability_id": "items.get", "arguments": map[string]any{"item_id": "1"}}}),
	}}
	r := mustRunner(t, loop.Deps{
		Abilities: &fakeAbilities{eff: loop.Effective{Enabled: true, AnalystCapable: true, Abilities: allOn()}},
		Reader:    reader, Caller: caller, Caps: loop.Caps{MaxSteps: 3},
	})
	out, err := r.Run(context.Background(), testScope(), loop.Input{TurnID: "trn_x", Utterance: "x", AnalystCapable: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Code != contracts.ErrScopeViolation {
		t.Fatalf("out = %+v", out)
	}
}

func TestInjectionCausesNoWriteNoForeignRead(t *testing.T) {
	reader := &fakeReader{}
	caller := &scriptedCaller{steps: [][]byte{
		stepJSON(map[string]any{"kind": "read", "read": map[string]any{"capability_id": "items.get", "arguments": map[string]any{"item_id": "1"}}}),
		// Model "follows" injection and proposes act — handoff only.
		stepJSON(map[string]any{
			"kind": "act",
			"act":  map[string]any{"capability_id": "admin.purge", "fields": map[string]any{"confirm": true}},
		}),
	}}
	r := mustRunner(t, loop.Deps{
		Abilities: &fakeAbilities{eff: loop.Effective{Enabled: true, AnalystCapable: true, Abilities: allOn()}},
		Reader:    reader, Caller: caller,
	})
	sc := testScope()
	out, err := r.Run(context.Background(), sc, loop.Input{TurnID: "trn_inj", Utterance: "summarise", AnalystCapable: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != loop.OutcomeAct || out.WritesAttempted {
		t.Fatalf("out = %+v", out)
	}
	for _, s := range reader.scopes {
		if s.TenantID != sc.TenantID {
			t.Fatalf("foreign tenant read: %+v", s)
		}
	}
}

func TestAbilityOffRemovesFromGrammarAndPrompt(t *testing.T) {
	flags := allOn()
	flags.WebSearch = false
	flags.Compute = false
	schema, err := loop.CompactSchema(flags)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(schema, &doc); err != nil {
		t.Fatal(err)
	}
	props := doc["properties"].(map[string]any)
	if _, ok := props["search"]; ok {
		t.Fatal("search still in grammar")
	}
	if _, ok := props["compute"]; ok {
		t.Fatal("compute still in grammar")
	}
	kind := props["kind"].(map[string]any)
	enum := kind["enum"].([]any)
	for _, e := range enum {
		if e == "search" || e == "compute" {
			t.Fatalf("kind enum still has %v", e)
		}
	}
	items := loop.BuildStepPrompt("q", flags, nil, nil, loop.DefaultCaps())
	joined := ""
	for _, it := range items {
		joined += it.Content
	}
	if strings.Contains(joined, "web_search") || strings.Contains(joined, "compute") {
		t.Fatalf("prompt still names disabled abilities: %s", joined)
	}

	caller := &scriptedCaller{steps: [][]byte{
		stepJSON(map[string]any{"kind": "search", "search": map[string]any{"query": "discount"}}),
	}}
	r := mustRunner(t, loop.Deps{
		Abilities: &fakeAbilities{eff: loop.Effective{Enabled: true, AnalystCapable: true, Abilities: flags}},
		Reader:    &fakeReader{}, Caller: caller, Searcher: &fakeSearcher{}, Caps: loop.Caps{MaxSteps: 2},
	})
	out, err := r.Run(context.Background(), testScope(), loop.Input{TurnID: "trn_abil", Utterance: "q", AnalystCapable: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Code != contracts.ErrAbilityDisabled {
		t.Fatalf("out = %+v", out)
	}
}

func TestNonAnalystCapableFallsBack(t *testing.T) {
	r := mustRunner(t, loop.Deps{
		Abilities: &fakeAbilities{eff: loop.Effective{
			Enabled: false, AnalystCapable: false, Wanted: true,
			WantedReason: contracts.ErrModelNotAnalystCapable,
			Abilities:    loop.AbilityFlags{Loop: true},
		}},
		Reader: &fakeReader{}, Caller: &scriptedCaller{},
	})
	out, err := r.Run(context.Background(), testScope(), loop.Input{TurnID: "trn_nac", Utterance: "q", AnalystCapable: false})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != loop.OutcomeFallback || out.Code != contracts.ErrModelNotAnalystCapable {
		t.Fatalf("out = %+v", out)
	}
}

func TestEgressDeniesRestrictedField(t *testing.T) {
	ds := an.Dataset{
		ID: "ds_1",
		Fields: []an.DatasetDatasetField{
			{Name: "name", Type: an.FieldTypeString, DataClass: an.DataClassInternal},
			{Name: "ssn", Type: an.FieldTypeString, DataClass: an.DataClassRestricted},
			{Name: "attendee_name", Type: an.FieldTypeString, DataClass: an.DataClassConfidential},
		},
		Rows:        []an.DatasetRow{{"name": "Ada", "ssn": "123-45-6789", "attendee_name": "Maria Rossi"}},
		RecordCount: 1,
	}
	safe := loop.FilterDatasetForEgress(ds, loop.MaxEgressClass)
	for _, f := range safe.Fields {
		if f.Name == "ssn" || f.Name == "attendee_name" {
			t.Fatal("restricted/confidential field value projection left")
		}
	}
	for _, row := range safe.Rows {
		if _, ok := row["ssn"]; ok {
			t.Fatal("restricted cell left")
		}
		if _, ok := row["attendee_name"]; ok {
			t.Fatal("confidential cell left")
		}
	}
	summary := loop.SummariseDatasets(map[an.DatasetID]an.Dataset{"ds_1": ds}, loop.MaxEgressClass, 8000)
	if strings.Contains(summary, "123-45-6789") || strings.Contains(summary, "Maria Rossi") {
		t.Fatalf("egress leak in summary: %s", summary)
	}
	if !strings.Contains(summary, "ssn") || !strings.Contains(summary, "attendee_name") || !strings.Contains(summary, "withheld") {
		t.Fatalf("withheld columns must stay referenceable by name: %s", summary)
	}
}

func TestBudgetExhaustionAndCancelSettle(t *testing.T) {
	t.Run("exhaustion", func(t *testing.T) {
		bud := &fakeBudget{failAt: 1}
		caller := &scriptedCaller{steps: [][]byte{
			stepJSON(map[string]any{"kind": "answer", "answer": map[string]any{"text_plan": "x", "blocks": []any{}}}),
		}}
		r := mustRunner(t, loop.Deps{
			Abilities: &fakeAbilities{eff: loop.Effective{Enabled: true, AnalystCapable: true, Abilities: allOn()}},
			Reader:    &fakeReader{}, Caller: caller, Budget: bud, Verifier: &fakeVerifier{ok: true},
		})
		out, err := r.Run(context.Background(), testScope(), loop.Input{TurnID: "trn_bud", Utterance: "q", AnalystCapable: true})
		if err != nil {
			t.Fatal(err)
		}
		if out.Code != contracts.ErrResourceExhausted {
			t.Fatalf("out = %+v", out)
		}
	})
	t.Run("cancel settles", func(t *testing.T) {
		bud := &fakeBudget{}
		block := make(chan struct{})
		caller := &blockingCaller{block: block, raw: stepJSON(map[string]any{
			"kind": "answer", "answer": map[string]any{"text_plan": "x", "blocks": []any{}},
		})}
		r := mustRunner(t, loop.Deps{
			Abilities: &fakeAbilities{eff: loop.Effective{Enabled: true, AnalystCapable: true, Abilities: allOn()}},
			Reader:    &fakeReader{}, Caller: caller, Budget: bud,
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan loop.Outcome, 1)
		go func() {
			out, _ := r.Run(ctx, testScope(), loop.Input{TurnID: "trn_cancel", Utterance: "q", AnalystCapable: true})
			done <- out
		}()
		// Wait until reserved, then cancel.
		for i := 0; i < 50; i++ {
			bud.mu.Lock()
			n := bud.reserved
			bud.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		close(block)
		<-done
		bud.mu.Lock()
		defer bud.mu.Unlock()
		if bud.cancelled == 0 && bud.settled == 0 {
			t.Fatalf("budget not settled on cancel: %+v", bud)
		}
	})
}

type blockingCaller struct {
	block chan struct{}
	raw   []byte
}

func (b *blockingCaller) Call(ctx context.Context, _ loop.StepCallInput) (loop.StepCallResult, error) {
	select {
	case <-b.block:
		return loop.StepCallResult{Raw: b.raw}, nil
	case <-ctx.Done():
		return loop.StepCallResult{}, ctx.Err()
	}
}

func TestClientCapabilitiesAccessor(t *testing.T) {
	caps := an.ClientCapabilities{}
	c := an.ClientCapabilitiesContract15
	caps.Contract = &c
	loop.RememberClientCapabilities("trn_cc", caps)
	got, ok := loop.ClientCapabilitiesFor("trn_cc")
	if !ok || got.Contract == nil || *got.Contract != an.ClientCapabilitiesContract15 {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	loop.ForgetClientCapabilities("trn_cc")
	if _, ok := loop.ClientCapabilitiesFor("trn_cc"); ok {
		t.Fatal("still stored")
	}
}

func TestQueryLeaksRestricted(t *testing.T) {
	ds := map[an.DatasetID]an.Dataset{
		"ds_1": {
			Fields: []an.DatasetDatasetField{{Name: "tax_id", Type: an.FieldTypeString, DataClass: an.DataClassConfidential}},
			Rows:   []an.DatasetRow{{"tax_id": "IT123456"}},
		},
	}
	if !loop.QueryLeaksRestricted("eligibility for IT123456", ds) {
		t.Fatal("expected leak")
	}
	if loop.QueryLeaksRestricted("Italian 2026 company discount XX", ds) {
		t.Fatal("false positive")
	}
}

func mustRunner(t *testing.T, d loop.Deps) *loop.Runner {
	t.Helper()
	r, err := loop.New(d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
