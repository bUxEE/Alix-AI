package dataegress

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestCredentialsAndSecretsNeverEgress(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	secret := tSecret()

	// A part the caller declares a credential is removed whole; the rest of the request goes.
	cred := Part{Kind: PartText, Text: "api credential " + secret, Category: string(providers.BlockedCategoryCredential), Class: string(providers.DataClassificationInternal)}
	res := f.mustAllow(f.candidate(cred))
	assertAbsent(t, "envelope", whole(t, res.Envelope), secret, "api credential")
	if !hasCategory(res.Decision.Categories.Stripped, providers.BlockedCategoryCredential) {
		t.Errorf("stripped = %v, want credential", res.Decision.Categories.Stripped)
	}
	last := res.Parts[len(res.Parts)-1]
	if last.Kept || last.Why != "blocked_category" || last.EnvelopeMessage != -1 {
		t.Errorf("the credential part is reported as removed: %+v", last)
	}

	// A secret inside ordinary text is scrubbed wherever text can reach the provider:
	// a user message, the instructions, a tool description, a tool schema and a response schema.
	c := f.candidate()
	c.Messages[0].Parts[0].Text = "my key is " + secret + " please use it"
	c.Instructions = "Use header Authorization: Bearer " + secret + " when asked."
	c.Tools = []providers.RequestEnvelopeToolSpec{{
		Name: "orders_list", Description: "Lists orders; token=" + secret,
		Parameters: map[string]any{"type": "object", "default": "password=" + secret},
	}}
	c.ResponseFormat = providers.RequestEnvelopeResponseFormat{
		Kind: providers.RequestEnvelopeResponseFormatKindJSONSchema, Strict: true,
		Schema: map[string]any{"type": "object", "description": "secret=" + secret},
	}
	got := f.mustAllow(c)
	assertAbsent(t, "envelope", whole(t, got.Envelope), secret)
	assertPresent(t, "wire payload", wire(t, got.Envelope), "please use it", "Lists orders", "[REDACTED]")
	if !hasCategory(got.Decision.Categories.Stripped, providers.BlockedCategoryCredential) {
		t.Errorf("stripped = %v, want credential", got.Decision.Categories.Stripped)
	}

	// A credential reference handle is a reusable application credential: it does not leave either.
	ref := "cred_01J0000000000000000000000C"
	c = f.candidate()
	c.Messages[0].Parts[0].Text = "connect with " + ref
	got = f.mustAllow(c)
	assertAbsent(t, "envelope", whole(t, got.Envelope), ref)

	// A request that is nothing but credentials has nothing left to send: it is not sent.
	only := f.candidate()
	only.Messages = []Message{{Role: providers.RequestEnvelopeMessageRoleUser, Parts: []Part{cred}}}
	f.mustDeny(only, providers.EgressDecisionReasonEnvelopeInvalid, providers.EgressDecisionCheckNameEnvelope)
}

func TestRawDatabaseContentNeverEgresses(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	rows := `{"id":7,"email":"` + tUserMarker + `@example.com","iban":"XX00 0000"}`
	raw := Part{Kind: PartText, Text: rows, Category: string(providers.BlockedCategoryRawDatabaseContent), Class: string(providers.DataClassificationPublic)}

	// Declared raw database content is removed even when it claims to be public, and recorded as stripped.
	res := f.mustAllow(f.candidate(raw))
	assertAbsent(t, "envelope", whole(t, res.Envelope), tUserMarker+"@example.com", "XX00")
	if !hasCategory(res.Decision.Categories.Stripped, providers.BlockedCategoryRawDatabaseContent) {
		t.Errorf("stripped = %v, want raw_database_content", res.Decision.Categories.Stripped)
	}
	if res.Parts[len(res.Parts)-1].Kept {
		t.Error("the raw database part is reported as kept")
	}
	// Not even as a Required part, and not under an allow_excerpt source: it has no sendable category.
	raw.Required = true
	f.mustAllow(f.candidate(raw))

	// Content whose category the caller cannot state is unclassified: it denies instead of leaking.
	unk := raw
	unk.Category, unk.Required = "db_rows", false
	f.mustDeny(f.candidate(unk), providers.EgressDecisionReasonUnclassifiedData, providers.EgressDecisionCheckNameClassification)

	// A database handle inside text is removed as a handle.
	c := f.candidate()
	c.Messages[0].Parts[0].Text = "connect to postgres://svc:" + "pw123" + "@db.internal:5432/crm and run it"
	got := f.mustAllow(c)
	assertAbsent(t, "envelope", whole(t, got.Envelope), "db.internal", "pw123", "postgres://")
	if !hasCategory(got.Decision.Categories.Stripped, providers.BlockedCategoryDatabaseHandle) {
		t.Errorf("stripped = %v, want database_handle", got.Decision.Categories.Stripped)
	}
}

