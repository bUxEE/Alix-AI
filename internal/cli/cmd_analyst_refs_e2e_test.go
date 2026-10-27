//go:build !noexternal

package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

func analystRefsCaps() map[string]any {
	return map[string]any{
		"contract": "1.5",
		"blocks": []any{"markdown", "table", "record_card", "chart", "provenance", "kpi", "timeline", "draft",
			"code", "diagram", "citation_list", "action_card", "clarification_choices", "confirmation_panel"},
	}
}

func analystRefsAnswerStep() string {
	return analystE2EStep("answer", map[string]any{"answer": map[string]any{
		"text_plan": "The assembly was attended by {{ref:{{dataset_id}}:0:attendee_name}}, " +
			"chaired by {{ref:{{dataset_id}}:0:chair_name}}, with secretary {{ref:{{dataset_id}}:0:secretary_name}}.",
		"blocks": []any{
			map[string]any{"type": "table", "dataset_id": "{{dataset_id}}"},
		},
	}})
}

func analystRefsReadStep() string {
	return analystE2EStep("read", map[string]any{"read": map[string]any{
		"capability_id": "catalog.find_title", "arguments": map[string]any{"query": "assembly"},
	}})
}

func assertNoAttendeeNames(t *testing.T, where, body string) {
	t.Helper()
	for _, name := range []string{analystE2EAttendeeA, analystE2EAttendeeB, analystE2EAttendeeSens} {
		if strings.Contains(body, name) {
			t.Fatalf("%s: confidential attendee %q must not appear", where, name)
		}
	}
}

func assertHasAttendeeNames(t *testing.T, where, body string) {
	t.Helper()
	for _, name := range []string{analystE2EAttendeeA, analystE2EAttendeeB, analystE2EAttendeeSens} {
		if !strings.Contains(body, name) {
			t.Fatalf("%s: expected attendee %q; body=%s", where, name, body)
		}
	}
}

// TestAnalystRefsE2E_ConfidentialNamesResolvedNotEgresed proves ADR 0049:
// turn 1 text_plan uses {{ref:…}} → client + history show names; provider never
// sees them; turn 2 session history still clean.
func TestAnalystRefsE2E_ConfidentialNamesResolvedNotEgresed(t *testing.T) {
	r := startAnalystTurnRig(t, 32000)
	r.oa.enqueue(analystRefsReadStep(), analystRefsAnswerStep())
	r.openSession("ten_a", "ses_refs")
	caps := analystRefsCaps()
	code, body := r.turn("ten_a", "ses_refs", "Who attended the last assembly?", caps)
	if code != http.StatusOK {
		t.Fatalf("turn1=%d body=%s", code, mustJSON(body))
	}
	if r.host.readCount() == 0 {
		t.Fatalf("expected host read; body=%s", mustJSON(body))
	}
	assertNoAttendeeNames(t, "turn1 provider", r.oa.allBodies())
	raw := mustJSON(body)
	assertHasAttendeeNames(t, "turn1 client", raw)
	if strings.Contains(raw, "{{ref:") {
		t.Fatalf("client must not see raw ref tokens; body=%s", raw)
	}

	// History API must show resolved names (transcript display form).
	histTok := r.token("ten_a", "ses_refs", "catalog.find_title", "history.read")
	hCode, hBody := r.srv.callToken(t, histTok, http.MethodGet,
		"/v1/history/sessions/"+url.PathEscape("ses_refs")+"/segments", "")
	if hCode != http.StatusOK {
		t.Fatalf("history segments=%d body=%s", hCode, hBody)
	}
	assertHasAttendeeNames(t, "history API", string(hBody))
	if strings.Contains(string(hBody), "{{ref:") {
		t.Fatalf("history must not show raw tokens: %s", hBody)
	}

	callsAfter1 := r.oa.callCount()
	r.oa.enqueue(
		analystE2EStep("answer", map[string]any{"answer": map[string]any{
			"text_plan": "Noted.",
			"blocks":    []any{},
		}}),
	)
	code2, body2 := r.turn("ten_a", "ses_refs", "Thanks — please remember that for next time.", caps)
	if code2 != http.StatusOK {
		t.Fatalf("turn2=%d body=%s", code2, mustJSON(body2))
	}
	if turnCode(body2) == string(contracts.ErrOutOfDomain) {
		t.Fatalf("turn2 unexpectedly out_of_domain: %s", mustJSON(body2))
	}
	assertNoAttendeeNames(t, "turn2 provider (incl. session history)", r.oa.allBodies())
	if r.oa.callCount() <= callsAfter1 {
		t.Fatal("turn2 expected additional provider calls")
	}
}

