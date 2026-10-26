package providerbudget

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"unicode"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// budgetRow is a stored budget with the scope columns of its row.
type budgetRow struct {
	tenant, workspace string
	b                 providers.Budget
}

func (r budgetRow) provider() string {
	if r.b.Subject.ProviderID == nil {
		return ""
	}
	return string(*r.b.Subject.ProviderID)
}

func ident(p *common.Identifier) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

// validIdent mirrors schemas/common identifier: 1 to 128 characters, no space, no control character,
// no wildcard.
func validIdent(s string) bool {
	if s == "" || len([]rune(s)) > contracts.MaxScopeValueLen {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '*' || (r >= 0x7f && r <= 0xa0) {
			return false
		}
	}
	return true
}

// subjectColumns returns the stored columns of a validated subject: tenant, workspace, principal and
// provider (” when absent).
func subjectColumns(s providers.BudgetSubject) (tenant, workspace, principal, provider string) {
	provider = ""
	if s.ProviderID != nil {
		provider = string(*s.ProviderID)
	}
	switch s.Kind {
	case providers.BudgetSubjectKindTenant:
		return ident(s.TenantID), "", "", provider
	case providers.BudgetSubjectKindWorkspace:
		return ident(s.TenantID), ident(s.WorkspaceID), "", provider
	case providers.BudgetSubjectKindPrincipal:
		return ident(s.TenantID), ident(s.WorkspaceID), ident(s.PrincipalID), provider
	}
	return "", "", "", provider
}

