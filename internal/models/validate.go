package models

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

var (
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// filenamePattern is a single path segment: no separators, no leading dot.
	filenamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func ptrBlank(s *string) bool { return s == nil || blank(*s) }

// safeRepoPath reports whether p is a clean repository-relative path: slash
// separated, not absolute, no empty, "." or ".." segment, no backslash.
func safeRepoPath(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") {
		return false
	}
	for _, seg := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return path.Clean(strings.TrimSuffix(p, "/")) == strings.TrimSuffix(p, "/")
}

// Validate applies the rules that make an entry trustworthy on its own. The
// loader runs it after the schema, and Verify runs it again, so an Entry built
// in code gets the same checks as one read from a file. It returns every
// violation (File is left empty for the caller to fill in).
//
// The lifecycle rules:
//
//   - every status: a complete licence (SEC-091);
//   - qualified-emulated and certified: a pinned artefact (file name,
//     revision, sha256) and an efficiency-review date and evidence path;
//   - certified: additionally never an emulated measurement (an emulated
//     result is not a certification);
//   - rejected and retired: not the default for anything.
func (e *Entry) Validate() []Violation {
	var vs []Violation
	add := func(rule Rule, field, format string, args ...any) {
		vs = append(vs, Violation{Entry: e.ID, Rule: rule, Field: field, Detail: fmt.Sprintf(format, args...)})
	}

	if !e.Status.Valid() {
		add(RuleStatus, "status", "%q is not one of candidate, qualified-emulated, certified, rejected, retired", string(e.Status))
	}

	// Licence: SPDX id, URL, redistribution notes. Required for every status.
	if blank(e.Licence.SPDX) {
		add(RuleLicence, "licence.spdx", "SPDX licence id is required")
	}
	if blank(e.Licence.URL) {
		add(RuleLicence, "licence.url", "licence URL is required")
	} else if !strings.HasPrefix(e.Licence.URL, "https://") {
		add(RuleLicence, "licence.url", "licence URL must be https")
	}
	if blank(e.Licence.RedistributionNotes) {
		add(RuleLicence, "licence.redistribution_notes", "redistribution notes are required")
	}

	// A recorded artefact field must be well formed whatever the status.
	a := e.Artifact
	if a.SHA256 != nil && !sha256Pattern.MatchString(*a.SHA256) {
		add(RulePinned, "artifact.sha256", "must be 64 lower-case hex characters")
	}
	if a.Filename != nil && !filenamePattern.MatchString(*a.Filename) {
		add(RulePinned, "artifact.filename", "must be a single file name, not a path")
	}
	if a.Revision != nil && !revisionPattern.MatchString(*a.Revision) {
		add(RulePinned, "artifact.revision", "must be a 40-character commit id, not a branch or tag")
	}
	if a.SizeBytes != nil && *a.SizeBytes < 1 {
		add(RulePinned, "artifact.size_bytes", "must be positive")
	}

	needsPin := e.Status == StatusQualifiedEmulated || e.Status == StatusCertified
	if needsPin {
		if a.Filename == nil {
			add(RulePinned, "artifact.filename", "%s requires the exact artefact file name", e.Status)
		}
		if a.Revision == nil {
			add(RulePinned, "artifact.revision", "%s requires the pinned artefact revision", e.Status)
		}
		if a.SHA256 == nil {
			add(RulePinned, "artifact.sha256", "%s requires a sha256", e.Status)
		}
		if ptrBlank(e.Review.Date) {
			add(RuleEvidence, "last_efficiency_review.date", "%s requires the date of the last efficiency review", e.Status)
		}
		if ptrBlank(e.Review.EvidencePath) {
			add(RuleEvidence, "last_efficiency_review.evidence_path", "%s requires an evidence path", e.Status)
		}
	}
	if e.Review.EvidencePath != nil && !safeRepoPath(*e.Review.EvidencePath) {
		add(RuleEvidence, "last_efficiency_review.evidence_path", "must be a clean repository-relative path")
	}
	if e.Measurement.ResultsPath != nil && !safeRepoPath(*e.Measurement.ResultsPath) {
		add(RuleEvidence, "measurement.results_path", "must be a clean repository-relative path")
	}
	if e.Status == StatusCertified && e.Measurement.Emulated != nil && *e.Measurement.Emulated {
		add(RuleEmulated, "measurement.emulated", "a certified entry cannot rest on an emulated measurement")
	}

	// default_for.
	seen := map[Default]bool{}
	for i, d := range e.DefaultFor {
		field := fmt.Sprintf("default_for[%d]", i)
		if !e.HasRole(d.Role) {
			add(RuleDefault, field, "default for role %q, which this entry does not have", string(d.Role))
		}
		if seen[d] {
			add(RuleDefault, field, "(%s, %s) is listed more than once", d.Role, d.Profile)
		}
		seen[d] = true
	}
	if (e.Status == StatusRejected || e.Status == StatusRetired) && len(e.DefaultFor) > 0 {
		add(RuleDefault, "default_for", "a %s entry cannot be the default for anything", e.Status)
	}

	// An embedding model fixes the index layout through its vector dimension.
	if e.HasRole(RoleEmbedding) && (e.Embedding == nil || e.Embedding.Dimensions == nil) {
		add(RuleEmbedding, "embedding.dimensions", "an entry with the embedding role must declare its vector dimension")
	}

	if e.Fallback.ModelID != nil && *e.Fallback.ModelID == e.ID {
		add(RuleFallback, "fallback.model_id", "an entry cannot be its own fallback")
	}
	vs = append(vs, e.validateDecisionLayer()...)
	return vs
}

