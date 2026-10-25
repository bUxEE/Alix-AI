package scan

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

var (
	scanComposeNameRE = regexp.MustCompile(`^(docker-|podman-)?compose([.\-][a-z0-9.\-]+)?\.ya?ml$`)
	scanPortItemRE    = regexp.MustCompile(`^\s*-\s*["']?(?:(?:\d{1,3}(?:\.\d{1,3}){3}|\[[0-9a-fA-F:]+\]):)?(?:(\d{1,5})(?:-\d{1,5})?:)?(\d{1,5})(?:/[a-z]+)?["']?\s*$`)
	scanPortHostRE    = regexp.MustCompile(`^\s*-\s*["']?(?:((?:\d{1,3}(?:\.\d{1,3}){3}|\[[0-9a-fA-F:]+\])):)?(?:(\d{1,5})(?:-\d{1,5})?:)?(\d{1,5})`)
	scanVolumeRE      = regexp.MustCompile(`^\s*-\s*["']?([^:"'\s]+):([^:"'\s]+)`)
	scanVolumeDocRE   = regexp.MustCompile(`(?i)(^|[/\\._-])(docs?|documents?|documentation|knowledge|library|manuals|uploads|corpus|kb|wiki)($|[/\\._-])`)
	scanRagProviderRE = regexp.MustCompile(`(?i)\b(qdrant|pgvector|chroma(?:db)?|weaviate|milvus|elasticsearch|opensearch|meilisearch|sharepoint|confluence|notion|google[_-]?drive|minio)\b|s3://`)
	scanRuntimeImgRE  = regexp.MustCompile(`(?i)(localai|ollama|vllm|llama\.cpp|llama-cpp|text-generation|lmstudio|infinity-emb)`)
	scanProviderRE    = regexp.MustCompile(`(?i)(api\.openai\.com|api\.anthropic\.com|generativelanguage\.googleapis\.com|api\.mistral\.ai|api\.groq\.com|openrouter\.ai|api\.cohere\.(?:ai|com)|\b(?:OPENAI|ANTHROPIC|GEMINI|MISTRAL|GROQ|COHERE)_API_KEY\b)`)
	// a quoted route whose last segment, or a segment followed by more, is a health or inspection word: "/health",
	// "/v1/health", "/api/v1/inspect", "/healthz/live". Only quoted paths count, never a word in prose.
	scanHealthRouteRE = regexp.MustCompile("[\"'`](?:/[A-Za-z0-9_.:{}$-]+)*/(health|healthz|ready|readyz|livez|inspect|_health)[\"'`/?]")
	scanSmokeRE       = regexp.MustCompile(`(?i)((create|provision|seed|cleanup)[_ -]?(a[_ -]?)?(disposable[_ -]|test[_ -])?tenant|test_create_tenant|createDisposableTenant|cleanupDisposableTenant)`)
	scanExposeRE      = regexp.MustCompile(`(?i)^\s*EXPOSE\s+(.+)$`)
	scanCreateTableRE = regexp.MustCompile(`(?i)^\s*create\s+table`)
	scanFrontKeyRE    = regexp.MustCompile(`(?i)^(visibility|access|roles|acl|audience)\s*:`)
	scanCitationDocRE = regexp.MustCompile(`(?i)\b(citations?|resolve[sd]? (a )?document)\b`)
	scanDigitsRE      = regexp.MustCompile(`^\d{1,5}$`)
)

type scanRagDecl struct {
	path     string
	complete bool
	citation bool
	scope    bool
}

type scanPortFact struct {
	ports []string
	line  int
}

