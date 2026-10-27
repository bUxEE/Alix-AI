package selfhosted

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// LifecycleSetter updates endpoint lifecycle after health probes (ADR 0045).
type LifecycleSetter func(ctx context.Context, ready bool) error

// Checker probes health_url or baseURL+/v1/models on an interval with backoff.
type Checker struct {
	HTTP       *http.Client
	Interval   time.Duration
	Backoff    time.Duration
	MaxBackoff time.Duration
	Probe      func(ctx context.Context, url string) error
	Set        LifecycleSetter
}

func (c *Checker) Run(ctx context.Context, healthURL, baseURL string) {
	if c == nil || c.Set == nil {
		return
	}
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	if c.Backoff <= 0 {
		c.Backoff = 5 * time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 5 * time.Minute
	}
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}
	if c.Probe == nil {
		c.Probe = defaultProbe(c.HTTP)
	}
	url := strings.TrimSuffix(healthURL, "/")
	if url == "" {
		url = strings.TrimSuffix(baseURL, "/") + "/v1/models"
	}
	wait := c.Interval
	for {
		err := c.Probe(ctx, url)
		_ = c.Set(ctx, err == nil)
		if err != nil {
			wait = min(wait*2, c.MaxBackoff)
		} else {
			wait = c.Interval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func defaultProbe(hc *http.Client) func(context.Context, string) error {
	return func(ctx context.Context, url string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		return http.ErrBodyReadAfterClose
	}
}
