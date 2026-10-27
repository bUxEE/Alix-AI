package verify

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// AnalystFactSet is the turn's verified ground truth for an analyst answer
// (ADR 0047 / docs/integration/domain-scope.md).
type AnalystFactSet struct {
	Datasets []analyst.Dataset
	Derived  []analyst.DerivedFact
	// Citations pair a WebCitation with the fetched page text (untrusted data).
	Citations []AnalystCitation
}

// AnalystCitation is a web citation plus the fetched text used for verification.
type AnalystCitation struct {
	Meta        analyst.WebCitation
	FetchedText string
}

// AnalystAnswer is the model answer to verify.
type AnalystAnswer struct {
	Text        string
	SourceSpans []analyst.SourceSpan
	Blocks      []analyst.StepProposalBlockPlan
	// FinalDatasetID selects which dataset the fallback template table shows.
	FinalDatasetID *analyst.DatasetID
}

// AnalystOptions configure analyst verification.
type AnalystOptions struct {
	Language Language
}

// AnalystOutcome is the verified reply or the analyst template replacement.
type AnalystOutcome struct {
	Text       string
	Source     Source
	Reason     Reason
	Violations []Violation
	Blocks     []conversation.RichBlock
	// CoreMaterialised is parallel to Blocks: true for every block this package
	// materialised from datasets (never a model-writable member).
	CoreMaterialised []bool
	Streamed         bool
}

// markCoreMaterialised returns a parallel true slice for core-built blocks.
func markCoreMaterialised(n int) []bool {
	if n == 0 {
		return nil
	}
	out := make([]bool, n)
	for i := range out {
		out[i] = true
	}
	return out
}

// VerifyAnalyst checks answer text (and optional source spans / block plan) against
// the turn fact set. Qualitative text without numbers passes. On failure it returns
// the analyst template (final dataset table + provenance) with ReasonVerificationFailed.
//
// Integrator: call VerifyAnalyst / RealiseAnalyst for a finished answer; use
// NewAnalystStream for verified streaming (ADR 0046) — a later failing segment
// yields Replace with the template.
func VerifyAnalyst(answer AnalystAnswer, facts AnalystFactSet, opts AnalystOptions) (AnalystOutcome, error) {
	if opts.Language != LangEN && opts.Language != LangIT {
		return AnalystOutcome{}, failure(contracts.ErrValidationFailed, ErrUnsupportedLanguage, "language %q is not supported", string(opts.Language))
	}
	res := checkAnalyst(answer.Text, answer.SourceSpans, facts, opts.Language)
	if res.OK {
		blocks, err := materialiseAnalystBlocks(answer.Blocks, facts)
		if err != nil {
			out, terr := analystTemplate(facts, answer.FinalDatasetID, opts.Language)
			if terr != nil {
				return AnalystOutcome{}, terr
			}
			out.Reason = ReasonVerificationFailed
			out.Violations = []Violation{{Kind: ViolationUnknownNumber, Got: "block"}}
			return out, nil
		}
		return AnalystOutcome{
			Text: strings.TrimSpace(answer.Text), Source: SourceModel, Reason: ReasonVerified,
			Blocks: blocks, CoreMaterialised: markCoreMaterialised(len(blocks)),
		}, nil
	}
	out, err := analystTemplate(facts, answer.FinalDatasetID, opts.Language)
	if err != nil {
		return AnalystOutcome{}, err
	}
	out.Reason = ReasonVerificationFailed
	out.Violations = res.Violations
	return out, nil
}

// RealiseAnalyst is VerifyAnalyst: model text when it verifies, else the template.
func RealiseAnalyst(answer AnalystAnswer, facts AnalystFactSet, opts AnalystOptions) (AnalystOutcome, error) {
	return VerifyAnalyst(answer, facts, opts)
}

