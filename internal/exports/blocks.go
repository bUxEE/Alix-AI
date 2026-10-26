package exports

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// IdentifiedBlock is one verified rich block with the id the host / AG-UI kits use.
type IdentifiedBlock struct {
	// ID is act_<turn>_bN (1-based) or a stable index string.
	ID    string
	Index int // 1-based
	Block conversation.RichBlock
}

// BlockID builds the AG-UI activity id for the Nth block of a turn (N >= 1).
func BlockID(turnID string, n int) string {
	return fmt.Sprintf("act_%s_b%d", turnID, n)
}

// selectBlocks keeps the blocks named by ids. Empty ids means all. An unknown id is an error.
func selectBlocks(all []IdentifiedBlock, ids []string) ([]IdentifiedBlock, error) {
	if len(ids) == 0 {
		return all, nil
	}
	byID := map[string]IdentifiedBlock{}
	for _, b := range all {
		byID[b.ID] = b
		byID[strconv.Itoa(b.Index)] = b
		byID["b"+strconv.Itoa(b.Index)] = b
	}
	out := make([]IdentifiedBlock, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		b, ok := byID[id]
		if !ok {
			return nil, notFound("block", "the block is not in this turn")
		}
		if seen[b.ID] {
			continue
		}
		seen[b.ID] = true
		out = append(out, b)
	}
	return out, nil
}

func identify(turnID string, blocks []conversation.RichBlock) []IdentifiedBlock {
	out := make([]IdentifiedBlock, len(blocks))
	for i, b := range blocks {
		n := i + 1
		out[i] = IdentifiedBlock{ID: BlockID(turnID, n), Index: n, Block: b}
	}
	return out
}
