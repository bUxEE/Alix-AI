package ledger

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/pkg/bridge"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The reads: finding invoices (capability invoices.find, level 0) and the entity
// resolvers AlixAI uses to turn a mention into a record before it prepares
// anything. A read decides nothing and changes nothing.

var (
	invoiceStatuses = []string{invoiceIssued, invoicePartial, invoicePaid, invoiceVoid}
	idPatterns      = map[string]*regexp.Regexp{
		"invoice":  regexp.MustCompile(`^inv_[0-9]+$`),
		"quote":    regexp.MustCompile(`^quo_[0-9]+$`),
		"customer": regexp.MustCompile(`^cus_[0-9]+$`),
	}
	numberPatterns = map[string]*regexp.Regexp{
		"invoice": regexp.MustCompile(`(?i)^inv-?0*([0-9]+)$`),
		"quote":   regexp.MustCompile(`(?i)^quo-?0*([0-9]+)$`),
	}
)

// MaxFound is the most invoices one find lists, and MaxCandidates the most a
// resolver offers.
const (
	MaxFound      = 10
	MaxCandidates = 10
)

// normalisedNumber turns "inv 7", "INV-7" and "inv-0007" into INV-0007.
func normalisedNumber(kind, mention string) (string, bool) {
	m := numberPatterns[kind].FindStringSubmatch(strings.ReplaceAll(strings.TrimSpace(mention), " ", "-"))
	if m == nil {
		return "", false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return "", false
	}
	prefix := "INV-"
	if kind == "quote" {
		prefix = "QUO-"
	}
	return prefix + pad(n, 4), true
}

func (l *Ledger) invoiceBalance(inv *invoice) Money {
	return Money{Minor: inv.total.Minor - inv.paid, Currency: inv.total.Currency}
}

func (l *Ledger) matchInvoices(c *caller, mention string, tenantWide bool) ([]*invoice, *refusal) {
	if idPatterns["invoice"].MatchString(mention) {
		inv, bad := c.invoice(mention, tenantWide)
		if bad != nil {
			return nil, bad
		}
		return []*invoice{inv}, nil
	}
	number, isNumber := normalisedNumber("invoice", mention)
	needle := strings.ToLower(mention)
	var out []*invoice
	for _, id := range sortedKeys(c.tenant.invoices) {
		inv := c.tenant.invoices[id]
		if !c.canSeeTeam(inv.team, tenantWide) {
			continue
		}
		cu := c.tenant.customers[inv.customerID]
		if (isNumber && inv.number == number) || strings.Contains(strings.ToLower(inv.number), needle) ||
			strings.Contains(strings.ToLower(cu.name), needle) {
			out = append(out, inv)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].number < out[j].number })
	return out, nil
}

// findInvoices serves invoices.find: invoices of the caller's reach matching a
// number or a customer name. A manager reaches every team of the organisation;
// everyone else reaches their own team.
func (l *Ledger) findInvoices(id bridge.Identity, raw map[string]any) Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, bad := l.resolveCaller(id, PermInvoicesRead)
	if bad != nil {
		return bad.result()
	}
	f, bad := parseFields(raw, "query", "status", "max_results")
	if bad != nil {
		return bad.result()
	}
	query, bad := f.text("query", true, 2, 100)
	if bad != nil {
		return bad.result()
	}
	status, bad := f.enum("status", false, invoiceStatuses...)
	if bad != nil {
		return bad.result()
	}
	limit, bad := f.integer("max_results", false, 1, MaxFound, 5)
	if bad != nil {
		return bad.result()
	}
	matches, bad := l.matchInvoices(c, query, true)
	if bad != nil {
		return bad.result()
	}
	var listed []*invoice
	more := false
	for _, inv := range matches {
		if status != "" && inv.status != status {
			continue
		}
		if int64(len(listed)) == limit {
			more = true
			break
		}
		listed = append(listed, inv)
	}

	versions := make([]actions.DataVersion, 0, len(listed))
	res := Result{Status: "ok"}
	summary := make([]string, 0, len(listed))
	values := []actions.Fact{intFact("invoice_count", int64(len(listed)))}
	for i, inv := range listed {
		n := i + 1
		cu := c.tenant.customers[inv.customerID]
		p := fmt.Sprintf("invoice_%d_", n)
		values = append(values,
			textFact(p+"number", inv.number), textFact(p+"customer", cu.name),
			moneyFact(p+"total", inv.total), moneyFact(p+"balance", l.invoiceBalance(inv)),
			textFact(p+"status", inv.status), dateFact(p+"due_on", inv.dueOn))
		summary = append(summary, inv.number+" for "+cu.name+" ("+inv.status+")")
		versions = append(versions, dataVersion("invoice", inv.id, inv.version))
		res.Identifiers = append(res.Identifiers, ref("invoice", inv.id))
	}
	text := "no matching invoices"
	if len(summary) > 0 {
		text = strings.Join(summary, "; ")
	}
	values = append(values, textFact("invoice_summary", text), boolFact("more_available", more))
	res.Values = values
	res.Freshness = freshnessOf(l.now(), versions)
	if len(listed) > 0 {
		if c.principal.role.allows(PermPaymentsRecord) {
			res.AllowedNextActions = append(res.AllowedNextActions,
				NextAction{Action: "invoke_capability", CapabilityID: "payments.record", Label: "Record a payment"})
		}
		if c.principal.role.allows(PermInvoicesVoid) {
			res.AllowedNextActions = append(res.AllowedNextActions,
				NextAction{Action: "invoke_capability", CapabilityID: "invoices.void", Label: "Void an invoice"})
		}
	}
	return res
}

