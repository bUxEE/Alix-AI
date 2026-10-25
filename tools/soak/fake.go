package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/pkg/sdk-go/sdktest"
)

// The harness's own fakes: a scripted memory source, a scripted stack probe and
// a fake AlixAI API (pkg/sdk-go/sdktest, which speaks the real HTTP contract
// with single-use tokens, sessions, turns and RAG sources). The tests drive the
// whole of Run with them in seconds, and `--fake` runs the same stack from the
// command line to try the harness end to end. A result from them is flagged
// synthetic: it measures the harness, not a stack.

// scriptedMemory is a MemorySource whose reading a test sets.
type scriptedMemory struct {
	mu  sync.Mutex
	s   MemSample
	err error
}

func (m *scriptedMemory) Describe() MemoryDescriptor {
	return MemoryDescriptor{Kind: MemSourceFake, Target: "scripted"}
}

func (m *scriptedMemory) Sample(_ context.Context) (MemSample, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s, m.err
}

// Set replaces the reading.
func (m *scriptedMemory) Set(s MemSample) {
	m.mu.Lock()
	m.s, m.err = s, nil
	m.mu.Unlock()
}

// Fail makes every reading fail.
func (m *scriptedMemory) Fail(err error) {
	m.mu.Lock()
	m.err = err
	m.mu.Unlock()
}

// scriptedProbe is a StackProbe whose reading a test changes. A change is tied to
// the number of reads (After), never to the clock: the first read is the
// preflight, so a change at read 3 or later is never part of the baseline, whatever
// the speed of the machine.
type scriptedProbe struct {
	mu       sync.Mutex
	st       StackStats
	err      error
	reads    int
	triggers []probeTrigger
}

type probeTrigger struct {
	at   int
	fn   func(*StackStats)
	done bool
}

func (p *scriptedProbe) Kind() string { return "fake" }

func (p *scriptedProbe) Stats(_ context.Context) (StackStats, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	for i := range p.triggers {
		if t := &p.triggers[i]; !t.done && p.reads >= t.at {
			t.done = true
			t.fn(&p.st)
		}
	}
	if p.err != nil {
		return StackStats{}, p.err
	}
	st := p.st
	st.Roles = make(map[string]RoleState, len(p.st.Roles))
	for k, v := range p.st.Roles {
		st.Roles[k] = v
	}
	return st, nil
}

// After runs fn, once, in the nth read of the probe (the first read is the
// preflight). fn runs under the probe's lock and must not call it.
func (p *scriptedProbe) After(n int, fn func(*StackStats)) {
	p.mu.Lock()
	p.triggers = append(p.triggers, probeTrigger{at: n, fn: fn})
	p.mu.Unlock()
}

// Update changes the reading in place.
func (p *scriptedProbe) Update(fn func(*StackStats)) {
	p.mu.Lock()
	fn(&p.st)
	p.mu.Unlock()
}

// Fail makes every reading fail.
func (p *scriptedProbe) Fail(err error) {
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
}

// fakeStack is a stack that does nothing but answer: the fake API, scripted
// memory (30% of the profile, no swap) and a scripted probe (a controller and an
// embedder ok, a router with the decision axis on, no reloads).
type fakeStack struct {
	API    *sdktest.Fake
	Mem    *scriptedMemory
	Probe  *scriptedProbe
	claims sdktest.Claims
	mix    Mix
}

// fakeSourcePassage is the passage the fake API declares for the ingestion source.
const fakeSourcePassage = "An appointment can be booked from 60 minutes to 90 days ahead."

// newFakeStack builds the fake stack for a profile, mix and decision axis.
func newFakeStack(profile models.Profile, mix Mix, decision string) (*fakeStack, error) {
	limit, ok := profileMemMiB(profile)
	if !ok {
		return nil, errors.New("unknown profile")
	}
	api := sdktest.New()
	if mix.Ingestion.Enabled {
		api.DeclareSource(mix.Ingestion.Source, "Clinic policies (fake)", fakeSourcePassage)
	}
	api.OnTurn(func(t sdktest.Turn) sdktest.TurnPlan {
		if strings.HasPrefix(t.Input.Text, "Book ") {
			return sdktest.PrepareAction(sdktest.PrepareInput{CapabilityID: "demo.book", Summary: "Book an appointment", Text: "Please confirm the booking."})
		}
		return sdktest.Reply("Fake answer.")
	})
	roles := map[string]RoleState{
		"controller": {Configured: true, Status: statusOK},
		"embedder":   {Configured: true, Status: statusOK},
	}
	if decision == DecisionOn {
		roles[roleRouter] = RoleState{Configured: true, Status: statusOK}
	}
	return &fakeStack{
		API:    api,
		Mem:    &scriptedMemory{s: MemSample{UsedMiB: 0.3 * limit, SwapKnown: true}},
		Probe:  &scriptedProbe{st: StackStats{Roles: roles, QueueKnown: true, ReloadsKnown: true}},
		claims: sdktest.Claims{PrincipalID: "usr_soak", SessionID: "ses_soak_fake", CapabilityScopes: []string{"demo.book", "demo.lookup"}},
		mix:    mix,
	}, nil
}

// Deps wires the fake stack into a run. The driver is the real SDK driver, over
// real HTTP to the fake API.
func (f *fakeStack) Deps() (Deps, error) {
	turns, err := f.API.Client(f.claims)
	if err != nil {
		return Deps{}, err
	}
	ingest, err := f.API.Client(f.claims)
	if err != nil {
		return Deps{}, err
	}
	drv, err := newSDKDriver(turns, ingest, f.claims.SessionID, f.mix.Ingestion)
	if err != nil {
		return Deps{}, err
	}
	drv.pollEvery = 5 * time.Millisecond
	return Deps{Mem: f.Mem, Probe: f.Probe, Driver: drv}, nil
}

// Close stops the fake API.
func (f *fakeStack) Close() { f.API.Close() }
