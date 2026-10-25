// Command coverage reports which SEC-NNN ids of docs/security/test-backlog.md
// have a test (master plan 22.5, task M5-08).
//
//	go run ./tests/security/cmd/coverage            exit 1 if an id has no test and is not pending
//	go run ./tests/security/cmd/coverage -strict    release view: also fail on pending, defects, stale names, missing evidence
//	go run ./tests/security/cmd/coverage -v         list every id with the tests that cover it
//
// An id is covered when a name in its Test(s) column resolves to a top-level
// Test*/Fuzz* function in the named package directory, or a test function
// carries the tag `// SEC-NNN` directly above it. Where the row lists tests
// under tests/security/, one of those (or a tagged test there) must resolve:
// another task's legacy test does not stand in for the cross-cutting one the
// backlog assigns to this suite. Names listed for other tasks that do not exist
// (not landed yet, or renamed) are reported as stale, never counted.
//
// An id whose Kind is `pentest` or `human` only is covered by the evidence
// report section 6.2 names once that report mentions the id; until then it is
// "assigned" (explicitly owned by the pentest or the human review), which passes
// the default run. A combined kind (`automated+pentest`) needs the automated
// tests now and the evidence for -strict.
//
// A test that cannot pass because the control it checks is wrong in code this
// suite may not edit calls secutil.Known(t, "SEC-NNN", "ref", "why") first. It
// skips, never counts as coverage, and its id is a DEFECT line on every run (and
// a -strict failure) until the call is removed. A control that is missing
// altogether has no test to gate: pending.txt lists it with kind `defect`.
//
// pending.txt also lists ids owned by a task that has not landed (kind
// `pending`). A listed id that has become covered fails the run until its line
// is removed, so the list can only shrink.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed pending.txt
var pendingFile string

const (
	defaultBacklog = "docs/security/test-backlog.md"
	securityDir    = "tests/security/"
)

// status of one backlog id.
type status int

const (
	statusCovered   status = iota // a test resolves (under tests/security where the row asks for it)
	statusEvidence                // pentest/human only, and the evidence report names the id
	statusAssigned                // pentest/human only, evidence not recorded yet
	statusPending                 // owned by a task that has not landed (pending.txt)
	statusDefect                  // the control is wrong or missing outside this suite's scope
	statusUncovered               // nothing covers it and nothing explains why
)

func (s status) String() string {
	return [...]string{"covered", "evidence", "assigned", "pending", "DEFECT", "UNCOVERED"}[s]
}

// pendingEntry is one line of pending.txt.
type pendingEntry struct {
	Kind   string // "pending" or "defect"
	Ref    string // owning task or backlog gap
	Reason string
}

// result is the verdict for one id.
type result struct {
	Row          row
	Status       status
	Resolved     []string // tests that count ("dir: Name")
	Missing      []string // listed names that do not exist ("dir: Name")
	Evidence     []string // evidence files for the id
	EvidenceDone bool     // an evidence file exists and names the id
	Defects      []defect // defect-gated tests
	Pending      *pendingEntry
	StalePending bool // the pending.txt line is no longer needed
}

