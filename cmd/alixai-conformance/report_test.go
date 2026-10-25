package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/apppkg"
)

func stageOf(name string, statuses ...Status) Stage {
	st := Stage{Name: name}
	for i, s := range statuses {
		st.Items = append(st.Items, Item{ID: name + "." + string(rune('a'+i)), Name: name + " item " + string(rune('a'+i)), Status: s, Message: string(s) + " message"})
	}
	return st
}

func TestReportResultFollowsTheStatuses(t *testing.T) {
	for _, c := range []struct {
		name       string
		stages     []Stage
		strict     bool
		want       string
		wantStatus map[string]Status
	}{
		{"all pass", []Stage{stageOf("a", StatusPass, StatusPass)}, false, "pass", map[string]Status{"a": StatusPass}},
		{"warnings pass", []Stage{stageOf("a", StatusPass, StatusWarn)}, false, "pass", map[string]Status{"a": StatusWarn}},
		{"unverified passes without -strict, and is named", []Stage{stageOf("a", StatusPass, StatusUnverified)}, false, "pass", map[string]Status{"a": StatusUnverified}},
		{"warnings fail under -strict", []Stage{stageOf("a", StatusWarn)}, true, "fail", nil},
		{"unverified fails under -strict", []Stage{stageOf("a", StatusUnverified)}, true, "fail", nil},
		{"a failure fails", []Stage{stageOf("a", StatusPass, StatusFail)}, false, "fail", map[string]Status{"a": StatusFail}},
		{"an item that did not run fails", []Stage{stageOf("a", StatusPass, StatusNotRun)}, false, "fail", map[string]Status{"a": StatusNotRun}},
		{"a stage that did not run fails whatever its items say", []Stage{{Name: "a", Status: StatusNotRun, Items: []Item{{ID: "x", Name: "x", Status: StatusPass}}}}, false, "fail", map[string]Status{"a": StatusNotRun}},
		{"no stage is not a pass", nil, false, "fail", nil},
		{"an unknown status fails", []Stage{stageOf("a", Status("maybe"))}, false, "fail", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &Report{Stages: c.stages, Strict: c.strict}
			r.finish()
			if r.Result != c.want {
				t.Fatalf("result = %s, want %s", r.Result, c.want)
			}
			for _, st := range r.Stages {
				if want, ok := c.wantStatus[st.Name]; ok && st.Status != want {
					t.Errorf("stage %s = %s, want %s", st.Name, st.Status, want)
				}
			}
		})
	}
}

func TestReportSummaryCountsEveryItem(t *testing.T) {
	r := &Report{Stages: []Stage{
		stageOf("a", StatusPass, StatusPass, StatusWarn),
		stageOf("b", StatusFail, StatusUnverified, StatusNotRun),
	}}
	r.Summary.ExamplesDeclared, r.Summary.Examples = 5, 4
	r.finish()
	s := r.Summary
	if s.Items != 6 || s.Passed != 2 || s.Warnings != 1 || s.Failed != 1 || s.Unverified != 1 || s.NotRun != 1 || s.ExamplesDeclared != 5 || s.Examples != 4 {
		t.Fatalf("%+v", s)
	}
}

