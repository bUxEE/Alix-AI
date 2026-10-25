package report

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/readiness/catalog"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// RdyLang is a report language. Both are mandatory (master plan 9.5).
type RdyLang string

// The languages.
const (
	RdyLangEN RdyLang = "en"
	RdyLangIT RdyLang = "it"
)

// Valid reports whether l is a supported language.
func (l RdyLang) Valid() bool { return l == RdyLangEN || l == RdyLangIT }

// The first line of every rendered report, in each language. It is the whole point of the
// report's standing: advice, not an audit pass.
const (
	RdyAdvisoryLineEN = "A readiness report is advisory: it is not an integration audit pass, and the audit still decides activation."
	RdyAdvisoryLineIT = "Un rapporto di readiness e' consultivo: non e' il superamento dell'audit di integrazione e l'audit decide comunque l'attivazione."
)

// RdyAdvisoryLine is the first line of the report in the language.
func RdyAdvisoryLine(l RdyLang) string {
	if l == RdyLangIT {
		return RdyAdvisoryLineIT
	}
	return RdyAdvisoryLineEN
}

// RdyItem is one requirement of a report section with the findings behind it.
type RdyItem struct {
	Item     readiness.ReportChecklistItem
	Req      readiness.Requirement
	Known    bool // false when the catalog of this build has no such requirement
	Findings []readiness.Finding
}

// RdySections is the report grouped for reading. It is derived from the document alone (and the
// catalog for titles), so a stored report renders the same as a fresh one.
type RdySections struct {
	Covered      []RdyItem // found and partial
	Missing      []RdyItem
	Contradicted []RdyItem
	Unknown      []RdyItem // unverified
	// NotApplicable are the requirements of a transport the project does not use, because another transport is met.
	NotApplicable []RdyItem
	// TransportsMet are the transport requirements that are found, which make the others not applicable.
	TransportsMet []readiness.RequirementID
	// Unconfirmed are model proposals the verifier could not confirm. They are listed, never counted.
	Unconfirmed []readiness.Finding
}

// RdySectionsOf groups a report.
func RdySectionsOf(doc readiness.Report, cat *catalog.Catalog) RdySections {
	byID := map[readiness.FindingID]readiness.Finding{}
	for _, f := range doc.Findings {
		byID[f.FindingID] = f
	}
	var s RdySections
	for _, it := range doc.Checklist {
		ri := RdyItem{Item: it}
		if cat != nil {
			ri.Req, ri.Known = cat.Get(it.RequirementID)
		}
		for _, id := range it.FindingIDs {
			if f, ok := byID[id]; ok {
				ri.Findings = append(ri.Findings, f)
			}
		}
		if it.Status == readiness.StatusFound && catalog.CatalogIsTransport(it.Category) {
			s.TransportsMet = append(s.TransportsMet, it.RequirementID)
		}
		switch it.Status {
		case readiness.StatusFound, readiness.StatusPartial:
			s.Covered = append(s.Covered, ri)
		case readiness.StatusMissing:
			s.Missing = append(s.Missing, ri)
		case readiness.StatusContradicted:
			s.Contradicted = append(s.Contradicted, ri)
		case readiness.StatusNotApplicable:
			s.NotApplicable = append(s.NotApplicable, ri)
		default:
			s.Unknown = append(s.Unknown, ri)
		}
	}
	for _, f := range doc.Findings {
		if f.Source == readiness.SourceModelUnverified {
			s.Unconfirmed = append(s.Unconfirmed, f)
		}
	}
	return s
}

type rdyText struct {
	title, report, generated, root, mode, modeDet, modelWord, modelLine, inputs, catalogLine, previous string
	summary, covered, missing, contradictions, unknowns, limits                                        string
	status                                                                                             map[readiness.Status]string
	level                                                                                              map[readiness.Level]string
	dropped, feeds, howToFix, evidence, lines, nothing, noReason, noRule, unconfirmed, unconfirmedNote string
	filesRead, filesSkipped, bytesRead, secrets, redacted, truncated, notTruncated, italianNote        string
	coveredIntro, missingIntro, contradictionsIntro, unknownsIntro                                     string
	notApplicable, notApplicableIntro, transportInUse                                                  string
	legWord, legCalls, legOverrides, legTemplate, legBudget, legIdle                                   string
	leg                                                                                                map[readiness.ReportModelLegStatus]string
}

