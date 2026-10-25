package metrics

import (
	"context"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Type is the type of a metric family. Only the two types a pull exporter
// needs here exist: histograms and summaries are not offered.
type Type string

const (
	// Counter only ever goes up (until the process restarts).
	Counter Type = "counter"
	// Gauge goes up and down.
	Gauge Type = "gauge"
)

// Label is one label of a sample.
type Label struct{ Name, Value string }

// Sample is one value of a family.
type Sample struct {
	Labels []Label
	Value  float64
}

// Family is a named group of samples with one help text and one type.
type Family struct {
	Name    string
	Help    string
	Type    Type
	Samples []Sample
}

// Collector produces families when the registry is gathered. Collect must be
// quick and must not call out of the process; it may read the context's
// deadline.
type Collector interface {
	// Name identifies the collector in alixai_scrape_collector_up.
	Name() string
	Collect(ctx context.Context) ([]Family, error)
}

// CollectorFunc builds a Collector from a function.
func CollectorFunc(name string, fn func(ctx context.Context) ([]Family, error)) Collector {
	return funcCollector{name: name, fn: fn}
}

type funcCollector struct {
	name string
	fn   func(ctx context.Context) ([]Family, error)
}

func (c funcCollector) Name() string                                  { return c.name }
func (c funcCollector) Collect(ctx context.Context) ([]Family, error) { return c.fn(ctx) }

var (
	metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	// collectorNameRE keeps a collector's name a safe label value of its own.
	collectorNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// DefaultCollectTimeout bounds one collector: a slow one is abandoned and
// reported as down rather than holding a scrape open.
const DefaultCollectTimeout = 2 * time.Second

// Registry holds the collectors of one process. The zero value is not usable;
// use NewRegistry.
type Registry struct {
	mu         sync.RWMutex
	collectors []Collector
	names      map[string]bool
	timeout    time.Duration
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{names: map[string]bool{}, timeout: DefaultCollectTimeout}
}

// Register adds a collector. A bad or repeated collector name is an error: two
// collectors that share a name would hide each other's failures.
func (r *Registry) Register(c Collector) error {
	if c == nil {
		return fmt.Errorf("metrics: cannot register a nil collector")
	}
	name := c.Name()
	if !collectorNameRE.MatchString(name) {
		return fmt.Errorf("metrics: collector name %q must match %s", name, collectorNameRE)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[name] {
		return fmt.Errorf("metrics: collector %q is registered twice", name)
	}
	r.names[name] = true
	r.collectors = append(r.collectors, c)
	return nil
}

// Gather runs every collector and returns the valid families sorted by name,
// plus alixai_scrape_collector_up for each collector. A collector that returns
// an error, a malformed family or nothing before the deadline is down: its
// families are dropped as a whole, so a scrape never contains half a family.
func (r *Registry) Gather(ctx context.Context) []Family {
	r.mu.RLock()
	cs := append([]Collector(nil), r.collectors...)
	timeout := r.timeout
	r.mu.RUnlock()

	var out []Family
	seen := map[string]bool{"alixai_scrape_collector_up": true}
	up := Family{Name: "alixai_scrape_collector_up", Help: "1 when the collector answered in this scrape, 0 when it failed.", Type: Gauge}
	for _, c := range cs {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		fams, err := collectOne(cctx, c)
		cancel()
		if err == nil {
			err = validateFamilies(fams, seen)
		}
		val := 1.0
		if err != nil {
			val, fams = 0, nil
		}
		up.Samples = append(up.Samples, Sample{Labels: []Label{{"collector", c.Name()}}, Value: val})
		for _, f := range fams {
			seen[f.Name] = true
			out = append(out, f)
		}
	}
	out = append(out, up)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// collectOne runs one collector, giving up at the context deadline and turning
// a panic into an error: one broken collector must not take the scrape down.
func collectOne(ctx context.Context, c Collector) ([]Family, error) {
	type result struct {
		fams []Family
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				ch <- result{err: fmt.Errorf("collector %s panicked", c.Name())}
			}
		}()
		fams, err := c.Collect(ctx)
		ch <- result{fams: fams, err: err}
	}()
	select {
	case res := <-ch:
		return res.fams, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func validateFamilies(fams []Family, seen map[string]bool) error {
	local := map[string]bool{}
	for _, f := range fams {
		switch {
		case !metricNameRE.MatchString(f.Name):
			return fmt.Errorf("metrics: invalid metric name %q", f.Name)
		case f.Type != Counter && f.Type != Gauge:
			return fmt.Errorf("metrics: %s has unknown type %q", f.Name, f.Type)
		case seen[f.Name] || local[f.Name]:
			return fmt.Errorf("metrics: %s is exported twice", f.Name)
		}
		local[f.Name] = true
		for _, s := range f.Samples {
			for _, l := range s.Labels {
				if !labelNameRE.MatchString(l.Name) || strings.HasPrefix(l.Name, "__") {
					return fmt.Errorf("metrics: %s has invalid label name %q", f.Name, l.Name)
				}
			}
		}
	}
	return nil
}

// ContentType is the media type of the text exposition format.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// WriteText writes families in the Prometheus text exposition format 0.0.4:
// HELP and TYPE lines, one line per sample, label values escaped, samples
// sorted by their labels so the output is deterministic.
func WriteText(w io.Writer, fams []Family) error {
	var b strings.Builder
	for _, f := range fams {
		if f.Help != "" {
			b.WriteString("# HELP " + f.Name + " " + escapeHelp(f.Help) + "\n")
		}
		b.WriteString("# TYPE " + f.Name + " " + string(f.Type) + "\n")
		samples := append([]Sample(nil), f.Samples...)
		sort.SliceStable(samples, func(i, j int) bool { return labelKey(samples[i].Labels) < labelKey(samples[j].Labels) })
		for _, s := range samples {
			b.WriteString(f.Name)
			if len(s.Labels) > 0 {
				ls := append([]Label(nil), s.Labels...)
				sort.Slice(ls, func(i, j int) bool { return ls[i].Name < ls[j].Name })
				b.WriteByte('{')
				for i, l := range ls {
					if i > 0 {
						b.WriteByte(',')
					}
					b.WriteString(l.Name + `="` + escapeLabel(l.Value) + `"`)
				}
				b.WriteByte('}')
			}
			b.WriteByte(' ')
			b.WriteString(formatFloat(s.Value))
			b.WriteByte('\n')
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func labelKey(ls []Label) string {
	cp := append([]Label(nil), ls...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Name < cp[j].Name })
	var b strings.Builder
	for _, l := range cp {
		b.WriteString(l.Name + "\x00" + l.Value + "\x00")
	}
	return b.String()
}

var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
)

func escapeHelp(s string) string  { return helpEscaper.Replace(s) }
func escapeLabel(s string) string { return labelEscaper.Replace(s) }

func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// B2F is 1 for true and 0 for false: the usual way to export a flag.
func B2F(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
