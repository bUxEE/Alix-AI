package sdkgo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

// MemoryClient is long-term memory (ADR 0042, 1.2.0).
type MemoryClient struct{ c *Client }

// Memory returns the memory client.
func (c *Client) Memory() *MemoryClient { return &MemoryClient{c: c} }

// MemoryListOptions are optional filters for List.
type MemoryListOptions struct {
	Q      string
	Status string
	Cursor string
	Limit  int
}

func (m *MemoryClient) List(ctx context.Context, listOpts *MemoryListOptions, options ...CallOption) (*mem.MemoryList, error) {
	query := url.Values{}
	if listOpts != nil {
		if listOpts.Q != "" {
			query.Set("q", listOpts.Q)
		}
		if listOpts.Status != "" {
			query.Set("status", listOpts.Status)
		}
		if listOpts.Cursor != "" {
			query.Set("cursor", listOpts.Cursor)
		}
		if listOpts.Limit > 0 {
			query.Set("limit", fmt.Sprintf("%d", listOpts.Limit))
		}
	}
	req := request{op: opListMemory, query: query, accept: "application/json"}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), checkMemoryList)
}

func (m *MemoryClient) Create(ctx context.Context, body mem.MemoryCreate, options ...CallOption) (*mem.MemoryItem, error) {
	if body.Text == "" {
		return nil, invalidArgument("text", "text is required")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req := request{op: opCreateMemory, accept: "application/json", contentType: "application/json", body: raw}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), checkMemoryItem)
}

func (m *MemoryClient) Patch(ctx context.Context, id string, body mem.MemoryPatch, options ...CallOption) (*mem.MemoryItem, error) {
	if err := pathID("id", id); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req := request{op: opPatchMemory, params: map[string]string{"id": id}, accept: "application/json", contentType: "application/json", body: raw}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), checkMemoryItem)
}

func (m *MemoryClient) Delete(ctx context.Context, id string, options ...CallOption) error {
	if err := pathID("id", id); err != nil {
		return err
	}
	req := request{op: opDeleteMemory, params: map[string]string{"id": id}}
	return doEmpty(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout))
}

func (m *MemoryClient) Confirm(ctx context.Context, id string, options ...CallOption) (*mem.MemoryItem, error) {
	if err := pathID("id", id); err != nil {
		return nil, err
	}
	req := request{op: opConfirmMemory, params: map[string]string{"id": id}, accept: "application/json"}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), checkMemoryItem)
}

func (m *MemoryClient) Reject(ctx context.Context, id string, options ...CallOption) (*mem.MemoryItem, error) {
	if err := pathID("id", id); err != nil {
		return nil, err
	}
	req := request{op: opRejectMemory, params: map[string]string{"id": id}, accept: "application/json"}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), checkMemoryItem)
}

func checkMemoryList(l *mem.MemoryList) error {
	if l.Items == nil {
		return fmt.Errorf("memory list: items must be present")
	}
	return nil
}

func checkMemoryItem(it *mem.MemoryItem) error {
	if it.ID == "" || it.Text == "" {
		return fmt.Errorf("memory item: id and text are required")
	}
	return nil
}