var rdyTextEN = rdyText{
	legWord: "Model leg", legCalls: "model calls", legOverrides: "proposals that tried to change or remove a rule's result (dropped, never applied)",
	legTemplate: "prompt template", legBudget: "The model budget ran out before every excerpt was read.",
	legIdle: "a model is configured but there was nothing left for it to read, so it made no call",
	leg: map[readiness.ReportModelLegStatus]string{
		readiness.ReportModelLegStatusNotConfigured: "no model is configured, so only the deterministic scan ran",
		readiness.ReportModelLegStatusUnavailable:   "the model was unavailable, so only the deterministic scan ran",
		readiness.ReportModelLegStatusFailed:        "the model answered in a way that could not be used, so its output was discarded and only the deterministic scan counts",
		readiness.ReportModelLegStatusRefused:       "policy refused to send the project to the model, so only the deterministic scan ran",
		readiness.ReportModelLegStatusRan:           "a model proposed findings and the verifier checked each one",
	},
	title: "Integration readiness report", report: "Report", generated: "Generated", root: "Scanned directory",
	mode: "Mode", modeDet: "No model ran: this report comes from the deterministic scan alone.",
	modelWord: "Model", modelLine: "Model %s (%s), %s, advisory. A model only proposes; the verifier decides what counts.",
	inputs: "Inputs hash", catalogLine: "Catalog", previous: "Follows report",
	summary: "Summary", covered: "Covered", missing: "Missing, and how to fix it", contradictions: "Contradictions",
	unknowns: "Could not be determined", limits: "What the scan covered",
	status: map[readiness.Status]string{
		readiness.StatusFound: "Met", readiness.StatusPartial: "Partial", readiness.StatusMissing: "Missing",
		readiness.StatusUnverified: "Unverified", readiness.StatusContradicted: "Contradicted",
		readiness.StatusNotApplicable: "Not applicable",
	},
	level: map[readiness.Level]string{readiness.LevelMandatory: "mandatory", readiness.LevelRecommended: "recommended"},
	feeds: "Feeds the audit check", howToFix: "How to fix", evidence: "Evidence", lines: "lines",
	nothing: "Nothing to list.", noReason: "The status could not be determined.", noRule: "No rule produced a finding for this requirement.",
	dropped: "Model proposals dropped without becoming findings", unconfirmed: "Model proposals that did not confirm", unconfirmedNote: "Listed for the record. They never count as covered.",
	filesRead: "Files read", filesSkipped: "Files not read", bytesRead: "Bytes read",
	secrets: "Secret-looking files recorded as present and never read", redacted: "Secret-looking values redacted",
	truncated:           "The scan stopped at a size or file-count cap: the absence of a finding is not evidence of absence.",
	notTruncated:        "The scan was not cut short by a cap.",
	coveredIntro:        "Each item cites the file, the lines and the file hash a verifier re-read.",
	missingIntro:        "Nothing was found for these. Each says what to add.",
	contradictionsIntro: "Two pieces of evidence disagree. Fix the disagreement before relying on either.",
	unknownsIntro:       "These could not be confirmed. They are not counted as covered.",
	notApplicable:       "Not applicable",
	notApplicableIntro:  "MCP, OpenAPI and a native adapter are alternative transports. The project uses another one, so these are not needed; they are not counted as missing.",
	transportInUse:      "Transport in use",
}

