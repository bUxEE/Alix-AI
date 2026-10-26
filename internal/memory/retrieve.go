package memory

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

// Retrieved is one active item for context assembly, with ranking metadata.
type Retrieved struct {
	Item  mem.MemoryItem
	Score int
}

// RetrieveActive returns up to MaxRetrieve active, non-expired items for the
// exact scope, ranked by lexical match to query plus recency. No capability
// check: the dialogue engine calls this for the verified turn scope.
func (s *Service) RetrieveActive(ctx context.Context, scope contracts.Scope, query string) ([]Retrieved, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	now := toMS(s.clk.Now().UTC())
	rows, err := store.QueryScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL:       "SELECT " + itemCols + " FROM memory_items",
		Where:     "principal_id = ? AND status = ? AND (expires_at IS NULL OR expires_at > ?)",
		WhereArgs: []any{scope.PrincipalID, string(mem.MemoryStatusActive), now},
		Tail:      "ORDER BY updated_at DESC LIMIT ?", TailArgs: []any{s.cfg.MaxItemsPerScope},
	})
	if err != nil {
		return nil, wrapDB(err)
	}
	defer rows.Close()
	terms := tokenize(query)
	var out []Retrieved
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, wrapDB(err)
		}
		it := r.item()
		out = append(out, Retrieved{Item: it, Score: rank(it, terms, r.updated)})
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDB(err)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return string(out[i].Item.UpdatedAt) > string(out[j].Item.UpdatedAt)
	})
	if len(out) > s.cfg.MaxRetrieve {
		out = out[:s.cfg.MaxRetrieve]
	}
	return out, nil
}

func tokenize(s string) []string {
	s = strings.ToLower(s)
	var b strings.Builder
	var out []string
	flush := func() {
		if b.Len() >= 2 {
			out = append(out, b.String())
		}
		b.Reset()
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func rank(it mem.MemoryItem, terms []string, updatedMS int64) int {
	score := int(updatedMS / 1_000_000) // coarse recency bump
	if len(terms) == 0 {
		return score
	}
	hay := strings.ToLower(string(it.Text))
	for _, k := range it.Structured {
		hay += " " + strings.ToLower(k)
	}
	for _, t := range terms {
		if strings.Contains(hay, t) {
			score += 1000
		}
	}
	return score
}
