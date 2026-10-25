package rag

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// Master plan rule 14 and the additive external-provider plan: anything
// security-related defaults to the most restrictive reading.
func TestMissingSourceMetadataReadsMostRestrictive(t *testing.T) {
	t.Run("egress", func(t *testing.T) {
		bad := SourceEgress("send_everything")
		cases := []struct {
			name string
			in   *SourceEgress
			want SourceEgress
		}{
			{"missing", nil, SourceEgressLocalOnly},
			{"unknown value", &bad, SourceEgressLocalOnly},
		}
		for _, v := range []SourceEgress{SourceEgressLocalOnly, SourceEgressAllowExcerpt, SourceEgressAllowMetadata, SourceEgressDeny} {
			v := v
			cases = append(cases, struct {
				name string
				in   *SourceEgress
				want SourceEgress
			}{"stated " + string(v), &v, v})
		}
		for _, c := range cases {
			if got := EffectiveSourceEgress(c.in); got != c.want {
				t.Errorf("%s: got %q, want %q", c.name, got, c.want)
			}
		}
	})
	t.Run("classification", func(t *testing.T) {
		bad := SourceClassification("top_secret")
		cases := []struct {
			name string
			in   *SourceClassification
			want SourceClassification
		}{
			{"missing", nil, SourceClassificationRestricted},
			{"unknown value", &bad, SourceClassificationRestricted},
		}
		for _, v := range []SourceClassification{SourceClassificationPublic, SourceClassificationInternal, SourceClassificationConfidential, SourceClassificationRestricted} {
			v := v
			cases = append(cases, struct {
				name string
				in   *SourceClassification
				want SourceClassification
			}{"stated " + string(v), &v, v})
		}
		for _, c := range cases {
			if got := EffectiveSourceClassification(c.in); got != c.want {
				t.Errorf("%s: got %q, want %q", c.name, got, c.want)
			}
		}
	})
	t.Run("items and citations without metadata", func(t *testing.T) {
		var item EvidenceItem
		var cit EvidenceCitation
		if item.EffectiveEgress() != SourceEgressLocalOnly || cit.EffectiveEgress() != SourceEgressLocalOnly {
			t.Error("a record without source_egress must read as local_only")
		}
		if item.EffectiveClassification() != SourceClassificationRestricted || cit.EffectiveClassification() != SourceClassificationRestricted {
			t.Error("a record without source_classification must read as restricted")
		}
	})
}

func readExample(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "schemas", "rag", "examples", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The client Citation is exactly the internal record without the two internal
// members: nothing else is dropped, nothing is added, and the two values never
// reach the wire.
func TestClientCitationDropsOnlyTheInternalMetadata(t *testing.T) {
	for _, name := range []string{
		"evidence-citation/valid/with-metadata.json",
		"evidence-citation/valid/record-local-only-restricted.json",
		"evidence-citation/valid/without-metadata-reads-most-restrictive.json",
		"evidence-citation/valid/stale-removed-public.json",
	} {
		raw := readExample(t, name)
		var internal EvidenceCitation
		if err := json.Unmarshal(raw, &internal); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		wire, err := json.Marshal(internal.ClientCitation())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		var want, got map[string]any
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
		delete(want, "source_egress")
		delete(want, "source_classification")
		if err := json.Unmarshal(wire, &got); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%s: client citation differs from the internal record minus the two internal members (-want +got):\n%s", name, diff)
		}
		for _, k := range []string{"source_egress", "source_classification"} {
			if _, ok := got[k]; ok {
				t.Errorf("%s: client citation carries %s", name, k)
			}
		}
	}
}