var rdyTextIT = rdyText{
	legWord: "Fase del modello", legCalls: "chiamate al modello", legOverrides: "proposte che hanno tentato di cambiare o togliere il risultato di una regola (scartate, mai applicate)",
	legTemplate: "modello di prompt", legBudget: "Il budget del modello e' terminato prima di leggere tutti gli estratti.",
	legIdle: "un modello e' configurato ma non c'era nulla da fargli leggere, quindi non e' stata fatta alcuna chiamata",
	leg: map[readiness.ReportModelLegStatus]string{
		readiness.ReportModelLegStatusNotConfigured: "nessun modello e' configurato, quindi e' stata eseguita solo la scansione deterministica",
		readiness.ReportModelLegStatusUnavailable:   "il modello non era disponibile, quindi e' stata eseguita solo la scansione deterministica",
		readiness.ReportModelLegStatusFailed:        "il modello ha risposto in modo non utilizzabile, quindi il suo output e' stato scartato e conta solo la scansione deterministica",
		readiness.ReportModelLegStatusRefused:       "la policy ha rifiutato di inviare il progetto al modello, quindi e' stata eseguita solo la scansione deterministica",
		readiness.ReportModelLegStatusRan:           "un modello ha proposto dei riscontri e il verificatore ha controllato ciascuno",
	},
	title: "Rapporto di readiness dell'integrazione", report: "Rapporto", generated: "Generato", root: "Cartella analizzata",
	mode: "Modalita'", modeDet: "Nessun modello e' stato eseguito: il rapporto deriva solo dalla scansione deterministica.",
	modelWord: "Modello", modelLine: "Modello %s (%s), %s, consultivo. Un modello si limita a proporre; decide il verificatore.",
	inputs: "Hash degli input", catalogLine: "Catalogo", previous: "Segue il rapporto",
	summary: "Riepilogo", covered: "Coperto", missing: "Mancante, e come rimediare", contradictions: "Contraddizioni",
	unknowns: "Non determinabile", limits: "Cosa ha coperto la scansione",
	status: map[readiness.Status]string{
		readiness.StatusFound: "Soddisfatto", readiness.StatusPartial: "Parziale", readiness.StatusMissing: "Mancante",
		readiness.StatusUnverified: "Non verificato", readiness.StatusContradicted: "Contraddittorio",
		readiness.StatusNotApplicable: "Non applicabile",
	},
	level: map[readiness.Level]string{readiness.LevelMandatory: "obbligatorio", readiness.LevelRecommended: "raccomandato"},
	feeds: "Alimenta il controllo di audit", howToFix: "Come rimediare", evidence: "Evidenza", lines: "righe",
	nothing: "Niente da elencare.", noReason: "Non e' stato possibile determinare lo stato.", noRule: "Nessuna regola ha prodotto un riscontro per questo requisito.",
	dropped: "Proposte del modello scartate senza diventare riscontri", unconfirmed: "Proposte del modello non confermate", unconfirmedNote: "Elencate a titolo di registro. Non contano mai come coperte.",
	filesRead: "File letti", filesSkipped: "File non letti", bytesRead: "Byte letti",
	secrets: "File che sembrano segreti, registrati come presenti e mai letti", redacted: "Valori che sembrano segreti, oscurati",
	truncated:           "La scansione si e' fermata a un limite di dimensione o numero di file: l'assenza di un riscontro non prova che manchi.",
	notTruncated:        "La scansione non e' stata interrotta da nessun limite.",
	coveredIntro:        "Ogni voce cita il file, le righe e l'hash del file riletti da un verificatore.",
	missingIntro:        "Per questi non e' stato trovato nulla. Ognuno indica cosa aggiungere.",
	contradictionsIntro: "Due evidenze non concordano. Risolvere il disaccordo prima di fidarsi dell'una o dell'altra.",
	unknownsIntro:       "Non e' stato possibile confermarli. Non contano come coperti.",
	notApplicable:       "Non applicabile",
	notApplicableIntro:  "MCP, OpenAPI e un adapter nativo sono trasporti alternativi. Il progetto ne usa un altro, quindi questi non servono; non contano come mancanti.",
	transportInUse:      "Trasporto in uso",
	italianNote:         "I riepiloghi dei singoli riscontri sono prodotti dal rilevatore in inglese.",
}

// rdyEscape makes untrusted text safe in a markdown line: one line, no control characters, and
// every markdown-active character escaped.
func rdyEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || r == utf8.RuneError:
			b.WriteByte('?')
		case strings.ContainsRune("\\`*_{}[]<>()#+!|~&", r):
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// rdyCode makes untrusted text safe inside a code span.
func rdyCode(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '`':
			b.WriteByte('\'')
		case r < 0x20 || r == 0x7f || r == utf8.RuneError:
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	return "`" + b.String() + "`"
}

