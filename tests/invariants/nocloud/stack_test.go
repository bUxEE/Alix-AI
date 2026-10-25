package nocloud

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// assertStack checks the properties of the production compose file that the no-cloud check
// relies on: every service drops capabilities and has a read-only root, nothing but the relay publishes
// a port and only on the loopback, every volume is a bind mount, every start-order dependency waits for
// health, there is no one-shot init service, and the networks the services share have no route out.
func assertStack(t *testing.T, raw []byte) {
	t.Helper()
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
		Networks map[string]map[string]any `yaml:"networks"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("compose.yaml: %v", err)
	}
	if len(doc.Services) < 4 {
		t.Fatalf("the stack has %d services, want localai, anchor, alixai and relay", len(doc.Services))
	}
	var names []string
	for n := range doc.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s := doc.Services[n]
		if strings.Contains(n, "init") {
			t.Errorf("service %s looks like a one-shot init container", n)
		}
		if ro, _ := s["read_only"].(bool); !ro {
			t.Errorf("service %s does not have a read-only root filesystem", n)
		}
		if drop, _ := s["cap_drop"].([]any); len(drop) != 1 || fmt.Sprint(drop[0]) != "ALL" {
			t.Errorf("service %s does not drop every capability: %v", n, s["cap_drop"])
		}
		if _, privileged := s["privileged"]; privileged {
			t.Errorf("service %s sets privileged", n)
		}
		if img, ok := s["image"].(string); ok && !strings.Contains(img, "@sha256:") {
			t.Errorf("service %s image %q is not pinned by digest", n, img)
		}
		if ports, ok := s["ports"].([]any); ok {
			if n != "relay" {
				t.Errorf("service %s publishes ports %v; only the relay does", n, ports)
			}
			for _, p := range ports {
				if !strings.HasPrefix(fmt.Sprint(p), "127.0.0.1:") {
					t.Errorf("service %s publishes %v, which is not on the loopback", n, p)
				}
			}
		}
		for _, v := range asList(s["volumes"]) {
			src, _, _ := strings.Cut(fmt.Sprint(v), ":")
			if !strings.HasPrefix(src, ".") && !strings.HasPrefix(src, "$") {
				t.Errorf("service %s volume %q is not a bind mount", n, v)
			}
		}
		if deps, ok := s["depends_on"].(map[string]any); ok {
			for dep, cond := range deps {
				c, _ := cond.(map[string]any)
				if fmt.Sprint(c["condition"]) != "service_healthy" {
					t.Errorf("service %s depends on %s without waiting for health: %v", n, dep, cond)
				}
			}
		}
	}
	for _, n := range []string{"backend", "front"} {
		if internal, _ := doc.Networks[n]["internal"].(bool); !internal {
			t.Errorf("network %s is not internal: it would have a route out", n)
		}
	}
	// The one network with a route out is joined by the relay alone.
	for _, n := range names {
		for _, net := range asList(doc.Services[n]["networks"]) {
			if fmt.Sprint(net) == "edge" && n != "relay" {
				t.Errorf("service %s joins the edge network; only the relay does", n)
			}
		}
	}
	if internal, _ := doc.Networks["edge"]["internal"].(bool); internal {
		t.Error("the edge network is internal: the published port could not be carried in")
	}
}

// asList reads a YAML list of scalars; a map (compose also allows one) lists its keys.
func asList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case map[string]any:
		var out []any
		for k := range x {
			out = append(out, k)
		}
		sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]) < fmt.Sprint(out[j]) })
		return out
	}
	return nil
}
