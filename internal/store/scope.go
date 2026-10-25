package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// The three columns every table carries (master plan §10.2).
const (
	ColApplicationID = "application_id"
	ColTenantID      = "tenant_id"
	ColWorkspaceID   = "workspace_id"
)

// ScopeColumns returns the scope column names in the order the helpers bind
// their values: application, tenant, workspace.
func ScopeColumns() []string {
	return []string{ColApplicationID, ColTenantID, ColWorkspaceID}
}

var (
	identRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	whereRE = regexp.MustCompile(`(?i)\bwhere\b`)
)

// scopeValues validates the whole Scope and returns the bound values in
// ScopeColumns order. An invalid scope keeps its contracts code
// (scope_missing, scope_violation): there is no default tenant and no
// wildcard, and nothing is ever filled in.
func scopeValues(scope contracts.Scope) ([]any, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return []any{scope.ApplicationID, scope.TenantID, scope.WorkspaceID}, nil
}

// ScopedWhere returns the predicate that confines a query to the scope's
// application, tenant and workspace, with its arguments:
//
//	application_id = ? AND tenant_id = ? AND workspace_id = ?
//
// The scope arguments are positional and come first, so write it at the start
// of the WHERE clause and put other arguments after them. It returns the
// scope's contracts error (scope_missing or scope_violation) when any level of
// scope is absent or malformed.
func ScopedWhere(scope contracts.Scope) (pred string, args []any, err error) {
	return ScopedWhereAs(scope, "")
}

// ScopedWhereAs is ScopedWhere with a table alias qualifying each column, for
// joins. alias must be a plain identifier or empty.
func ScopedWhereAs(scope contracts.Scope, alias string) (pred string, args []any, err error) {
	prefix := ""
	if alias != "" {
		if !identRE.MatchString(alias) {
			return "", nil, contracts.Newf(contracts.ErrInternal, "store: invalid table alias %q", alias)
		}
		prefix = alias + "."
	}
	args, err = scopeValues(scope)
	if err != nil {
		return "", nil, err
	}
	cols := ScopeColumns()
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = prefix + c + " = ?"
	}
	return strings.Join(parts, " AND "), args, nil
}

// ScopedStmt describes a SELECT, UPDATE or DELETE whose WHERE clause always
// starts with the scope predicate. SQL, Where and Tail are constant text
// written by the module (never user input); values go in the argument slices.
//
//	ScopedStmt{
//		SQL:   "SELECT id, title FROM notes",
//		Where: "title LIKE ?", WhereArgs: []any{"a%"},
//		Tail:  "ORDER BY id LIMIT ?", TailArgs: []any{20},
//	}
//
// builds "SELECT id, title FROM notes WHERE application_id = ? AND tenant_id = ?
// AND workspace_id = ? AND (title LIKE ?) ORDER BY id LIMIT ?".
type ScopedStmt struct {
	// SQL is the statement up to, but not including, WHERE: "SELECT ... FROM t",
	// "UPDATE t SET a = ?", "DELETE FROM t". It must not contain WHERE.
	SQL string
	// Args are the arguments of SQL itself (for example SET values); they
	// precede the scope arguments.
	Args []any
	// Alias qualifies the scope columns when SQL joins several tables.
	Alias string
	// Where is an optional extra condition, wrapped in parentheses and ANDed
	// after the scope predicate.
	Where     string
	WhereArgs []any
	// Tail is optional trailing SQL (ORDER BY, LIMIT, RETURNING).
	Tail     string
	TailArgs []any
}

// Build returns the final SQL and arguments, or the scope's contracts error.
func (s ScopedStmt) Build(scope contracts.Scope) (string, []any, error) {
	if strings.TrimSpace(s.SQL) == "" {
		return "", nil, contracts.New(contracts.ErrInternal, "store: scoped statement has no SQL")
	}
	if whereRE.MatchString(s.SQL) {
		return "", nil, contracts.New(contracts.ErrInternal, "store: ScopedStmt.SQL must not contain WHERE; put extra conditions in ScopedStmt.Where")
	}
	pred, scopeArgs, err := ScopedWhereAs(scope, s.Alias)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	b.WriteString(s.SQL)
	b.WriteString(" WHERE ")
	b.WriteString(pred)
	if strings.TrimSpace(s.Where) != "" {
		b.WriteString(" AND (")
		b.WriteString(s.Where)
		b.WriteString(")")
	}
	if strings.TrimSpace(s.Tail) != "" {
		b.WriteString(" ")
		b.WriteString(s.Tail)
	}
	args := make([]any, 0, len(s.Args)+len(scopeArgs)+len(s.WhereArgs)+len(s.TailArgs))
	args = append(args, s.Args...)
	args = append(args, scopeArgs...)
	args = append(args, s.WhereArgs...)
	args = append(args, s.TailArgs...)
	return b.String(), args, nil
}

// QueryScoped runs a scoped SELECT. Nothing is executed when the scope is
// invalid.
func QueryScoped(ctx context.Context, q Reader, scope contracts.Scope, s ScopedStmt) (*sql.Rows, error) {
	query, args, err := s.Build(scope)
	if err != nil {
		return nil, err
	}
	return q.QueryContext(ctx, query, args...)
}

// QueryRowScoped runs a scoped single-row SELECT. The error is the scope or
// statement error; row errors (including sql.ErrNoRows) come from Scan.
func QueryRowScoped(ctx context.Context, q Reader, scope contracts.Scope, s ScopedStmt) (*sql.Row, error) {
	query, args, err := s.Build(scope)
	if err != nil {
		return nil, err
	}
	return q.QueryRowContext(ctx, query, args...), nil
}

// ExecScoped runs a scoped UPDATE or DELETE. Nothing is executed when the
// scope is invalid.
func ExecScoped(ctx context.Context, q Querier, scope contracts.Scope, s ScopedStmt) (sql.Result, error) {
	query, args, err := s.Build(scope)
	if err != nil {
		return nil, err
	}
	return q.ExecContext(ctx, query, args...)
}

// InsertScoped inserts one row with the scope columns filled from scope.
// table and cols are identifiers, cols must not name a scope column (the
// scope cannot be overridden), and vals line up with cols.
func InsertScoped(ctx context.Context, q Querier, scope contracts.Scope, table string, cols []string, vals ...any) (sql.Result, error) {
	if !identRE.MatchString(table) {
		return nil, contracts.Newf(contracts.ErrInternal, "store: invalid table name %q", table)
	}
	if len(cols) != len(vals) {
		return nil, contracts.Newf(contracts.ErrInternal, "store: InsertScoped has %d columns but %d values", len(cols), len(vals))
	}
	scopeArgs, err := scopeValues(scope)
	if err != nil {
		return nil, err
	}
	all := ScopeColumns()
	for _, c := range cols {
		if !identRE.MatchString(c) {
			return nil, contracts.Newf(contracts.ErrInternal, "store: invalid column name %q", c)
		}
		for _, sc := range all {
			if strings.EqualFold(c, sc) {
				return nil, contracts.Newf(contracts.ErrInternal, "store: InsertScoped fills %s from the scope; do not pass it", c)
			}
		}
	}
	all = append(all, cols...)
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(all)), ", ")
	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(all, ", "), marks)
	return q.ExecContext(ctx, query, append(scopeArgs, vals...)...)
}
