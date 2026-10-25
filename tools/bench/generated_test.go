package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers/fake"
	genbench "github.com/buxee/alix-ai/pkg/contracts/gen/bench"
)

// The tool keeps its own Result and Case types (they carry the scoring and
// aggregation code), but the schemas are the contract and tools/schemagen
// generates the public types from them. These tests tie the two together: a
// document written by the tool must decode into the generated type, fail
// closed and unchanged, and so must every corpus line. A schema, a generated
// package or a tool type that drifts from the others fails here.

func semanticallyEqual(t *testing.T, what string, a, b []byte) {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if !reflect.DeepEqual(x, y) {
		t.Errorf("%s: the generated type does not round-trip the document\n tool:      %.400s\n generated: %.400s", what, a, b)
	}
}

func TestResultRoundTripsThroughTheGeneratedType(t *testing.T) {
	corpus := realCorpus(t)
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	p := newOracle(corpus)
	p.On(caseIs("rt-en-02")).Fail(fake.FailTimeout)
	gen := &timedGen{inner: p, clk: clk, latency: scriptedLatency(corpus)}
	cfg := testRunConfig(t, corpus, gen, clk)
	cfg.Limit = 12
	cfg.Memory = &MemoryPlan{Source: &scriptedSource{failFrom: -1, readings: []MemSample{{UsedMiB: 300, LimitMiB: 8192}, {UsedMiB: 1800, LimitMiB: 8192}}}, Container: "c", Interval: time.Hour}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	data, err := marshalResult(res)
	if err != nil {
		t.Fatal(err)
	}

	var g genbench.Result
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&g); err != nil {
		t.Fatalf("the generated type refuses a result the tool wrote: %v", err)
	}
	again, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	semanticallyEqual(t, "result", data, again)

	t.Run("the shipped examples decode too", func(t *testing.T) {
		for _, name := range []string{"fake-lite-streamed", "localai-lite-with-memory-and-errors", "fake-4g-not-streamed-grammar"} {
			b, err := os.ReadFile(repoPath(t, "schemas/bench/examples/result/valid/"+name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var gg genbench.Result
			d := json.NewDecoder(bytes.NewReader(b))
			d.DisallowUnknownFields()
			if err := d.Decode(&gg); err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			out, _ := json.Marshal(gg)
			semanticallyEqual(t, name, b, out)
		}
	})

	t.Run("an unknown enum value is refused by the generated type", func(t *testing.T) {
		bad := bytes.Replace(data, []byte(`"profile": "lite"`), []byte(`"profile": "huge"`), 1)
		var gg genbench.Result
		if err := json.Unmarshal(bad, &gg); err == nil {
			t.Error("the generated type accepted a profile that does not exist")
		}
	})
}

func TestCorpusRoundTripsThroughTheGeneratedType(t *testing.T) {
	corpus := realCorpus(t)
	for _, c := range corpus.Cases {
		line, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var g genbench.CorpusCase
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&g); err != nil {
			t.Fatalf("%s: the generated type refuses a corpus line: %v", c.ID, err)
		}
		again, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		semanticallyEqual(t, c.ID, line, again)
	}
}
