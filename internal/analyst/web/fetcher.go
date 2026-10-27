package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// WebFetcher fetches one allow-listed search-hit URL for an analyst turn.
type WebFetcher interface {
	Fetch(ctx context.Context, turn *Turn, rawURL string) (FetchResult, error)
}

// Fetcher is the default WebFetcher with SSRF protection and size caps.
type Fetcher struct {
	Resolver Resolver
	Clock    clock.Clock
	IDs      *ids.Generator
	Timeout  time.Duration
	client   *safeClient
}

// NewFetcher builds a Fetcher. Nil clock/ids use defaults.
func NewFetcher(r Resolver, clk clock.Clock, gen *ids.Generator) *Fetcher {
	if clk == nil {
		clk = clock.Real{}
	}
	if gen == nil {
		gen = ids.NewGenerator(clk)
	}
	f := &Fetcher{Resolver: r, Clock: clk, IDs: gen}
	f.client = newSafeClient(r, f.Timeout)
	return f
}

// SetHTTPClientForTest replaces the SSRF-safe client (serve-turn E2E: dial a
// TLS fixture for an allow-listed public host). Production never calls this.
func (f *Fetcher) SetHTTPClientForTest(c *http.Client) {
	if f == nil || c == nil {
		return
	}
	f.client = &safeClient{client: c}
}

// Fetch retrieves page text for a URL that was returned by a prior search.
func (f *Fetcher) Fetch(ctx context.Context, turn *Turn, rawURL string) (FetchResult, error) {
	if f == nil {
		return FetchResult{}, contracts.New(contracts.ErrUnavailable, "web: fetcher is not wired")
	}
	u, err := validatePublicURL(rawURL, true)
	if err != nil {
		return FetchResult{}, err
	}
	norm := normalizeURL(u.String())
	if turn == nil || !turn.AllowedFetch(norm) {
		return FetchResult{}, contracts.New(contracts.ErrWebQueryRefused, "web: URL was not returned by a prior search in this turn").
			WithDetails("reason", "not_from_search")
	}
	if err := checkHostPolicy(turn.policy, norm); err != nil {
		return FetchResult{}, err
	}
	if _, err := resolvePublic(ctx, f.Resolver, u.Hostname()); err != nil {
		return FetchResult{}, err
	}
	if !turn.takeFetch() {
		return FetchResult{}, contracts.New(contracts.ErrAnalystCapExceeded, "web: fetch budget exhausted")
	}
	if f.client == nil {
		f.client = newSafeClient(f.Resolver, f.Timeout)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, norm, nil)
	if err != nil {
		return FetchResult{}, contracts.Wrap(contracts.ErrInternal, err, "web: cannot build request")
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,text/plain;q=0.8")
	req.Header.Set("User-Agent", "AlixAI-Analyst/1.5")
	resp, err := f.client.client.Do(req)
	if err != nil {
		return FetchResult{}, contracts.Wrap(contracts.ErrUnavailable, stripURL(err), "web: fetch failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return FetchResult{}, contracts.New(contracts.ErrUnavailable, "web: fetch returned a non-success status").
			WithDetails("http_status", resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, maxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return FetchResult{}, contracts.Wrap(contracts.ErrUnavailable, stripURL(err), "web: reading body failed")
	}
	if len(body) > maxBodyBytes {
		return FetchResult{}, contracts.New(contracts.ErrWebQueryRefused, "web: page exceeds size cap").
			WithDetails("reason", "too_large")
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	var title, text string
	switch {
	case strings.Contains(ct, "html") || strings.Contains(ct, "xml") || ct == "":
		title, text = ExtractText(string(body))
	default:
		title = "untitled"
		text = truncateRunes(collapseSpace(string(body)), maxExtractRunes)
	}
	if hit, ok := turn.hits[norm]; ok && title == "untitled" && hit.Title != "" {
		title = hit.Title
	}
	sum := sha256.Sum256([]byte(text))
	hash := hex.EncodeToString(sum[:])
	now := f.Clock.Now().UTC()
	id := f.IDs.New("wcit")
	snippet := text
	if utf8Len(snippet) > 500 {
		snippet = truncateRunes(snippet, 500)
	}
	hashRef := "sha256:" + hash
	cit := an.WebCitation{
		ID:          id,
		URL:         norm,
		Title:       title,
		RetrievedAt: common.Timestamp(now.Format(time.RFC3339Nano)),
		Snippet:     &snippet,
		ContentHash: &hashRef,
		SourceLabel: "web",
	}
	return FetchResult{
		URL:         norm,
		Title:       title,
		Text:        text,
		ContentHash: hash,
		RetrievedAt: now,
		Citation:    cit,
		Fenced:      fencePage(title, norm, text),
	}, nil
}

func utf8Len(s string) int { return len([]rune(s)) }

func stripURL(err error) error {
	if err == nil {
		return nil
	}
	// Avoid leaking query strings from net/http errors into typed wrappers.
	msg := err.Error()
	if i := strings.Index(msg, "http"); i >= 0 {
		return contracts.New(contracts.ErrUnavailable, "web: transport failure")
	}
	return err
}
