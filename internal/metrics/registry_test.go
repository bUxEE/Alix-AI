package metrics

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The text format is deterministic and escapes what the format says to escape.
func TestWriteTextFormat(t *testing.T) {
	fams := []Family{{
		Name: "alixai_demo_total", Help: "Line one.\nBack\\slash.", Type: Counter,
		Samples: []Sample{
			{Labels: []Label{{"state", "b"}, {"class", `x"y`}}, Value: 2},
			{Labels: []Label{{"state", "a"}, {"class", "line\nbreak"}}, Value: math.Inf(1)},
			{Value: math.NaN()},
		},
	}}
	var buf bytes.Buffer
	if err := WriteText(&buf, fams); err != nil {
		t.Fatal(err)
	}
	want := "# HELP alixai_demo_total Line one.\\nBack\\\\slash.\n" +
		"# TYPE alixai_demo_total counter\n" +
		"alixai_demo_total NaN\n" +
		"alixai_demo_total{class=\"line\\nbreak\",state=\"a\"} +Inf\n" +
		"alixai_demo_total{class=\"x\\\"y\",state=\"b\"} 2\n"
	if diff := cmp.Diff(want, buf.String()); diff != "" {
		t.Errorf("text format (-want +got):\n%s", diff)
	}
}

// A failing, malformed, panicking or slow collector is reported down and never
// takes the scrape or another collector with it.
func TestGatherIsolatesBrokenCollectors(t *testing.T) {
	reg := NewRegistry()
	reg.timeout = 200 * time.Millisecond
	good := Family{Name: "alixai_ok", Type: Gauge, Samples: []Sample{{Value: 1}}}
	for _, c := range []Collector{
		CollectorFunc("good", func(context.Context) ([]Family, error) { return []Family{good}, nil }),
		CollectorFunc("failing", func(context.Context) ([]Family, error) { return nil, errors.New("boom") }),
		CollectorFunc("badname", func(context.Context) ([]Family, error) {
			return []Family{good, {Name: "9 bad name", Type: Gauge}}, nil
		}),
		CollectorFunc("duplicate", func(context.Context) ([]Family, error) { return []Family{good}, nil }),
		CollectorFunc("badlabel", func(context.Context) ([]Family, error) {
			return []Family{{Name: "alixai_l", Type: Gauge, Samples: []Sample{{Labels: []Label{{"__reserved", "x"}}}}}}, nil
		}),
		CollectorFunc("panics", func(context.Context) ([]Family, error) { panic("secret-in-panic") }),
		CollectorFunc("slow", func(ctx context.Context) ([]Family, error) { <-ctx.Done(); return nil, ctx.Err() }),
	} {
		if err := reg.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := WriteText(&buf, reg.Gather(context.Background())); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"alixai_ok 1\n",
		`alixai_scrape_collector_up{collector="good"} 1`,
		`alixai_scrape_collector_up{collector="failing"} 0`,
		`alixai_scrape_collector_up{collector="badname"} 0`,
		`alixai_scrape_collector_up{collector="duplicate"} 0`,
		`alixai_scrape_collector_up{collector="badlabel"} 0`,
		`alixai_scrape_collector_up{collector="panics"} 0`,
		`alixai_scrape_collector_up{collector="slow"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "alixai_ok 1\n") != 1 || strings.Contains(out, "bad name") || strings.Contains(out, "__reserved") || strings.Contains(out, "secret-in-panic") {
		t.Errorf("a broken collector leaked into the scrape:\n%s", out)
	}
}

func TestRegisterRefusesBadAndRepeatedCollectors(t *testing.T) {
	reg := NewRegistry()
	ok := CollectorFunc("one", func(context.Context) ([]Family, error) { return nil, nil })
	if err := reg.Register(ok); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(ok); err == nil {
		t.Error("a collector name was registered twice")
	}
	if err := reg.Register(CollectorFunc("Bad Name", nil)); err == nil {
		t.Error("a collector name with a space and capitals was accepted")
	}
	if err := reg.Register(nil); err == nil {
		t.Error("a nil collector was accepted")
	}
}

func TestRuntimeCollectorReportsTheProcess(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(RuntimeCollector(nil)); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteText(&buf, reg.Gather(context.Background())); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alixai_build_info{", "alixai_process_uptime_seconds ", "alixai_go_goroutines ", "alixai_go_heap_alloc_bytes ", `alixai_scrape_collector_up{collector="runtime"} 1`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("missing %q in:\n%s", want, buf.String())
		}
	}
}
