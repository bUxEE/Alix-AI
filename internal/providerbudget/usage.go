package providerbudget

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// UsageFilter selects usage rows. From and To (To exclusive, each optional) select by the UTC day the
// spend was CHARGED to, which is the day the call was reserved: From is taken down to the start of its
// day. By default a caller sees only the usage of its own tenant and workspace; TenantWide widens it to
// every workspace of its tenant (a tenant administrator's export) and ApplicationWide to the whole
// application; both are operator functions the gateway must gate by role. The other fields narrow
// further.
type UsageFilter struct {
	From, To        time.Time
	TenantWide      bool
	ApplicationWide bool
	ProviderID      providers.ProviderID
	Channel         providers.Channel
	TenantID        string
	WorkspaceID     string
	PrincipalID     string
}

// UsageRow is one exported call.
type UsageRow struct {
	Record providers.UsageRecord `json:"record"`
	// ChargedMicroUnits is what the budgets were charged: the actual cost, or the reserved upper
	// bound when the actual is not known (an uncertain call).
	ChargedMicroUnits providers.MicroUnits `json:"charged_micro_units"`
	// ChargedDay is the UTC day the spend counts in.
	ChargedDay string `json:"charged_day"`
}

// UsageTotal sums the rows of one provider, channel and currency.
type UsageTotal struct {
	ProviderID providers.ProviderID `json:"provider_id"`
	Channel    providers.Channel    `json:"channel"`
	Currency   providers.Currency   `json:"currency"`
	Records    int64                `json:"records"`
	Uncertain  int64                `json:"uncertain"`
	// ReservedMicroUnits is the sum of the estimates, ActualMicroUnits the sum of the known actuals
	// and ChargedMicroUnits the sum of what the budgets were charged.
	ReservedMicroUnits providers.MicroUnits `json:"reserved_micro_units"`
	ActualMicroUnits   providers.MicroUnits `json:"actual_micro_units"`
	ChargedMicroUnits  providers.MicroUnits `json:"charged_micro_units"`
}

// UsageExport is the usage of a period with its totals.
type UsageExport struct {
	Rows   []UsageRow   `json:"rows"`
	Totals []UsageTotal `json:"totals"`
}

func (f UsageFilter) validate() error {
	if f.Channel != "" && !f.Channel.Valid() {
		return invalid("bad_filter", "providerbudget: the channel filter is not valid")
	}
	if !f.From.IsZero() && !f.To.IsZero() && !f.To.After(f.From) {
		return invalid("bad_filter", "providerbudget: the period ends before it starts")
	}
	return nil
}

// where builds the WHERE clause of usage queries. The default confines the rows to the caller's
// tenant and workspace, TenantWide to the caller's tenant; neither ever reaches another tenant.
func (f UsageFilter) where(scope contracts.Scope) (string, []any, error) {
	if err := f.validate(); err != nil {
		return "", nil, err
	}
	var parts []string
	var args []any
	switch {
	case f.ApplicationWide:
		parts, args = append(parts, "application_id = ?"), append(args, scope.ApplicationID)
	case f.TenantWide:
		if f.TenantID != "" && f.TenantID != scope.TenantID {
			return "", nil, contracts.New(contracts.ErrScopeViolation, "providerbudget: the usage of another tenant needs an application-wide export")
		}
		parts, args = append(parts, "application_id = ? AND tenant_id = ?"), append(args, scope.ApplicationID, scope.TenantID)
	default:
		pred, a, err := store.ScopedWhere(scope)
		if err != nil {
			return "", nil, err
		}
		parts, args = append(parts, pred), append(args, a...)
		if f.TenantID != "" && f.TenantID != scope.TenantID || f.WorkspaceID != "" && f.WorkspaceID != scope.WorkspaceID {
			return "", nil, contracts.New(contracts.ErrScopeViolation, "providerbudget: the usage of another tenant or workspace needs an application-wide export")
		}
	}
	add := func(col, v string) {
		if v != "" {
			parts, args = append(parts, col+" = ?"), append(args, v)
		}
	}
	add("provider_id", string(f.ProviderID))
	add("channel", string(f.Channel))
	if f.ApplicationWide {
		add("tenant_id", f.TenantID)
	}
	if f.ApplicationWide || f.TenantWide {
		add("workspace_id", f.WorkspaceID)
	}
	add("principal_id", f.PrincipalID)
	if !f.From.IsZero() {
		parts, args = append(parts, "charged_day >= ?"), append(args, dayWindow(f.From).start.UnixMilli())
	}
	if !f.To.IsZero() {
		parts, args = append(parts, "charged_day < ?"), append(args, f.To.UTC().UnixMilli())
	}
	return strings.Join(parts, " AND "), args, nil
}

