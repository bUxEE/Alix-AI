package dialogue

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestStreamVerifiedEmitsSegmentsThenEqualsNonStreamed(t *testing.T) {
	var mu sync.Mutex
	var segs []VerifiedSegment
	good := "Sure. How many would you like?"
	e := newEnv(t, withConfig(Config{StreamVerified: true}), withDeps(func(d *Deps) {
		d.OnVerifiedSegment = func(_ context.Context, _ contracts.Scope, step VerifiedSegment) {
			mu.Lock()
			segs = append(segs, step)
			mu.Unlock()
		}
	}))
	e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget"}))
	e.onResponse(good)
	res := e.say(memberScope(), sayCreate)
	wantAct(t, res, "ask_missing")
	if res.Response.Source != verify.SourceModel || res.Response.Text != good {
		t.Fatalf("response %+v", res.Response)
	}
	if !res.Response.Streamed {
		t.Fatal("expected Streamed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(segs) == 0 {
		t.Fatal("no segments emitted")
	}
	var shown strings.Builder
	for _, s := range segs {
		if s.Replace {
			t.Fatalf("unexpected replace: %+v", s)
		}
		shown.WriteString(s.Text)
	}
	if shown.String() != good {
		t.Fatalf("streamed %q != %q", shown.String(), good)
	}
}

func TestStreamVerifiedUnknownNumberReplaces(t *testing.T) {
	var mu sync.Mutex
	var segs []VerifiedSegment
	e := newEnv(t, withConfig(Config{StreamVerified: true}), withDeps(func(d *Deps) {
		d.OnVerifiedSegment = func(_ context.Context, _ contracts.Scope, step VerifiedSegment) {
			mu.Lock()
			segs = append(segs, step)
			mu.Unlock()
		}
	}))
	e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget"}))
	e.onResponse("Sure. There are 99 left.")
	res := e.say(memberScope(), sayCreate)
	wantAct(t, res, "ask_missing")
	if res.Response.Source != verify.SourceTemplate || res.Response.Reason != verify.ReasonVerificationFailed {
		t.Fatalf("want template after failed segment, got %+v", res.Response)
	}
	mu.Lock()
	defer mu.Unlock()
	var sawReplace bool
	for _, s := range segs {
		if strings.Contains(s.Text, "99") && !s.Replace {
			t.Fatalf("unverified number emitted: %+v", segs)
		}
		if s.Replace {
			sawReplace = true
		}
	}
	if !sawReplace {
		t.Fatalf("expected replace event, got %+v", segs)
	}
}

func TestStreamVerifiedFirstSegmentBeforeEnd(t *testing.T) {
	var firstAt, endAt time.Time
	var once sync.Once
	e := newEnv(t, withConfig(Config{StreamVerified: true}), withDeps(func(d *Deps) {
		d.OnVerifiedSegment = func(_ context.Context, _ contracts.Scope, step VerifiedSegment) {
			if step.Replace || step.Text == "" {
				return
			}
			once.Do(func() { firstAt = time.Now() })
		}
	}))
	e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget"}))
	e.onResponse("Sure. How many would you like?")
	start := time.Now()
	res := e.say(memberScope(), sayCreate)
	endAt = time.Now()
	wantAct(t, res, "ask_missing")
	if firstAt.IsZero() {
		t.Fatal("no segment received")
	}
	if !firstAt.Before(endAt) && !firstAt.Equal(start) {
		// first segment must not wait for the whole turn to finish recording
		t.Fatalf("first=%v end=%v start=%v", firstAt, endAt, start)
	}
	if !res.Response.Streamed {
		t.Fatal("expected Streamed")
	}
}
