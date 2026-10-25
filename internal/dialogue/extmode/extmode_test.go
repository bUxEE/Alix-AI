package extmode

import (
	"encoding/json"
	"github.com/buxee/alix-ai/internal/providers"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	sayBook = "Book Luca Ferri for 2026-10-14 10:30"
	sayFree = "Which appointments are free on 2026-10-14?"
)

func (w *world) noCommits() {
	w.t.Helper()
	if _, commits := w.counter.counts(); commits != 0 {
		w.t.Fatalf("the host saw %d Commit calls, want 0", commits)
	}
}

func (w *world) noLocalCalls() {
	w.t.Helper()
	if n := w.local.Calls(); n != 0 {
		w.t.Fatalf("the local runtime was called %d times in external mode", n)
	}
}

// A provider can only propose. Nothing a Sender is given, and nothing in this package, reaches an
// adapter; the proposal ends at a prepared action awaiting the client's confirmation.
func TestExternalProviderCannotCallAdapterDirectly(t *testing.T) {
	t.Run("the package has no path to an adapter or the action coordinator", func(t *testing.T) {
		const mod = "github.com/buxee/alix-ai/"
		forbidden := []string{mod + "internal/adapters", mod + "internal/actions", mod + "adapters/", "net/http", "os/exec"}
		files, err := filepath.Glob("*.go")
		if err != nil || len(files) < 6 {
			t.Fatalf("glob: %v (%d files)", err, len(files))
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range af.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				for _, bad := range forbidden {
					if p == bad || (strings.HasSuffix(bad, "/") && strings.HasPrefix(p, bad)) {
						t.Errorf("%s imports %s", f, p)
					}
				}
			}
		}
	})
	t.Run("a tool call that names a consequential capability is prepared, never committed", func(t *testing.T) {
		w := newWorld(t)
		w.toolCall("appointments_book", `{"patient":"Luca Ferri","slot":"2026-10-14 10:30"}`)
		res := w.say(receptionist(), sayBook)

		wantAct(t, res, "request_confirmation")
		if res.Prepared == nil {
			t.Fatal("nothing was prepared")
		}
		w.noCommits()
		w.noLocalCalls()
		if len(w.sender.sent()) != 1 {
			t.Fatalf("the provider was called %d times, want 1", len(w.sender.sent()))
		}
	})
	t.Run("a proposal is validated before any adapter call", func(t *testing.T) {
		w := newWorld(t)
		w.toolCall("appointments_book", `{"patient":"Luca Ferri"}`)
		res := w.say(receptionist(), sayBook)

		wantAct(t, res, "ask_missing")
		if prepares, commits := w.counter.counts(); commits != 0 || res.Prepared != nil {
			t.Fatalf("prepares = %d, commits = %d, prepared = %v: an incomplete proposal must not reach the host", prepares, commits, res.Prepared)
		}
	})
}

// The provider's tool call becomes a TaskProposal and goes through the engine's own checks. The
// tools it was offered are the small allowlist of the turn; what it adds is dropped.
func TestProviderToolCallValidatedAsTaskProposal(t *testing.T) {
	w := newWorld(t)
	w.toolCall("appointments_book", `{"patient":"Luca Ferri","slot":"2026-10-14 10:30","discount":"100","is_admin":true}`)

	res := w.say(receptionist(), sayBook)

	wantAct(t, res, "request_confirmation")
	g, err := w.store.Open(ctxb(), receptionist())
	if err != nil {
		t.Fatal(err)
	}
	if g.State.Goal == nil || g.State.Goal.CapabilityID != "appointments.book" {
		t.Fatalf("goal = %+v", g.State.Goal)
	}
	for name := range g.State.Goal.Slots {
		if name == "discount" || name == "is_admin" {
			t.Fatalf("a field the provider invented reached the goal: %s", name)
		}
	}
	reqs := w.sender.sent()
	if len(reqs) != 1 {
		t.Fatalf("sends = %d", len(reqs))
	}
	var offered []string
	for _, tl := range reqs[0].Rich.Tools {
		offered = append(offered, tl.Name)
		if strings.Contains(string(tl.Parameters), "discount") {
			t.Fatalf("tool %s offers a field the contract does not have", tl.Name)
		}
	}
	if len(offered) == 0 || len(offered) > 6 {
		t.Fatalf("offered tools = %v, want the small shortlist of the turn", offered)
	}
	w.noCommits()
}

