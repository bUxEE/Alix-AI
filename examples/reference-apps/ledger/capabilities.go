package ledger

import (
	"strconv"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// The four consequential capabilities. Each is a risk level of master plan 11.4:
//
//	quotes.draft     level 1  reversible: a draft quote nobody has been billed for
//	invoices.issue   level 2  external: the customer is sent an invoice
//	payments.record  level 2  external: the books and the customer are told of a payment
//	invoices.void    level 3  privileged: only the application's own UI can finish it

// Business limits, reported by inspect.
const (
	MaxDescription = 200
	MaxQuoteDays   = 90
	MaxDueDays     = 120
)

var (
	paymentMethods = []string{"bank_transfer", "card", "cash"}
	voidReasons    = []string{"duplicate", "wrong_amount", "customer_cancelled", "other"}
)

var quoteDraftDef = &actionDef{
	id: "quotes.draft", permission: PermQuotesDraft, risk: 1, validFor: 600,
	fieldNames: []string{"customer", "description", "amount_minor", "currency", "valid_days"},
	load: func(l *Ledger, c *caller, f fields) (*plan, *refusal) {
		custID, bad := f.text("customer", true, 1, 128)
		if bad != nil {
			return nil, bad
		}
		desc, bad := f.text("description", true, 3, MaxDescription)
		if bad != nil {
			return nil, bad
		}
		minor, bad := f.integer("amount_minor", true, 1, MaxMinor, 0)
		if bad != nil {
			return nil, bad
		}
		cur, bad := f.enum("currency", false, Currencies()...)
		if bad != nil {
			return nil, bad
		}
		days, bad := f.integer("valid_days", false, 1, MaxQuoteDays, 30)
		if bad != nil {
			return nil, bad
		}
		cu, bad := c.customer(custID, false)
		if bad != nil {
			return nil, bad
		}
		if cur == "" {
			cur = cu.currency
		}
		if cur != cu.currency {
			return nil, refuse("validation_failed", "currency", "the customer is billed in "+cu.currency)
		}
		amount := Money{Minor: minor, Currency: cur}
		until := addDays(l.today(), days)
		return &plan{
			versions: []actions.DataVersion{dataVersion("customer", cu.id, cu.version)},
			values: []actions.Fact{
				textFact("customer_name", cu.name), moneyFact("quote_total", amount), dateFact("quote_valid_until", until),
			},
			summary: "Draft a quote for " + cu.name + " of " + amount.Decimal() + " " + cur + ", valid until " + until + ".",
			effects: []actions.Effect{{
				Operation: actions.EffectOperationCreate, EntityType: "quote",
				Description: "Create a draft quote for " + cu.name + ".",
				Changes: []actions.Fact{
					identFact("customer", cu.id), textFact("description", desc), intFact("amount_minor", minor),
					textFact("currency", cur), intFact("valid_days", days),
					moneyFact("quote_total", amount), dateFact("quote_valid_until", until),
				},
			}},
			next: nextCancel(),
			apply: func() applied {
				c.tenant.quoteSeq++
				q := &quote{
					id: l.nextID("quo"), number: "QUO-" + pad(int64(c.tenant.quoteSeq), 4), team: cu.team,
					customerID: cu.id, description: desc, amount: amount, validUntil: until, status: quoteDraft, version: 1,
				}
				c.tenant.quotes[q.id] = q
				return applied{
					ids: []actions.RecordRef{ref("quote", q.id)},
					values: []actions.Fact{
						textFact("quote_number", q.number), textFact("customer_name", cu.name),
						moneyFact("quote_total", amount), dateFact("quote_valid_until", until),
					},
					versions: []actions.DataVersion{dataVersion("quote", q.id, q.version), dataVersion("customer", cu.id, cu.version)},
					next:     []NextAction{{Action: "invoke_capability", CapabilityID: "invoices.issue", Label: "Issue an invoice from this quote"}},
				}
			},
		}, nil
	},
}

var invoiceIssueDef = &actionDef{
	id: "invoices.issue", permission: PermInvoicesIssue, risk: 2, validFor: 600,
	fieldNames: []string{"quote", "due_days"},
	load: func(l *Ledger, c *caller, f fields) (*plan, *refusal) {
		quoteID, bad := f.text("quote", true, 1, 128)
		if bad != nil {
			return nil, bad
		}
		dueDays, bad := f.integer("due_days", false, 0, MaxDueDays, 30)
		if bad != nil {
			return nil, bad
		}
		q, bad := c.quote(quoteID)
		if bad != nil {
			return nil, bad
		}
		if q.status != quoteDraft {
			return nil, refuse("conflict", "quote", "the quote was already invoiced")
		}
		if q.validUntil < l.today() {
			return nil, refuse("validation_failed", "quote", "the quote is no longer valid")
		}
		cu := c.tenant.customers[q.customerID]
		dueOn := addDays(l.today(), dueDays)
		return &plan{
			versions: []actions.DataVersion{dataVersion("quote", q.id, q.version)},
			values: []actions.Fact{
				textFact("quote_number", q.number), textFact("customer_name", cu.name),
				moneyFact("invoice_total", q.amount), dateFact("invoice_due_on", dueOn),
			},
			summary: "Issue an invoice for " + cu.name + " of " + q.amount.Decimal() + " " + q.amount.Currency + " from quote " + q.number + ", due " + dueOn + ".",
			effects: []actions.Effect{
				{
					Operation: actions.EffectOperationCreate, EntityType: "invoice",
					Description: "Create an invoice from quote " + q.number + ".",
					Changes: []actions.Fact{
						identFact("quote", q.id), intFact("due_days", dueDays),
						moneyFact("invoice_total", q.amount), dateFact("invoice_due_on", dueOn),
					},
				},
				{
					Operation: actions.EffectOperationUpdate, EntityType: "quote", EntityID: idPtr(q.id),
					Description: "Mark quote " + q.number + " as invoiced.", Changes: []actions.Fact{textFact("status", quoteInvoiced)},
				},
				sendEffect(cu.name, "Send the invoice to "+cu.name+"."),
			},
			next: nextConfirm(),
			apply: func() applied {
				c.tenant.invoiceSeq++
				inv := &invoice{
					id: l.nextID("inv"), number: "INV-" + pad(int64(c.tenant.invoiceSeq), 4), team: q.team,
					customerID: q.customerID, quoteID: q.id, total: q.amount, issuedOn: l.today(), dueOn: dueOn,
					status: invoiceIssued, version: 1,
				}
				c.tenant.invoices[inv.id] = inv
				q.status = quoteInvoiced
				q.version++
				return applied{
					ids: []actions.RecordRef{ref("invoice", inv.id)},
					values: []actions.Fact{
						textFact("invoice_number", inv.number), textFact("quote_number", q.number), textFact("customer_name", cu.name),
						moneyFact("invoice_total", inv.total), dateFact("invoice_due_on", dueOn),
					},
					versions: []actions.DataVersion{dataVersion("invoice", inv.id, inv.version), dataVersion("quote", q.id, q.version)},
					next:     []NextAction{{Action: "invoke_capability", CapabilityID: "payments.record", Label: "Record a payment on this invoice"}},
				}
			},
		}, nil
	},
}

var paymentRecordDef = &actionDef{
	id: "payments.record", permission: PermPaymentsRecord, risk: 2, validFor: 600,
	fieldNames: []string{"invoice", "amount_minor", "currency", "paid_on", "method"},
	load: func(l *Ledger, c *caller, f fields) (*plan, *refusal) {
		invID, bad := f.text("invoice", true, 1, 128)
		if bad != nil {
			return nil, bad
		}
		minor, bad := f.integer("amount_minor", true, 1, MaxMinor, 0)
		if bad != nil {
			return nil, bad
		}
		cur, bad := f.enum("currency", false, Currencies()...)
		if bad != nil {
			return nil, bad
		}
		paidOn, bad := f.text("paid_on", false, 10, 10)
		if bad != nil {
			return nil, bad
		}
		method, bad := f.enum("method", false, paymentMethods...)
		if bad != nil {
			return nil, bad
		}
		inv, bad := c.invoice(invID, false)
		if bad != nil {
			return nil, bad
		}
		if inv.status != invoiceIssued && inv.status != invoicePartial {
			return nil, refuse("conflict", "invoice", "the invoice is "+inv.status+" and takes no payment")
		}
		if cur == "" {
			cur = inv.total.Currency
		}
		if cur != inv.total.Currency {
			return nil, refuse("validation_failed", "currency", "the invoice is in "+inv.total.Currency)
		}
		if paidOn == "" {
			paidOn = l.today()
		}
		if _, ok := parseDate(paidOn); !ok || paidOn > l.today() || paidOn < inv.issuedOn {
			return nil, refuse("validation_failed", "paid_on", "the payment date must be a date from the invoice date to today")
		}
		if method == "" {
			method = paymentMethods[0]
		}
		balance, _ := Sub(inv.total, Money{Minor: inv.paid, Currency: inv.total.Currency})
		amount := Money{Minor: minor, Currency: cur}
		after, err := Sub(balance, amount)
		if err != nil {
			return nil, refuse("validation_failed", "amount_minor", "the payment is more than the balance of the invoice")
		}
		statusAfter := invoicePartial
		if after.Minor == 0 {
			statusAfter = invoicePaid
		}
		cu := c.tenant.customers[inv.customerID]
		return &plan{
			versions: []actions.DataVersion{dataVersion("invoice", inv.id, inv.version)},
			values: []actions.Fact{
				textFact("invoice_number", inv.number), moneyFact("payment_amount", amount),
				moneyFact("balance_after", after), textFact("invoice_status_after", statusAfter),
			},
			summary: "Record a payment of " + amount.Decimal() + " " + cur + " on invoice " + inv.number + ", leaving " + after.Decimal() + " " + cur + " to pay.",
			effects: []actions.Effect{
				{
					Operation: actions.EffectOperationCreate, EntityType: "payment",
					Description: "Record a payment on invoice " + inv.number + ".",
					Changes: []actions.Fact{
						identFact("invoice", inv.id), intFact("amount_minor", minor), textFact("currency", cur),
						dateFact("paid_on", paidOn), textFact("method", method), moneyFact("payment_amount", amount),
					},
				},
				{
					Operation: actions.EffectOperationUpdate, EntityType: "invoice", EntityID: idPtr(inv.id),
					Description: "Reduce the balance of invoice " + inv.number + ".",
					Changes:     []actions.Fact{moneyFact("balance_after", after), textFact("status", statusAfter)},
				},
				sendEffect(cu.name, "Send a payment receipt to "+cu.name+"."),
			},
			next: nextConfirm(),
			apply: func() applied {
				c.tenant.paymentSeq++
				pay := &payment{
					id: l.nextID("pay"), number: "PAY-" + pad(int64(c.tenant.paymentSeq), 4), invoiceID: inv.id,
					team: inv.team, paidOn: paidOn, method: method, amount: amount,
				}
				c.tenant.payments[pay.id] = pay
				inv.paid += minor
				inv.status = statusAfter
				inv.version++
				kind := "invoice.payment_recorded"
				if statusAfter == invoicePaid {
					kind = "invoice.paid"
				}
				l.emit(c.tenant, inv.team, kind, "invoice", inv.id, inv.version)
				return applied{
					ids: []actions.RecordRef{ref("payment", pay.id), ref("invoice", inv.id)},
					values: []actions.Fact{
						textFact("payment_reference", pay.number), textFact("invoice_number", inv.number),
						moneyFact("payment_amount", amount), moneyFact("balance_remaining", after),
						textFact("invoice_status", statusAfter),
					},
					versions: []actions.DataVersion{dataVersion("invoice", inv.id, inv.version)},
				}
			},
		}, nil
	},
}

// voidReference is what a manager quotes in the application's own UI to finish a
// void: it names the invoice and the exact version that was prepared, so an
// approval can never be reused for an invoice that changed since.
func voidReference(inv *invoice) string {
	return "VOID-" + inv.number + "-" + strconv.FormatInt(inv.version, 10)
}

var invoiceVoidDef = &actionDef{
	id: "invoices.void", permission: PermInvoicesVoid, risk: 3, validFor: 900,
	fieldNames: []string{"invoice", "reason"},
	load: func(l *Ledger, c *caller, f fields) (*plan, *refusal) {
		invID, bad := f.text("invoice", true, 1, 128)
		if bad != nil {
			return nil, bad
		}
		reason, bad := f.enum("reason", true, voidReasons...)
		if bad != nil {
			return nil, bad
		}
		inv, bad := c.invoice(invID, true)
		if bad != nil {
			return nil, bad
		}
		switch {
		case inv.status == invoiceVoid:
			return nil, refuse("conflict", "invoice", "the invoice is already void")
		case inv.paid > 0:
			return nil, refuse("conflict", "invoice", "an invoice with payments cannot be voided")
		}
		cu := c.tenant.customers[inv.customerID]
		ref0 := voidReference(inv)
		return &plan{
			versions: []actions.DataVersion{dataVersion("invoice", inv.id, inv.version)},
			values: []actions.Fact{
				textFact("invoice_number", inv.number), textFact("customer_name", cu.name),
				moneyFact("invoice_total", inv.total), textFact("void_reference", ref0),
			},
			summary: "Void invoice " + inv.number + " of " + inv.total.Decimal() + " " + inv.total.Currency + " for " + cu.name + ".",
			effects: []actions.Effect{
				{
					Operation: actions.EffectOperationUpdate, EntityType: "invoice", EntityID: idPtr(inv.id),
					Description: "Void invoice " + inv.number + ".",
					Changes:     []actions.Fact{identFact("invoice", inv.id), textFact("reason", reason), textFact("status", invoiceVoid)},
				},
				sendEffect(cu.name, "Tell "+cu.name+" that invoice "+inv.number+" is void."),
			},
			next: nextCancel(),
			// The AI cannot finish this alone: the commit needs an approval that the
			// application's own authenticated UI recorded for exactly this reference.
			gate: func() *refusal {
				if c.tenant.approvals[ref0] == "" {
					return refuse("step_up_required", "", "this must be completed in the application with reference "+ref0)
				}
				return nil
			},
			apply: func() applied {
				inv.status = invoiceVoid
				inv.version++
				delete(c.tenant.approvals, ref0)
				l.emit(c.tenant, inv.team, "invoice.voided", "invoice", inv.id, inv.version)
				return applied{
					ids: []actions.RecordRef{ref("invoice", inv.id)},
					values: []actions.Fact{
						textFact("invoice_number", inv.number), textFact("invoice_status", invoiceVoid),
						textFact("void_reference", ref0),
					},
					versions: []actions.DataVersion{dataVersion("invoice", inv.id, inv.version)},
				}
			},
		}, nil
	},
}

func idPtr(id string) *common.Identifier { v := common.Identifier(id); return &v }