// detectRAG reads RAG source declarations and finds source candidates in configuration.
func (s *scanState) detectRAG(f *scanFileRec, lines []string) {
	p := f.Path
	if scanIsYAML(p) && (scanInDir(p, "rag-sources") || scanInDir(p, "rag_sources")) {
		keys := map[string]int{}
		for i, l := range lines {
			if scanIndent(l) == 0 && !scanBlankOrComment(l) {
				if k, _, ok := strings.Cut(l, ":"); ok {
					if _, dup := keys[k]; !dup {
						keys[k] = i + 1
					}
				}
			}
		}
		fam := func(names ...string) int {
			for _, n := range names {
				if v := keys[n]; v > 0 {
					return v
				}
			}
			return 0
		}
		d := scanRagDecl{path: p}
		s.add("rag.decl", f, 1, 1, p)
		owner, scope, rules := fam("owner"), fam("application", "scope", "tenant", "workspace"), fam("include", "exclude")
		cred, refresh, del, cit := fam("credential_ref", "credential", "credentials"), fam("sync", "refresh"), fam("deletion"), fam("citation", "citation_resolver")
		d.complete = owner > 0 && scope > 0 && rules > 0 && cred > 0 && refresh > 0 && del > 0 && cit > 0
		d.citation, d.scope = cit > 0, scope > 0
		if d.complete {
			s.add("rag.decl.complete", f, owner, owner, "source declaration with owner, scope, rules, credential reference, refresh, deletion and citation")
		}
		if cit > 0 {
			s.add("rag.citation", f, cit, cit, "citation resolver declared")
		}
		if scope > 0 {
			s.add("rag.scope", f, scope, scope, "source scope declared")
		}
		s.ragDecls = append(s.ragDecls, d)
	}
	if scanIsDocText(p) && len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines) && i < 30 && strings.TrimSpace(lines[i]) != "---"; i++ {
			if scanFrontKeyRE.MatchString(lines[i]) {
				s.add("rag.scope.meta", f, i+1, i+1, lines[i])
				break
			}
		}
	}
	if scanIsDocText(p) {
		for i, l := range lines {
			if scanCitationDocRE.MatchString(l) {
				s.add("rag.citation.doc", f, i+1, i+1, l)
				break
			}
		}
	}
	if scanIsCode(p) || scanIsConfig(p) || scanComposeNameRE.MatchString(scanBase(p)) {
		for i, l := range lines {
			if l != "" && scanRagProviderRE.MatchString(l) {
				s.add("rag.provider", f, i+1, i+1, l)
				break
			}
		}
	}
}