// ExportUsage returns the usage records of the filter, oldest first, with totals per provider,
// channel and currency. Records hold no prompt and no response.
func (s *Service) ExportUsage(ctx context.Context, scope contracts.Scope, f UsageFilter) (UsageExport, error) {
	if err := scope.Validate(); err != nil {
		return UsageExport{}, err
	}
	where, args, err := f.where(scope)
	if err != nil {
		return UsageExport{}, err
	}
	out := UsageExport{Rows: []UsageRow{}, Totals: []UsageTotal{}}
	err = s.db.View(ctx, func(tx *store.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT document, charged_micro, charged_day FROM providerbudget_usage WHERE `+where+` ORDER BY seq`, args...)
		if err != nil {
			return wrapStore(err, "providerbudget: reading usage failed")
		}
		defer rows.Close()
		for rows.Next() {
			var doc string
			var charged, day int64
			if err := rows.Scan(&doc, &charged, &day); err != nil {
				return wrapStore(err, "providerbudget: reading usage failed")
			}
			var rec providers.UsageRecord
			dec := json.NewDecoder(bytes.NewReader([]byte(doc)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&rec); err != nil {
				return contracts.Wrap(contracts.ErrInternal, err, "providerbudget: a stored usage record cannot be decoded")
			}
			out.Rows = append(out.Rows, UsageRow{
				Record: rec, ChargedMicroUnits: providers.MicroUnits(charged), ChargedDay: fromMillis(day).Format("2006-01-02"),
			})
		}
		return wrapStore(rows.Err(), "providerbudget: reading usage failed")
	})
	if err != nil {
		return UsageExport{}, err
	}
	out.Totals = totals(out.Rows)
	return out, nil
}

func totals(rows []UsageRow) []UsageTotal {
	type key struct {
		p providers.ProviderID
		c providers.Channel
		m providers.Currency
	}
	idx := map[key]*UsageTotal{}
	var keys []key
	for _, r := range rows {
		k := key{r.Record.Provider.ProviderID, r.Record.Channel, r.Record.Cost.Currency}
		t := idx[k]
		if t == nil {
			t = &UsageTotal{ProviderID: k.p, Channel: k.c, Currency: k.m}
			idx[k] = t
			keys = append(keys, k)
		}
		t.Records++
		if r.Record.Outcome == providers.UsageRecordOutcomeUncertain {
			t.Uncertain++
		}
		t.ReservedMicroUnits += r.Record.Cost.ReservedMicroUnits
		if a := r.Record.Cost.ActualMicroUnits; a != nil {
			t.ActualMicroUnits += *a
		}
		t.ChargedMicroUnits += r.ChargedMicroUnits
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.p != b.p {
			return a.p < b.p
		}
		if a.c != b.c {
			return a.c < b.c
		}
		return a.m < b.m
	})
	out := make([]UsageTotal, 0, len(keys))
	for _, k := range keys {
		out = append(out, *idx[k])
	}
	return out
}

// WriteJSON writes the export as one JSON document.
func (e UsageExport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(e)
}

// csvHeader is the fixed column order of WriteCSV.
var csvHeader = []string{
	"usage_id", "recorded_at", "charged_day", "application_id", "tenant_id", "workspace_id", "principal_id",
	"provider_id", "profile_id", "model_id", "model_version", "role", "channel", "reservation_id", "outcome",
	"error_kind", "metering_basis", "toks_in", "toks_out", "toks_in_cached", "images_in", "images_out",
	"audio_seconds_in", "audio_seconds_out", "file_bytes_in", "requests", "currency", "reserved_micro_units",
	"actual_micro_units", "cost_basis", "charged_micro_units", "total_ms",
}

// csvCell neutralises a cell a spreadsheet would run as a formula.
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// WriteCSV writes the rows as CSV, one line per call, money as integer micro-units, an unknown
// actual as an empty cell.
func (e UsageExport) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return err
	}
	n := func(v int64) string { return strconv.FormatInt(v, 10) }
	for _, r := range e.Rows {
		rec := r.Record
		errKind, actual, rsv := "", "", ""
		if rec.ErrorKind != nil {
			errKind = string(*rec.ErrorKind)
		}
		if rec.Cost.ActualMicroUnits != nil {
			actual = n(int64(*rec.Cost.ActualMicroUnits))
		}
		if rec.ReservationID != nil {
			rsv = string(*rec.ReservationID)
		}
		m := rec.Metering
		line := []string{
			string(rec.UsageID), string(rec.RecordedAt), r.ChargedDay,
			string(rec.Subject.ApplicationID), string(rec.Subject.TenantID), string(rec.Subject.WorkspaceID), string(rec.Subject.PrincipalID),
			string(rec.Provider.ProviderID), string(rec.Provider.ProfileID), string(rec.Provider.ModelID), string(rec.Provider.ModelVersion),
			string(rec.Role), string(rec.Channel), rsv, string(rec.Outcome), errKind, string(rec.MeteringBasis),
			n(int64(m.ToksIn)), n(int64(m.ToksOut)), n(int64(m.ToksInCached)), n(int64(m.ImagesIn)), n(int64(m.ImagesOut)),
			n(int64(m.AudioSecondsIn)), n(int64(m.AudioSecondsOut)), n(int64(m.FileBytesIn)), n(int64(m.Requests)),
			string(rec.Cost.Currency), n(int64(rec.Cost.ReservedMicroUnits)), actual, string(rec.Cost.Basis),
			n(int64(r.ChargedMicroUnits)), n(int64(rec.Latency.TotalMS)),
		}
		for i := range line {
			line[i] = csvCell(line[i])
		}
		if err := cw.Write(line); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// Invoice is a provider's own figure for a period, to reconcile against.
type Invoice struct {
	ProviderID providers.ProviderID
	Currency   providers.Currency
	MicroUnits providers.MicroUnits
}

// ReconcileRow compares, for one provider and currency, what the usage records say with the settled
// spend in the budget counters and with the provider's invoice.
type ReconcileRow struct {
	ProviderID providers.ProviderID `json:"provider_id"`
	Currency   providers.Currency   `json:"currency"`
	Records    int64                `json:"records"`
	Uncertain  int64                `json:"uncertain"`
	// ReservedMicroUnits is the sum of estimates and ChargedMicroUnits the sum charged; the ratio
	// shows how much the upper bound over-reserves.
	ReservedMicroUnits providers.MicroUnits `json:"reserved_micro_units"`
	ChargedMicroUnits  providers.MicroUnits `json:"charged_micro_units"`
	// LedgerMicroUnits is the spend in the counters of the provider's budgets for the same days,
	// nil when no provider budget exists. It is only computed for an application-wide period with
	// both ends set. LedgerMatches is true when it equals the charged sum.
	LedgerMicroUnits *providers.MicroUnits `json:"ledger_micro_units"`
	LedgerMatches    *bool                 `json:"ledger_matches"`
	// InvoiceMicroUnits is the provider's figure when given and DifferenceMicroUnits is charged less
	// invoice (positive: AlixAI charged more than the invoice).
	InvoiceMicroUnits    *providers.MicroUnits `json:"invoice_micro_units"`
	DifferenceMicroUnits *int64                `json:"difference_micro_units"`
}

// Reconciliation is the result of Reconcile.
type Reconciliation struct {
	Rows []ReconcileRow `json:"rows"`
}

// Reconcile compares the usage records of the filter with the budget counters and with the invoice
// figures, per provider and currency. AlixAI's number remains an estimate until it matches an
// invoice (ADR-0025). The counter comparison needs an application-wide filter with From and To.
func (s *Service) Reconcile(ctx context.Context, scope contracts.Scope, f UsageFilter, invoices []Invoice) (Reconciliation, error) {
	exp, err := s.ExportUsage(ctx, scope, f)
	if err != nil {
		return Reconciliation{}, err
	}
	type key struct {
		p providers.ProviderID
		c providers.Currency
	}
	idx := map[key]*ReconcileRow{}
	var keys []key
	get := func(p providers.ProviderID, c providers.Currency) *ReconcileRow {
		k := key{p, c}
		if idx[k] == nil {
			idx[k] = &ReconcileRow{ProviderID: p, Currency: c}
			keys = append(keys, k)
		}
		return idx[k]
	}
	for _, t := range exp.Totals {
		r := get(t.ProviderID, t.Currency)
		r.Records += t.Records
		r.Uncertain += t.Uncertain
		r.ReservedMicroUnits += t.ReservedMicroUnits
		r.ChargedMicroUnits += t.ChargedMicroUnits
	}
	for _, inv := range invoices {
		r := get(inv.ProviderID, inv.Currency)
		v := inv.MicroUnits
		r.InvoiceMicroUnits = &v
	}
	if f.ApplicationWide && !f.From.IsZero() && !f.To.IsZero() && f.TenantID == "" && f.WorkspaceID == "" && f.PrincipalID == "" {
		for _, k := range keys {
			l, ok, err := s.ledger(ctx, scope, f, k.p, k.c)
			if err != nil {
				return Reconciliation{}, err
			}
			if ok {
				r := idx[k]
				match := l == r.ChargedMicroUnits
				r.LedgerMicroUnits, r.LedgerMatches = &l, &match
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].p != keys[j].p {
			return keys[i].p < keys[j].p
		}
		return keys[i].c < keys[j].c
	})
	out := Reconciliation{Rows: make([]ReconcileRow, 0, len(keys))}
	for _, k := range keys {
		r := idx[k]
		if r.InvoiceMicroUnits != nil {
			d := int64(r.ChargedMicroUnits) - int64(*r.InvoiceMicroUnits)
			r.DifferenceMicroUnits = &d
		}
		out.Rows = append(out.Rows, *r)
	}
	return out, nil
}

// ledger sums the daily cost counters of the provider's budgets over the days of the period. A
// provider budget is charged once per call, so with the filter's channel (or all channels) it equals
// the sum of the charged usage of that provider.
func (s *Service) ledger(ctx context.Context, scope contracts.Scope, f UsageFilter, p providers.ProviderID, cur providers.Currency) (providers.MicroUnits, bool, error) {
	var total int64
	var n int64
	err := s.db.View(ctx, func(tx *store.Tx) error {
		q := `SELECT COUNT(DISTINCT b.budget_id), COALESCE(SUM(c.used), 0)
		        FROM providerbudget_budgets b
		        LEFT JOIN providerbudget_counters c
		          ON c.application_id = b.application_id AND c.tenant_id = b.tenant_id AND c.workspace_id = b.workspace_id
		         AND c.budget_id = b.budget_id AND c.dimension = 'cost' AND c.window_kind = 'daily'
		         AND c.window_start >= ? AND c.window_start < ?
		       WHERE b.application_id = ? AND b.tenant_id = '' AND b.workspace_id = '' AND b.kind = 'provider'
		         AND b.provider_id = ? AND b.currency = ?`
		args := []any{dayWindow(f.From).start.UnixMilli(), f.To.UTC().UnixMilli(), scope.ApplicationID, string(p), string(cur)}
		if f.Channel != "" {
			q += " AND b.channel = ?"
			args = append(args, string(f.Channel))
		}
		return wrapStore(tx.QueryRowContext(ctx, q, args...).Scan(&n, &total), "providerbudget: reading the ledger failed")
	})
	return providers.MicroUnits(total), n > 0, err
}