// scorerCapMiB is the memory a decision scorer may peak at to be the default of
// a profile (docs/operations/resource-budgets.md section 4.1: the scorer row).
// experimental-4g has no row: it never runs a scorer. high has no cap here
// because its row is the container budget itself; the measurement must still
// exist.
var scorerCapMiB = map[Profile]float64{ProfileLite: 300, ProfileBalanced: 600}

// validateDecisionLayer applies the rules of the optional decision layer
// (master plan 8.5, ADR 0029) that one entry can be judged on alone:
//
//   - a decision-scorer carries the scorer block, and only a scorer does;
//   - a bi-encoder scorer declares a frozen-encoder dependency;
//   - a decision-encoder is a dependency, the default for nothing;
//   - a scorer is the default for a profile only with evidence: qualified or
//     certified, measured on that profile, with a measured peak inside the
//     profile's scorer row; never on experimental-4g; and one with an encoder
//     dependency only on the high profile.
//
// Whether a dependency resolves needs the whole registry (checkDependencies).
func (e *Entry) validateDecisionLayer() []Violation {
	var vs []Violation
	add := func(rule Rule, field, format string, args ...any) {
		vs = append(vs, Violation{Entry: e.ID, Rule: rule, Field: field, Detail: fmt.Sprintf(format, args...)})
	}

	isScorer := e.HasRole(RoleDecisionScorer)
	switch {
	case isScorer && e.Scorer == nil:
		add(RuleScorer, "scorer", "a decision-scorer must declare the scorer block (architecture, max_options)")
	case !isScorer && e.Scorer != nil:
		add(RuleScorer, "scorer", "the scorer block belongs to an entry with the decision-scorer role")
	}
	if e.Scorer != nil {
		switch e.Scorer.Architecture {
		case ScorerNonAutoregressiveEncoder, ScorerAutoregressiveLetter:
		case ScorerBiEncoder:
			if !e.hasDependency(DependencyFrozenEncoder) {
				add(RuleScorer, "dependencies", "a bi-encoder scorer must declare its frozen-encoder dependency")
			}
		default:
			add(RuleScorer, "scorer.architecture", "%q is not a known scorer architecture", e.Scorer.Architecture)
		}
		if e.Scorer.MaxOptions != nil && *e.Scorer.MaxOptions < 2 {
			add(RuleScorer, "scorer.max_options", "a choice needs at least 2 options")
		}
	}

	seen := map[string]bool{}
	for i, d := range e.Dependencies {
		field := fmt.Sprintf("dependencies[%d]", i)
		if d.ID == e.ID {
			add(RuleDependency, field, "an entry cannot depend on itself")
		}
		if d.Relation != DependencyFrozenEncoder {
			add(RuleDependency, field, "relation %q is not frozen-encoder", d.Relation)
		}
		if seen[d.ID] {
			add(RuleDependency, field, "%q is listed more than once", d.ID)
		}
		seen[d.ID] = true
	}

	for i, d := range e.DefaultFor {
		field := fmt.Sprintf("default_for[%d]", i)
		if d.Role == RoleDecisionEncoder {
			add(RuleDefault, field, "a decision-encoder is a dependency, never the default for a profile")
			continue
		}
		if d.Role != RoleDecisionScorer {
			continue
		}
		if d.Profile == ProfileExperimental4G {
			add(RuleScorer, field, "the experimental-4g profile runs no decision scorer (resource-budgets rule R2)")
			continue
		}
		if e.Status != StatusQualifiedEmulated && e.Status != StatusCertified {
			add(RuleScorer, field, "a %s scorer cannot be the default for %s: it needs qualified-emulated or certified status", e.Status, d.Profile)
		}
		if ptrBlank(e.Measurement.ResultsPath) {
			add(RuleScorer, field, "a scorer default for %s needs measurement.results_path (the evidence)", d.Profile)
		}
		if e.Measurement.Profile == nil || *e.Measurement.Profile != d.Profile {
			add(RuleScorer, field, "a scorer default for %s must have been measured on %s", d.Profile, d.Profile)
		}
		if len(e.Dependencies) > 0 && d.Profile != ProfileHigh {
			add(RuleScorer, field, "a scorer that needs a frozen encoder is only a candidate default for the high profile, not %s", d.Profile)
		}
		if limit, capped := scorerCapMiB[d.Profile]; capped {
			switch {
			case e.Memory.PeakRSSMB == nil:
				add(RuleScorer, field, "a scorer default for %s needs a measured memory.peak_rss_mb", d.Profile)
			case *e.Memory.PeakRSSMB > limit:
				add(RuleScorer, field, "peak %.0f MiB is over the %.0f MiB scorer row of %s", *e.Memory.PeakRSSMB, limit, d.Profile)
			}
		} else if e.Memory.PeakRSSMB == nil {
			add(RuleScorer, field, "a scorer default for %s needs a measured memory.peak_rss_mb", d.Profile)
		}
	}
	return vs
}