func rdyShort(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// RdyRenderMarkdown renders a report in one language. The first line is the advisory sentence,
// with no heading before it. cat supplies the requirement titles and fixes; a requirement the
// catalog does not know is shown by its id.
func RdyRenderMarkdown(doc readiness.Report, cat *catalog.Catalog, lang RdyLang) (string, error) {
	return RdyRenderMarkdownWith(doc, cat, lang, RdyRenderOptions{})
}

// RdyRenderOptions carry what is known when a report is built but is not part of the document.
type RdyRenderOptions struct {
	// DroppedProposals is how many model proposals were refused outright (an unknown requirement,
	// a repeat, too many). A report built with a model leg records the total itself in
	// model_leg.dropped_proposals; the larger of the two is shown.
	DroppedProposals int
}

// RdyRenderMarkdownWith is RdyRenderMarkdown with those extra facts.
func RdyRenderMarkdownWith(doc readiness.Report, cat *catalog.Catalog, lang RdyLang, opt RdyRenderOptions) (string, error) {
	if !lang.Valid() {
		return "", fmt.Errorf("readiness report: unsupported language %q", string(lang))
	}
	if !doc.Advisory {
		return "", fmt.Errorf("readiness report: the document is not marked advisory")
	}
	tx := rdyTextEN
	if lang == RdyLangIT {
		tx = rdyTextIT
	}
	sec := RdySectionsOf(doc, cat)
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("%s\n\n", RdyAdvisoryLine(lang))
	w("# %s\n\n", tx.title)
	w("- %s: %s\n", tx.report, rdyCode(string(doc.ReportID)))
	w("- %s: %s\n", tx.generated, rdyCode(string(doc.GeneratedAt)))
	w("- %s: %s\n", tx.root, rdyCode(doc.ScannedRoot))
	if doc.Mode == readiness.ReportModeDeterministicAndModel && doc.Model != nil {
		q := "unqualified"
		if lang == RdyLangIT {
			q = "non qualificato"
		}
		if doc.Model.Qualification == readiness.ReportModelQualificationQualified {
			q = "qualified"
			if lang == RdyLangIT {
				q = "qualificato"
			}
		}
		w("- %s: %s\n", tx.mode, rdyEscape(fmt.Sprintf(tx.modelLine, string(doc.Model.ModelID), string(doc.Model.Runtime), q)))
	} else {
		w("- %s: %s\n", tx.mode, tx.modeDet)
	}
	if l := doc.ModelLeg; l != nil {
		desc := tx.leg[l.Status]
		if l.Status == readiness.ReportModelLegStatusRan && l.Calls == 0 {
			desc = tx.legIdle
		}
		w("- %s: %s\n", tx.legWord, rdyEscape(desc))
		w("- %s: %d\n", tx.legCalls, l.Calls)
		if l.PromptTemplateSHA256 != nil {
			w("- %s: %s\n", tx.legTemplate, rdyCode(rdyShort(string(*l.PromptTemplateSHA256))))
		}
		if l.AttemptedOverrides > 0 {
			w("- %s: %d\n", tx.legOverrides, l.AttemptedOverrides)
		}
		if l.BudgetExhausted != nil && *l.BudgetExhausted {
			w("- %s\n", tx.legBudget)
		}
	}
	w("- %s: %s\n", tx.inputs, rdyCode(string(doc.InputsSHA256)))
	w("- %s: v%d %s\n", tx.catalogLine, doc.CatalogVersion, rdyCode(rdyShort(string(doc.CatalogSHA256))))
	if doc.PreviousReportID != nil {
		w("- %s: %s\n", tx.previous, rdyCode(string(*doc.PreviousReportID)))
	}
	if lang == RdyLangIT {
		w("- %s\n", tx.italianNote)
	}
	w("\n## %s\n\n", tx.summary)
	for _, st := range []readiness.Status{readiness.StatusFound, readiness.StatusPartial, readiness.StatusMissing, readiness.StatusUnverified, readiness.StatusContradicted} {
		w("- %s: %d\n", tx.status[st], rdyCount(doc.Summary, st))
	}
	if n := rdyCount(doc.Summary, readiness.StatusNotApplicable); n > 0 {
		w("- %s: %d\n", tx.status[readiness.StatusNotApplicable], n)
	}

	w("\n## %s\n\n%s\n\n", tx.covered, tx.coveredIntro)
	rdyItems(&b, sec.Covered, lang, tx, rdyShowEvidence)
	if len(sec.NotApplicable) > 0 {
		w("\n## %s\n\n%s\n\n", tx.notApplicable, tx.notApplicableIntro)
		var used []string
		for _, id := range sec.TransportsMet {
			used = append(used, rdyCode(string(id)))
		}
		for _, it := range sec.NotApplicable {
			title := string(it.Item.RequirementID)
			if it.Known {
				title = string(it.Req.Title.En)
				if lang == RdyLangIT {
					title = string(it.Req.Title.It)
				}
			}
			w("- %s %s (%s). %s: %s\n", rdyCode(string(it.Item.RequirementID)), rdyEscape(title), tx.level[it.Item.Level], tx.transportInUse, strings.Join(used, ", "))
		}
	}
	w("\n## %s\n\n%s\n\n", tx.missing, tx.missingIntro)
	rdyItems(&b, sec.Missing, lang, tx, rdyShowFix)
	w("\n## %s\n\n%s\n\n", tx.contradictions, tx.contradictionsIntro)
	rdyItems(&b, sec.Contradicted, lang, tx, rdyShowEvidence|rdyShowFix)
	w("\n## %s\n\n%s\n\n", tx.unknowns, tx.unknownsIntro)
	rdyItems(&b, sec.Unknown, lang, tx, rdyShowReason|rdyShowFix)
	dropped := int64(opt.DroppedProposals)
	if doc.ModelLeg != nil && doc.ModelLeg.DroppedProposals > dropped {
		dropped = doc.ModelLeg.DroppedProposals
	}
	if dropped > 0 {
		w("\n%s: %d\n", tx.dropped, dropped)
	}
	if len(sec.Unconfirmed) > 0 {
		w("\n### %s\n\n%s\n\n", tx.unconfirmed, tx.unconfirmedNote)
		for _, f := range sec.Unconfirmed {
			w("- %s %s: %s\n", rdyCode(string(f.RequirementID)), rdyCode(string(f.FindingID)), rdyEscape(rdyReasonOf(f, tx)))
		}
	}

	w("\n## %s\n\n", tx.limits)
	s := doc.Scan
	w("- %s: %d\n- %s: %d\n- %s: %d\n- %s: %d\n- %s: %d\n", tx.filesRead, s.FilesScanned, tx.filesSkipped, s.FilesSkipped, tx.bytesRead, s.BytesScanned, tx.secrets, s.SecretFilesRecorded, tx.redacted, s.ValuesRedacted)
	if s.Truncated {
		w("- %s\n", tx.truncated)
	} else {
		w("- %s\n", tx.notTruncated)
	}
	return b.String(), nil
}

func rdyCount(s readiness.Summary, st readiness.Status) int64 {
	switch st {
	case readiness.StatusFound:
		return s.Found
	case readiness.StatusPartial:
		return s.Partial
	case readiness.StatusMissing:
		return s.Missing
	case readiness.StatusUnverified:
		return s.Unverified
	case readiness.StatusNotApplicable:
		if s.NotApplicable != nil {
			return *s.NotApplicable
		}
		return 0
	}
	return s.Contradicted
}

const (
	rdyShowEvidence = 1 << iota
	rdyShowFix
	rdyShowReason
)

func rdyReasonOf(f readiness.Finding, tx rdyText) string {
	if f.UnverifiedReason != nil && *f.UnverifiedReason != "" {
		return string(*f.UnverifiedReason)
	}
	return tx.noReason
}

func rdyItems(b *strings.Builder, items []RdyItem, lang RdyLang, tx rdyText, show int) {
	if len(items) == 0 {
		fmt.Fprintf(b, "%s\n", tx.nothing)
		return
	}
	for _, it := range items {
		title := string(it.Item.RequirementID)
		fix := ""
		if it.Known {
			title = string(it.Req.Title.En)
			fix = string(it.Req.Remediation.En)
			if lang == RdyLangIT {
				title, fix = string(it.Req.Title.It), string(it.Req.Remediation.It)
			}
		}
		fmt.Fprintf(b, "### %s %s\n\n", rdyCode(string(it.Item.RequirementID)), rdyEscape(title))
		fmt.Fprintf(b, "- %s (%s). %s: %s\n", tx.status[it.Item.Status], tx.level[it.Item.Level], tx.feeds, rdyCode(string(it.Item.CheckID)))
		for _, f := range it.Findings {
			if f.Source == readiness.SourceModelUnverified {
				continue // listed once, in the section for proposals that did not confirm
			}
			summary := ""
			if f.Summary != nil && *f.Summary != "" {
				summary = rdyEscape(string(*f.Summary))
				fmt.Fprintf(b, "- %s\n", summary)
			}
			// A rule that could not tell says why in its summary too: the reason is printed once.
			if reason := rdyEscape(rdyReasonOf(f, tx)); show&rdyShowReason != 0 && f.Status == readiness.StatusUnverified && reason != summary {
				fmt.Fprintf(b, "- %s\n", reason)
			}
			if show&rdyShowEvidence != 0 {
				for _, e := range f.Evidence {
					fmt.Fprintf(b, "- %s: %s %s %d-%d, sha256 %s\n", tx.evidence, rdyCode(string(e.Path)), tx.lines, e.LineStart, e.LineEnd, rdyCode(rdyShort(string(e.FileSHA256))))
				}
			}
		}
		if len(it.Findings) == 0 && show&rdyShowReason != 0 {
			fmt.Fprintf(b, "- %s\n", tx.noRule)
		}
		if show&rdyShowFix != 0 && fix != "" {
			fmt.Fprintf(b, "- %s: %s\n", tx.howToFix, rdyEscape(fix))
		}
		b.WriteString("\n")
	}
}