func checkAnalyst(text string, spans []analyst.SourceSpan, facts AnalystFactSet, lang Language) Result {
	text = strings.TrimSpace(text)
	if text == "" {
		return Result{Violations: []Violation{{Kind: ViolationEmptyText}}}
	}
	if hasInvisible(text) {
		return Result{Violations: []Violation{{Kind: ViolationInvisibleCharacters}}}
	}
	allowed := buildAnalystAllowed(facts, lang)
	tenantNums := map[string]bool{}
	for n := range allowed.numbers {
		tenantNums[n] = true
	}
	webByID := map[string]AnalystCitation{}
	for _, c := range facts.Citations {
		webByID[c.Meta.ID] = c
	}
	hasKnowledge, hasAppOrComputed := false, false
	for _, sp := range spans {
		switch sp.Source {
		case analyst.SourceLabelKnowledge:
			hasKnowledge = true
		case analyst.SourceLabelApp, analyst.SourceLabelComputed:
			hasAppOrComputed = true
		}
	}

	for _, df := range facts.Derived {
		if df.Forecast == nil {
			continue
		}
		canon := canonFromAny(df.Value)
		if canon == "" || !numberMentioned(text, canon, lang) {
			continue
		}
		if !forecastLabelled(text, df.Forecast.Method, lang) {
			return Result{Violations: []Violation{{Kind: ViolationUnknownNumber, Got: canon}}}
		}
	}

	norm := normalise(text)
	ex := newExtractor(norm, lang, findKnown(norm, allowed.idList()), allowed.currs, allowed.author)
	ex.markAuthored()
	ex.run()
	for _, c := range ex.out {
		switch c.kind {
		case ckCitation, ckCompletion:
			continue
		}
		if claimAllowedAgainst(c, allowed) {
			// knowledge may not restate tenant-looking figures (domain-scope.md)
			if hasKnowledge && !hasAppOrComputed && claimIsTenantNumber(c, tenantNums) {
				bad := firstNumberAtom(c)
				return Result{Violations: []Violation{{Kind: ViolationUnknownNumber, Got: bad.s}}}
			}
			continue
		}
		if webClaimOK(c, spans, webByID) {
			continue
		}
		bad := firstBadAtom(c, allowed)
		return Result{Violations: []Violation{blameAtom(c, bad)}}
	}
	return Result{OK: true}
}

func claimIsTenantNumber(c claim, tenant map[string]bool) bool {
	for _, r := range c.readings {
		for _, at := range r {
			if (at.kind == atomNumber || at.kind == atomMoney) && tenant[at.s] {
				return true
			}
		}
	}
	return false
}

func firstNumberAtom(c claim) atom {
	for _, r := range c.readings {
		for _, at := range r {
			if at.kind == atomNumber || at.kind == atomMoney {
				return at
			}
		}
	}
	return atom{}
}

