package deploy

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// M5-22: a packaged install can ingest files. RAG over files needs the parser worker (cmd/alixai-parser, ADR 0014),
// a separate executable the service starts once per document and finds beside itself (parse.DefaultWorkerPath). The
// release archives, the service image and the native install steps must all carry it, next to alixai, built the same
// static, trimmed way and WITHOUT the parsertest tag (fault-injection formats), and nothing may relax the sandbox the
// worker installs on itself (Job Object; rlimits, Landlock and seccomp; fail closed). Static checks only: nothing
// here builds an image or an archive. Every detector has inline negative cases, so a rule cannot pass by looking at
// nothing.

const (
	parserShipBinary     = "alixai-parser"
	parserShipMain       = "./cmd/alixai-parser"
	parserShipGoreleaser = ".goreleaser.yaml"
	parserShipNativeDir  = "deploy/native"
	parserShipGuidesDir  = "docs/operations/guides"
	parserShipImageDir   = "/usr/local/bin"
)

// ---- GoReleaser ----

type parserShipBuild struct {
	ID           string   `yaml:"id"`
	Main         string   `yaml:"main"`
	Binary       string   `yaml:"binary"`
	Env          []string `yaml:"env"`
	Flags        []string `yaml:"flags"`
	LDFlags      []string `yaml:"ldflags"`
	Tags         []string `yaml:"tags"`
	Targets      []string `yaml:"targets"`
	ModTimestamp string   `yaml:"mod_timestamp"`
}

type parserShipGoreleaserFile struct {
	Builds   []parserShipBuild `yaml:"builds"`
	Archives []struct {
		ID     string   `yaml:"id"`
		IDs    []string `yaml:"ids"`
		Builds []string `yaml:"builds"`
	} `yaml:"archives"`
}

// parserShipGoreleaserProblems checks that the parser is a build of its own with the rules of the service build, and that
// every archive holds it.
func parserShipGoreleaserProblems(raw []byte) []string {
	var g parserShipGoreleaserFile
	if err := yaml.Unmarshal(raw, &g); err != nil {
		return []string{".goreleaser.yaml: " + err.Error()}
	}
	var svc, worker *parserShipBuild
	for i := range g.Builds {
		switch g.Builds[i].Main {
		case "./cmd/alixai":
			svc = &g.Builds[i]
		case parserShipMain:
			worker = &g.Builds[i]
		}
	}
	if svc == nil {
		return []string{".goreleaser.yaml: the service build (./cmd/alixai) is missing"}
	}
	if worker == nil {
		return []string{".goreleaser.yaml: no build of " + parserShipMain + ": the release archives would not carry the parser worker"}
	}
	var ps []string
	if worker.Binary != parserShipBinary {
		ps = append(ps, fmt.Sprintf(".goreleaser.yaml: the parser build writes binary %q, want %q (the service looks for that name)", worker.Binary, parserShipBinary))
	}
	if worker.ID == "" || worker.ID == svc.ID {
		ps = append(ps, fmt.Sprintf(".goreleaser.yaml: the parser build id %q must be its own", worker.ID))
	}
	if !slices.Contains(worker.Env, "CGO_ENABLED=0") {
		ps = append(ps, fmt.Sprintf(".goreleaser.yaml: parser env %v: the worker must be static (CGO_ENABLED=0)", worker.Env))
	}
	if !slices.Contains(worker.Flags, "-trimpath") {
		ps = append(ps, fmt.Sprintf(".goreleaser.yaml: parser flags %v: want -trimpath", worker.Flags))
	}
	if !strings.Contains(strings.Join(worker.LDFlags, " "), "-s -w") {
		ps = append(ps, fmt.Sprintf(".goreleaser.yaml: parser ldflags %v: want -s -w", worker.LDFlags))
	}
	if len(worker.Tags) != 0 || strings.Contains(strings.Join(worker.Flags, " ")+strings.Join(worker.LDFlags, " "), "parsertest") {
		ps = append(ps, fmt.Sprintf(".goreleaser.yaml: the parser build carries tags %v: the parsertest tag adds fault-injection formats and must never ship", worker.Tags))
	}
	if worker.ModTimestamp == "" || worker.ModTimestamp != svc.ModTimestamp {
		// Without it the file times differ between two GoReleaser runs and the release pipeline's checksum comparison fails (M5-03).
		ps = append(ps, fmt.Sprintf(".goreleaser.yaml: parser mod_timestamp %q must equal the service's %q so two runs give the same bytes", worker.ModTimestamp, svc.ModTimestamp))
	}
	a, b := slices.Clone(worker.Targets), slices.Clone(svc.Targets)
	slices.Sort(a)
	slices.Sort(b)
	if len(a) == 0 || !slices.Equal(a, b) {
		ps = append(ps, fmt.Sprintf(".goreleaser.yaml: parser targets %v differ from the service's %v: every archive needs both", a, b))
	}
	if len(g.Archives) == 0 {
		ps = append(ps, ".goreleaser.yaml: no archive")
	}
	for _, ar := range g.Archives {
		// An empty filter takes every build; a filter must name both.
		for _, filter := range [][]string{ar.IDs, ar.Builds} {
			if len(filter) != 0 && (!slices.Contains(filter, worker.ID) || !slices.Contains(filter, svc.ID)) {
				ps = append(ps, fmt.Sprintf(".goreleaser.yaml: archive %q selects builds %v and leaves out %q or %q", ar.ID, filter, svc.ID, worker.ID))
			}
		}
	}
	sort.Strings(ps)
	return ps
}