func (e *Entry) hasDependency(relation string) bool {
	for _, d := range e.Dependencies {
		if d.Relation == relation {
			return true
		}
	}
	return false
}

// loaded is an entry that parsed on its own, with the file it came from.
type loaded struct {
	file  string
	entry Entry
}

// checkRegistry applies the rules that need more than one entry.
func checkRegistry(items []loaded) []Violation {
	var vs []Violation

	byID := make(map[string]*loaded, len(items))
	for i := range items {
		it := &items[i]
		if prev, dup := byID[it.entry.ID]; dup {
			vs = append(vs, Violation{File: it.file, Entry: it.entry.ID, Rule: RuleDuplicateID,
				Detail: fmt.Sprintf("id %q is already used by %s", it.entry.ID, prev.file)})
			continue
		}
		byID[it.entry.ID] = it
	}

	// At most one default per (role, profile).
	owners := map[Default][]string{}
	for i := range items {
		e := &items[i].entry
		seen := map[Default]bool{}
		for _, d := range e.DefaultFor {
			if seen[d] {
				continue // a duplicate inside one entry is reported by Entry.Validate
			}
			seen[d] = true
			owners[d] = append(owners[d], e.ID)
		}
	}
	keys := make([]Default, 0, len(owners))
	for k := range owners {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Role != keys[j].Role {
			return keys[i].Role < keys[j].Role
		}
		return keys[i].Profile < keys[j].Profile
	})
	for _, k := range keys {
		ids := owners[k]
		if len(ids) > 1 {
			sort.Strings(ids)
			vs = append(vs, Violation{Rule: RuleDefaultUnique, Field: fmt.Sprintf("default_for(%s, %s)", k.Role, k.Profile),
				Detail: fmt.Sprintf("more than one default: %s", strings.Join(ids, ", "))})
		}
	}

	vs = append(vs, checkDependencies(items, byID)...)

	// Fallback references and cycles.
	for i := range items {
		it := &items[i]
		e := &it.entry
		if e.Fallback.ModelID == nil || *e.Fallback.ModelID == e.ID {
			continue // none, or a self reference already reported by Entry.Validate
		}
		bad := func(format string, args ...any) {
			vs = append(vs, Violation{File: it.file, Entry: e.ID, Rule: RuleFallback, Field: "fallback.model_id",
				Detail: fmt.Sprintf(format, args...)})
		}
		target, ok := byID[*e.Fallback.ModelID]
		if !ok {
			bad("fallback %q is not in the registry", *e.Fallback.ModelID)
			continue
		}
		if target.entry.Status == StatusRejected || target.entry.Status == StatusRetired {
			bad("fallback %q is %s", target.entry.ID, target.entry.Status)
		}
		if !sharesRole(e, &target.entry) {
			bad("fallback %q serves none of this entry's roles", target.entry.ID)
		}

		// Follow the chain. Reaching e again is a loop through e; reaching any
		// other entry twice is a loop that is reported for its own members.
		visited := map[string]bool{e.ID: true}
		for cur := target; ; {
			if visited[cur.entry.ID] {
				if cur.entry.ID == e.ID {
					bad("the fallback chain loops back to this entry")
				}
				break
			}
			visited[cur.entry.ID] = true
			if cur.entry.Fallback.ModelID == nil {
				break
			}
			next, ok := byID[*cur.entry.Fallback.ModelID]
			if !ok {
				break
			}
			cur = next
		}
	}
	return vs
}