func TestReportFormats(t *testing.T) {
	r := &Report{Package: PackageInfo{Path: "p", ApplicationID: "app"}, Stages: []Stage{
		stageOf("a", StatusPass, StatusWarn, StatusUnverified, StatusFail),
		{Name: "b", Status: StatusNotRun, Message: "a stage <message> & more", Items: []Item{{ID: "b.x", Name: "b <x> & y", Status: StatusNotRun, Message: "first line\nsecond line", Details: map[string]any{"k": "v"}}}},
	}}
	r.finish()

	var js bytes.Buffer
	if err := r.writeJSON(&js); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(js.Bytes(), &back); err != nil || back.Result != "fail" || len(back.Stages) != 2 || back.Tool != toolName {
		t.Fatalf("JSON does not round-trip: %v\n%s", err, js.String())
	}
	if strings.Contains(js.String(), "\\u003c") {
		t.Errorf("the JSON escapes HTML: %s", js.String())
	}

	var x bytes.Buffer
	if err := r.writeJUnit(&x); err != nil {
		t.Fatal(err)
	}
	var doc junitSuites
	if err := xml.Unmarshal(x.Bytes(), &doc); err != nil {
		t.Fatalf("JUnit is not XML: %v\n%s", err, x.String())
	}
	if doc.Tests != 5 || doc.Failures != 2 || doc.Skipped != 1 || len(doc.Suites) != 2 {
		t.Fatalf("junit totals: %+v", doc)
	}
	a := doc.Suites[0]
	if a.Cases[0].Failure != nil || a.Cases[1].SystemOut == "" || a.Cases[2].Skipped == nil || a.Cases[3].Failure == nil {
		t.Errorf("suite a: %+v", a.Cases)
	}
	b := doc.Suites[1]
	if b.Cases[0].Name != "b <x> & y" || b.Cases[0].Failure == nil || b.Cases[0].Failure.Message != "first line" || !strings.Contains(b.Cases[0].Failure.Text, "second line") {
		t.Errorf("suite b: %+v", b.Cases[0])
	}
	if b.SystemOut != "a stage <message> & more" {
		t.Errorf("suite b system-out = %q", b.SystemOut)
	}

	// strict: the warning and the unverified item are failures
	r.Strict = true
	var sx bytes.Buffer
	if err := r.writeJUnit(&sx); err != nil {
		t.Fatal(err)
	}
	var strict junitSuites
	if err := xml.Unmarshal(sx.Bytes(), &strict); err != nil || strict.Failures != 4 || strict.Skipped != 0 {
		t.Fatalf("strict junit: %v %+v", err, strict)
	}
}

func TestRetargetPointsTheManifestAtTheApplicationUnderTest(t *testing.T) {
	dir := t.TempDir()
	if err := copyPackage(clinicPackage, dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "alixai.yaml")
	if err := retarget(path, map[string]string{"clinic-mcp": "http://127.0.0.1:45678/mcp"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "url: http://127.0.0.1:45678/mcp") || !strings.Contains(text, "- 45678") || strings.Contains(text, "8787") {
		t.Fatalf("both places the manifest names the destination must move together:\n%s", text)
	}
	if err := retarget(path, map[string]string{"no-such-endpoint": "http://127.0.0.1:1/"}); err == nil {
		t.Error("an endpoint the package does not declare must be refused")
	}
	if err := retarget(path, map[string]string{"clinic-mcp": "not a url"}); err == nil {
		t.Error("a target that is not a url must be refused")
	}
}

// The copy that is installed is signed with a key made for the run and verifies under the
// throwaway deployment's trust store, loads as the same package and answers at the target.
func TestThrowawayDeploymentInstallsTheRetargetedCopy(t *testing.T) {
	src, err := apppkg.FromPath(clinicPackage)
	if err != nil {
		t.Fatal(err)
	}
	orig, err := apppkg.Load(t.Context(), src, apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil || orig.Package == nil {
		t.Fatalf("load the clinic package: %v %v", err, orig)
	}
	d, err := newDeployment(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	copyDir := filepath.Join(t.TempDir(), "package")
	if err := os.MkdirAll(copyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const target = "http://127.0.0.1:45679/mcp"
	keyID := string(orig.Package.Manifest.Integrity.PublisherKeyIDs[0])
	if err := d.prepareCopy(clinicPackage, copyDir, "clinic-booking", string(orig.Package.Manifest.Package.Version), keyID, map[string]string{"clinic-mcp": target}); err != nil {
		t.Fatal(err)
	}
	inst, err := d.install(t.Context(), copyDir, keyID, "clinic-booking")
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != "quarantined" || inst.Application != "clinic-booking" || inst.ManifestHash == "" || inst.ManifestHash == orig.ManifestHash {
		t.Errorf("installed = %+v: the copy is a different signed package from the original (%s)", inst, orig.ManifestHash)
	}
	pkg, err := loadPackage(t.Context(), copyDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := endpointURL(pkg.Manifest.Endpoints["clinic-mcp"]); got != target {
		t.Errorf("endpoint = %s, want %s", got, target)
	}
	// the original is untouched
	if got := endpointURL(orig.Package.Manifest.Endpoints["clinic-mcp"]); got == target {
		t.Errorf("the original package was changed")
	}
}
