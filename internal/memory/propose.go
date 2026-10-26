package memory

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

// ProposalDraft is a schema-validated assistant proposal (never free text).
type ProposalDraft struct {
	Text       string
	Structured map[string]string
	DataClass  string // empty → internal
}

// Propose stores a proposed item from the dialogue engine. Capability is not
// required (the engine acts for the principal); per-turn and per-day limits apply.
// Callers must have already schema-validated the draft.
func (s *Service) Propose(ctx context.Context, scope contracts.Scope, draft ProposalDraft) (mem.MemoryItem, error) {
	if err := scope.Validate(); err != nil {
		return mem.MemoryItem{}, err
	}
	if err := validateText(draft.Text); err != nil {
		return mem.MemoryItem{}, err
	}
	structured, err := encodeStructured(draft.Structured)
	if err != nil {
		return mem.MemoryItem{}, err
	}
	class := mem.MemoryItemDataClassInternal
	if draft.DataClass != "" {
		c := mem.MemoryItemDataClass(draft.DataClass)
		if !c.Valid() {
			return mem.MemoryItem{}, contracts.New(contracts.ErrValidationFailed, "memory: data_class is not valid")
		}
		class = c
	}
	n, err := s.countPrincipal(ctx, scope)
	if err != nil {
		return mem.MemoryItem{}, err
	}
	if n >= s.cfg.MaxItemsPerScope {
		return mem.MemoryItem{}, contracts.Newf(contracts.ErrValidationFailed, "memory: at most %d items per scope", s.cfg.MaxItemsPerScope)
	}
	dayStart := s.clk.Now().UTC().Truncate(24 * time.Hour)
	proposedToday, err := s.countProposedSince(ctx, scope, dayStart)
	if err != nil {
		return mem.MemoryItem{}, err
	}
	if proposedToday >= s.cfg.MaxProposalsPerDay {
		return mem.MemoryItem{}, contracts.Newf(contracts.ErrValidationFailed, "memory: at most %d proposals per day", s.cfg.MaxProposalsPerDay)
	}
	now := s.clk.Now().UTC()
	exp := now.Add(s.cfg.ProposalTTL)
	id := s.ids.New(IDPrefix)
	if err := s.audit(ctx, scope, EventProposed, map[string]any{
		"id": id, "origin": string(mem.MemoryOriginAssistantProposed), "status": string(mem.MemoryStatusProposed), "data_class": string(class),
	}); err != nil {
		return mem.MemoryItem{}, err
	}
	_, err = store.InsertScoped(ctx, s.db, scope, "memory_items",
		[]string{"id", "principal_id", "text", "structured_json", "origin", "status", "data_class", "expires_at", "created_at", "updated_at"},
		id, scope.PrincipalID, draft.Text, structured,
		string(mem.MemoryOriginAssistantProposed), string(mem.MemoryStatusProposed), string(class),
		toMS(exp), toMS(now), toMS(now),
	)
	if err != nil {
		return mem.MemoryItem{}, wrapDB(err)
	}
	r, err := s.get(ctx, scope, id)
	if err != nil {
		return mem.MemoryItem{}, err
	}
	return r.item(), nil
}

func (s *Service) countProposedSince(ctx context.Context, scope contracts.Scope, since time.Time) (int, error) {
	qr, err := store.QueryRowScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL:       "SELECT COUNT(*) FROM memory_items",
		Where:     "principal_id = ? AND origin = ? AND created_at >= ?",
		WhereArgs: []any{scope.PrincipalID, string(mem.MemoryOriginAssistantProposed), toMS(since)},
	})
	if err != nil {
		return 0, wrapDB(err)
	}
	var n int
	if err := qr.Scan(&n); err != nil {
		return 0, wrapDB(err)
	}
	return n, nil
}

// MaxProposalsPerTurn returns the configured per-turn limit.
func (s *Service) MaxProposalsPerTurn() int { return s.cfg.MaxProposalsPerTurn }

// ValidateProposalDraft checks bounds without writing (for schema-adjacent field checks).
func ValidateProposalDraft(text string, structured map[string]string, dataClass string) error {
	if err := validateText(strings.TrimSpace(text)); err != nil {
		return err
	}
	if utf8.RuneCountInString(text) > MaxTextLen {
		return contracts.Newf(contracts.ErrValidationFailed, "memory: text must be at most %d characters", MaxTextLen)
	}
	if _, err := encodeStructured(structured); err != nil {
		return err
	}
	if dataClass != "" && !mem.MemoryItemDataClass(dataClass).Valid() {
		return contracts.New(contracts.ErrValidationFailed, "memory: data_class is not valid")
	}
	return nil
}
