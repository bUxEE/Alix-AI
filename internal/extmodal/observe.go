package extmodal

import (
	"sync"
	"time"
)

// Totals are the counters of one modality at one provider.
type Totals struct {
	Calls         int
	Denied        int
	BytesSent     int64
	BytesReceived int64
	Latency       time.Duration
}

// Meter is an Observer that sums events per modality and provider, for the efficiency record. It
// holds numbers only.
type Meter struct {
	mu sync.Mutex
	m  map[string]*Totals
}

// NewMeter returns an empty Meter.
func NewMeter() *Meter { return &Meter{m: map[string]*Totals{}} }

// Observe implements Observer. Admission events (no bytes, no route) count as denials when refused;
// call events (a route is set) count as calls.
func (m *Meter) Observe(e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := string(e.Modality) + "/" + e.Provider
	t := m.m[k]
	if t == nil {
		t = &Totals{}
		m.m[k] = t
	}
	if !e.Allowed {
		t.Denied++
		return
	}
	if e.Route != "" {
		t.Calls++
		t.BytesSent += e.BytesSent
		t.BytesReceived += e.BytesReceived
		t.Latency += e.Latency
	}
}

// Totals returns the counters of a modality at a provider.
func (m *Meter) Totals(mod Modality, provider string) Totals {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.m[string(mod)+"/"+provider]; t != nil {
		return *t
	}
	return Totals{}
}
