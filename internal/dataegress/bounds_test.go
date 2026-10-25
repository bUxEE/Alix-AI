package dataegress

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestEnvelopeIsBoundedAndWellFormed(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	f.mustAllow(f.candidate())

	deny := func(name, code string, mut func(*Candidate)) {
		t.Helper()
		t.Logf("case %s", name)
		c := f.candidate()
		c.Profile = cloneProfile(t, f.prof)
		mut(&c)
		res, _ := f.mustDeny(c, providers.EgressDecisionReasonEnvelopeInvalid, providers.EgressDecisionCheckNameEnvelope)
		if got := string(res.Decision.Checks[len(res.Decision.Checks)-1].Reason); got != code {
			t.Errorf("%s: reason code %q, want %q", name, got, code)
		}
	}
	tool := func(n int) []providers.RequestEnvelopeToolSpec {
		out := make([]providers.RequestEnvelopeToolSpec, n)
		for i := range out {
			out[i] = providers.RequestEnvelopeToolSpec{Name: "tool_" + strconv.Itoa(i), Description: "does a thing", Parameters: map[string]any{}}
		}
		return out
	}
	deny("empty instructions", "bad_instructions", func(c *Candidate) { c.Instructions = " " })
	deny("long instructions", "bad_instructions", func(c *Candidate) { c.Instructions = strings.Repeat("i", maxInstructionChars+1) })
	deny("long part", "part_too_large", func(c *Candidate) { c.Messages[0].Parts[0].Text = strings.Repeat("x", maxTextChars+1) })
	deny("too many messages", "too_many_messages", func(c *Candidate) {
		for i := 0; i < maxMessages; i++ {
			c.Messages = append(c.Messages, Message{Role: providers.RequestEnvelopeMessageRoleUser, Parts: []Part{userPart("m")}})
		}
	})
	deny("too many parts", "too_many_parts", func(c *Candidate) {
		for i := 0; i < maxPartsPerMessage; i++ {
			c.Messages[0].Parts = append(c.Messages[0].Parts, userPart("p"))
		}
	})
	deny("bad role", "bad_message_role", func(c *Candidate) { c.Messages[0].Role = "system" })
	deny("no messages", "no_content", func(c *Candidate) { c.Messages = nil })
	deny("empty text only", "no_content", func(c *Candidate) { c.Messages[0].Parts = []Part{userPart("  ")} })
	deny("tool name", "bad_tool", func(c *Candidate) {
		c.Tools = tool(1)
		c.Tools[0].Name = "not a name"
	})
	deny("tool description", "bad_tool", func(c *Candidate) {
		c.Tools = tool(1)
		c.Tools[0].Description = ""
	})
	deny("too many tools", "too_many_tools", func(c *Candidate) { c.Tools = tool(maxTools + 1) })
	deny("profile tool limit", "too_many_tools", func(c *Candidate) {
		two := int64(2)
		c.Profile.ToolCalling.MaxTools = &two
		c.Tools = tool(3)
	})
	deny("tools without support", "tool_calling_unsupported", func(c *Candidate) {
		c.Profile.ToolCalling.Supported = false
		c.Tools = tool(1)
	})
	deny("schema format without schema", "bad_response_format", func(c *Candidate) {
		c.ResponseFormat = providers.RequestEnvelopeResponseFormat{Kind: providers.RequestEnvelopeResponseFormatKindJSONSchema}
	})
	deny("text format with schema", "bad_response_format", func(c *Candidate) { c.ResponseFormat.Schema = map[string]any{"type": "object"} })
	deny("unknown format", "bad_response_format", func(c *Candidate) { c.ResponseFormat.Kind = "xml" })
	deny("no output limit", "bad_limits", func(c *Candidate) { c.MaxOutputToks = 0 })
	deny("output above the profile", "bad_limits", func(c *Candidate) { c.MaxOutputToks = c.Profile.Limits.MaxOutputToks + 1 })
	deny("no timeout", "bad_limits", func(c *Candidate) { c.TimeoutMS = 0 })
	deny("timeout too long", "bad_limits", func(c *Candidate) { c.TimeoutMS = maxTimeoutMS + 1 })
	deny("request over the profile", "payload_too_large", func(c *Candidate) { c.Profile.Limits.MaxRequestBytes = 200 })
	deny("rag part without a source", "rag_part_without_source", func(c *Candidate) {
		c.Messages[0].Parts = append(c.Messages[0].Parts, Part{Kind: PartText, Text: "x", Category: string(providers.SendCategoryRAGExcerpt), Class: "internal"})
	})
	deny("text part in a media category", "category_kind_mismatch", func(c *Candidate) {
		c.Messages[0].Parts[0].Category = string(providers.SendCategoryImageContent)
	})
	deny("a rag category on a source of text without kind", "bad_part_kind", func(c *Candidate) { c.Messages[0].Parts[0].Kind = "" })

	// Media goes by reference, only to a profile that declares the modality, within its size.
	fileID := "fil_01J0000000000000000000000F"
	img := Part{Kind: PartImage, FileID: fileID, SizeBytes: 100, Category: string(providers.SendCategoryImageContent), Class: "internal"}
	deny("image to a text-only profile", "unsupported_modality", func(c *Candidate) { c.Messages[0].Parts = append(c.Messages[0].Parts, img) })
	vision := cloneProfile(t, f.prof)
	vision.Modalities.Input = []providers.Modality{providers.ModalityText, providers.ModalityImage}
	vision.Media = &providers.ProviderProfileMedia{Images: &providers.ProviderProfileMediaHandling{MaxBytes: 1000, MimeTypes: []string{"image/png"}, Retention: providers.RetentionModeZero}}
	mc := f.candidate()
	mc.Profile = vision
	mc.Messages[0].Parts = append(mc.Messages[0].Parts, img)
	res := f.mustAllow(mc)
	if !hasCategory(res.Decision.Categories.Sent, providers.SendCategoryImageContent) {
		t.Errorf("sent = %v", res.Decision.Categories.Sent)
	}
	assertAbsent(t, "wire", wire(t, res.Envelope), "/", ".png")
	for name, mut := range map[string]func(*Part){
		"too large":   func(p *Part) { p.SizeBytes = 1001 },
		"bad file id": func(p *Part) { p.FileID = "/var/data/a.png" },
		"wrong kind":  func(p *Part) { p.Category = string(providers.SendCategoryFileContent) },
		"unclassed":   func(p *Part) { p.Class = "" },
	} {
		bad := img
		mut(&bad)
		c := f.candidate()
		c.Profile = vision
		c.Messages[0].Parts = append(c.Messages[0].Parts, bad)
		if _, err := f.eval(c); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("media %s: %v", name, err)
		}
	}

	// A malformed candidate is not a decision: nothing is returned and nothing is recorded.
	before := len(f.decisionEvents(tApp, tTenantA))
	for name, mut := range map[string]func(*Candidate){
		"no scope":        func(c *Candidate) { c.Scope = contracts.Scope{} },
		"wildcard tenant": func(c *Candidate) { c.Scope.TenantID = "*" },
		"bad role":        func(c *Candidate) { c.Role = "router" },
		"bad channel":     func(c *Candidate) { c.Channel = "" },
		"bad trace":       func(c *Candidate) { c.TraceID = "a b" },
		"bad reservation": func(c *Candidate) { c.ReservationID = "rsv_x" },
		"empty profile":   func(c *Candidate) { c.Profile = providers.ProviderProfile{} },
	} {
		c := f.candidate()
		mut(&c)
		res, err := f.eval(c)
		if err == nil || res.Envelope != nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if after := len(f.decisionEvents(tApp, tTenantA)); after != before {
		t.Errorf("a malformed candidate was recorded as a decision")
	}

	// The envelope carries the reservation, the budget channel and the trace, and a tighter token estimate can be injected.
	g := newFixture(t, func(d *Deps) { d.EstimateToksIn = func(b int64) int64 { return b / 4 } })
	g.enableAll()
	c := g.candidate()
	c.ReservationID, c.Channel = "rsv_01J0000000000000000000000R", providers.ChannelBackground
	out := g.mustAllow(c)
	in := out.Envelope.Internal
	if in.ReservationID == nil || string(*in.ReservationID) != c.ReservationID || in.Channel != providers.ChannelBackground || string(in.TraceID) != "trc_test" ||
		int64(in.EstimatedToksIn) != in.PayloadBytes/4 || in.Subject.TenantID != identifier(tTenantA) {
		t.Errorf("envelope internals = %+v", in)
	}
	// Strict is only a request where the profile guarantees a strict schema.
	if f.prof.StructuredOutput == providers.ProviderProfileStructuredOutputStrictSchema {
		sc := f.candidate()
		sc.ResponseFormat = providers.RequestEnvelopeResponseFormat{Kind: providers.RequestEnvelopeResponseFormatKindJSONSchema, Strict: true, Schema: map[string]any{"type": "object"}}
		if !f.mustAllow(sc).Envelope.Payload.ResponseFormat.Strict {
			t.Error("strict is kept where the profile guarantees it")
		}
		sc.Profile = cloneProfile(t, f.prof)
		sc.Profile.StructuredOutput = providers.ProviderProfileStructuredOutputJSONMode
		if f.mustAllow(sc).Envelope.Payload.ResponseFormat.Strict {
			t.Error("strict is not asked for where the profile guarantees nothing")
		}
	}
}

