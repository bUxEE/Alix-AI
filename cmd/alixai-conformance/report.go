package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"
)

// Status is the verdict on one item or one stage.
//
//	pass        looked at, and as it should be
//	warn        looked at, and worth a person's attention; does not stop a package
//	unverified  could not be looked at in this build or this world (the integration
//	            audit records the same thing, and still lets a package activate)
//	not_run     the check was never reached, or its stage could not run at all
//	fail        looked at, and wrong
//
// Fail and NotRun always fail a run: a stage that did not run proves nothing. Warn and
// Unverified fail it only under -strict, so a run is not red for something the audit
// itself accepts, and never reads as a pass either: both are named in every report.
type Status string

const (
	StatusPass       Status = "pass"
	StatusWarn       Status = "warn"
	StatusFail       Status = "fail"
	StatusUnverified Status = "unverified"
	StatusNotRun     Status = "not_run"
)

// failing reports whether the status fails a run. strict also fails warnings and
// anything unverified. A status this tool does not know fails.
func (s Status) failing(strict bool) bool {
	switch s {
	case StatusPass:
		return false
	case StatusWarn, StatusUnverified:
		return strict
	}
	return true
}

// rank orders statuses by severity, for the roll-up of a stage.
func (s Status) rank() int {
	switch s {
	case StatusFail:
		return 4
	case StatusNotRun:
		return 3
	case StatusUnverified:
		return 2
	case StatusWarn:
		return 1
	case StatusPass:
		return 0
	}
	return 5 // a status nobody defined is worse than any that is
}

// Item is one check of a stage: a section of the integration audit, one replayed
// contract example, one capability.
type Item struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Message string `json:"message,omitempty"`
	// Details holds what the item observed, for a person reading the report. It never
	// carries a secret, a token or a generated identifier.
	Details map[string]any `json:"details,omitempty"`
}