// ---- Dockerfile ----

var (
	parserShipFromRE   = regexp.MustCompile(`(?m)^FROM\s+\S+(?:\s+AS\s+(\S+))?`)
	parserShipBuildRE  = regexp.MustCompile(`go build [^\n]*`)
	parserShipWorkerRE = regexp.MustCompile(`\./cmd/alixai-parser(\s|;|\\|$)`)
	parserShipCopyRE   = regexp.MustCompile(`(?m)^COPY\s+--from=build\s+(?:--\S+\s+)*(/out/alixai-parser)\s+(\S+)\s*$`)
	parserShipEntryRE  = regexp.MustCompile(`(?m)^ENTRYPOINT\s+\["([^"]+)"`)
)

// parserShipCode drops the comment lines: a comment may name the parsertest tag to say it is never used.
func parserShipCode(df string) string {
	var kept []string
	for _, l := range strings.Split(df, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// parserShipStages splits a Dockerfile into its stages: name to body.
func parserShipStages(df string) map[string]string {
	out := map[string]string{}
	idx := parserShipFromRE.FindAllStringSubmatchIndex(df, -1)
	for i, m := range idx {
		end := len(df)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		name := ""
		if m[2] >= 0 {
			name = df[m[2]:m[3]]
		}
		out[name] = df[m[0]:end]
	}
	return out
}

// parserShipDockerfileProblems checks that the service stage of the image carries the worker beside the service binary,
// built static and trimmed with no tags, and the forwarder stage does not.
func parserShipDockerfileProblems(df string) []string {
	df = parserShipCode(df)
	var ps []string
	if strings.Contains(df, "parsertest") {
		ps = append(ps, "Dockerfile: the parsertest tag (fault-injection formats) is named: it must never be built into an image")
	}
	var builds []string
	for _, b := range parserShipBuildRE.FindAllString(df, -1) {
		if parserShipWorkerRE.MatchString(b) {
			builds = append(builds, b)
		}
	}
	if len(builds) != 1 {
		ps = append(ps, fmt.Sprintf("Dockerfile: want exactly one go build of %s, found %d", parserShipMain, len(builds)))
	}
	for _, b := range builds {
		switch {
		case !strings.Contains(b, "-trimpath"):
			ps = append(ps, "Dockerfile: the parser build needs -trimpath: "+b)
		case !strings.Contains(b, `-ldflags "-s -w"`):
			ps = append(ps, `Dockerfile: the parser build needs -ldflags "-s -w": `+b)
		case !strings.Contains(b, "-o /out/"+parserShipBinary):
			ps = append(ps, "Dockerfile: the parser build must write /out/"+parserShipBinary+": "+b)
		case strings.Contains(b, "-tags") || strings.Contains(b, "GO_TAGS"):
			ps = append(ps, "Dockerfile: GO_TAGS belongs to the service build; the parser is built with no tags: "+b)
		}
	}
	stages := parserShipStages(df)
	svc, ok := stages["alixai"]
	if !ok {
		return append(ps, "Dockerfile: no alixai stage")
	}
	var dest string
	if m := parserShipCopyRE.FindStringSubmatch(svc); m == nil {
		ps = append(ps, "Dockerfile: the alixai stage does not COPY /out/"+parserShipBinary+": the image would not ingest files")
	} else {
		dest = m[2]
	}
	entry := parserShipEntryRE.FindStringSubmatch(svc)
	switch {
	case entry == nil:
		ps = append(ps, "Dockerfile: the alixai stage has no exec-form ENTRYPOINT")
	case dest != "" && (path.Base(dest) != parserShipBinary || path.Dir(dest) != path.Dir(entry[1])):
		ps = append(ps, fmt.Sprintf("Dockerfile: the worker is at %s but the service at %s: the service looks beside itself", dest, entry[1]))
	case dest != "" && path.Dir(dest) != parserShipImageDir:
		ps = append(ps, fmt.Sprintf("Dockerfile: the worker is at %s, documented as %s/%s", dest, parserShipImageDir, parserShipBinary))
	}
	if strings.Contains(stages["netctl"], parserShipBinary) {
		ps = append(ps, "Dockerfile: the netctl stage carries the parser worker: the forwarder image stays minimal")
	}
	sort.Strings(ps)
	return ps
}

// ---- native install steps ----

var (
	parserShipLinuxInstallRE   = regexp.MustCompile(`(?m)^sudo install -m 0755 alixai alixai-parser (/\S+/)\s`)
	parserShipWindowsCopyRE    = regexp.MustCompile(`(?m)^Copy-Item alixai\.exe, alixai-parser\.exe '([^']+)'`)
	parserShipWindowsServiceRE = regexp.MustCompile(`(?m)^& '([^']+)alixai\.exe' service install`)
	parserShipPlistProgramRE   = regexp.MustCompile(`<key>ProgramArguments</key>\s*<array>\s*<string>([^<]+)</string>`)
)

// parserShipSection returns the body of one "## " section of a Markdown file whose heading starts with prefix.
func parserShipSection(md, prefix string) string {
	for _, sec := range strings.Split("\n"+md, "\n## ")[1:] {
		if strings.HasPrefix(sec, prefix) {
			return sec
		}
	}
	return ""
}

// parserShipNativeProblems checks that each platform's install steps put the worker into the directory of the service
// binary named by the unit, the plist and the service registration, and that no definition runs it by itself or withholds
// what its self-confinement needs.
func parserShipNativeProblems(readme, systemd, plist string) []string {
	var ps []string
	u := parseUnit("alixai.service", []byte(systemd))

	// Linux: the unit's ExecStart binary and the worker are installed into one directory.
	unitBin := strings.Fields(u.get("Service", "ExecStart"))
	linux := parserShipSection(readme, "Linux")
	switch m := parserShipLinuxInstallRE.FindStringSubmatch(linux); {
	case len(unitBin) == 0:
		ps = append(ps, "alixai.service: no ExecStart")
	case m == nil:
		ps = append(ps, "README: the Linux steps do not install alixai-parser with alixai (sudo install -m 0755 alixai alixai-parser <dir>/)")
	case path.Dir(unitBin[0]) != strings.TrimSuffix(m[1], "/"):
		ps = append(ps, fmt.Sprintf("README: the Linux steps install the worker into %s but the unit runs %s: the service looks beside itself", m[1], unitBin[0]))
	}
	for _, key := range []string{"ExecStart", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecReload"} {
		for _, cmd := range u.all("Service", key) {
			if strings.Contains(cmd, parserShipBinary) {
				ps = append(ps, fmt.Sprintf("alixai.service: %s=%q starts the worker: only the service starts it, once per document, inside its own sandbox", key, cmd))
			}
		}
	}

	// macOS: the same against the plist.
	mac := parserShipSection(readme, "macOS")
	pm := parserShipPlistProgramRE.FindStringSubmatch(plist)
	switch m := parserShipLinuxInstallRE.FindStringSubmatch(mac); {
	case pm == nil:
		ps = append(ps, "plist: no ProgramArguments")
	case m == nil:
		ps = append(ps, "README: the macOS steps do not install alixai-parser with alixai")
	case path.Dir(strings.TrimSpace(pm[1])) != strings.TrimSuffix(m[1], "/"):
		ps = append(ps, fmt.Sprintf("README: the macOS steps install the worker into %s but the plist runs %s", m[1], strings.TrimSpace(pm[1])))
	}
	if strings.Count(plist, "<key>ProgramArguments</key>") != 1 || strings.Contains(plist, parserShipBinary+"</string>") {
		ps = append(ps, "plist: the worker must not be a program of the daemon")
	}

	// Windows: one Copy-Item for both executables, into the directory the service is registered from.
	win := parserShipSection(readme, "Windows")
	cm, sm := parserShipWindowsCopyRE.FindStringSubmatch(win), parserShipWindowsServiceRE.FindStringSubmatch(win)
	switch {
	case cm == nil:
		ps = append(ps, "README: the Windows steps do not copy alixai-parser.exe with alixai.exe")
	case sm == nil:
		ps = append(ps, "README: the Windows steps do not register the service from the install directory")
	case strings.TrimRight(cm[1], `\`) != strings.TrimRight(sm[1], `\`):
		ps = append(ps, fmt.Sprintf(`README: the Windows steps copy the worker into %s but register the service from %s`, cm[1], sm[1]))
	}

	// The worker's own sandbox (ADR 0014) is installed by the worker and needs the parent's unit to leave its system
	// calls alone and to give it nothing more: no capability, no privilege gain, and no deny entry that would stop
	// it from being spawned, limited or confined. A tightening of these is a change to the worker's sandbox, not to the unit.
	for key, want := range map[string]string{"NoNewPrivileges": "yes", "CapabilityBoundingSet": "", "AmbientCapabilities": ""} {
		if !u.has("Service", key) || u.get("Service", key) != want {
			ps = append(ps, fmt.Sprintf("alixai.service: %s = %q, want %q: the worker gets no capability the service does not have", key, u.get("Service", key), want))
		}
	}
	filter := strings.Fields(strings.ReplaceAll(strings.Join(u.all("Service", "SystemCallFilter"), " "), "~", " "))
	for _, name := range []string{
		"@sandbox", "@resources", "@process", "@default", "seccomp", "prctl", "setrlimit", "prlimit64",
		"landlock_create_ruleset", "landlock_add_rule", "landlock_restrict_self", "execve", "execveat", "clone", "clone3",
	} {
		if slices.Contains(filter, name) {
			ps = append(ps, fmt.Sprintf("alixai.service: SystemCallFilter denies %s, which the worker needs to be started, limited or confined", name))
		}
	}
	sort.Strings(ps)
	return ps
}

// parserShipConfigProblems: a shipped configuration may name where the worker is, and nothing else about it. Its limits
// are the defaults the sandbox was validated with (ADR 0014); loosening them is an operator's decision, not a package's.
func parserShipConfigProblems(name string, raw []byte) []string {
	var doc struct {
		RagIngest struct {
			Parser map[string]any `yaml:"parser"`
		} `yaml:"rag_ingest"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return []string{name + ": " + err.Error()}
	}
	var ps []string
	for k := range doc.RagIngest.Parser {
		if k != "worker_path" {
			ps = append(ps, fmt.Sprintf("%s: rag_ingest.parser.%s is set: a shipped configuration keeps the worker's sandbox limits at their defaults", name, k))
		}
	}
	sort.Strings(ps)
	return ps
}

// ---- the container stack ----

// parserShipStackProblems: the worker runs inside the alixai container, so the container must keep the writable /tmp its
// working directory needs and must not trade away the confinement it shares with the worker.
func parserShipStackProblems(compose, quadlet string) []string {
	var ps []string
	c, err := parseCompose("compose.yaml", []byte(compose))
	if err != nil {
		return []string{"compose.yaml: " + err.Error()}
	}
	svc := c.services["alixai"]
	if svc == nil {
		return []string{"compose.yaml: no alixai service"}
	}
	if !anyContains(listStrings(svc["tmpfs"]), "/tmp") {
		ps = append(ps, "compose.yaml: the alixai service has no /tmp tmpfs: the worker's working directory is created there")
	}
	if !contains(listStrings(svc["cap_drop"]), "ALL") || len(listStrings(svc["cap_add"])) != 0 {
		ps = append(ps, "compose.yaml: the alixai service must drop every capability and add none")
	}
	if anyContains(listStrings(svc["security_opt"]), "unconfined") || svc["privileged"] == true {
		ps = append(ps, "compose.yaml: the alixai service weakens its confinement (unconfined seccomp or privileged): the worker's Landlock and seccomp layer would not hold")
	}
	q := parseUnit("alixai.container", []byte(quadlet))
	if !strings.HasPrefix(q.get("Container", "Tmpfs"), "/tmp") {
		ps = append(ps, "alixai.container: no /tmp tmpfs for the worker's working directory")
	}
	if !strings.EqualFold(q.get("Container", "DropCapability"), "ALL") || q.has("Container", "AddCapability") ||
		strings.Contains(strings.ToLower(q.get("Container", "SeccompProfile")), "unconfined") {
		ps = append(ps, "alixai.container: must drop every capability, add none and keep the default seccomp profile")
	}
	sort.Strings(ps)
	return ps
}

// ---- the guides ----

// parserShipGuideProblems: the guides say where the worker lives, and none of them still says it is not packaged.
func parserShipGuideProblems(files map[string]string) []string {
	var ps []string
	idx := files["README.md"]
	for _, want := range []string{"/usr/local/bin/alixai-parser", `C:\Program Files\AlixAI\alixai-parser.exe`, "archive"} {
		if !strings.Contains(idx, want) {
			ps = append(ps, fmt.Sprintf("guides/README.md does not say where the worker lives: %q is missing", want))
		}
	}
	for _, name := range []string{"local-machine.md", "server.md", "troubleshooting.md"} {
		if !strings.Contains(files[name], parserShipBinary) {
			ps = append(ps, fmt.Sprintf("guides/%s does not mention %s", name, parserShipBinary))
		}
	}
	for name, body := range files {
		for _, stale := range []string{"is not packaged", "build `cmd/alixai` only", "carry it yet", "does not work from a packaged install yet"} {
			if strings.Contains(body, stale) {
				ps = append(ps, fmt.Sprintf("guides/%s still says %q", name, stale))
			}
		}
	}
	sort.Strings(ps)
	return ps
}

// ---- the tests ----

// TestPackagedInstallsShipTheParserWorker (M5-22): the release archives, the service image and the native install steps
// all carry alixai-parser next to alixai, so RAG file ingestion works from a packaged install and not only from a source
// checkout, with the worker's sandbox limits (ADR 0014) untouched.
func TestPackagedInstallsShipTheParserWorker(t *testing.T) {
	goreleaser := string(readRepoFile(t, parserShipGoreleaser))
	df := string(readRepoFile(t, dockerfile))
	readme := string(readRepoFile(t, parserShipNativeDir+"/README.md"))
	systemd := string(readRepoFile(t, parserShipNativeDir+"/systemd/alixai.service"))
	plist := string(readRepoFile(t, parserShipNativeDir+"/launchd/ai.alixai.service.plist"))
	composeYAML := string(readRepoFile(t, stackDir+"/compose.yaml"))
	quadlet := string(readRepoFile(t, quadletDir+"/alixai.container"))
	ignore := string(readRepoFile(t, "deploy/container/alixai/Dockerfile.dockerignore"))
	guides := map[string]string{}
	for _, f := range repoGlob(t, parserShipGuidesDir, "*.md") {
		guides[path.Base(f)] = string(readRepoFile(t, f))
	}
	configs := map[string]string{}
	for _, f := range []string{"systemd", "launchd", "windows"} {
		configs[parserShipNativeDir+"/"+f+"/alixai.yaml"] = string(readRepoFile(t, parserShipNativeDir+"/"+f+"/alixai.yaml"))
	}
	configs[stackDir+"/config/alixai.yaml"] = string(readRepoFile(t, stackDir+"/config/alixai.yaml"))

	t.Run("the release archives", func(t *testing.T) {
		reportProblems(t, parserShipGoreleaserProblems([]byte(goreleaser)))
		for name, edit := range map[string][2]string{
			"no parser build":            {"main: ./cmd/alixai-parser", "main: ./cmd/alixai-parser-gone"},
			"wrong binary name":          {"binary: alixai-parser", "binary: parser"},
			"parsertest tag":             {"binary: alixai-parser\n", "binary: alixai-parser\n    tags: [parsertest]\n"},
			"not static":                 {"  - id: alixai-parser\n    main: ./cmd/alixai-parser\n    binary: alixai-parser\n    env:\n      - CGO_ENABLED=0", "  - id: alixai-parser\n    main: ./cmd/alixai-parser\n    binary: alixai-parser\n    env:\n      - CGO_ENABLED=1"},
			"no -s -w":                   {"    ldflags:\n      - -s -w\n    mod_timestamp", "    ldflags:\n      - -X main.x=1\n    mod_timestamp"},
			"no timestamp":               {"      - -s -w\n    mod_timestamp: \"{{ .CommitTimestamp }}\"\n    targets:\n      - linux_amd64\n      - linux_arm64\n      - windows_amd64\n      - darwin_arm64\n\narchives", "      - -s -w\n    targets:\n      - linux_amd64\n      - linux_arm64\n      - windows_amd64\n      - darwin_arm64\n\narchives"},
			"fewer targets":              {"      - darwin_arm64\n\narchives", "\narchives"},
			"archive without the worker": {"  - id: alixai\n    formats", "  - id: alixai\n    ids: [alixai]\n    formats"},
		} {
			if !strings.Contains(goreleaser, edit[0]) {
				t.Fatalf("%s: the edit does not apply to .goreleaser.yaml (the fixture moved)", name)
			}
			if len(parserShipGoreleaserProblems([]byte(strings.Replace(goreleaser, edit[0], edit[1], 1)))) == 0 {
				t.Errorf("%s went unnoticed", name)
			}
		}
	})

	t.Run("the service image", func(t *testing.T) {
		reportProblems(t, parserShipDockerfileProblems(df))
		for _, want := range []string{"!cmd", "!internal"} {
			if !strings.Contains(ignore, want+"\n") {
				t.Errorf("Dockerfile.dockerignore does not allow %s into the build context: the worker's sources would be missing", want)
			}
		}
		copyLine := "COPY --from=build /out/alixai-parser /usr/local/bin/alixai-parser\n"
		netctlCopy := "COPY --from=build /out/alixai-netctl /usr/local/bin/alixai-netctl\n"
		for name, edit := range map[string][2]string{
			"no parser build":         {"./cmd/alixai-parser;", "./cmd/alixai-parser-gone;"},
			"no trimpath":             {"go build -trimpath -buildvcs=false -ldflags \"-s -w\" -o /out/alixai-parser", "go build -buildvcs=false -ldflags \"-s -w\" -o /out/alixai-parser"},
			"no -s -w":                {"-ldflags \"-s -w\" -o /out/alixai-parser", "-o /out/alixai-parser"},
			"service tags":            {"-ldflags \"-s -w\" -o /out/alixai-parser", "-tags \"$GO_TAGS\" -ldflags \"-s -w\" -o /out/alixai-parser"},
			"parsertest tag":          {"-ldflags \"-s -w\" -o /out/alixai-parser", "-tags parsertest -ldflags \"-s -w\" -o /out/alixai-parser"},
			"not copied":              {copyLine, ""},
			"copied elsewhere":        {copyLine, "COPY --from=build /out/alixai-parser /opt/alixai-parser\n"},
			"copied to the forwarder": {netctlCopy, netctlCopy + copyLine},
		} {
			if !strings.Contains(df, edit[0]) {
				t.Fatalf("%s: the edit does not apply to the Dockerfile (the fixture moved)", name)
			}
			if len(parserShipDockerfileProblems(strings.Replace(df, edit[0], edit[1], 1))) == 0 {
				t.Errorf("%s went unnoticed", name)
			}
		}
		// "copied to the forwarder" must also keep the service's own copy, or the first check would hide the second.
		if got := parserShipDockerfileProblems(strings.Replace(df, netctlCopy, netctlCopy+copyLine, 1)); !anyContains(got, "netctl stage") {
			t.Errorf("a worker in the forwarder stage is not reported as such: %v", got)
		}
	})

	t.Run("the native install steps", func(t *testing.T) {
		reportProblems(t, parserShipNativeProblems(readme, systemd, plist))
		for name, edit := range map[string][3]string{
			"linux install without the worker": {"readme", "sudo install -m 0755 alixai alixai-parser /usr/local/bin/ ", "sudo install -m 0755 alixai /usr/local/bin/ "},
			"worker in another directory":      {"readme", "sudo install -m 0755 alixai alixai-parser /usr/local/bin/ ", "sudo install -m 0755 alixai alixai-parser /opt/alixai/ "},
			"windows copy without the worker":  {"readme", "Copy-Item alixai.exe, alixai-parser.exe ", "Copy-Item alixai.exe "},
			"windows split directories":        {"readme", "Copy-Item alixai.exe, alixai-parser.exe 'C:\\Program Files\\AlixAI\\'", "Copy-Item alixai.exe, alixai-parser.exe 'C:\\Tools\\'"},
			"unit starts the worker":           {"systemd", "ExecStart=/usr/local/bin/alixai serve", "ExecStartPre=/usr/local/bin/alixai-parser\nExecStart=/usr/local/bin/alixai serve"},
			"unit binary moved":                {"systemd", "ExecStart=/usr/local/bin/alixai serve", "ExecStart=/opt/alixai/alixai serve"},
			"capability for the worker":        {"systemd", "AmbientCapabilities=\n", "AmbientCapabilities=CAP_SYS_ADMIN\n"},
			"landlock denied":                  {"systemd", "SystemCallFilter=~@mount", "SystemCallFilter=~landlock_restrict_self @mount"},
			"resource limits denied":           {"systemd", "SystemCallFilter=~@mount", "SystemCallFilter=~@resources @mount"},
			"plist binary moved":               {"plist", "<string>/usr/local/bin/alixai</string>", "<string>/opt/alixai/alixai</string>"},
			"macos install without the worker": {"readme", "sudo install -m 0755 alixai alixai-parser /usr/local/bin/         # the worker must sit beside the service binary\nsudo install -d -m 0700 -o _alixai", "sudo install -m 0755 alixai /usr/local/bin/alixai\nsudo install -d -m 0700 -o _alixai"},
		} {
			in := map[string]string{"readme": readme, "systemd": systemd, "plist": plist}
			if !strings.Contains(in[edit[0]], edit[1]) {
				t.Fatalf("%s: the edit does not apply to %s (the fixture moved)", name, edit[0])
			}
			in[edit[0]] = strings.Replace(in[edit[0]], edit[1], edit[2], 1)
			if len(parserShipNativeProblems(in["readme"], in["systemd"], in["plist"])) == 0 {
				t.Errorf("%s went unnoticed", name)
			}
		}
	})

	t.Run("the sandbox limits stay at their defaults", func(t *testing.T) {
		names := make([]string, 0, len(configs))
		for n := range configs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			reportProblems(t, parserShipConfigProblems(n, []byte(configs[n])))
		}
		for name, doc := range map[string]string{
			"wall timeout raised": "rag_ingest:\n  parser:\n    wall_timeout: 1h\n",
			"concurrency raised":  "rag_ingest:\n  parser:\n    max_concurrent: 64\n    worker_path: /usr/local/bin/alixai-parser\n",
			"startup raised":      "rag_ingest:\n  parser:\n    startup_timeout: 10m\n",
		} {
			if len(parserShipConfigProblems(name, []byte(doc))) == 0 {
				t.Errorf("%s went unnoticed", name)
			}
		}
		if got := parserShipConfigProblems("ok", []byte("rag_ingest:\n  parser:\n    worker_path: /usr/local/bin/alixai-parser\n")); len(got) != 0 {
			t.Errorf("naming the worker is allowed: %v", got)
		}

		reportProblems(t, parserShipStackProblems(composeYAML, quadlet))
		for name, edit := range map[string][3]string{
			"no tmpfs":           {"compose", "      - /tmp:rw,noexec,nosuid,nodev,size=64m\n    read_only: true\n    depends_on:\n      anchor:", "      - /run/other\n    read_only: true\n    depends_on:\n      anchor:"},
			"unconfined seccomp": {"compose", "      - no-new-privileges:true\n    cap_drop:\n      - ALL\n    mem_limit: 512m", "      - no-new-privileges:true\n      - seccomp:unconfined\n    cap_drop:\n      - ALL\n    mem_limit: 512m"},
			"cap added":          {"compose", "    cap_drop:\n      - ALL\n    mem_limit: 512m", "    cap_drop:\n      - ALL\n    cap_add:\n      - SYS_ADMIN\n    mem_limit: 512m"},
			"quadlet unconfined": {"quadlet", "DropCapability=ALL\n", "DropCapability=ALL\nSeccompProfile=unconfined\n"},
			"quadlet no tmpfs":   {"quadlet", "Tmpfs=/tmp:rw,noexec,nosuid,nodev,size=64m\n", ""},
		} {
			in := map[string]string{"compose": composeYAML, "quadlet": quadlet}
			if !strings.Contains(in[edit[0]], edit[1]) {
				t.Fatalf("%s: the edit does not apply to %s (the fixture moved)", name, edit[0])
			}
			in[edit[0]] = strings.Replace(in[edit[0]], edit[1], edit[2], 1)
			if len(parserShipStackProblems(in["compose"], in["quadlet"])) == 0 {
				t.Errorf("%s went unnoticed", name)
			}
		}
	})

	t.Run("the guides say where it lives", func(t *testing.T) {
		reportProblems(t, parserShipGuideProblems(guides))
		stale := map[string]string{}
		for k, v := range guides {
			stale[k] = v
		}
		stale["README.md"] += "\nThe document parser is not packaged.\n"
		if len(parserShipGuideProblems(stale)) == 0 {
			t.Error("a guide that still says the parser is not packaged went unnoticed")
		}
		stale = map[string]string{"README.md": "", "local-machine.md": "", "server.md": "", "troubleshooting.md": ""}
		if len(parserShipGuideProblems(stale)) == 0 {
			t.Error("guides that do not mention the worker went unnoticed")
		}
	})
}