// A malformed call keeps the capability and drops the data, so the engine asks the user; arguments
// that are not JSON at all are rejected by the result check and fail the turn closed.
func TestMalformedToolCallRejectedWithClarification(t *testing.T) {
	t.Run("arguments of the wrong shape are dropped and the user is asked", func(t *testing.T) {
		w := newWorld(t)
		w.toolCall("appointments_book", `{"patient":{"nested":["x"]},"slot":["a",{"b":1}]}`)
		res := w.say(receptionist(), sayBook)

		wantAct(t, res, "ask_missing")
		if res.Prepared != nil {
			t.Fatal("a malformed call prepared an action")
		}
		w.noCommits()
	})
	t.Run("an invalid value is asked again", func(t *testing.T) {
		w := newWorld(t)
		w.toolCall("appointments_book", `{"patient":"Luca Ferri","slot":"not a time at all"}`)
		res := w.say(receptionist(), sayBook)

		if a := string(res.Directive.Act); a != "ask_invalid" && a != "ask_missing" && a != "disambiguate" {
			t.Fatalf("act = %s, want a clarification", a)
		}
		if res.Prepared != nil {
			t.Fatal("an invalid value prepared an action")
		}
		w.noCommits()
	})
	t.Run("truncated JSON arguments fail the turn closed", func(t *testing.T) {
		w := newWorld(t)
		w.ext.On(offersAnyTool()).ProposeToolCall("call_1", "appointments_book", `{"patient": `).Times(1)
		res := w.say(receptionist(), sayBook)

		if !res.Failed() || res.Prepared != nil {
			t.Fatalf("code = %q prepared = %v, want a typed failure", res.Code, res.Prepared)
		}
		w.noCommits()
	})
}

// A tool the contract did not offer is an unknown capability: refused, never run.
func TestUnknownCapabilityProposalRefused(t *testing.T) {
	for _, tool := range []string{"delete_all_patients", "export_everything", "Admin.Shell"} {
		t.Run(tool, func(t *testing.T) {
			w := newWorld(t)
			w.toolCall(tool, `{"everything":true}`)
			res := w.say(receptionist(), sayBook)

			// The result check refuses a tool that was not on the allowlist (schema_invalid); a name that
			// got past it would be refused by the engine as unknown_capability. Either way it is a refusal.
			if res.Code != contracts.ErrUnknownCapability && res.Code != contracts.ErrSchemaInvalid {
				t.Fatalf("code = %q (act %s), want a refusal", res.Code, res.Directive.Act)
			}
			if res.Prepared != nil {
				t.Fatal("an unknown capability prepared an action")
			}
			if prepares, commits := w.counter.counts(); prepares != 0 || commits != 0 {
				t.Fatalf("the host saw prepares=%d commits=%d for an unknown capability", prepares, commits)
			}
		})
	}
}

var _ = dialogue.ClassPublic
var _ = modes.ModeLocal

func TestUnknownToolNameIsPassedOnAsAnUnknownCapability(t *testing.T) {
	st := &staged{tools: toolSet{capability: map[string]string{"appointments_book": "appointments.book"}}}
	got := string(st.proposalFromCall(providers.ToolCallProposal{ID: "c1", Name: "exports.dump", Arguments: json.RawMessage(`{"x":1}`)}))
	if !strings.Contains(got, `"capability_id":"exports.dump"`) || !strings.Contains(got, `"fields":{}`) {
		t.Fatalf("proposal = %s", got)
	}
	got = string(st.proposalFromCall(providers.ToolCallProposal{ID: "c1", Name: "Not An Id!", Arguments: json.RawMessage(`{}`)}))
	if !strings.Contains(got, `"out_of_scope"`) {
		t.Fatalf("proposal = %s", got)
	}
}
