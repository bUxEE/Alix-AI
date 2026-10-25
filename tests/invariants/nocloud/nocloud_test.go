package nocloud

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

const (
	ncBookSay      = "Book Luca Ferri for 2026-10-14 10:30"
	ncBookPatient  = "Luca Ferri"
	ncBookSlot     = "2026-10-14 10:30"
	ncPolicyAsk    = "Which records does a doctor see in this clinic group?"
	ncParserEnvVar = "ALIXAI_M5_PARSER"
)

// ncParserWorker is the parser worker the flow ingests documents with: the one named by ALIXAI_M5_PARSER (a
// prebuilt one, for the container run of the live test), else the one the gate builds.
func ncParserWorker(t *testing.T) string {
	t.Helper()
	if p := os.Getenv(ncParserEnvVar); p != "" {
		return p
	}
	return programs(t).parser
}

// TestNoCloudDependency ("no cloud service is required"; SEC-071): the stack completes a full text + RAG + action conversation with every outbound
// connection and every name lookup refused and nothing attempted.
//
// What runs: the real dialogue engine, policy gateway, action coordinator and session store over the
// clinic-booking package and its stand-in host, and the real RAG pipeline (the sandboxed parser worker,
// chunker, FTS5 and vector indexes, durable job queue, ingestion, query with RRF, grounded answerer with its
// claim verifier and citation resolver) over the package's own policies folder. Only the models are
// scripted. The conversation is one session: a typed booking request (text, prepared and
// waiting for its confirmation), a policy question answered from the documents with citations (RAG), and the
// confirmation, which commits one receipt (action). Every connection the process makes through the default
// HTTP transport and every lookup through the default resolver goes through a guard that allows the loopback
// and records anything else; the guard has controls, so a clean record means nothing was tried and not that it
// could not see.
//
// The static half runs as well: every network the services of the container stack share is `internal: true`,
// only the relay joins the one network that is not, and no profile override adds a route out.
//
// SEC-071
func TestNoCloudDependency(t *testing.T) {
	t.Run("the guard sees what it looks for", func(t *testing.T) {
		g := &netGuard{}
		g.install(t)
		// a loopback destination is the host's own and goes through
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
		if conn, derr := g.dial(context.Background(), "tcp", ln.Addr().String()); derr != nil {
			t.Errorf("a loopback connection was refused: %v", derr)
		} else {
			_ = conn.Close()
		}
		if len(g.seen()) != 0 {
			t.Fatalf("a loopback connection was recorded as an attempt: %v", g.seen())
		}
		// a public address, a name and a request through the default client are each recorded and fail
		if _, err := g.dial(context.Background(), "tcp", "192.0.2.1:443"); err == nil {
			t.Error("a dial to a public address was allowed")
		}
		if _, err := net.LookupHost("example.invalid"); err == nil {
			t.Error("a name lookup was allowed")
		}
		client := &http.Client{Timeout: 5 * time.Second}
		if resp, err := client.Get("http://192.0.2.1/telemetry"); err == nil {
			_ = resp.Body.Close()
			t.Error("a request through the default client left the machine")
		}
		seen := g.seen()
		for _, want := range []string{"dial tcp 192.0.2.1:443", "resolve udp", "dial tcp 192.0.2.1:80"} {
			if !slices.ContainsFunc(seen, func(s string) bool { return strings.Contains(s, want) }) {
				t.Errorf("the guard did not record %q; it recorded %v", want, seen)
			}
		}
	})

	t.Run("no network of the stack has a route out except the relay's", func(t *testing.T) {
		assertStack(t, readFile(t, "deploy/container/stack/compose.yaml"))
		// the profile overrides add resources, never a route out; the external override is the opt-in
		for _, name := range []string{"compose.lite.yaml", "compose.balanced.yaml", "compose.4g.yaml", "compose.noexternal.yaml"} {
			var doc struct {
				Services map[string]map[string]any `yaml:"services"`
				Networks map[string]any            `yaml:"networks"`
			}
			if err := yaml.Unmarshal(readFile(t, "deploy/container/stack/"+name), &doc); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if len(doc.Networks) != 0 {
				t.Errorf("%s declares networks %v: a profile must not change the stack's topology", name, doc.Networks)
			}
			for svc, def := range doc.Services {
				if def["networks"] != nil || def["network_mode"] != nil || def["ports"] != nil {
					t.Errorf("%s changes the networking of %s", name, svc)
				}
			}
		}
		if ext := string(readFile(t, "deploy/container/stack/compose.external.yaml")); !strings.Contains(ext, "External") && !strings.Contains(ext, "external") {
			t.Error("compose.external.yaml no longer says it is the External-mode opt-in")
		}
		// the guards that are tested where they live
		requireTests(t, "tests/deploy", "TestExternalOverrideIsOptIn", "TestPortsLoopbackOnly", "TestComposeHardening")
		requireTests(t, "internal/metrics", "TestMetricsLoopbackOnly", "TestNoOutboundTelemetry")
		requireTests(t, "internal/egress", "TestEgressDenyByDefault")
	})

	t.Run("a full text + RAG + action conversation completes with outbound blocked and nothing attempted", func(t *testing.T) {
		guard := &netGuard{}
		guard.install(t)
		s := newNCStack(t, ncParserWorker(t))
		ctx := context.Background()

		// ---- the documents: the package's own policies folder is ingested ----
		st := s.syncPolicies()
		if !st.Available || st.Counts.Indexed != len(s.files) || st.Counts.Failed != 0 {
			t.Fatalf("after the sync: available %v, %+v, want all %d files indexed", st.Available, st.Counts, len(s.files))
		}

		// ---- text: a typed booking request is prepared and waits for its confirmation ----
		s.propose("appointments.book", map[string]any{"patient": ncBookPatient, "slot": ncBookSlot})
		booked := s.typed(dialogue.TurnInput{Text: ncBookSay, Language: "en"})
		if booked.Directive.Act != conversation.DialogueDirectiveActRequestConfirmation || booked.Prepared == nil || booked.Confirmation.IsZero() {
			t.Fatalf("the typed booking did not reach a confirmation request: act %s code %q", booked.Directive.Act, booked.Code)
		}
		if n := s.count("action_receipts"); n != 0 {
			t.Fatalf("%d receipts before the confirmation: nothing may be committed before it", n)
		}

		// ---- RAG: a policy question on the same session is answered from the documents, with citations ----
		s.proposeOutOfScope()
		asked := s.typed(dialogue.TurnInput{Text: ncPolicyAsk, Language: "en"})
		if !dialogue.KnowledgeTurn(asked) || asked.Directive.Act != conversation.DialogueDirectiveActPresentResult || asked.Code != "" {
			t.Fatalf("the policy question was not answered from the documents: act %s code %q stages %v", asked.Directive.Act, asked.Code, asked.Trace.Stages)
		}
		if len(asked.Directive.Sources) == 0 {
			t.Fatal("the grounded answer has no source")
		}
		for i, c := range asked.Directive.Sources {
			if string(c.SourceID) != s.srcID || c.Locator == nil || len(c.Claims) == 0 || c.Availability != conversation.CitationAvailabilityAvailable {
				t.Errorf("citation %d is not a valid reference to the registered source: %+v", i, c)
			}
			for _, claim := range c.Claims {
				found := false
				for _, data := range s.files {
					if strings.Contains(strings.Join(strings.Fields(string(data)), " "), strings.Join(strings.Fields(claim), " ")) {
						found = true
					}
				}
				if !found {
					t.Errorf("citation %d supports %q, which no original file states", i, claim)
				}
			}
		}

		// ---- action: the confirmation commits exactly one receipt ----
		done := s.typed(dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: string(booked.Prepared.PreparationID), Token: booked.Confirmation}})
		if done.Receipt == nil || done.Receipt.Status != actions.ActionReceiptStatusCommitted || done.Code != "" {
			t.Fatalf("the confirmation did not commit: act %s code %q receipt %+v", done.Directive.Act, done.Code, done.Receipt)
		}
		if s.count("sessions") != 1 || s.count("action_preparations") != 1 || s.count("action_receipts") != 1 {
			t.Errorf("sessions %d, preparations %d, receipts %d, want 1, 1 and 1", s.count("sessions"), s.count("action_preparations"), s.count("action_receipts"))
		}
		if _, err := s.eng.Turn(ctx, s.scope, dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: string(booked.Prepared.PreparationID), Token: booked.Confirmation}}); err == nil && s.count("action_receipts") != 1 {
			t.Errorf("a replayed confirmation committed again: %d receipts", s.count("action_receipts"))
		}

		// ---- the models were used (the run was not vacuous) and the network was never tried ----
		if len(s.ctl.Prompts()) == 0 || len(s.fp.Prompts()) == 0 {
			t.Errorf("the scripted models were not consulted: dialogue %d calls, retrieval and answering %d calls", len(s.ctl.Prompts()), len(s.fp.Prompts()))
		}
		if seen := guard.seen(); len(seen) != 0 {
			t.Errorf("the stack tried the network with outbound blocked: %v", seen)
		}
		t.Logf("text + RAG (%d citation(s) from %d files) + action (1 receipt) completed; outbound attempts: 0", len(asked.Directive.Sources), len(s.files))
		fmt.Fprintln(os.Stderr, "m5 gate: no-cloud flow completed, outbound attempts: 0, parser:", filepath.Base(ncParserWorker(t)))
	})
}
