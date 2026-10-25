package sdkgo_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
	"github.com/buxee/alix-ai/pkg/sdk-go/sdktest"
)

// Files in a turn (M3-20 gateway, M3-29 SDK): TurnInput.Attachments names up to four uploaded file ids. The SDK checks
// the list before a context token is spent, the fake resolves the ids under the scope before any handler runs, and every
// reference that does not resolve for the caller is the same scope_violation.

const unknownFile = "fil_0123456789ABCDEFGHJKMNPQRS"

// recordTurns makes the fake answer with a reply and remember what the turns it ran were given.
func recordTurns(h *harness) func() []sdktest.Turn {
	var mu sync.Mutex
	var turns []sdktest.Turn
	h.fake.OnTurn(func(turn sdktest.Turn) sdktest.TurnPlan {
		mu.Lock()
		defer mu.Unlock()
		turns = append(turns, turn)
		return sdktest.Reply("ok")
	})
	return func() []sdktest.Turn {
		mu.Lock()
		defer mu.Unlock()
		return append([]sdktest.Turn(nil), turns...)
	}
}

func TestSendTurnWithAttachments(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ran := recordTurns(h)
	first, second := h.fake.AddFile(h.claims), h.fake.AddFile(h.claims)

	// The ids travel with the text, in the order given, and the handler sees them in that order.
	stream := must[*sdkgo.TurnStream](t, "send")(h.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("read these").WithLanguage("en").WithAttachments(second, first)))
	result := must[*sdkgo.TurnResult](t, "result")(stream.Result())
	if done, _ := sdkgo.DoneOf(result.Events); done.FinishReason != "completed" {
		t.Fatalf("the turn completes, got %s", done.FinishReason)
	}
	turns := ran()
	if len(turns) != 1 || !reflect.DeepEqual(turns[0].Input.Attachments, []string{second, first}) || turns[0].Input.Text != "read these" {
		t.Fatalf("the handler is told the text and the ids in order: %+v", turns)
	}
	_ = must[*sdkgo.TurnResult](t, "json")(h.client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("again").WithAttachments(first)))
	if got := ran(); len(got) != 2 || !reflect.DeepEqual(got[1].Input.Attachments, []string{first}) {
		t.Fatalf("the JSON form carries them too: %+v", got)
	}

	// The wire body has the member, in the document's shape; an input without files has none.
	raw, _ := json.Marshal(sdkgo.TextTurn("hi").WithAttachments(first))
	if string(raw) != `{"text":"hi","attachments":["`+first+`"]}` {
		t.Fatalf("the request body: %s", raw)
	}
	if raw, _ = json.Marshal(sdkgo.TextTurn("hi").WithAttachments()); strings.Contains(string(raw), "attachments") {
		t.Fatalf("an empty list is left out: %s", raw)
	}

	// Four files, and a file named twice, are accepted.
	ids := []string{first, second, h.fake.AddFile(h.claims), h.fake.AddFile(h.claims)}
	must[*sdkgo.TurnResult](t, "four")(h.client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("all").WithAttachments(ids...)))
	must[*sdkgo.TurnResult](t, "twice")(h.client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("twice").WithAttachments(first, first)))
}

func TestSendTurnAttachmentsAreCheckedBeforeATokenIsSpent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.fake.AddFile(h.claims)
	confirm := sdkgo.ConfirmTurn("prep_1", "cfm_"+strings.Repeat("A", 43))
	confirm.Attachments = []string{id}
	choice := sdkgo.ChoiceTurn("c1")
	choice.Attachments = []string{id}
	reject := sdkgo.RejectTurn("prep_1")
	reject.Attachments = []string{id}
	for name, input := range map[string]sdkgo.TurnInput{
		"more than four":       sdkgo.TextTurn("x").WithAttachments(id, id, id, id, id),
		"not a file id":        sdkgo.TextTurn("x").WithAttachments("../../etc/passwd"),
		"a lower-case id":      sdkgo.TextTurn("x").WithAttachments(strings.ToLower(id)),
		"an empty id":          sdkgo.TextTurn("x").WithAttachments(""),
		"without a text":       {Attachments: []string{id}},
		"with a choice":        choice,
		"with a confirmation":  confirm,
		"with a rejection":     reject,
		"another kind of id":   sdkgo.TextTurn("x").WithAttachments("src_0123456789ABCDEFGHJKMNPQRS"),
		"an id with a suffix":  sdkgo.TextTurn("x").WithAttachments(id + "0"),
		"a path in the middle": sdkgo.TextTurn("x").WithAttachments(id, "fil_/../x"),
	} {
		if _, err := h.client.SendTurn(ctx, "ses-1", input); !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Errorf("%s: SendTurn want validation_failed, got %v", name, err)
		}
		if _, err := h.client.SendTurnJSON(ctx, "ses-1", input); !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Errorf("%s: SendTurnJSON want validation_failed, got %v", name, err)
		}
	}
	if n := len(h.tokenRequests()); n != 0 {
		t.Fatalf("a malformed turn costs no context token, %d were requested", n)
	}
	if n := len(h.fake.RequestsOf("sendTurn")); n != 0 {
		t.Fatalf("nothing reached the wire, %d requests did", n)
	}
}