// checkDependencies makes every dependency resolve: the target exists, has the
// decision-encoder role, is neither rejected nor retired and is pinned (file
// name, revision and sha256), so a scorer cannot rest on an encoder nobody can
// verify (SEC-063).
func checkDependencies(items []loaded, byID map[string]*loaded) []Violation {
	var vs []Violation
	for i := range items {
		it := &items[i]
		e := &it.entry
		for j, d := range e.Dependencies {
			if d.ID == e.ID {
				continue // reported by Entry.Validate
			}
			field := fmt.Sprintf("dependencies[%d]", j)
			bad := func(format string, args ...any) {
				vs = append(vs, Violation{File: it.file, Entry: e.ID, Rule: RuleDependency, Field: field,
					Detail: fmt.Sprintf(format, args...)})
			}
			target, ok := byID[d.ID]
			if !ok {
				bad("dependency %q is not in the registry", d.ID)
				continue
			}
			t := &target.entry
			if !t.HasRole(RoleDecisionEncoder) {
				bad("dependency %q does not have the decision-encoder role", d.ID)
			}
			if t.Status == StatusRejected || t.Status == StatusRetired {
				bad("dependency %q is %s", d.ID, t.Status)
			}
			if t.Artifact.Filename == nil || t.Artifact.Revision == nil || t.Artifact.SHA256 == nil {
				bad("dependency %q is not pinned (artifact.filename, revision and sha256 are required)", d.ID)
			}
		}
	}
	return vs
}

func sharesRole(a, b *Entry) bool {
	for _, r := range a.Roles {
		if b.HasRole(r) {
			return true
		}
	}
	return false
}

// checkEvidence reports an evidence or results path that does not exist in
// root. It is only run when the caller supplies a root to look in.
func checkEvidence(e *Entry, file string, root fs.FS) []Violation {
	var vs []Violation
	check := func(field string, p *string) {
		if p == nil || !safeRepoPath(*p) {
			return // unset, or already reported as unsafe
		}
		if _, err := fs.Stat(root, strings.TrimSuffix(*p, "/")); err != nil {
			vs = append(vs, Violation{File: file, Entry: e.ID, Rule: RuleEvidence, Field: field,
				Detail: fmt.Sprintf("%q does not exist", *p)})
		}
	}
	check("last_efficiency_review.evidence_path", e.Review.EvidencePath)
	check("measurement.results_path", e.Measurement.ResultsPath)
	return vs
}