func TestLocalOnlySourceBlocked(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	body := "confidential-ish body " + tExcerptMarker

	// A source with no policy is local_only: it is the default of every source.
	unknown := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationInternal, nil)
	if _, err := f.db.ExecContext(testCtx, `DELETE FROM dataegress_policies WHERE source_id = ?`, string(unknown)); err != nil {
		t.Fatal(err)
	}
	// A source that says local_only, and a source under a workspace ceiling of local_only.
	localOnly := f.newSource(providers.EgressModeLocalOnly, providers.DataClassificationInternal, nil)
	good := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationInternal, nil)

	baseline := f.mustAllow(f.candidate(excerptPart(good, "ok body", "Good")))
	if baseline.Decision.Sources[0].Outcome != providers.EgressDecisionSourceDecisionOutcomeExcerptIncluded {
		t.Fatalf("control: the allow_excerpt source goes in: %+v", baseline.Decision.Sources)
	}
	for name, src := range map[string]providers.SourceID{"no policy": unknown, "local_only policy": localOnly} {
		res := f.mustAllow(f.candidate(excerptPart(src, body, "Title"), excerptPart(good, "ok body", "Good")))
		assertAbsent(t, name+": envelope", whole(t, res.Envelope), tExcerptMarker, "Title")
		assertPresent(t, name+": envelope", wire(t, res.Envelope), "ok body")
		if !hasCategory(res.Decision.Categories.Withheld, providers.SendCategoryRAGExcerpt) {
			t.Errorf("%s: withheld = %v, want rag_excerpt", name, res.Decision.Categories.Withheld)
		}
		if res.Decision.Sources[0].SourceID != src || res.Decision.Sources[0].EffectiveMode != providers.EgressModeLocalOnly ||
			res.Decision.Sources[0].Outcome != providers.EgressDecisionSourceDecisionOutcomeExcluded {
			t.Errorf("%s: source decision = %+v", name, res.Decision.Sources[0])
		}
		if res.Parts[len(res.Parts)-2].Kept || res.Parts[len(res.Parts)-2].Why != "source_local_only" {
			t.Errorf("%s: part outcome = %+v", name, res.Parts[len(res.Parts)-2])
		}
		// When the answer requires the source, the request is denied, never sent without it and never with it.
		req := excerptPart(src, body, "Title")
		req.Required = true
		den, ce := f.mustDeny(f.candidate(req), providers.EgressDecisionReasonSourceLocalOnly, providers.EgressDecisionCheckNameSourceMode)
		if den.Decision.Checks[len(den.Decision.Checks)-1].Reason != "local_only_source_required" {
			t.Errorf("%s: reason code %q", name, den.Decision.Checks[len(den.Decision.Checks)-1].Reason)
		}
		assertAbsent(t, name+": error", ce.Error(), tExcerptMarker, "Title")
		if !hasCategory(den.Decision.Categories.Withheld, providers.SendCategoryRAGExcerpt) {
			t.Errorf("%s: a denial still records what was withheld: %v", name, den.Decision.Categories.Withheld)
		}
	}

	// A ceiling above the source wins over an allow_excerpt source.
	f.put(providers.EgressLevelTenant, func(in *PolicyInput) { in.SourceMode = providers.EgressModeLocalOnly })
	res := f.mustAllow(f.candidate(excerptPart(good, body, "Good")))
	assertAbsent(t, "envelope", whole(t, res.Envelope), tExcerptMarker)
}

