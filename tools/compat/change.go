// Package main is the v1 public-contract compatibility checker (master plan 20.1, 21 M6).
//
//	go run ./tools/compat check            compare the working tree with schemas/_frozen/v1; fail on a breaking change
//	go run ./tools/compat freeze           accept additive and docs-only changes into the frozen set (never a breaking one)
//	go run ./tools/compat compare OLD NEW  compare two frozen trees (CI: base branch against the proposed one)
//
// The frozen set is a snapshot of every public JSON Schema (schemas/<area>/*.json, without examples) and of the merged
// OpenAPI document (schemas/api). A change is classified as:
//
//	breaking   a document or call that was valid against the frozen contract can be rejected, or a guarantee a
//	           consumer could rely on is gone (removed field, new required field, narrower type or enum, removed
//	           path, operation, parameter or response, retargeted $ref, changed authority marker, ...).
//	additive   the contract accepts or offers strictly more (new optional field, new enum value, new path, new
//	           definition, relaxed bound). A minor release; consumers must tolerate growth.
//	docs-only  titles, descriptions, examples and other annotations. A patch release.
//
// Anything the classifier does not understand is breaking: it fails closed.
package main

import (
	"fmt"
	"sort"
	"strings"
)

// Severity orders the classes; a larger value is a bigger change.
type Severity int

const (
	DocsOnly Severity = iota + 1
	Additive
	Breaking
)

func (s Severity) String() string {
	switch s {
	case DocsOnly:
		return "docs-only"
	case Additive:
		return "additive"
	case Breaking:
		return "breaking"
	}
	return "unknown"
}

// Change is one classified difference between the frozen contract and the current one.
type Change struct {
	Severity Severity `json:"-"`
	Class    string   `json:"severity"`
	Subject  string   `json:"subject"` // "schema <area>/<name>" or "openapi"
	Pointer  string   `json:"pointer"` // JSON pointer inside the subject
	Code     string   `json:"code"`    // stable machine name, e.g. removed_field
	Message  string   `json:"message"`
}

func (c Change) String() string {
	return fmt.Sprintf("%-9s %s#%s: %s [%s]", strings.ToUpper(c.Severity.String()), c.Subject, c.Pointer, c.Message, c.Code)
}

// Report is the outcome of one comparison.
type Report struct {
	Changes []Change `json:"changes"`
}

// Count returns how many changes have the severity.
func (r Report) Count(s Severity) int {
	n := 0
	for _, c := range r.Changes {
		if c.Severity == s {
			n++
		}
	}
	return n
}

// Worst is the highest severity present, or 0 for no change at all.
func (r Report) Worst() Severity {
	var w Severity
	for _, c := range r.Changes {
		if c.Severity > w {
			w = c.Severity
		}
	}
	return w
}

// Has reports whether a change with this code exists.
func (r Report) Has(code string) bool {
	for _, c := range r.Changes {
		if c.Code == code {
			return true
		}
	}
	return false
}

func (r *Report) sort() {
	for i := range r.Changes {
		r.Changes[i].Class = r.Changes[i].Severity.String()
	}
	sort.SliceStable(r.Changes, func(i, j int) bool {
		a, b := r.Changes[i], r.Changes[j]
		if a.Severity != b.Severity {
			return a.Severity > b.Severity
		}
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		if a.Pointer != b.Pointer {
			return a.Pointer < b.Pointer
		}
		return a.Code < b.Code
	})
}

// differ collects the changes of one subject.
type differ struct {
	subject string
	out     *[]Change
}

func (d differ) add(sev Severity, ptr, code, format string, args ...any) {
	*d.out = append(*d.out, Change{Severity: sev, Subject: d.subject, Pointer: ptr, Code: code, Message: fmt.Sprintf(format, args...)})
}

// scratch returns a differ that writes into a separate list (used to ask "did anything but docs change?").
func (d differ) scratch() (differ, *[]Change) {
	var l []Change
	return differ{subject: d.subject, out: &l}, &l
}

// esc escapes one JSON pointer token.
func esc(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
