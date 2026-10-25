package ledger

import "github.com/buxee/alix-ai/pkg/bridge"

// ApproveVoid is the application's own authenticated UI finishing a level 3 void
// (confirmation mode step_up, channel application_ui). It is not an operation of the
// bridge: AlixAI can neither call it nor produce its effect. A signed-in manager
// quotes the void reference that the prepared void showed; the ledger records an
// approval for that reference only if it still names a voidable invoice at exactly
// the version that was prepared. Without the approval the commit answers
// step_up_required and changes nothing.
func (l *Ledger) ApproveVoid(id bridge.Identity, reference string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, bad := l.resolveCaller(id, PermInvoicesVoid)
	if bad != nil {
		return bad
	}
	for _, key := range sortedKeys(c.tenant.invoices) {
		inv := c.tenant.invoices[key]
		if !c.canSeeTeam(inv.team, true) || inv.status == invoiceVoid || inv.paid > 0 || voidReference(inv) != reference {
			continue
		}
		c.tenant.approvals[reference] = c.principal.id
		return nil
	}
	return refuse("scope_violation", "reference", "no voidable invoice has this reference")
}