func TestAllowExcerptBounded(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Limits = Limits{MaxExcerptBytes: 100, MaxEvidenceBytes: 250} })
	f.enableAll()
	src := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationInternal, nil)

	long := strings.Repeat("a", 60) + "<<CUT>>" + strings.Repeat("b", 400) + tExcerptMarker
	res := f.mustAllow(f.candidate(excerptPart(src, long, "T")))
	text := onlyText(t, res.Envelope, 1, 0)
	if len(text) > 100 || !strings.HasPrefix(text, strings.Repeat("a", 60)) {
		t.Errorf("an excerpt is cut to the bound: %d bytes", len(text))
	}
	assertAbsent(t, "envelope", whole(t, res.Envelope), tExcerptMarker)
	if o := res.Parts[2]; !o.Kept || o.Why != "truncated" {
		t.Errorf("part outcome = %+v", o)
	}
	if res.Decision.Sources[0].Outcome != providers.EgressDecisionSourceDecisionOutcomeExcerptIncluded || !res.Decision.RAGExcerptsIncluded {
		t.Errorf("decision = %+v", res.Decision.Sources)
	}

	// The cut falls on a character boundary: no broken UTF-8 reaches the provider.
	multi := strings.Repeat("é", 80) // two bytes each
	res = f.mustAllow(f.candidate(excerptPart(src, multi, "T")))
	if text := onlyText(t, res.Envelope, 1, 0); !utf8.ValidString(text) || len(text) > 100 {
		t.Errorf("cut text is valid UTF-8 within the bound: valid=%v len=%d", utf8.ValidString(text), len(text))
	}

	// The evidence of one request is bounded as a whole: an excerpt that would exceed it is withheld.
	e1, e2, e3 := strings.Repeat("1", 100), strings.Repeat("2", 100), strings.Repeat("3", 100)
	res = f.mustAllow(f.candidate(excerptPart(src, e1, "A"), excerptPart(src, e2, "B"), excerptPart(src, e3, "C")))
	w := wire(t, res.Envelope)
	assertPresent(t, "wire", w, e1, e2)
	assertAbsent(t, "wire", w, e3)
	if res.Parts[4].Kept || res.Parts[4].Why != "evidence_budget_exceeded" || !hasCategory(res.Decision.Categories.Withheld, providers.SendCategoryRAGExcerpt) {
		t.Errorf("third excerpt: %+v, withheld %v", res.Parts[4], res.Decision.Categories.Withheld)
	}
	// Required evidence that cannot fit is not silently dropped.
	req := excerptPart(src, e3, "C")
	req.Required = true
	f.mustDeny(f.candidate(excerptPart(src, e1, "A"), excerptPart(src, e2, "B"), req), providers.EgressDecisionReasonEnvelopeInvalid, providers.EgressDecisionCheckNameEnvelope)

	// The envelope names the mode that allowed the excerpt, as the schema requires.
	for _, m := range res.Envelope.Payload.Messages {
		for _, p := range m.Parts {
			meta := p.Value.(providers.RequestEnvelopeTextPart).Internal
			if meta.Category == providers.SendCategoryRAGExcerpt && (meta.SourceMode == nil || *meta.SourceMode != providers.EgressModeAllowExcerpt || meta.SourceID == nil || *meta.SourceID != src) {
				t.Errorf("excerpt metadata = %+v", meta)
			}
		}
	}
}

// onlyText returns the text of the part at (message, part) of an envelope.
func onlyText(t testing.TB, env *providers.RequestEnvelope, msg, part int) string {
	t.Helper()
	tp, ok := env.Payload.Messages[msg].Parts[part].Value.(providers.RequestEnvelopeTextPart)
	if !ok {
		t.Fatalf("part %d/%d is not text", msg, part)
	}
	return tp.Text
}

