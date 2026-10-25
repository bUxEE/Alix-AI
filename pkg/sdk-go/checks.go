package sdkgo

import (
	"fmt"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// The generated types decode enums and unions strictly, but a struct decodes leniently: a member the document
// requires and the body lacks would become a zero value. These checks refuse a success body that lacks the
// members its document cannot do without, so a 200 that is not what the contract says is never read as a result.

func checkPrepared(p *actions.PreparedAction) error {
	if p.PreparationID == "" || p.CapabilityID == "" || p.State == "" || p.Summary == "" || p.ExpiresAt == "" || p.IdempotencyKey == "" {
		return fmt.Errorf("prepared action: preparation_id, capability_id, state, summary, expires_at and idempotency_key are required")
	}
	return nil
}

func checkReceipt(r *actions.ActionReceipt) error {
	if r.ReceiptID == "" || r.PreparationID == "" || r.Status == "" || r.AuditRef == "" || r.IssuedAt == "" {
		return fmt.Errorf("receipt: receipt_id, preparation_id, status, audit_ref and issued_at are required")
	}
	return nil
}

func checkGeneration(g *rag.IndexGeneration) error {
	if g.GenerationID == "" || g.SourceID == "" || g.State == "" {
		return fmt.Errorf("index generation: generation_id, source_id and state are required")
	}
	return nil
}

func checkSearch(s *rag.SearchResponse) error {
	for _, h := range s.Results {
		if h.SourceID == "" || h.DisplayName == "" || h.Excerpt == "" {
			return fmt.Errorf("search hit: source_id, display_name and excerpt are required")
		}
	}
	return nil
}

// preparedID is the preparation a prepared_action event carries, or "".
func preparedID(ev conversation.TurnEvent) string {
	if pa, ok := ev.Value.(conversation.PreparedActionEvent); ok {
		return string(pa.PreparedAction.PreparationID)
	}
	return ""
}