// TestAnalystRefsE2E_AGUIClientHistoryDoesNotReegressResolvedNames: AG-UI run 1
// resolves names for the client; run 2 resends that assistant message in the
// thread → provider bodies still contain no attendee name (assistant hints dropped).
func TestAnalystRefsE2E_AGUIClientHistoryDoesNotReegressResolvedNames(t *testing.T) {
	r := startAnalystTurnRig(t, 32000)
	r.oa.enqueue(analystRefsReadStep(), analystRefsAnswerStep())
	const session = "ses_refs_agui"
	r.openSession("ten_a", session)

	fp := map[string]any{
		"alixai": map[string]any{
			"language":           "en",
			"clientCapabilities": analystRefsCaps(),
		},
	}
	run1 := map[string]any{
		"threadId": session, "runId": "run_refs_1",
		"messages":       []any{map[string]any{"id": "m1", "role": "user", "content": "Who attended the last assembly?"}},
		"tools":          []any{},
		"context":        []any{},
		"state":          map[string]any{},
		"forwardedProps": fp,
	}
	code, sse := r.aguiRun("ten_a", session, run1)
	if code != http.StatusOK {
		t.Fatalf("agui run1=%d body=%s", code, truncateOA(sse, 800))
	}
	assistantText := aguiAssistantText(t, sse)
	assertHasAttendeeNames(t, "agui run1 client text", assistantText)
	assertNoAttendeeNames(t, "agui run1 provider", r.oa.allBodies())

	callsAfter1 := r.oa.callCount()
	r.oa.enqueue(
		analystE2EStep("answer", map[string]any{"answer": map[string]any{
			"text_plan": "Noted from the prior turn.",
			"blocks":    []any{},
		}}),
	)
	// Client resends the whole thread, including the resolved assistant reply.
	run2 := map[string]any{
		"threadId": session, "runId": "run_refs_2",
		"messages": []any{
			map[string]any{"id": "m1", "role": "user", "content": "Who attended the last assembly?"},
			map[string]any{"id": "a1", "role": "assistant", "content": assistantText},
			map[string]any{"id": "m2", "role": "user", "content": "Thanks — note that for next time."},
		},
		"tools":          []any{},
		"context":        []any{},
		"state":          map[string]any{},
		"forwardedProps": fp,
	}
	code2, sse2 := r.aguiRun("ten_a", session, run2)
	if code2 != http.StatusOK {
		t.Fatalf("agui run2=%d body=%s", code2, truncateOA(sse2, 800))
	}
	if r.oa.callCount() <= callsAfter1 {
		t.Fatal("agui run2 expected additional provider calls")
	}
	// Bodies after run1: must still lack names even though the client resent them.
	oaAll := r.oa.allBodies()
	assertNoAttendeeNames(t, "agui run2 provider (client resent resolved assistant)", oaAll)
	_ = sse2
}

func (r *analystTurnRig) aguiRun(tenant, session string, body map[string]any) (int, []byte) {
	r.t.Helper()
	raw, _ := json.Marshal(body)
	tok := r.token(tenant, session)
	req, err := http.NewRequest(http.MethodPost, r.srv.base+"/v1/agui", bytes.NewReader(raw))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		r.t.Fatalf("POST /v1/agui: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func aguiAssistantText(t *testing.T, sse []byte) string {
	t.Helper()
	var text strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(sse))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		if ev["type"] == "TEXT_MESSAGE_CONTENT" {
			if d, ok := ev["delta"].(string); ok {
				text.WriteString(d)
			}
		}
	}
	got := text.String()
	if strings.TrimSpace(got) == "" {
		t.Fatalf("no TEXT_MESSAGE_CONTENT in AG-UI stream: %s", truncateOA(sse, 600))
	}
	return got
}