var providerIDOK = func(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case i > 0 && (r >= '0' && r <= '9' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

// validateBudget mirrors the schema rules the generated type cannot express and pins the subject to
// the caller's application.
func validateBudget(scope contracts.Scope, b providers.Budget) error {
	s := b.Subject
	if !s.Kind.Valid() {
		return invalid("bad_budget", "providerbudget: the budget subject kind is not valid")
	}
	has := map[string]bool{
		"application": s.ApplicationID != nil, "tenant": s.TenantID != nil,
		"workspace": s.WorkspaceID != nil, "principal": s.PrincipalID != nil,
	}
	var want []string
	switch s.Kind {
	case providers.BudgetSubjectKindProvider:
		if s.ProviderID == nil {
			return invalid("bad_budget", "providerbudget: a provider budget names the provider")
		}
	case providers.BudgetSubjectKindTenant:
		want = []string{"application", "tenant"}
	case providers.BudgetSubjectKindWorkspace:
		want = []string{"application", "tenant", "workspace"}
	case providers.BudgetSubjectKindPrincipal:
		want = []string{"application", "tenant", "workspace", "principal"}
	}
	allowed := map[string]bool{}
	for _, w := range want {
		allowed[w] = true
		if !has[w] {
			return invalid("bad_budget", fmt.Sprintf("providerbudget: a %s budget names its %s", s.Kind, w))
		}
	}
	for k, present := range has {
		if present && !allowed[k] {
			return invalid("bad_budget", fmt.Sprintf("providerbudget: a %s budget cannot name a %s", s.Kind, k))
		}
	}
	for _, id := range []*common.Identifier{s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID} {
		if id != nil && !validIdent(string(*id)) {
			return invalid("bad_budget", "providerbudget: a budget subject id is not a valid identifier")
		}
	}
	if s.ProviderID != nil && !providerIDOK(string(*s.ProviderID)) {
		return invalid("bad_budget", "providerbudget: the budget provider id is not valid")
	}
	if s.ApplicationID != nil && string(*s.ApplicationID) != scope.ApplicationID {
		return contracts.New(contracts.ErrScopeViolation, "providerbudget: a budget can only be set inside the caller's application").
			WithDetails("reason", "other_application")
	}
	if !b.Channel.Valid() {
		return invalid("bad_budget", "providerbudget: the budget channel is not valid")
	}
	if !currencyRE.MatchString(string(b.Currency)) {
		return invalid("bad_budget", "providerbudget: the budget currency is not three upper case letters")
	}
	for _, v := range []int64{int64(b.Limits.DailyMicroUnits), int64(b.Limits.MonthlyMicroUnits)} {
		if v < 0 || v > MaxAmount {
			return invalid("bad_budget", "providerbudget: a limit is outside 0..2^53-1")
		}
	}
	if b.WarnAtPercent < 1 || b.WarnAtPercent > 99 {
		return invalid("bad_budget", "providerbudget: warn_at_percent is 1 to 99")
	}
	if b.RateLimits != nil {
		rl := b.RateLimits
		if rl.RequestsPerMin == nil && rl.ConcurrentRequests == nil {
			return invalid("bad_budget", "providerbudget: rate_limits names at least one limit")
		}
		for _, p := range []*int64{rl.RequestsPerMin, rl.ConcurrentRequests} {
			if p != nil && (*p < 1 || *p > MaxAmount) {
				return invalid("bad_budget", "providerbudget: a rate limit is outside 1..2^53-1")
			}
		}
	}
	for _, q := range quotaList(b) {
		if q.q.Daily == nil && q.q.Monthly == nil {
			return invalid("bad_budget", fmt.Sprintf("providerbudget: the %s quota names a daily or monthly figure", q.unit))
		}
		for _, c := range []*providers.Counter{q.q.Daily, q.q.Monthly} {
			if c != nil && (*c < 0 || int64(*c) > MaxAmount) {
				return invalid("bad_budget", fmt.Sprintf("providerbudget: the %s quota is outside 0..2^53-1", q.unit))
			}
		}
	}
	return nil
}

type unitQuota struct {
	unit providers.MeterUnit
	q    providers.BudgetQuota
}

// quotaList returns the quotas the budget sets, in unit order.
func quotaList(b providers.Budget) []unitQuota {
	if b.Quotas == nil {
		return nil
	}
	q := b.Quotas
	var out []unitQuota
	for _, e := range []struct {
		u providers.MeterUnit
		q *providers.BudgetQuota
	}{
		{providers.MeterUnitToksIn, q.ToksIn}, {providers.MeterUnitToksOut, q.ToksOut},
		{providers.MeterUnitToksInCached, q.ToksInCached}, {providers.MeterUnitImagesIn, q.ImagesIn},
		{providers.MeterUnitImagesOut, q.ImagesOut}, {providers.MeterUnitAudioSecondsIn, q.AudioSecondsIn},
		{providers.MeterUnitAudioSecondsOut, q.AudioSecondsOut}, {providers.MeterUnitFileBytesIn, q.FileBytesIn},
		{providers.MeterUnitCharactersIn, q.CharactersIn}, {providers.MeterUnitRequests, q.Requests},
	} {
		if e.q != nil {
			out = append(out, unitQuota{e.u, *e.q})
		}
	}
	return out
}

func decodeBudget(doc string) (providers.Budget, error) {
	var b providers.Budget
	dec := json.NewDecoder(bytes.NewReader([]byte(doc)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return providers.Budget{}, contracts.Wrap(contracts.ErrInternal, err, "providerbudget: a stored budget cannot be decoded")
	}
	return b, nil
}

// PutBudget creates or changes a budget. It is an operator function: the gateway gates it by role,
// and the package checks that the subject lies in the caller's application. An empty BudgetID creates
// a budget (revision 1, a new ID); a given one must name an existing budget and carry its current
// revision (otherwise ErrConflict), and the subject, channel and currency of a budget never change
// (a different subject is a different budget). The core sets the ID, revision, updated_at and
// updated_by; whatever the caller put there is ignored. Changing limits never rewrites spend.
func (s *Service) PutBudget(ctx context.Context, scope contracts.Scope, b providers.Budget) (providers.Budget, error) {
	if err := scope.Validate(); err != nil {
		return providers.Budget{}, err
	}
	if err := validateBudget(scope, b); err != nil {
		return providers.Budget{}, err
	}
	now := s.clk.Now()
	tenant, workspace, principal, provider := subjectColumns(b.Subject)
	b.UpdatedAt = timestamp(now)
	b.UpdatedBy = common.Identifier(scope.PrincipalID)
	var out providers.Budget
	err := s.db.Tx(ctx, func(tx *store.Tx) error {
		if b.BudgetID == "" {
			out = b
			out.BudgetID = providers.BudgetID(s.ids.New(prefixBudget))
			out.Revision = 1
			doc, err := json.Marshal(out)
			if err != nil {
				return contracts.Wrap(contracts.ErrInternal, err, "providerbudget: a budget cannot be encoded")
			}
			_, err = tx.ExecContext(ctx,
				`INSERT INTO providerbudget_budgets
				   (application_id, tenant_id, workspace_id, budget_id, kind, provider_id, principal_id, channel,
				    currency, revision, document, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
				scope.ApplicationID, tenant, workspace, string(out.BudgetID), string(b.Subject.Kind), provider, principal,
				string(b.Channel), string(b.Currency), string(doc), now.UnixMilli())
			if store.IsUniqueViolation(err) {
				return conflict("budget_exists", "providerbudget: a budget for this subject and channel already exists")
			}
			return wrapStore(err, "providerbudget: storing a budget failed")
		}
		cur, found, err := loadBudget(ctx, tx, scope.ApplicationID, string(b.BudgetID))
		if err != nil {
			return err
		}
		if !found {
			return invalid("unknown_budget", "providerbudget: no such budget")
		}
		// The subject first: a caller naming a budget that is not the subject it
		// sends learns nothing about it, not even its revision.
		ct, cw, cp, cprov := subjectColumns(cur.b.Subject)
		if cur.b.Subject.Kind != b.Subject.Kind || ct != tenant || cw != workspace || cp != principal || cprov != provider ||
			cur.b.Channel != b.Channel || cur.b.Currency != b.Currency {
			return invalid("immutable_budget", "providerbudget: the subject, channel and currency of a budget cannot change")
		}
		if cur.b.Revision != b.Revision {
			return conflict("stale_revision", "providerbudget: the budget changed since it was read").
				WithDetails("revision", cur.b.Revision)
		}
		out = b
		out.Revision = cur.b.Revision + 1
		doc, err := json.Marshal(out)
		if err != nil {
			return contracts.Wrap(contracts.ErrInternal, err, "providerbudget: a budget cannot be encoded")
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE providerbudget_budgets SET revision = ?, document = ?, updated_at = ?
			  WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND budget_id = ?`,
			int64(out.Revision), string(doc), now.UnixMilli(), scope.ApplicationID, cur.tenant, cur.workspace, string(out.BudgetID))
		return wrapStore(err, "providerbudget: updating a budget failed")
	})
	if err != nil {
		return providers.Budget{}, err
	}
	return out, nil
}

const budgetCols = "tenant_id, workspace_id, document"

func scanBudget(sc interface{ Scan(...any) error }) (budgetRow, error) {
	var r budgetRow
	var doc string
	if err := sc.Scan(&r.tenant, &r.workspace, &doc); err != nil {
		return budgetRow{}, err
	}
	b, err := decodeBudget(doc)
	if err != nil {
		return budgetRow{}, err
	}
	r.b = b
	return r, nil
}

// loadBudget reads one budget of the application by ID.
func loadBudget(ctx context.Context, q store.Reader, app, id string) (budgetRow, bool, error) {
	row := q.QueryRowContext(ctx,
		"SELECT "+budgetCols+" FROM providerbudget_budgets WHERE application_id = ? AND budget_id = ?", app, id)
	r, err := scanBudget(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return budgetRow{}, false, nil
	case err != nil:
		return budgetRow{}, false, wrapStore(err, "providerbudget: reading a budget failed")
	}
	return r, true, nil
}

// ListBudgets returns every budget of the caller's application, oldest first. Operator function.
func (s *Service) ListBudgets(ctx context.Context, scope contracts.Scope) ([]providers.Budget, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var out []providers.Budget
	err := s.db.View(ctx, func(tx *store.Tx) error {
		rows, err := tx.QueryContext(ctx,
			"SELECT "+budgetCols+" FROM providerbudget_budgets WHERE application_id = ? ORDER BY seq", scope.ApplicationID)
		if err != nil {
			return wrapStore(err, "providerbudget: listing budgets failed")
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanBudget(rows)
			if err != nil {
				return wrapStore(err, "providerbudget: listing budgets failed")
			}
			out = append(out, r.b)
		}
		return wrapStore(rows.Err(), "providerbudget: listing budgets failed")
	})
	return out, err
}

// applicableSQL reads the budgets that apply to a call, oldest first (applicableArgs): one branch per
// kind of subject, each naming every column of providerbudget_budgets_subject, so each is a seek of
// that index. The branches are disjoint (by kind), so UNION ALL returns each budget once. The empty
// principal_id the provider, tenant and workspace branches ask for is what the table's CHECKs already
// require of those kinds; it is spelled out for the index.
const applicableSQL = `SELECT ` + budgetCols + ` FROM (
		  SELECT seq, ` + budgetCols + ` FROM providerbudget_budgets
		   WHERE application_id = ? AND tenant_id = '' AND workspace_id = '' AND kind = 'provider'
		     AND principal_id = '' AND provider_id = ? AND channel = ?
		  UNION ALL
		  SELECT seq, ` + budgetCols + ` FROM providerbudget_budgets
		   WHERE application_id = ? AND tenant_id = ? AND workspace_id = '' AND kind = 'tenant'
		     AND principal_id = '' AND provider_id IN ('', ?) AND channel = ?
		  UNION ALL
		  SELECT seq, ` + budgetCols + ` FROM providerbudget_budgets
		   WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND kind = 'workspace'
		     AND principal_id = '' AND provider_id IN ('', ?) AND channel = ?
		  UNION ALL
		  SELECT seq, ` + budgetCols + ` FROM providerbudget_budgets
		   WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND kind = 'principal'
		     AND principal_id = ? AND provider_id IN ('', ?) AND channel = ?)
		 ORDER BY seq`

// applicableArgs are the arguments of applicableSQL.
func applicableArgs(scope contracts.Scope, provider providers.ProviderID, channel providers.Channel) []any {
	p, ch := string(provider), string(channel)
	return []any{
		scope.ApplicationID, p, ch,
		scope.ApplicationID, scope.TenantID, p, ch,
		scope.ApplicationID, scope.TenantID, scope.WorkspaceID, p, ch,
		scope.ApplicationID, scope.TenantID, scope.WorkspaceID, scope.PrincipalID, p, ch,
	}
}

// applicable returns the budgets that apply to a call of scope to provider on channel: the
// provider's, and the scope's tenant, workspace and principal budgets (each unnarrowed or narrowed
// to this provider). A budget of another application, tenant, workspace or principal never matches.
func applicable(ctx context.Context, q store.Reader, scope contracts.Scope, provider providers.ProviderID, channel providers.Channel) ([]budgetRow, error) {
	rows, err := q.QueryContext(ctx, applicableSQL, applicableArgs(scope, provider, channel)...)
	if err != nil {
		return nil, wrapStore(err, "providerbudget: reading budgets failed")
	}
	defer rows.Close()
	var out []budgetRow
	for rows.Next() {
		r, err := scanBudget(rows)
		if err != nil {
			return nil, wrapStore(err, "providerbudget: reading budgets failed")
		}
		out = append(out, r)
	}
	return out, wrapStore(rows.Err(), "providerbudget: reading budgets failed")
}
