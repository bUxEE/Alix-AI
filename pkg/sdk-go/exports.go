package sdkgo

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

var (
	exportTurnID  = regexp.MustCompile(`^trn_[0-9A-HJKMNP-TV-Z]{26}$`)
	exportBlockID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

// ExportsClient generates downloadable files from a turn's verified rich blocks (1.2.0).
type ExportsClient struct{ c *Client }

// Exports returns the exports client.
func (c *Client) Exports() *ExportsClient { return &ExportsClient{c: c} }

// Create is POST /v1/sessions/{session_id}/exports. Needs exports.create in the context token.
// The returned FileRef has origin generated; download via the files content route.
func (e *ExportsClient) Create(ctx context.Context, sessionID string, body conversation.ExportRequest, options ...CallOption) (*conversation.FileRef, error) {
	if err := pathID("sessionID", sessionID); err != nil {
		return nil, err
	}
	if !exportTurnID.MatchString(string(body.TurnID)) {
		return nil, invalidArgument("turn_id", "it is not a well-formed turn id")
	}
	if !body.Format.Valid() {
		return nil, invalidArgument("format", "it must be csv, xlsx, html or md")
	}
	if len(body.BlockIDs) > 64 {
		return nil, invalidArgument("block_ids", "at most 64 ids")
	}
	seen := map[string]struct{}{}
	for _, id := range body.BlockIDs {
		if !exportBlockID.MatchString(id) {
			return nil, invalidArgument("block_ids", "each id must be 1 to 128 safe characters")
		}
		if _, ok := seen[id]; ok {
			return nil, invalidArgument("block_ids", "ids must be unique")
		}
		seen[id] = struct{}{}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req := request{
		op:          opCreateExport,
		params:      map[string]string{"session_id": sessionID},
		accept:      "application/json",
		contentType: "application/json",
		body:        raw,
	}
	return doJSON(ctx, e.c.t, req, applyOptions(options).deadline(e.c.timeout), checkFileRef)
}

func checkFileRef(r *conversation.FileRef) error {
	if r.FileID == "" || r.Name == "" || r.MimeType == "" {
		return fmt.Errorf("file ref: missing required fields")
	}
	if !r.Origin.Valid() {
		return fmt.Errorf("file ref: origin is unknown")
	}
	if len(r.Permissions) == 0 {
		return fmt.Errorf("file ref: permissions are required")
	}
	return nil
}