// evaluate decides every row.
func evaluate(root string, b *backlog, ix *testIndex, pending map[string]pendingEntry) []result {
	out := make([]result, 0, len(b.Rows))
	for _, r := range b.Rows {
		res := result{Row: r}
		seen := map[string]bool{}
		secNamed := false
		for _, e := range r.Tests {
			if e.Evidence {
				res.Evidence = append(res.Evidence, e.Dir)
				continue
			}
			for _, n := range e.Names {
				key := e.Dir + ": " + n
				if strings.HasPrefix(e.Dir+"/", securityDir) {
					secNamed = true
				}
				if seen[key] {
					continue
				}
				seen[key] = true
				switch {
				case ix.gated[key]:
					// Counted as a defect below, never as coverage.
				case ix.has(e.Dir, n):
					res.Resolved = append(res.Resolved, key)
				default:
					res.Missing = append(res.Missing, key)
				}
			}
		}
		for _, key := range ix.tagged[r.ID] {
			if !ix.gated[key] && !seen[key] {
				res.Resolved = append(res.Resolved, key)
			}
		}
		sort.Strings(res.Resolved)
		res.Resolved = dedupe(res.Resolved)
		res.Defects = ix.defects[r.ID]
		res.Evidence = dedupe(sortedCopy(append(res.Evidence, b.Evidence[r.ID]...)))
		for _, f := range res.Evidence {
			if evidenceNames(root, f, r.ID) {
				res.EvidenceDone = true
			}
		}
		if p, ok := pending[r.ID]; ok {
			res.Pending = &p
		}
		res.Status = classify(&res, secNamed)
		out = append(out, res)
	}
	return out
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// hasCoverage applies the covering rule: where the row names tests under
// tests/security, one of those must resolve; otherwise any resolved test counts.
func hasCoverage(res *result, secNamed bool) bool {
	if !secNamed {
		return len(res.Resolved) > 0
	}
	for _, key := range res.Resolved {
		if strings.HasPrefix(key, securityDir) {
			return true
		}
	}
	return false
}

func classify(res *result, secNamed bool) status {
	r := res.Row
	covered := hasCoverage(res, secNamed)
	switch {
	case len(res.Defects) > 0:
		return statusDefect
	case res.Pending != nil:
		if covered {
			res.StalePending = true
			return statusCovered
		}
		if res.Pending.Kind == "defect" {
			return statusDefect
		}
		return statusPending
	case !r.needsAutomated() && r.needsEvidence():
		switch {
		case res.EvidenceDone:
			return statusEvidence
		case len(res.Evidence) > 0:
			return statusAssigned
		}
		return statusUncovered
	case covered:
		return statusCovered
	}
	return statusUncovered
}

// evidenceNames reports whether the report exists and records a result for the id.
func evidenceNames(root, rel, id string) bool {
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil && strings.Contains(string(raw), id)
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// parsePending reads pending.txt: `SEC-NNN pending|defect ref reason...`, `#`
// comments. Every field is required: a bare id explains nothing.
func parsePending(text string) (map[string]pendingEntry, error) {
	out := map[string]pendingEntry{}
	for n, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 {
			return nil, fmt.Errorf("pending.txt line %d: want `SEC-NNN pending|defect ref reason`, got %q", n+1, line)
		}
		id, kind, ref := f[0], f[1], f[2]
		if !tagPattern.MatchString(id) || len(id) != 7 {
			return nil, fmt.Errorf("pending.txt line %d: %q is not a SEC id", n+1, id)
		}
		if kind != "pending" && kind != "defect" {
			return nil, fmt.Errorf("pending.txt line %d: kind %q is neither pending nor defect", n+1, kind)
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("pending.txt line %d: %s listed twice", n+1, id)
		}
		out[id] = pendingEntry{Kind: kind, Ref: ref, Reason: strings.Join(f[3:], " ")}
	}
	return out, nil
}

// report prints the table and returns the failure messages.
func report(w io.Writer, results []result, strict, verbose bool, pending map[string]pendingEntry) []string {
	var fails []string
	counts := map[status]int{}
	stale := 0
	for _, res := range results {
		counts[res.Status]++
		stale += len(res.Missing)
	}
	for _, res := range results {
		line := fmt.Sprintf("%s  %-9s %-8s %s", res.Row.ID, res.Status, res.Row.Sev, res.Row.Kind)
		switch res.Status {
		case statusUncovered, statusPending, statusAssigned, statusDefect:
			fmt.Fprintf(w, "%s  owner=%s", line, res.Row.Owner)
			if res.Pending != nil {
				fmt.Fprintf(w, "  %s %s: %s", res.Pending.Kind, res.Pending.Ref, res.Pending.Reason)
			}
			for _, d := range res.Defects {
				fmt.Fprintf(w, "\n    DEFECT %s (%s) in %s: %s", d.ID, d.Ref, d.Func, d.Why)
			}
			if res.Status == statusAssigned {
				fmt.Fprintf(w, "  evidence=%s", strings.Join(res.Evidence, ","))
			}
			if res.Status == statusUncovered && len(res.Missing) > 0 {
				fmt.Fprintf(w, "  missing=%s", strings.Join(res.Missing, ", "))
			}
			fmt.Fprintln(w)
		default:
			if verbose {
				fmt.Fprintf(w, "%s  %s\n", line, strings.Join(res.Resolved, "; "))
			}
		}
		if res.StalePending {
			fails = append(fails, fmt.Sprintf("%s is covered now: remove it from pending.txt", res.Row.ID))
		}
		if res.Status == statusUncovered {
			fails = append(fails, fmt.Sprintf("%s has no test (owner %s)", res.Row.ID, res.Row.Owner))
		}
		if len(res.Missing) > 0 && res.Status != statusUncovered && (verbose || strict) {
			msg := fmt.Sprintf("%s lists tests that do not exist: %s", res.Row.ID, strings.Join(res.Missing, ", "))
			fmt.Fprintf(w, "stale   %s\n", msg)
			if strict {
				fails = append(fails, msg)
			}
		}
		if strict {
			switch res.Status {
			case statusPending, statusAssigned, statusDefect:
				fails = append(fails, fmt.Sprintf("%s is %s (%s)", res.Row.ID, res.Status, res.Row.Owner))
			case statusCovered:
				if res.Row.needsEvidence() && !res.EvidenceDone {
					fails = append(fails, fmt.Sprintf("%s: the pentest or human evidence is not recorded (%s)", res.Row.ID, strings.Join(res.Evidence, ", ")))
				}
			}
		}
	}
	known := map[string]bool{}
	for _, res := range results {
		known[res.Row.ID] = true
	}
	var ghosts []string
	for id := range pending {
		if !known[id] {
			ghosts = append(ghosts, id)
		}
	}
	sort.Strings(ghosts)
	for _, id := range ghosts {
		fails = append(fails, fmt.Sprintf("pending.txt names %s, which is not in the backlog", id))
	}
	fmt.Fprintf(w, "\n%d ids: %d covered, %d evidence recorded, %d assigned to a pentest or human report, %d pending a task, %d defects, %d uncovered; %d listed test names do not exist yet\n",
		len(results), counts[statusCovered], counts[statusEvidence], counts[statusAssigned], counts[statusPending], counts[statusDefect], counts[statusUncovered], stale)
	return fails
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("coverage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rootFlag := fs.String("root", "", "repository root (default: the directory holding go.mod above the working directory)")
	backlogPath := fs.String("backlog", defaultBacklog, "backlog file, relative to the root")
	strict := fs.Bool("strict", false, "release view: fail on pending ids, defects, stale names and missing evidence too")
	verbose := fs.Bool("v", false, "list every id and its tests, and every stale name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root := *rootFlag
	if root == "" {
		var err error
		if root, err = repoRoot("."); err != nil {
			fmt.Fprintln(stderr, "coverage:", err)
			return 2
		}
	}
	b, err := parseBacklog(filepath.Join(root, filepath.FromSlash(*backlogPath)))
	if err != nil {
		fmt.Fprintln(stderr, "coverage:", err)
		return 2
	}
	pending, err := parsePending(pendingFile)
	if err != nil {
		fmt.Fprintln(stderr, "coverage:", err)
		return 2
	}
	ix, err := buildIndex(root)
	if err != nil {
		fmt.Fprintln(stderr, "coverage:", err)
		return 2
	}
	results := evaluate(root, b, ix, pending)
	fails := report(stdout, results, *strict, *verbose, pending)
	ids := map[string]bool{}
	for _, r := range b.Rows {
		ids[r.ID] = true
	}
	for id, ds := range ix.defects {
		if !ids[id] {
			fails = append(fails, fmt.Sprintf("%s calls secutil.Known for %s, which is not in the backlog", ds[0].Func, id))
		}
	}
	sort.Strings(fails)
	for _, f := range fails {
		fmt.Fprintln(stderr, "FAIL:", f)
	}
	if len(fails) > 0 {
		return 1
	}
	return 0
}
