package dr

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// The documents of this task name commands. This test holds every one of them to the binary the drill
// built: a command that is not registered, or a flag that the command's own -h does not list, fails
// the test. Commands in a shell block are only the ones that are checked here (alixai, the release
// tool, curl against the endpoints the drill reads, and the drill's own go test line): a document that
// needs another program says so in prose, outside a command block.

var (
	fenceRE  = regexp.MustCompile("(?s)```([a-z]*)\n(.*?)```")
	inlineRE = regexp.MustCompile("`((?:alixai|go run \\./tools/release) [^`\n]+)`")
	spanRE   = regexp.MustCompile("`([a-z][^`\\n]*)`")
	helpRow  = regexp.MustCompile(`(?m)^  ([a-z]+(?: [a-z-]+)?)\s{2,}\S`)
)

// documentedEndpoints are the paths a document may curl: the drill reads each of them from a running
// service (TestDRMonitoringSignals).
var documentedEndpoints = map[string]bool{"/healthz": true, "/readyz": true, "/metrics": true, "/diagnostics": true}

type docCommand struct {
	file string
	line string
}

func TestDRDocumentedCommandsExist(t *testing.T) {
	e := sharedEnv(t)
	docs := []string{
		filepath.Join(e.moduleRoot, "docs", "operations", "production.md"),
		filepath.Join(e.moduleRoot, "docs", "evidence", "m6-dr", "report.md"),
	}
	help := execProgram(time.Minute, e.moduleRoot, e.bin, "help")
	known := map[string]bool{}
	for _, m := range helpRow.FindAllStringSubmatch(help.Stdout+help.Stderr, -1) {
		known[m[1]] = true
	}
	if len(known) < 20 {
		t.Fatalf("could not read the command list from `alixai help`:\n%s", help)
	}
	releaseHelp := execProgram(time.Minute, e.moduleRoot, e.releaseBin, "help")
	releaseCmds := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z-]+)\s{2,}\S`).FindAllStringSubmatch(releaseHelp.Stdout+releaseHelp.Stderr, -1) {
		releaseCmds[m[1]] = true
	}

	checked := 0
	flagsCache := map[string]string{}
	flagsOf := func(program string, words ...string) string {
		key := program + " " + strings.Join(words, " ")
		if s, ok := flagsCache[key]; ok {
			return s
		}
		r := execProgram(time.Minute, e.moduleRoot, program, append(append([]string{}, words...), "-h")...)
		if r.Code != 0 {
			t.Errorf("`%s -h` exits %d:\n%s", key, r.Code, r)
		}
		flagsCache[key] = r.Stdout + r.Stderr
		return flagsCache[key]
	}

	for _, path := range docs {
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) && os.Getenv("ALIXAI_DR_EVIDENCE_DIR") != "" && strings.HasSuffix(filepath.ToSlash(path), "m6-dr/report.md") {
			continue // this run writes the report; the next run checks it
		}
		if err != nil {
			t.Fatalf("a document of this task is missing: %v", err)
		}
		text := string(raw)
		var cmds []docCommand
		for _, m := range fenceRE.FindAllStringSubmatch(text, -1) {
			if m[1] != "sh" && m[1] != "bash" && m[1] != "console" {
				continue
			}
			for _, line := range logicalLines(m[2]) {
				cmds = append(cmds, docCommand{file: path, line: line})
				if !allowedBlockLine(line) {
					t.Errorf("%s: the shell block has a command the drill does not check: %q", filepath.Base(path), line)
				}
			}
		}
		for _, m := range inlineRE.FindAllStringSubmatch(text, -1) {
			cmds = append(cmds, docCommand{file: path, line: m[1]})
		}
		// A bare command name in an inline span ("backup create -dir DIR", "trust revoke-key") is checked
		// as the alixai command it names.
		for _, m := range spanRE.FindAllStringSubmatch(text, -1) {
			words := strings.Fields(m[1])
			switch {
			case len(words) >= 2 && known[words[0]+" "+words[1]]:
				cmds = append(cmds, docCommand{file: path, line: "alixai " + m[1]})
			case words[0] == "serve" && known["serve"], words[0] == "version" && known["version"]:
				cmds = append(cmds, docCommand{file: path, line: "alixai " + m[1]})
			}
		}

		for _, c := range cmds {
			line := strings.TrimPrefix(c.line, "$ ")
			switch {
			case strings.HasPrefix(line, "curl "):
				checkCurl(t, c, line)
			case strings.HasPrefix(line, "go test "):
				if !strings.Contains(line, "./tests/dr/") {
					t.Errorf("%s: a go test line that is not the drill: %q", filepath.Base(c.file), line)
				}
			case strings.HasPrefix(line, "ALIXAI_DR_EVIDENCE_DIR="):
				if !strings.Contains(line, "go test ") || !strings.Contains(line, "./tests/dr/") {
					t.Errorf("%s: an evidence line that is not the drill: %q", filepath.Base(c.file), line)
				}
			case strings.HasPrefix(line, "go run ./tools/release "):
				words := strings.Fields(strings.TrimPrefix(line, "go run ./tools/release "))
				if len(words) == 0 || !releaseCmds[words[0]] {
					t.Errorf("%s: %q is not a command of tools/release", filepath.Base(c.file), line)
					continue
				}
				checkFlags(t, c, words[1:], flagsOf(e.releaseBin, words[0]))
				checked++
			case strings.HasPrefix(line, "alixai "):
				words := strings.Fields(strings.TrimPrefix(line, "alixai "))
				name := ""
				switch {
				case len(words) >= 2 && known[words[0]+" "+words[1]]:
					name, words = words[0]+" "+words[1], words[2:]
				case len(words) >= 1 && known[words[0]]:
					name, words = words[0], words[1:]
				default:
					t.Errorf("%s: %q is not a command of the built alixai", filepath.Base(c.file), line)
					continue
				}
				checkFlags(t, c, words, flagsOf(e.bin, strings.Fields(name)...))
				checked++
			}
		}
	}
	if checked < 10 {
		t.Fatalf("only %d command lines were checked: the documents do not say how to operate the product", checked)
	}
	var names []string
	for k := range flagsCache {
		names = append(names, strings.TrimPrefix(strings.TrimPrefix(k, e.bin+" "), e.releaseBin+" release "))
	}
	sort.Strings(names)
	t.Logf("checked %d command lines against %d commands: %s", checked, len(names), strings.Join(names, ", "))
}

// logicalLines joins the continuation lines of a shell block and drops comments and blank lines.
func logicalLines(block string) []string {
	var out []string
	var cur strings.Builder
	for _, l := range strings.Split(block, "\n") {
		l = strings.TrimRight(l, " \t\r")
		if cur.Len() == 0 && (strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#")) {
			continue
		}
		if i := strings.Index(l, " # "); i >= 0 {
			l = l[:i]
		}
		if strings.HasSuffix(l, "\\") {
			cur.WriteString(strings.TrimSuffix(l, "\\") + " ")
			continue
		}
		cur.WriteString(l)
		out = append(out, strings.TrimSpace(cur.String()))
		cur.Reset()
	}
	return out
}

func allowedBlockLine(line string) bool {
	line = strings.TrimPrefix(line, "$ ")
	for _, p := range []string{"alixai ", "go run ./tools/release ", "curl ", "go test ", "ALIXAI_DR_EVIDENCE_DIR="} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// checkFlags requires every flag a documented command line uses to be listed by the command's -h.
func checkFlags(t *testing.T, c docCommand, args []string, help string) {
	t.Helper()
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			continue
		}
		name := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
		if name == "" || name == "h" || name == "help" {
			continue
		}
		if !regexp.MustCompile(`(?m)^\s+-` + regexp.QuoteMeta(name) + `(\s|$)`).MatchString(help) {
			t.Errorf("%s: `%s` uses -%s, which the command's -h does not list:\n%s", filepath.Base(c.file), c.line, name, help)
		}
	}
}

func checkCurl(t *testing.T, c docCommand, line string) {
	t.Helper()
	for _, w := range strings.Fields(line) {
		w = strings.Trim(w, `"'`)
		if !strings.HasPrefix(w, "http://") && !strings.HasPrefix(w, "https://") {
			continue
		}
		u, err := url.Parse(w)
		if err != nil {
			t.Errorf("%s: %q is not a URL", filepath.Base(c.file), w)
			continue
		}
		if !documentedEndpoints[u.Path] {
			t.Errorf("%s: `%s` reads %s, which the drill does not read from a running service", filepath.Base(c.file), line, u.Path)
		}
	}
}