// Stage is one of the three stages of a run.
type Stage struct {
	Name       string `json:"name"`
	Status     Status `json:"status"`
	Message    string `json:"message,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Items      []Item `json:"items"`
}

// PackageInfo says which package the run was about.
type PackageInfo struct {
	Path          string `json:"path"`
	ApplicationID string `json:"application_id,omitempty"`
	Version       string `json:"package_version,omitempty"`
	ManifestHash  string `json:"manifest_hash,omitempty"`
	Authenticity  string `json:"authenticity,omitempty"`
	SignedBy      string `json:"signed_by,omitempty"`
}

// Summary counts the items of every stage.
type Summary struct {
	Items      int `json:"items"`
	Passed     int `json:"passed"`
	Warnings   int `json:"warnings"`
	Failed     int `json:"failed"`
	Unverified int `json:"unverified"`
	NotRun     int `json:"not_run"`
	// Examples is how many contract examples the conversation stage replayed, and
	// ExamplesDeclared how many the package declares: a run that replays fewer than
	// the package declares did not check the package.
	Examples         int `json:"examples_replayed"`
	ExamplesDeclared int `json:"examples_declared"`
}

// Report is what a run produces. It is the same data the JUnit file carries.
type Report struct {
	Tool string `json:"tool"`
	// Result is "pass" or "fail". It is what the exit status says.
	Result  string      `json:"result"`
	Strict  bool        `json:"strict"`
	Package PackageInfo `json:"package"`
	// Targets are the endpoints the run retargeted, by endpoint id (the URLs, which are
	// loopback addresses of the application under test).
	Targets map[string]string `json:"targets,omitempty"`
	Stages  []Stage           `json:"stages"`
	Summary Summary           `json:"summary"`
}

const toolName = "alixai-conformance"

// finish rolls items up into stage statuses and the summary, and sets the result. It
// is the only place a result is decided.
func (r *Report) finish() {
	r.Tool = toolName
	r.Summary = Summary{ExamplesDeclared: r.Summary.ExamplesDeclared, Examples: r.Summary.Examples}
	failed := false
	for i := range r.Stages {
		st := &r.Stages[i]
		if st.Items == nil {
			st.Items = []Item{}
		}
		worst := StatusPass
		for _, it := range st.Items {
			r.Summary.Items++
			switch it.Status {
			case StatusPass:
				r.Summary.Passed++
			case StatusWarn:
				r.Summary.Warnings++
			case StatusFail:
				r.Summary.Failed++
			case StatusUnverified:
				r.Summary.Unverified++
			case StatusNotRun:
				r.Summary.NotRun++
			}
			if it.Status.rank() > worst.rank() {
				worst = it.Status
			}
		}
		// A stage that says it failed, or was not run, is that even with no item to say it.
		if st.Status != "" && st.Status.rank() > worst.rank() {
			worst = st.Status
		}
		if worst.rank() == 5 {
			worst = StatusFail
		}
		st.Status = worst
		if worst.failing(r.Strict) {
			failed = true
		}
	}
	if len(r.Stages) == 0 {
		failed = true
	}
	r.Result = "pass"
	if failed {
		r.Result = "fail"
	}
}

// writeJSON writes the report as indented JSON.
func (r *Report) writeJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// ---- JUnit ----

type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Errors   int          `xml:"errors,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Time     string       `xml:"time,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Time     string      `xml:"time,attr"`
	Cases    []junitCase `xml:"testcase"`
	// SystemOut carries the stage's own message when it has one.
	SystemOut string `xml:"system-out,omitempty"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Skipped   *junitSkipped `xml:"skipped,omitempty"`
	SystemOut string        `xml:"system-out,omitempty"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

// writeJUnit writes the report as JUnit XML: one testsuite per stage, one testcase per
// item. A failed or not-run item is a failure, and so is everything that fails the run
// (under -strict, a warning or an unverified item too), so the XML and the exit status
// always agree. Otherwise an unverified item is a skip, which is what JUnit calls
// something that could not be checked, and a warning is a pass that says so in
// system-out.
func (r *Report) writeJUnit(w io.Writer) error {
	doc := junitSuites{Name: toolName}
	var total time.Duration
	for _, st := range r.Stages {
		suite := junitSuite{Name: st.Name, Time: seconds(time.Duration(st.DurationMS) * time.Millisecond)}
		if st.Message != "" {
			suite.SystemOut = st.Message
		}
		total += time.Duration(st.DurationMS) * time.Millisecond
		// the stage's own time is shared out to nothing: the cases carry none of their
		// own, and the suite carries the whole of it.
		for _, it := range st.Items {
			tc := junitCase{Name: it.Name, Classname: toolName + "." + st.Name, Time: "0"}
			text := itemText(it)
			switch {
			case it.Status.failing(r.Strict):
				tc.Failure = &junitFailure{Message: firstLine(it.Message, string(it.Status)), Type: string(it.Status), Text: text}
				suite.Failures++
			case it.Status == StatusUnverified:
				tc.Skipped = &junitSkipped{Message: firstLine(it.Message, "unverified")}
				suite.Skipped++
			case it.Status == StatusWarn:
				tc.SystemOut = "warning: " + it.Message
			}
			suite.Cases = append(suite.Cases, tc)
		}
		suite.Tests = len(suite.Cases)
		doc.Suites = append(doc.Suites, suite)
		doc.Tests += suite.Tests
		doc.Failures += suite.Failures
		doc.Skipped += suite.Skipped
	}
	doc.Time = seconds(total)
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

func seconds(d time.Duration) string { return fmt.Sprintf("%.3f", d.Seconds()) }

// firstLine is the one-line message of a failure: the item's message cut at its first
// line break, or the fallback when there is none.
func firstLine(msg, fallback string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return fallback
	}
	if i := strings.IndexAny(msg, "\r\n"); i >= 0 {
		msg = msg[:i]
	}
	return msg
}

// itemText is the body of a failure: the message and the details, as JSON.
func itemText(it Item) string {
	var sb strings.Builder
	sb.WriteString(it.Message)
	if len(it.Details) > 0 {
		if b, err := json.MarshalIndent(it.Details, "", "  "); err == nil {
			sb.WriteString("\n")
			sb.Write(b)
		}
	}
	return sb.String()
}
