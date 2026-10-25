// Package catalog holds the requirement catalog of integration readiness discovery
// (master plan 9.5, ADR 0037): the checklist of what a project needs to offer before
// the integration audit (master plan 4.5) can pass it.
//
// The catalog is data (requirements.json, embedded) plus a loader. It is derived from what
// already exists, the audit check ids, the application package layout of master plan 9 and
// the transports of 9.3, and every requirement names the audit check it feeds. English and
// Italian are both mandatory. The catalog is advisory input to a scan: it decides nothing,
// certifies nothing and grants nothing.
//
// Every package-level identifier here starts with catalog or Catalog, so a task that adds
// files next to these never collides with them.
package catalog

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

//go:embed requirements.json
var catalogRaw []byte

// CatalogVersion is the version of the catalog. Reports record it so a later scan can tell
// that the checklist itself changed. Bump it when a requirement is added, removed or changes
// what it asks for; requirement IDs themselves are never renamed.
const CatalogVersion = 1

var (
	catalogIDRE      = regexp.MustCompile(`^req(\.[a-z][a-z0-9_]*){1,4}$`)
	catalogCheckIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
)

// Catalog is the loaded requirement catalog.
type Catalog struct {
	// Requirements are in catalog order, which is stable.
	Requirements []readiness.Requirement
	sha          string
	byID         map[readiness.RequirementID]int
}

// CatalogLoad parses and validates the embedded catalog. It fails closed: an unknown field,
// an unknown enum value, a duplicate ID, a missing English or Italian text or a malformed ID
// is an error and no partial catalog is returned.
func CatalogLoad() (*Catalog, error) { return catalogParse(catalogRaw) }

func catalogParse(raw []byte) (*Catalog, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var reqs []readiness.Requirement
	if err := dec.Decode(&reqs); err != nil {
		return nil, fmt.Errorf("readiness catalog: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("readiness catalog: trailing data after the requirement list")
	}
	if len(reqs) == 0 {
		return nil, fmt.Errorf("readiness catalog: no requirements")
	}
	c := &Catalog{Requirements: reqs, byID: make(map[readiness.RequirementID]int, len(reqs))}
	sum := sha256.Sum256(raw)
	c.sha = hex.EncodeToString(sum[:])
	for i := range reqs {
		r := &reqs[i]
		if err := catalogValidate(r); err != nil {
			return nil, fmt.Errorf("readiness catalog: requirement %d (%q): %w", i, r.ID, err)
		}
		if _, dup := c.byID[r.ID]; dup {
			return nil, fmt.Errorf("readiness catalog: requirement %q appears twice", r.ID)
		}
		c.byID[r.ID] = i
	}
	return c, nil
}

// catalogValidate checks what the generated type does not: the grammar of the IDs, the
// contract version, and that every text is present in both languages.
func catalogValidate(r *readiness.Requirement) error {
	if r.SchemaVersion != 1 {
		return fmt.Errorf("schema_version %d, want 1", r.SchemaVersion)
	}
	if !catalogIDRE.MatchString(string(r.ID)) || len(r.ID) > 64 {
		return fmt.Errorf("id is not a req.* identifier")
	}
	if !catalogCheckIDRE.MatchString(string(r.CheckID)) || len(r.CheckID) > 64 {
		return fmt.Errorf("check_id %q is not a dotted check identifier", r.CheckID)
	}
	if !r.Category.Valid() || !r.Level.Valid() {
		return fmt.Errorf("category or level is not valid")
	}
	if n := len(r.SatisfiedBy); n < 1 || n > 2 {
		return fmt.Errorf("satisfied_by has %d entries, want 1 or 2", n)
	}
	seen := map[readiness.SatisfiedBy]bool{}
	for _, s := range r.SatisfiedBy {
		if !s.Valid() || seen[s] {
			return fmt.Errorf("satisfied_by %q is invalid or repeated", s)
		}
		seen[s] = true
	}
	for name, t := range map[string]readiness.LocalizedText{
		"title": r.Title, "description": r.Description, "how_satisfied": r.HowSatisfied, "remediation": r.Remediation,
	} {
		for lang, s := range map[string]string{"en": string(t.En), "it": string(t.It)} {
			if strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > 1000 {
				return fmt.Errorf("%s.%s is empty or longer than 1000 characters", name, lang)
			}
		}
	}
	return nil
}

// Get returns the requirement with the given ID.
func (c *Catalog) Get(id readiness.RequirementID) (readiness.Requirement, bool) {
	i, ok := c.byID[id]
	if !ok {
		return readiness.Requirement{}, false
	}
	return c.Requirements[i], true
}

// SHA256 is the hash of the catalog data, recorded in every report.
func (c *Catalog) SHA256() string { return c.sha }

// CheckIDs returns the audit check ids the requirements feed, sorted and without repeats.
func (c *Catalog) CheckIDs() []string {
	set := map[string]bool{}
	for _, r := range c.Requirements {
		set[string(r.CheckID)] = true
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ByCheck returns the requirements that feed one audit check, in catalog order.
func (c *Catalog) ByCheck(checkID string) []readiness.Requirement {
	var out []readiness.Requirement
	for _, r := range c.Requirements {
		if string(r.CheckID) == checkID {
			out = append(out, r)
		}
	}
	return out
}

// CatalogIsTransport reports whether a category is one of the control-surface transports of master plan 9.3: an MCP
// server, an OpenAPI description or a native adapter. They are alternatives (each requirement says "if the application
// exposes ...", and the MCP fix says "or choose OpenAPI or a native adapter"): a project needs one, not all three.
func CatalogIsTransport(c readiness.Category) bool {
	return c == readiness.CategoryMCP || c == readiness.CategoryOpenAPI || c == readiness.CategoryNativeAdapter
}

// CatalogOtherTransport reports whether a requirement of category c belongs to a transport the project does not use:
// c is a transport and used, the transport categories with a met requirement, holds another one but not c.
func CatalogOtherTransport(c readiness.Category, used map[readiness.Category]bool) bool {
	if !CatalogIsTransport(c) || used[c] {
		return false
	}
	for u, ok := range used {
		if ok && CatalogIsTransport(u) {
			return true
		}
	}
	return false
}

// CatalogRawJSON returns a copy of the embedded catalog data.
func CatalogRawJSON() []byte { return append([]byte(nil), catalogRaw...) }