func TestSendTurnRefusesAFileThatDoesNotResolveWithOneScopeViolation(t *testing.T) {
	ctx := context.Background()
	other := func(mutate func(*sdktest.Claims)) func(h *harness) string {
		return func(h *harness) string {
			c := h.claims
			mutate(&c)
			return h.fake.AddFile(c)
		}
	}
	cases := map[string]func(h *harness) string{
		"another principal's":    other(func(c *sdktest.Claims) { c.PrincipalID = "principal-2" }),
		"another tenant's":       other(func(c *sdktest.Claims) { c.TenantID = "tenant-2" }),
		"another workspace's":    other(func(c *sdktest.Claims) { c.WorkspaceID = "workspace-2" }),
		"another session's":      other(func(c *sdktest.Claims) { c.SessionID = "ses-9" }),
		"another application's":  other(func(c *sdktest.Claims) { c.ApplicationID = "other-app" }),
		"a deleted one":          func(h *harness) string { id := h.fake.AddFile(h.claims); h.fake.DeleteFile(id); return id },
		"an expired one":         func(h *harness) string { id := h.fake.AddFile(h.claims); h.fake.ExpireFile(id); return id },
		"one that never existed": func(*harness) string { return unknownFile },
	}
	var shapes []string
	for name, setup := range cases {
		h := newHarness(t)
		ran := recordTurns(h)
		id := setup(h)
		good := h.fake.AddFile(h.claims)
		before := must[*sdkgo.Session](t, "session")(h.client.GetSession(ctx, "ses-1")).TurnCount

		// A bad reference among good ones refuses the whole turn.
		_, err := h.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("x").WithAttachments(good, id))
		api := hasCode(t, name, err, contracts.ErrScopeViolation)
		if api.Status != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d", name, api.Status)
		}
		if len(ran()) != 0 {
			t.Errorf("%s: no model is called for a refused turn", name)
		}
		after := must[*sdkgo.Session](t, "session")(h.client.GetSession(ctx, "ses-1")).TurnCount
		if after != before {
			t.Errorf("%s: no turn is recorded (%d before, %d after)", name, before, after)
		}
		raw, _ := json.Marshal(api.Err.Details)
		shapes = append(shapes, api.Err.Message+"|"+string(raw))
		if _, err := h.client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("x").WithAttachments(id)); !contracts.HasCode(err, contracts.ErrScopeViolation) {
			t.Errorf("%s: SendTurnJSON want scope_violation, got %v", name, err)
		}
	}
	// Nothing tells the cases apart: the message and the details are the same for all of them (the request id aside).
	for _, s := range shapes {
		if strings.Contains(s, "reason") || strings.Contains(s, "levels") {
			t.Errorf("a refusal says why: %s", s)
		}
	}
	for _, s := range shapes[1:] {
		a, b := stripRequestID(s), stripRequestID(shapes[0])
		if a != b {
			t.Errorf("two refusals differ: %q and %q", a, b)
		}
	}
}

func stripRequestID(s string) string {
	var out map[string]any
	msg, details, _ := strings.Cut(s, "|")
	_ = json.Unmarshal([]byte(details), &out)
	delete(out, "request_id")
	raw, _ := json.Marshal(out)
	return msg + "|" + string(raw)
}