func TestAllowMetadataStripsContent(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	src := f.newSource(providers.EgressModeAllowMetadata, providers.DataClassificationInternal, nil)
	body := "the full contract text " + tExcerptMarker

	res := f.mustAllow(f.candidate(excerptPart(src, body, "Supplier contract (v3, current)")))
	assertAbsent(t, "envelope", whole(t, res.Envelope), tExcerptMarker, "full contract text")
	assertPresent(t, "wire", wire(t, res.Envelope), "Supplier contract (v3, current)")
	d := res.Decision
	if hasCategory(d.Categories.Sent, providers.SendCategoryRAGExcerpt) || !hasCategory(d.Categories.Sent, providers.SendCategoryRAGMetadata) || d.RAGExcerptsIncluded {
		t.Errorf("sent = %v (excerpts included %v): only metadata may go", d.Categories.Sent, d.RAGExcerptsIncluded)
	}
	if !hasCategory(d.Categories.Withheld, providers.SendCategoryRAGExcerpt) {
		t.Errorf("withheld = %v, want the excerpt content", d.Categories.Withheld)
	}
	if d.Sources[0].Outcome != providers.EgressDecisionSourceDecisionOutcomeMetadataIncluded || d.Sources[0].EffectiveMode != providers.EgressModeAllowMetadata {
		t.Errorf("source decision = %+v", d.Sources[0])
	}
	if o := res.Parts[2]; !o.Kept || o.Why != "metadata_only" {
		t.Errorf("part outcome = %+v", o)
	}

	// Nothing to show of a source without a label: nothing is sent, and the content stays out.
	res = f.mustAllow(f.candidate(excerptPart(src, body, "")))
	assertAbsent(t, "envelope", whole(t, res.Envelope), tExcerptMarker)

	// A metadata-only part is fine under allow_metadata, and not under a local_only source.
	meta := Part{Kind: PartText, Label: "Contract, status active", Category: string(providers.SendCategoryRAGMetadata), Class: string(providers.DataClassificationInternal), SourceID: string(src)}
	assertPresent(t, "wire", wire(t, f.mustAllow(f.candidate(meta)).Envelope), "status active")
	f.put(providers.EgressLevelWorkspace, func(in *PolicyInput) { in.SourceMode = providers.EgressModeLocalOnly })
	assertAbsent(t, "wire", wire(t, f.mustAllow(f.candidate(meta)).Envelope), "status active")
}

func TestDenySourceExcluded(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	denied := f.newSource(providers.EgressModeDeny, providers.DataClassificationInternal, nil)
	body := "denied body " + tExcerptMarker

	res := f.mustAllow(f.candidate(excerptPart(denied, body, "Denied title")))
	assertAbsent(t, "envelope", whole(t, res.Envelope), tExcerptMarker, "Denied title")
	if s := res.Decision.Sources[0]; s.EffectiveMode != providers.EgressModeDeny || s.Outcome != providers.EgressDecisionSourceDecisionOutcomeExcluded {
		t.Errorf("source decision = %+v", s)
	}
	if o := res.Parts[2]; o.Kept || o.Why != "source_denied" {
		t.Errorf("part outcome = %+v", o)
	}
	req := excerptPart(denied, body, "Denied title")
	req.Required = true
	den, _ := f.mustDeny(f.candidate(req), providers.EgressDecisionReasonSourceDenied, providers.EgressDecisionCheckNameSourceMode)
	if den.Decision.Checks[len(den.Decision.Checks)-1].Reason != "denied_source_required" {
		t.Errorf("reason code = %q", den.Decision.Checks[len(den.Decision.Checks)-1].Reason)
	}

	// A deny ceiling at any level above wins over a source that allows excerpts, and over metadata.
	good := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationInternal, nil)
	f.put(providers.EgressLevelApplication, func(in *PolicyInput) { in.SourceMode = providers.EgressModeDeny })
	res = f.mustAllow(f.candidate(excerptPart(good, body, "Good title")))
	if s := res.Decision.Sources[0]; s.EffectiveMode != providers.EgressModeDeny || s.Outcome != providers.EgressDecisionSourceDecisionOutcomeExcluded {
		t.Errorf("source decision = %+v", s)
	}
	assertAbsent(t, "envelope", whole(t, res.Envelope), tExcerptMarker, "Good title")
}