// ---- entity resolvers -------------------------------------------------------
//
// AlixAI resolves a mention ("INV-7", "Acme") to candidate identifiers with an
// adapter READ carrying the reserved arguments entity_type, mention and, when the
// model proposed one, candidate_id. A resolver answers with the candidates the
// caller may see, in a stable order, as identifiers plus one text value named label
// for each at the same position, and the snapshot version of those records as
// freshness. A record the caller may not see is refused as scope_violation exactly
// like one that does not exist. candidate_id is the model's proposal and is never an
// instruction: with a mention it changes nothing; alone it names the one record to
// re-resolve, access-checked.

type candidate struct {
	id      string
	version int64
	label   string
}

func (l *Ledger) resolve(entityType, permission string, tenantWide bool, id bridge.Identity, raw map[string]any) Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, bad := l.resolveCaller(id, permission)
	if bad != nil {
		return bad.result()
	}
	f, bad := parseFields(raw, "entity_type", "mention", "candidate_id")
	if bad != nil {
		return bad.result()
	}
	if stated, bad := f.text("entity_type", false, 1, 64); bad != nil {
		return bad.result()
	} else if stated != "" && stated != entityType {
		return refuse("validation_failed", "entity_type", "this resolver answers for "+entityType).result()
	}
	hint, bad := f.text("candidate_id", false, 1, 128)
	if bad != nil {
		return bad.result()
	}
	mention, bad := f.text("mention", false, 1, 100)
	if bad != nil {
		return bad.result()
	}
	if mention == "" {
		if hint == "" {
			return refuse("missing_fields", "mention", "mention is required").result()
		}
		mention = hint
	}
	var found []candidate
	switch entityType {
	case "invoice":
		invs, bad := l.matchInvoices(c, mention, tenantWide)
		if bad != nil {
			return bad.result()
		}
		for _, inv := range invs {
			cu := c.tenant.customers[inv.customerID]
			found = append(found, candidate{inv.id, inv.version, fmt.Sprintf("%s - %s - %s %s (%s)",
				inv.number, cu.name, inv.total.Decimal(), inv.total.Currency, inv.status)})
		}
	case "quote":
		qs, bad := l.matchQuotes(c, mention)
		if bad != nil {
			return bad.result()
		}
		for _, q := range qs {
			cu := c.tenant.customers[q.customerID]
			found = append(found, candidate{q.id, q.version, fmt.Sprintf("%s - %s - %s %s (%s)",
				q.number, cu.name, q.amount.Decimal(), q.amount.Currency, q.status)})
		}
	case "customer":
		cs, bad := l.matchCustomers(c, mention)
		if bad != nil {
			return bad.result()
		}
		for _, cu := range cs {
			found = append(found, candidate{cu.id, cu.version, cu.name + " (" + cu.currency + ")"})
		}
	}
	if len(found) > MaxCandidates {
		found = found[:MaxCandidates]
	}
	res := Result{Status: "ok", Values: []actions.Fact{intFact("candidate_count", int64(len(found)))}}
	versions := make([]actions.DataVersion, 0, len(found))
	for _, cd := range found {
		res.Identifiers = append(res.Identifiers, ref(entityType, cd.id))
		res.Values = append(res.Values, textFact("label", cd.label))
		versions = append(versions, dataVersion(entityType, cd.id, cd.version))
	}
	res.Freshness = freshnessOf(l.now(), versions)
	return res
}

func (l *Ledger) matchQuotes(c *caller, mention string) ([]*quote, *refusal) {
	if idPatterns["quote"].MatchString(mention) {
		q, bad := c.quote(mention)
		if bad != nil {
			return nil, bad
		}
		return []*quote{q}, nil
	}
	number, isNumber := normalisedNumber("quote", mention)
	needle := strings.ToLower(mention)
	var out []*quote
	for _, id := range sortedKeys(c.tenant.quotes) {
		q := c.tenant.quotes[id]
		if !c.canSeeTeam(q.team, false) {
			continue
		}
		cu := c.tenant.customers[q.customerID]
		if (isNumber && q.number == number) || strings.Contains(strings.ToLower(q.number), needle) ||
			strings.Contains(strings.ToLower(cu.name), needle) {
			out = append(out, q)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].number < out[j].number })
	return out, nil
}

func (l *Ledger) matchCustomers(c *caller, mention string) ([]*customer, *refusal) {
	if idPatterns["customer"].MatchString(mention) {
		cu, bad := c.customer(mention, false)
		if bad != nil {
			return nil, bad
		}
		return []*customer{cu}, nil
	}
	needle := strings.ToLower(mention)
	var out []*customer
	for _, id := range sortedKeys(c.tenant.customers) {
		cu := c.tenant.customers[id]
		if c.canSeeTeam(cu.team, false) && strings.Contains(strings.ToLower(cu.name), needle) {
			out = append(out, cu)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}