func claimAllowedAgainst(c claim, a *allowedSet) bool {
	for _, r := range c.readings {
		ok := len(r) > 0
		for _, at := range r {
			if !a.has(at) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func firstBadAtom(c claim, a *allowedSet) atom {
	if len(c.readings) == 0 {
		return atom{}
	}
	for _, at := range c.readings[0] {
		if !a.has(at) {
			return at
		}
	}
	return atom{}
}

func blameAtom(c claim, bad atom) Violation {
	out := Violation{Fingerprint: fingerprint(c.text)}
	switch bad.kind {
	case atomDate, atomInstant:
		out.Kind, out.Got = ViolationUnknownDate, bad.s
	case atomTime:
		out.Kind, out.Got = ViolationUnknownTime, bad.s
	case atomMoney:
		out.Kind, out.Got = ViolationUnknownAmount, bad.s+" "+bad.cur
	case atomID:
		out.Kind = ViolationUnknownIdentifier
	case atomNumber:
		out.Kind, out.Got = ViolationUnknownNumber, bad.s
	default:
		out.Kind = ViolationUnknownNumber
	}
	if c.kind == ckID {
		out.Kind = ViolationUnknownIdentifier
	}
	return out
}

func webClaimOK(c claim, spans []analyst.SourceSpan, webByID map[string]AnalystCitation) bool {
	hasWeb := false
	for _, sp := range spans {
		if sp.Source == analyst.SourceLabelWeb {
			hasWeb = true
			break
		}
	}
	if !hasWeb {
		return false
	}
	for _, r := range c.readings {
		ok := len(r) > 0
		for _, at := range r {
			if at.kind != atomNumber && at.kind != atomMoney {
				ok = false
				break
			}
			if !webCovers(at, spans, webByID) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func numberMentioned(text, canon string, lang Language) bool {
	e := newExtractor(normalise(text), lang, nil, nil, nil)
	e.run()
	for _, c := range e.out {
		for _, reading := range c.readings {
			for _, at := range reading {
				if at.kind == atomNumber && at.s == canon {
					return true
				}
			}
		}
	}
	return false
}

func forecastLabelled(text string, method analyst.DerivedFactForecastMetaMethod, lang Language) bool {
	low := strings.ToLower(text)
	est := false
	switch lang {
	case LangIT:
		est = strings.Contains(low, "stim") || strings.Contains(low, "previs") || strings.Contains(low, "estimate")
	default:
		est = strings.Contains(low, "estimate") || strings.Contains(low, "forecast") || strings.Contains(low, "projected")
	}
	if !est {
		return false
	}
	m := string(method)
	return strings.Contains(low, m) || strings.Contains(low, strings.ReplaceAll(m, "_", " ")) ||
		(method == analyst.DerivedFactForecastMetaMethodLinearTrend && (strings.Contains(low, "linear") || strings.Contains(low, "trend"))) ||
		(method == analyst.DerivedFactForecastMetaMethodHolt && strings.Contains(low, "holt")) ||
		(method == analyst.DerivedFactForecastMetaMethodSeasonalNaive && (strings.Contains(low, "seasonal") || strings.Contains(low, "naive")))
}

func webCovers(at atom, spans []analyst.SourceSpan, webByID map[string]AnalystCitation) bool {
	for _, sp := range spans {
		if sp.Source != analyst.SourceLabelWeb || sp.WebCitationID == nil {
			continue
		}
		cit, ok := webByID[*sp.WebCitationID]
		if !ok {
			continue
		}
		if numberInFetched(at, cit.FetchedText) {
			return true
		}
	}
	return false
}

func numberInFetched(at atom, fetched string) bool {
	if strings.TrimSpace(fetched) == "" {
		return false
	}
	e := newExtractor(normalise(fetched), LangEN, nil, nil, nil)
	e.run()
	for _, c := range e.out {
		for _, reading := range c.readings {
			for _, a := range reading {
				if a.kind == atomNumber && a.s == at.s {
					return true
				}
				if a.kind == atomMoney && at.kind == atomMoney && a.s == at.s {
					return true
				}
			}
		}
	}
	return strings.Contains(fetched, at.s) || strings.Contains(fetched, strings.ReplaceAll(at.s, ".", ","))
}

func buildAnalystAllowed(facts AnalystFactSet, lang Language) *allowedSet {
	a := newAllowedSet(lang)
	for _, ds := range facts.Datasets {
		for _, f := range ds.Fields {
			a.addString(string(f.Name))
			if f.Label != nil {
				a.addLabel(*f.Label)
			}
			if f.Currency != nil {
				a.currs[*f.Currency] = true
			}
		}
		for _, row := range ds.Rows {
			for _, v := range row {
				addCell(a, v)
			}
		}
		if ds.Provenance.CapabilityID != nil {
			a.addID(string(*ds.Provenance.CapabilityID))
		}
	}
	for _, df := range facts.Derived {
		addCell(a, df.Value)
		if df.Display != nil {
			a.addString(*df.Display)
		}
		a.addString(df.Label)
		a.addID(df.ID)
		if df.Currency != nil {
			a.currs[*df.Currency] = true
		}
		if df.Forecast != nil {
			a.addAtom(numberAtom(canonFromAny(df.Forecast.IntervalLow)))
			a.addAtom(numberAtom(canonFromAny(df.Forecast.IntervalHigh)))
		}
	}
	for _, c := range facts.Citations {
		a.addID(c.Meta.ID)
		a.addString(c.Meta.Title)
		if c.Meta.Snippet != nil {
			a.addString(*c.Meta.Snippet)
		}
	}
	for _, w := range []string{
		"estimate", "forecast", "projected", "linear_trend", "holt", "seasonal_naive",
		"linear", "trend", "seasonal", "naive", "stima", "previsione", "percent", "%",
	} {
		a.addString(w)
	}
	return a
}

func addCell(a *allowedSet, v any) {
	if v == nil {
		return
	}
	switch x := v.(type) {
	case string:
		a.addString(x)
		a.addID(x)
	case bool:
		if x {
			a.addString("true")
		} else {
			a.addString("false")
		}
	case float64:
		addNumberForms(a, canonFromAny(x))
	case int64:
		addNumberForms(a, canonFromAny(x))
	case int:
		addNumberForms(a, canonFromAny(x))
	default:
		a.addString(fmt.Sprint(x))
	}
}

// addNumberForms registers the canonical value and common display spellings the
// model may use (ADR 0047 numberFormat forms).
func addNumberForms(a *allowedSet, canon string) {
	if canon == "" {
		return
	}
	a.addAtom(numberAtom(canon))
	neg := strings.HasPrefix(canon, "-")
	body := strings.TrimPrefix(canon, "-")
	ip, fp, _ := strings.Cut(body, ".")
	for _, form := range groupForms(ip, fp) {
		if neg {
			form = "-" + form
		}
		a.addString(form)
		if n, ok := parseCanonical(strings.ReplaceAll(strings.ReplaceAll(form, ",", ""), " ", "")); ok {
			a.addAtom(numberAtom(n))
		}
	}
	// compact k/m when exact
	if fp == "" {
		if n, err := strconv.ParseInt(ip, 10, 64); err == nil {
			if n >= 1000 && n%100 == 0 {
				k := float64(n) / 1000
				a.addString(strings.TrimRight(strings.TrimRight(strconv.FormatFloat(k, 'f', 1, 64), "0"), ".") + "k")
				a.addString(strconv.FormatFloat(k, 'f', -1, 64) + "k")
			}
			if n >= 1_000_000 && n%100000 == 0 {
				m := float64(n) / 1_000_000
				a.addString(strconv.FormatFloat(m, 'f', -1, 64) + "m")
			}
		}
	}
}

func groupForms(ip, fp string) []string {
	var out []string
	out = append(out, ip)
	if fp != "" {
		out = append(out, ip+"."+fp)
		out = append(out, ip+","+fp)
	}
	grouped := groupThousands(ip)
	if grouped != ip {
		out = append(out, strings.ReplaceAll(grouped, ",", " "))
		out = append(out, strings.ReplaceAll(grouped, ",", "."))
		out = append(out, grouped) // comma
		if fp != "" {
			out = append(out, grouped+"."+fp, strings.ReplaceAll(grouped, ",", ".")+","+fp)
		}
	}
	return out
}

func groupThousands(ip string) string {
	if len(ip) <= 3 {
		return ip
	}
	var b strings.Builder
	first := len(ip) % 3
	if first == 0 {
		first = 3
	}
	b.WriteString(ip[:first])
	for i := first; i < len(ip); i += 3 {
		b.WriteByte(',')
		b.WriteString(ip[i : i+3])
	}
	return b.String()
}

func canonFromAny(v any) string {
	switch x := v.(type) {
	case float64:
		s := strconv.FormatFloat(x, 'f', -1, 64)
		if n, ok := parseCanonical(s); ok {
			return n
		}
		return s
	case int64:
		return canonDecimal(x < 0, strconv.FormatInt(abs64(x), 10), "")
	case int:
		return canonDecimal(x < 0, strconv.Itoa(absInt(x)), "")
	case string:
		if n, ok := parseCanonical(x); ok {
			return n
		}
		return ""
	default:
		return ""
	}
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func materialiseAnalystBlocks(plans []analyst.StepProposalBlockPlan, facts AnalystFactSet) ([]conversation.RichBlock, error) {
	byID := map[analyst.DatasetID]*analyst.Dataset{}
	for i := range facts.Datasets {
		d := &facts.Datasets[i]
		byID[d.ID] = d
	}
	var out []conversation.RichBlock
	for _, p := range plans {
		switch p.Type {
		case analyst.StepProposalBlockPlanTypeProvenance:
			out = append(out, conversation.RichBlock{Value: buildProvenanceBlock(facts)})
			continue
		case analyst.StepProposalBlockPlanTypeMarkdown:
			continue
		}
		if p.DatasetID == nil {
			continue
		}
		ds, ok := byID[*p.DatasetID]
		if !ok {
			return nil, fmt.Errorf("unknown dataset")
		}
		switch p.Type {
		case analyst.StepProposalBlockPlanTypeChart:
			ch, ok := chartFromPlan(ds, p)
			if !ok {
				return nil, fmt.Errorf("chart plan incomplete")
			}
			out = append(out, conversation.RichBlock{Value: ch})
		default:
			out = append(out, conversation.RichBlock{Value: conversation.TableBlock{
				Type:      "table",
				Columns:   tableCols(ds),
				Rows:      tableRows(ds),
				DatasetID: p.DatasetID,
			}})
		}
	}
	return out, nil
}

func chartFromPlan(ds *analyst.Dataset, p analyst.StepProposalBlockPlan) (conversation.ChartBlock, bool) {
	if ds == nil || len(ds.Fields) < 2 || len(ds.Rows) == 0 {
		return conversation.ChartBlock{}, false
	}
	mark := conversation.ChartBlockMarkBar
	if p.Mark != nil {
		mark = conversation.ChartBlockMark(*p.Mark)
		if !mark.Valid() {
			return conversation.ChartBlock{}, false
		}
	}
	xField, yField := string(ds.Fields[0].Name), string(ds.Fields[1].Name)
	if p.Encoding != nil {
		if p.Encoding.X != nil && *p.Encoding.X != "" {
			xField = string(*p.Encoding.X)
		}
		if p.Encoding.Y != nil && *p.Encoding.Y != "" {
			yField = string(*p.Encoding.Y)
		}
	}
	data := make([]conversation.ChartRecord, 0, len(ds.Rows))
	for _, row := range ds.Rows {
		data = append(data, conversation.ChartRecord{xField: row[xField], yField: row[yField]})
	}
	desc := conversation.PlainText("Chart of " + yField + " by " + xField + ".")
	if p.Description != nil && *p.Description != "" {
		desc = conversation.PlainText(*p.Description)
	}
	return conversation.ChartBlock{
		Type: "chart", Description: desc, Mark: mark, Data: data, DatasetID: p.DatasetID,
		Encoding: conversation.ChartEncoding{
			X: &conversation.ChartChannel{Field: common.FieldName(xField), Type: conversation.ChartChannelTypeNominal},
			Y: &conversation.ChartChannel{Field: common.FieldName(yField), Type: conversation.ChartChannelTypeQuantitative},
		},
	}, true
}

func tableCols(ds *analyst.Dataset) []conversation.TableColumn {
	cols := make([]conversation.TableColumn, len(ds.Fields))
	for i, f := range ds.Fields {
		label := string(f.Name)
		if f.Label != nil {
			label = *f.Label
		}
		cols[i] = conversation.TableColumn{Key: f.Name, Label: label}
	}
	return cols
}

func tableRows(ds *analyst.Dataset) [][]conversation.TableCell {
	rows := make([][]conversation.TableCell, len(ds.Rows))
	for i, r := range ds.Rows {
		row := make([]conversation.TableCell, len(ds.Fields))
		for j, f := range ds.Fields {
			row[j] = r[string(f.Name)]
		}
		rows[i] = row
	}
	return rows
}

func analystTemplate(facts AnalystFactSet, finalID *analyst.DatasetID, lang Language) (AnalystOutcome, error) {
	var ds *analyst.Dataset
	if finalID != nil {
		for i := range facts.Datasets {
			if facts.Datasets[i].ID == *finalID {
				ds = &facts.Datasets[i]
				break
			}
		}
	}
	if ds == nil && len(facts.Datasets) > 0 {
		ds = &facts.Datasets[len(facts.Datasets)-1]
	}
	var blocks []conversation.RichBlock
	text := "Replaced answer: final dataset table and provenance."
	if lang == LangIT {
		text = "Risposta sostituita: tabella del dataset finale e provenienza."
	}
	if ds != nil {
		id := ds.ID
		blocks = append(blocks, conversation.RichBlock{Value: conversation.TableBlock{
			Type: "table", Columns: tableCols(ds), Rows: tableRows(ds), DatasetID: &id,
		}})
	}
	blocks = append(blocks, conversation.RichBlock{Value: buildProvenanceBlock(facts)})
	return AnalystOutcome{
		Text: text, Source: SourceTemplate, Reason: ReasonVerificationFailed,
		Blocks: blocks, CoreMaterialised: markCoreMaterialised(len(blocks)),
	}, nil
}

func buildProvenanceBlock(facts AnalystFactSet) conversation.ProvenanceBlock {
	var steps []conversation.ProvenanceStep
	seen := map[string]bool{}
	for _, ds := range facts.Datasets {
		id := ds.ID
		switch ds.Provenance.Kind {
		case analyst.DatasetDatasetProvenanceKindRead:
			sum := conversation.PlainText("read")
			if ds.Provenance.CapabilityID != nil {
				sum = conversation.PlainText("read " + string(*ds.Provenance.CapabilityID))
			}
			steps = append(steps, conversation.ProvenanceStep{
				Kind: conversation.ProvenanceStepKindRead, Summary: sum,
				CapabilityID: ds.Provenance.CapabilityID, DatasetID: &id,
			})
		case analyst.DatasetDatasetProvenanceKindCompute:
			op := ""
			if ds.Provenance.Op != nil {
				op = *ds.Provenance.Op
			}
			key := "c:" + op + ":" + string(id)
			if seen[key] {
				continue
			}
			seen[key] = true
			kind := conversation.ProvenanceStepKindCompute
			if strings.HasPrefix(op, "forecast_") {
				kind = conversation.ProvenanceStepKindForecast
			}
			opCopy := op
			steps = append(steps, conversation.ProvenanceStep{
				Kind: kind, Summary: conversation.PlainText(op), Op: &opCopy, DatasetID: &id,
			})
		}
	}
	for _, df := range facts.Derived {
		if df.Forecast == nil {
			continue
		}
		m := string(df.Forecast.Method)
		steps = append(steps, conversation.ProvenanceStep{
			Kind:    conversation.ProvenanceStepKindForecast,
			Summary: conversation.PlainText("forecast " + m),
			Op:      &m,
		})
	}
	for _, c := range facts.Citations {
		cid := c.Meta.ID
		steps = append(steps, conversation.ProvenanceStep{
			Kind: conversation.ProvenanceStepKindFetch, Summary: conversation.PlainText(c.Meta.Title), WebCitationID: &cid,
		})
	}
	return conversation.ProvenanceBlock{Type: "provenance", Steps: steps}
}