// detectModelAndDeploy reads compose files, Dockerfiles, runtime configuration, provider
// references, health endpoints and test-tenant provisioning.
func (s *scanState) detectModelAndDeploy(f *scanFileRec, lines []string) {
	p := f.Path
	base := scanBase(p)
	code, cfg := scanIsCode(p), scanIsConfig(p)
	isCompose := scanComposeNameRE.MatchString(base)
	if isCompose {
		s.add("compose.file", f, 1, 1, p)
		inPorts, portsIndent := false, 0
		var cports []string
		firstPortLine := 0
		for i, l := range lines {
			if scanBlankOrComment(l) {
				continue
			}
			ind := scanIndent(l)
			t := strings.TrimSpace(l)
			if inPorts && ind <= portsIndent && !strings.HasPrefix(t, "-") {
				inPorts = false
			}
			switch {
			case strings.HasPrefix(t, "ports:"):
				inPorts, portsIndent = true, ind
			case inPorts && strings.HasPrefix(t, "-"):
				if m := scanPortHostRE.FindStringSubmatch(l); m != nil && scanPortItemRE.MatchString(l) {
					host, container := m[2], m[3]
					if host == "" {
						host = "-"
					}
					cports = append(cports, container)
					s.ports[p+": "+host+"->"+container] = true
					if firstPortLine == 0 {
						firstPortLine = i + 1
					}
					s.add("compose.port", f, i+1, i+1, l)
				}
			case inPorts && (strings.HasPrefix(t, "target:") || strings.HasPrefix(t, "published:")):
				if v := strings.Trim(strings.TrimSpace(t[strings.Index(t, ":")+1:]), `"'`); scanDigitsRE.MatchString(v) {
					if strings.HasPrefix(t, "target:") {
						cports = append(cports, v)
					}
					if firstPortLine == 0 {
						firstPortLine = i + 1
					}
					s.add("compose.port", f, i+1, i+1, l)
				}
			case strings.HasPrefix(t, "healthcheck:"):
				s.add("inspect.endpoint", f, i+1, i+1, "compose healthcheck")
			case strings.HasPrefix(t, "image:") && scanRuntimeImgRE.MatchString(t):
				s.add("model.runtime", f, i+1, i+1, l)
			case strings.HasPrefix(t, "-") && scanVolumeRE.MatchString(l) && !inPorts:
				if m := scanVolumeRE.FindStringSubmatch(l); m != nil && (strings.HasPrefix(m[1], ".") || strings.HasPrefix(m[1], "/") || strings.Contains(m[1], "/")) &&
					(scanVolumeDocRE.MatchString(m[1]) || scanVolumeDocRE.MatchString(m[2])) {
					s.add("rag.volume", f, i+1, i+1, l)
				}
			}
		}
		if len(cports) > 0 {
			s.composes[p] = scanPortFact{ports: cports, line: firstPortLine}
		}
	}
	if strings.HasPrefix(base, "dockerfile") {
		for i, l := range lines {
			if m := scanExposeRE.FindStringSubmatch(l); m != nil {
				var ports []string
				for _, tok := range strings.Fields(m[1]) {
					tok = strings.SplitN(tok, "/", 2)[0]
					if scanDigitsRE.MatchString(tok) {
						ports = append(ports, tok)
					}
				}
				if len(ports) > 0 {
					s.exposes[p] = scanPortFact{ports: ports, line: i + 1}
					s.add("deploy.expose", f, i+1, i+1, l)
				}
			}
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(l)), "HEALTHCHECK") {
				s.add("inspect.endpoint", f, i+1, i+1, l)
			}
		}
	}
	if base == "modelfile" {
		for i, l := range lines {
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(l)), "FROM ") {
				s.add("model.runtime", f, i+1, i+1, l)
				break
			}
		}
	}
	if scanIsYAML(p) && !isCompose && scanInDir(p, "models") {
		if _, n, ok := scanTopKey(lines, "backend"); ok {
			s.add("model.runtime", f, n, n, "runtime model configuration")
		}
	}
	if code || cfg || isCompose {
		var gotProv, gotHealth, gotSmoke bool
		for i, l := range lines {
			if l == "" {
				continue
			}
			if !gotProv && scanProviderRE.MatchString(l) {
				gotProv = true
				s.add("egress.provider", f, i+1, i+1, l)
			}
			if code && !gotHealth && scanHealthRouteRE.MatchString(l) {
				gotHealth = true
				s.add("inspect.endpoint", f, i+1, i+1, l)
			}
			if !gotSmoke && scanSmokeRE.MatchString(l) {
				gotSmoke = true
				s.add("smoke.script", f, i+1, i+1, l)
			}
		}
	}
	if scanExt(p) == ".sql" {
		for i, l := range lines {
			if scanCreateTableRE.MatchString(l) {
				s.add("db.schema", f, i+1, i+1, l)
				break
			}
		}
	}
}

// detectDeployConsistency compares what a Dockerfile exposes with what the compose file next to
// it publishes. A Dockerfile and a compose file in the same directory that share no container
// port disagree.
func (s *scanState) detectDeployConsistency() {
	composePaths := make([]string, 0, len(s.composes))
	for p := range s.composes {
		composePaths = append(composePaths, p)
	}
	sort.Strings(composePaths)
	dockerPaths := make([]string, 0, len(s.exposes))
	for p := range s.exposes {
		dockerPaths = append(dockerPaths, p)
	}
	sort.Strings(dockerPaths)
	for _, cp := range composePaths {
		for _, dp := range dockerPaths {
			if path.Dir(cp) != path.Dir(dp) {
				continue
			}
			cf, df := s.composes[cp], s.exposes[dp]
			shared := false
			for _, a := range cf.ports {
				for _, b := range df.ports {
					if a == b {
						shared = true
					}
				}
			}
			if shared {
				continue
			}
			s.add("deploy.conflict", s.byPath[dp], df.line, df.line, "Dockerfile exposes "+strings.Join(df.ports, ", ")+" but compose publishes container port "+strings.Join(cf.ports, ", "))
			s.add("deploy.conflict", s.byPath[cp], cf.line, cf.line, "compose publishes container port "+strings.Join(cf.ports, ", ")+" but the Dockerfile exposes "+strings.Join(df.ports, ", "))
		}
	}
}
