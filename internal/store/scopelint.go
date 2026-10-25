package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ScopeViolation is one table that breaks the "every table carries
// application_id, tenant_id and workspace_id" rule.
type ScopeViolation struct {
	Table   string
	Problem string
}

func (v ScopeViolation) String() string { return v.Table + ": " + v.Problem }

// DefaultScopeExempt lists the tables that legitimately hold no tenant data.
// Anything else needs the three scope columns.
var DefaultScopeExempt = []string{"schema_migrations"}

// CheckScopeColumns inspects the live schema and returns every table that
// lacks a scope column, or declares one that is not TEXT (and, for ordinary
// tables, NOT NULL). It covers ordinary and virtual (FTS5) tables; FTS5's own
// shadow tables, views, SQLite internals and the exempt names are skipped.
// DefaultScopeExempt is always exempt; pass more names for tables that
// legitimately hold only deployment-wide data. The result is sorted.
//
// Modules call it from their tests, usually through storetest.AssertScopedTables.
func CheckScopeColumns(ctx context.Context, q Reader, exempt ...string) ([]ScopeViolation, error) {
	skip := map[string]bool{}
	for _, n := range DefaultScopeExempt {
		skip[n] = true
	}
	for _, n := range exempt {
		skip[n] = true
	}

	rows, err := q.QueryContext(ctx,
		`SELECT name, type FROM pragma_table_list WHERE schema = 'main' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	type tbl struct{ name, kind string }
	var tables []tbl
	for rows.Next() {
		var t tbl
		if err := rows.Scan(&t.name, &t.kind); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var out []ScopeViolation
	for _, t := range tables {
		if skip[t.name] || (t.kind != "table" && t.kind != "virtual") {
			continue
		}
		cols := map[string]colInfo{}
		crs, err := q.QueryContext(ctx, `SELECT name, type, "notnull" FROM pragma_table_info(?)`, t.name)
		if err != nil {
			return nil, err
		}
		for crs.Next() {
			var name, typ string
			var notNull int
			if err := crs.Scan(&name, &typ, &notNull); err != nil {
				crs.Close()
				return nil, err
			}
			cols[strings.ToLower(name)] = colInfo{typ: strings.ToUpper(typ), notNull: notNull != 0}
		}
		if err := crs.Err(); err != nil {
			crs.Close()
			return nil, err
		}
		crs.Close()

		for _, want := range ScopeColumns() {
			ci, ok := cols[want]
			switch {
			case !ok:
				out = append(out, ScopeViolation{t.name, "missing column " + want})
			case ci.typ != "TEXT" && t.kind == "table":
				out = append(out, ScopeViolation{t.name, fmt.Sprintf("column %s must be TEXT, is %q", want, ci.typ)})
			case !ci.notNull && t.kind == "table":
				out = append(out, ScopeViolation{t.name, "column " + want + " must be NOT NULL"})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Problem < out[j].Problem
	})
	return out, nil
}

type colInfo struct {
	typ     string
	notNull bool
}
