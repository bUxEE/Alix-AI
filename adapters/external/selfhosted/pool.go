package selfhosted

import (
	"sync"
)

// Pool caches clients keyed by endpoint identity.
type Pool struct {
	mu    sync.Mutex
	items map[string]*Client
}

func NewPool() *Pool { return &Pool{items: map[string]*Client{}} }

func (p *Pool) Key(endpointID, baseURL, model, cred string) string {
	return endpointID + "|" + baseURL + "|" + model + "|" + cred
}

func (p *Pool) Get(key string) (*Client, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.items[key]
	return c, ok
}

func (p *Pool) Put(key string, c *Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items[key] = c
}