func TestPartOutcomesMapTheEnvelopeBack(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	lo := statePart("home")
	lo.LocalOnly = true
	c := f.candidate()
	c.Messages = []Message{
		{Role: providers.RequestEnvelopeMessageRoleUser, Parts: []Part{lo, lo}},                                          // removed whole: the message goes
		{Role: providers.RequestEnvelopeMessageRoleUser, Parts: []Part{lo, userPart("kept one"), statePart("kept two")}}, // positions shift
	}
	res := f.mustAllow(c)
	if len(res.Envelope.Payload.Messages) != 1 || len(res.Envelope.Payload.Messages[0].Parts) != 2 {
		t.Fatalf("envelope shape: %+v", res.Envelope.Payload.Messages)
	}
	want := []PartOutcome{
		{Message: 0, Part: 0, EnvelopeMessage: -1, EnvelopePart: -1, Why: "local_only_field"},
		{Message: 0, Part: 1, EnvelopeMessage: -1, EnvelopePart: -1, Why: "local_only_field"},
		{Message: 1, Part: 0, EnvelopeMessage: -1, EnvelopePart: -1, Why: "local_only_field"},
		{Message: 1, Part: 1, Kept: true, EnvelopeMessage: 0, EnvelopePart: 0},
		{Message: 1, Part: 2, Kept: true, EnvelopeMessage: 0, EnvelopePart: 1},
	}
	if len(res.Parts) != len(want) {
		t.Fatalf("outcomes = %+v", res.Parts)
	}
	for i := range want {
		if res.Parts[i] != want[i] {
			t.Errorf("outcome %d = %+v, want %+v", i, res.Parts[i], want[i])
		}
	}
	if got := onlyText(t, res.Envelope, 0, 0); got != "kept one" {
		t.Errorf("the outcome points at the part: %q", got)
	}
}

func TestStoreRowsAreScopedAndTamperingFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	f.mustAllow(f.candidate())
	// Every table of this module carries the scope columns (other modules' tables are theirs to check).
	violations, err := store.CheckScopeColumns(testCtx, f.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		if strings.HasPrefix(v.Table, "dataegress_") {
			t.Errorf("scope rule: %s", v)
		}
	}
	var tables int
	if err := f.db.QueryRowContext(testCtx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name LIKE 'dataegress\_%' ESCAPE '\'`).Scan(&tables); err != nil || tables != 2 {
		t.Errorf("tables = %d, %v", tables, err)
	}

	// A stored policy that disagrees with its key (moved to another tenant, or changed behind the engine's back) is
	// never trusted: the guard denies as a guard failure instead of using it.
	for name, stmt := range map[string]string{
		"revision":  `UPDATE dataegress_policies SET revision = 9 WHERE level = 'workspace'`,
		"target":    `UPDATE dataegress_policies SET document = replace(document, '"workspace_id":"` + tWsp1 + `"', '"workspace_id":"wsp_x"') WHERE level = 'workspace'`,
		"not json":  `UPDATE dataegress_policies SET document = '{"level":"workspace"}' WHERE level = 'workspace'`,
		"bad enum":  `UPDATE dataegress_policies SET document = replace(document, '"allow_excerpt"', '"allow_all"') WHERE level = 'workspace'`,
		"restored?": `SELECT 1`,
	} {
		g := newFixture(t)
		g.enableAll()
		if _, err := g.db.ExecContext(testCtx, stmt); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "restored?" {
			g.mustAllow(g.candidate())
			continue
		}
		res, err := g.eval(g.candidate())
		var ce *contracts.Error
		if err == nil || res.Envelope != nil || !asContractsError(err, &ce) || ce.Code != contracts.ErrPermissionDenied || ce.Details["rule"] != "guard_failure" {
			t.Errorf("%s: a tampered policy was used: %v", name, err)
		}
	}
}

// The package decides what DATA may leave. It must stay clear of the network, the shell, the
// SQLite driver, the adapters, the dialogue pipeline above it and the destination allowlist
// (internal/egress), which is a different gate: the two are never merged.
func TestImportsStayWithinTheEgressPolicyTier(t *testing.T) {
	const module = "github.com/buxee/alix-ai/"
	allowed := map[string]bool{
		"pkg/contracts":               true,
		"pkg/contracts/gen/common":    true,
		"pkg/contracts/gen/providers": true,
		"internal/clock":              true,
		"internal/ids":                true,
		"internal/store":              true,
		"internal/audit":              true,
		"internal/log":                true,
		"internal/store/storetest":    true, // tests only
	}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch {
			case p == "net" || (strings.HasPrefix(p, "net/") && p != "net/url") || p == "os/exec" || strings.Contains(p, "modernc.org/sqlite"):
				t.Errorf("%s imports %s: no network, shell-out or SQLite driver here", path, p)
			case strings.HasPrefix(p, module) && !allowed[strings.TrimPrefix(p, module)]:
				t.Errorf("%s imports %s, which the egress policy may not depend on", path, p)
			}
			if !strings.HasSuffix(path, "_test.go") && strings.Contains(p, "storetest") {
				t.Errorf("%s: a test helper in production code", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
