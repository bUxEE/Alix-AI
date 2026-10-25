package scan

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// Document flag bits, per documentation file.
const (
	scanDocFields uint8 = 1 << iota
	scanDocErrors
	scanDocRisk
	scanDocConfirm
)

var (
	scanCandidateRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.\-]{2,63}$`)
	scanCapIDRE      = regexp.MustCompile(`^[a-z][a-z0-9_.\-]{0,63}$`)
	scanDocFieldsRE  = regexp.MustCompile(`(?i)(^\s*\|.*\b(fields?|parameters?|params?|properties|name)\b.*\|.*\b(type|required)\b)|(^#{1,6}\s+.*\b(fields?|parameters?|inputs?)\b)`)
	scanDocErrorsRE  = regexp.MustCompile(`(?i)(^#{1,6}\s+.*\b(errors?|error codes?|failures?|failure states?|validation)\b)|(^\s*\|.*\b(error|code)\b.*\|.*\b(meaning|description|message)\b)`)
	scanDocRiskRE    = regexp.MustCompile(`(?i)\brisk\b`)
	scanDocConfirmRE = regexp.MustCompile(`(?i)\bconfirm(ation|ed|s)?\b`)
	scanTermHeadRE   = regexp.MustCompile(`(?i)^#{1,6}\s+.*\b(glossary|terminology|vocabulary|naming)\b`)

	scanTenantRE    = regexp.MustCompile(`(?i)(\btenant[_-]?id\b|\btenantId\b|\btenant\s*:|"tenant"|\btenant_[a-z]+\s)`)
	scanWorkspaceRE = regexp.MustCompile(`(?i)(\bworkspace[_-]?id\b|\bworkspaceId\b|\bworkspace\s*:|"workspace")`)
	scanRolesRE     = regexp.MustCompile(`(?i)(^\s*"?roles?"?\s*:|\b(enum|type|class)\s+Role\b|\broles?\s*[:=]\s*[\[{]|\brole\s+(text|varchar|enum|int|character)\b)`)
	scanAuthRE      = regexp.MustCompile(`(?i)\b(context_token|context-exchange|context_exchange|jwt|oauth2?|oidc|openid|better-?auth|passport|bearer|session[_-]?cookie|authenticate\w*)\b`)
	scanPermCheckRE = regexp.MustCompile(`(?i)\b(hasPermission|has_permission|requirePermission|require_permission|checkPermission|check_permission|assertPermission|authorize|isAllowed|is_allowed|allowedRoles|allowed_roles|rolePermissions|role_permissions|requireRole|require_role|assertRole|canPerform|can_perform)\b`)
	scanPermTableRE = regexp.MustCompile(`(?i)^\s*"?permissions?"?\s*:`)
	scanPermDocRE   = regexp.MustCompile(`(?i)\bpermissions?\b`)
	scanTermNameRE  = regexp.MustCompile(`(?i)(glossary|terminology|vocabulary)`)
)

// detectPackage finds the application package manifest and reads which sections it declares.
func (s *scanState) detectPackage() {
	bestDepth := -1
	for _, f := range s.files {
		b := scanBase(f.Path)
		if (b != "alixai.yaml" && b != "alixai.yml") || f.content == nil {
			continue
		}
		d := path.Dir(f.Path)
		depth := 0
		if d == "." {
			d = ""
		} else {
			depth = strings.Count(d, "/") + 1
		}
		if depth > 2 || (bestDepth >= 0 && depth >= bestDepth) {
			continue
		}
		bestDepth, s.pkgRoot, s.pkgFound = depth, d, true
	}
	if !s.pkgFound {
		return
	}
	mp := scanJoin(s.pkgRoot, "alixai.yaml")
	f := s.byPath[mp]
	if f == nil {
		mp = scanJoin(s.pkgRoot, "alixai.yml")
		f = s.byPath[mp]
	}
	lines := scanLines(f.content)
	line := func(key string) int {
		_, n, ok := scanTopKey(lines, key)
		if !ok {
			return 0
		}
		return n
	}
	first := line("format")
	if first == 0 {
		first = 1
	}
	s.add("manifest.file", f, first, first, "application package manifest")
	if a, p, c, e := line("application"), line("package"), line("compatibility"), line("endpoints"); a > 0 && p > 0 && c > 0 && e > 0 {
		s.add("manifest.fields", f, a, a, "manifest declares application, package, compatibility and endpoints")
	}
	if n := line("tenancy"); n > 0 {
		s.add("manifest.tenancy", f, n, n, "tenancy model declared")
	}
	if n := line("model_requirements"); n > 0 {
		s.add("model.manifest", f, n, n, "model requirements declared in the manifest")
	}
	if n := line("network"); n > 0 {
		s.add("egress.manifest", f, n, n, "network permissions declared in the manifest")
	}
	inCaps := false
	for i, l := range lines {
		if scanIndent(l) == 0 && !scanBlankOrComment(l) {
			inCaps = strings.HasPrefix(l, "capabilities:")
			continue
		}
		if inCaps && scanIndent(l) == 2 {
			if k, _, ok := strings.Cut(strings.TrimSpace(l), ":"); ok && scanCapIDRE.MatchString(k) {
				s.addCap(k)
			}
		}
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "transport:") {
			s.add("manifest.transport", f, i+1, i+1, l)
		}
	}
}

func (s *scanState) addCap(id string) {
	if !s.capSet[id] {
		s.capSet[id] = true
		s.caps = append(s.caps, id)
		sort.Strings(s.caps)
	}
}

// detectCapabilities reads capability declaration files: the ones in a directory called
// capabilities that have a top-level id and at least one of operations, fields or purpose.
func (s *scanState) detectCapabilities() {
	for _, f := range s.files {
		if f.content == nil || !scanInDir(f.Path, "capabilities") || !scanIsConfig(f.Path) || !scanIsYAML(f.Path) {
			continue
		}
		lines := scanLines(f.content)
		id, idLine, ok := scanTopKey(lines, "id")
		if !ok || !scanCapIDRE.MatchString(id) {
			continue
		}
		top := map[string]int{}
		for i, l := range lines {
			if scanIndent(l) == 0 && !scanBlankOrComment(l) {
				if k, _, ok := strings.Cut(l, ":"); ok {
					if _, dup := top[k]; !dup {
						top[k] = i + 1
					}
				}
			}
		}
		if top["operations"] == 0 && top["fields"] == 0 && top["purpose"] == 0 {
			continue
		}
		s.addCap(id)
		s.add("cap.file", f, idLine, idLine, id)
		if n := top["fields"]; n > 0 {
			s.add("def.fields", f, n, n, id+": fields declared")
		}
		if n := top["failure_states"]; n > 0 {
			s.add("def.errors", f, n, n, id+": failure states declared")
			s.add("err.enum", f, n, n, id+": failure states declared")
		}
		if n := top["risk_level"]; n > 0 {
			s.add("def.risk", f, n, n, id+": risk level declared")
		}
		if n := top["confirmation"]; n > 0 {
			s.add("def.confirm", f, n, n, id+": confirmation rule declared")
		}
		if n := top["permissions"]; n > 0 {
			s.add("perm.table", f, n, n, id+": permissions declared")
		}
		if n := top["idempotency"]; n > 0 {
			s.add("safe.idem", f, n, n, id+": idempotency declared")
		}
		if n := top["prepared_action"]; n > 0 {
			s.add("safe.expiry", f, n, n, id+": prepared action expiry declared")
			for i := n; i < len(lines) && (i == n || scanIndent(lines[i]) > 0 || scanBlankOrComment(lines[i])); i++ {
				if strings.Contains(lines[i], "invalidated_by") || strings.Contains(lines[i], "record_revision") {
					s.add("safe.version", f, i+1, i+1, id+": prepared action invalidated by data revision")
				}
			}
		}
		typed := false
		opStart := top["operations"]
		if opStart > 0 {
			kind := ""
			for i := opStart; i < len(lines); i++ {
				l := lines[i]
				if scanBlankOrComment(l) {
					continue
				}
				ind := scanIndent(l)
				if ind <= 0 {
					break
				}
				t := strings.TrimSpace(l)
				if ind == 2 {
					k, _, _ := strings.Cut(t, ":")
					kind = scanOpKindOfKey(k)
					if kind != "" {
						s.add("op."+kind, f, i+1, i+1, id+"."+k)
					}
					continue
				}
				if kind == "" {
					continue
				}
				if strings.HasPrefix(t, "endpoint:") || strings.HasPrefix(t, "adapter:") {
					s.add("cap.decl", f, i+1, i+1, id+" is served by "+strings.TrimSpace(t[strings.Index(t, ":")+1:]))
				}
				if strings.HasPrefix(t, "transport:") {
					s.add("cap.transport", f, i+1, i+1, t)
				}
				if strings.HasPrefix(t, "result_schema:") {
					typed = true
					s.add("op."+kind+".typed", f, i+1, i+1, t)
					if kind == "commit" && scanReceiptRE.MatchString(t) {
						s.add("op.commit.receipt", f, i+1, i+1, t)
					}
				}
			}
		}
		if typed && top["failure_states"] > 0 {
			s.add("err.typed", f, top["failure_states"], top["failure_states"], id+": typed results with failure states")
		}
	}
}

// scanOpKindOfKey maps an operation key of a capability to an operation kind.
func scanOpKindOfKey(k string) string {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "read", "list", "get", "find", "query", "search":
		return "read"
	case "validate":
		return "validate"
	case "prepare":
		return "prepare"
	case "commit":
		return "commit"
	case "status":
		return "status"
	}
	return ""
}

var (
	scanDocRootNames = map[string]bool{"docs": true, "doc": true, "documentation": true, "knowledge": true, "knowledge-base": true, "knowledgebase": true, "kb": true, "wiki": true, "handbook": true, "manuals": true, "guides": true}
	scanTestDirNames = map[string]bool{"tests": true, "test": true, "fixtures": true, "__fixtures__": true, "seed": true, "testdata": true, "samples": true, "e2e": true}
	scanModelExts    = map[string]bool{".gguf": true, ".safetensors": true, ".onnx": true, ".ggml": true, ".pt": true, ".pth": true}
	scanSeedNameRE   = regexp.MustCompile(`(?i)(seed|provision|fixture|bootstrap)`)
)

// detectPathSignals records the signals that come from where a file is and what it is called.
func (s *scanState) detectPathSignals() {
	ragDirs := map[string]bool{}
	for _, f := range s.files {
		p := f.Path
		base := scanBase(p)
		ext := scanExt(p)
		if scanModelExts[ext] {
			s.models = append(s.models, p)
		}
		if f.Kind == scanKindSecret || f.SHA256 == "" {
			continue
		}
		dirOf := path.Dir(p)
		hasLines := f.lines > 0
		switch {
		case !hasLines:
		case scanInDir(p, "conversation-contracts") || scanInDir(p, "conversation_contracts"):
			s.add("cc.file", f, 1, 1, p)
		}
		if hasLines && scanInDir(p, "policies") && (scanIsConfig(p) || scanIsDocText(p) || ext == ".rego") {
			s.add("policy.file", f, 1, 1, p)
		}
		if hasLines && (scanInDir(p, "terminology") || scanTermNameRE.MatchString(base)) {
			s.add("terms.file", f, 1, 1, p)
		}
		if hasLines && scanInDir(p, "signatures") {
			s.add("sig.dir", f, 1, 1, p)
			if strings.HasPrefix(base, "signature-manifest") || strings.HasPrefix(base, "signatures.") {
				s.add("sig.manifest", f, 1, 1, p)
			}
		}
		if hasLines && s.pkgFound && scanUnder(p, scanJoinRoot(s.pkgRoot, "tests")) && (scanIsConfig(p)) {
			s.add("tests.case", f, 1, 1, p)
		}
		if hasLines && scanInDir(p, "schemas") && s.pkgFound && scanUnder(p, scanJoinRoot(s.pkgRoot, "schemas")) {
			s.add("def.fields", f, 1, 1, p+": schema file")
		}
		segs := strings.Split(dirOf, "/")
		isMig := false
		for _, seg := range segs {
			l := strings.ToLower(seg)
			if l == "migrations" || l == "migrate" || l == "alembic" {
				isMig = true
			}
		}
		if ext == ".sql" || isMig || base == "schema.prisma" {
			if hasLines && (isMig || ext == ".sql") {
				s.migs[p] = true
				s.add("db.migration", f, 1, 1, p)
			}
		}
		// RAG source candidates: a documentation folder holding documents.
		if hasLines {
			docExt := scanIsDocText(p) || ext == ".pdf" || ext == ".docx" || ext == ".html" || ext == ".htm"
			if docExt {
				for _, seg := range segs {
					if scanDocRootNames[strings.ToLower(seg)] && !ragDirs[dirOf] {
						ragDirs[dirOf] = true
						s.add("rag.folder", f, 1, 1, "document folder "+dirOf)
						break
					}
				}
				if scanInDirAny(p, scanTestDirNames) && (ext == ".md" || ext == ".txt" || ext == ".pdf" || ext == ".docx" || ext == ".html") {
					s.add("rag.synth", f, 1, 1, p)
				}
			}
			if (ext == ".json" || ext == ".csv") && scanInDir(p, "seed") || (ext == ".json" && scanInDir(p, "tests") && strings.Contains(base, "seed")) {
				s.add("rag.synth.partial", f, 1, 1, p)
			}
		}
		if hasLines && scanSeedNameRE.MatchString(base) && (scanIsCode(p) || ext == ".sql" || ext == ".sh" || ext == ".json" || ext == ".ps1") {
			s.add("smoke.file", f, 1, 1, p)
		}
	}
	sort.Strings(s.models)
}

func scanJoinRoot(root, name string) string { return scanJoin(root, name) }

func scanInDirAny(p string, names map[string]bool) bool {
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if names[strings.ToLower(seg)] {
			return true
		}
	}
	return false
}

// detectDocs finds documentation signals in documentation files and the other text signals that
// need every file: terminology headings, scope, roles, permissions, safe execution and failures.
func (s *scanState) detectDocs(f *scanFileRec, lines []string) {
	if !scanIsDocText(f.Path) {
		return
	}
	readme := strings.HasPrefix(scanBase(f.Path), "readme")
	var seen uint8
	for i, l := range lines {
		if l == "" {
			continue
		}
		if scanTermHeadRE.MatchString(l) {
			s.add("terms.heading", f, i+1, i+1, l)
		}
		if scanPermDocRE.MatchString(l) {
			s.add("perm.doc", f, i+1, i+1, l)
		}
		if readme {
			continue
		}
		if seen&scanDocFields == 0 && scanDocFieldsRE.MatchString(l) {
			seen |= scanDocFields
			s.add("doc.fields", f, i+1, i+1, l)
		}
		if seen&scanDocErrors == 0 && scanDocErrorsRE.MatchString(l) {
			seen |= scanDocErrors
			s.add("doc.errors", f, i+1, i+1, l)
		}
		if seen&scanDocRisk == 0 && scanDocRiskRE.MatchString(l) {
			seen |= scanDocRisk
			s.add("doc.risk", f, i+1, i+1, l)
		}
		if seen&scanDocConfirm == 0 && scanDocConfirmRE.MatchString(l) {
			seen |= scanDocConfirm
			s.add("doc.confirm", f, i+1, i+1, l)
		}
		if strings.HasPrefix(strings.TrimSpace(l), "#") && scanSafeHeadingRE.MatchString(l) {
			s.add("safe.doc", f, i+1, i+1, l)
		}
	}
	if seen != 0 {
		if s.docFlags == nil {
			s.docFlags = map[string]uint8{}
		}
		s.docFlags[f.Path] = seen
	}
}

var scanSafeHeadingRE = regexp.MustCompile(`(?i)(idempoten|expir|etag|if-match|optimistic)`)

// scanTextSafeRE are the safe-execution signals in code and configuration.
var (
	scanIdemRE     = regexp.MustCompile(`(?i)idempoten(?:cy|t)[-_ ]?key|\bidempotency\b`)
	scanExpiryRE   = regexp.MustCompile(`(?i)\b(expires?_?at|expiry|expires_in|ttl_seconds|valid_until|prepared_?action_?ttl)\b`)
	scanVersionRE  = regexp.MustCompile(`(?i)\b(etag|if-match|if_match|row_?version|record_?revision|optimistic(?:_lock)?|version_?conflict)\b`)
	scanErrEnumRE  = regexp.MustCompile(`(?i)\b(error_?codes?|ErrorCode|failure_?states?)\b`)
	scanErrTypedRE = regexp.MustCompile(`(?i)(ErrorResponse|ProblemDetails|application/problem\+json|\berrors?\s*:\s*\[|typed[_ ]?errors?|\bapi_?error\b)`)
)

// detectScopeAndPermissions finds scope, role, authentication, permission, safe-execution and
// failure signals in schemas, migrations, configuration and code.
func (s *scanState) detectScopeAndPermissions(f *scanFileRec, lines []string) {
	p := f.Path
	code, cfg, sql := scanIsCode(p), scanIsConfig(p), scanExt(p) == ".sql"
	if !code && !cfg && !sql {
		return
	}
	isLock := strings.HasSuffix(scanBase(p), "-lock.json") || scanBase(p) == "package-lock.json" || strings.HasSuffix(scanBase(p), ".lock")
	if isLock {
		return
	}
	var got = map[string]bool{}
	hit := func(group string, i int, l string) {
		if !got[group] {
			got[group] = true
			s.add(group, f, i+1, i+1, l)
		}
	}
	for i, l := range lines {
		if l == "" {
			continue
		}
		if scanTenantRE.MatchString(l) {
			hit("scope.tenant", i, l)
		}
		if scanWorkspaceRE.MatchString(l) {
			hit("scope.workspace", i, l)
		}
		if scanRolesRE.MatchString(l) {
			hit("roles.def", i, l)
		}
		if scanAuthRE.MatchString(l) {
			hit("auth.mech", i, l)
		}
		if code && scanPermCheckRE.MatchString(l) {
			hit("perm.check", i, l)
		}
		if cfg && scanPermTableRE.MatchString(l) {
			hit("perm.table", i, l)
		}
		if scanIdemRE.MatchString(l) {
			hit("safe.idem", i, l)
		}
		if scanExpiryRE.MatchString(l) {
			hit("safe.expiry", i, l)
		}
		if scanVersionRE.MatchString(l) {
			hit("safe.version", i, l)
		}
		if scanErrEnumRE.MatchString(l) {
			hit("err.enum", i, l)
		}
		if scanErrTypedRE.MatchString(l) {
			hit("err.typed", i, l)
		}
		if sql && scanCreateTableRE.MatchString(l) {
			hit("db.schema", i, l)
		}
	}
}

// detectCoverage matches capabilities to documentation and to conversation contracts.
func (s *scanState) detectCoverage() {
	caps := s.caps
	if len(caps) == 0 {
		set := map[string]bool{}
		for _, d := range s.apis {
			for _, op := range d.Operations {
				if scanCandidateRE.MatchString(op.OperationID) {
					set[op.OperationID] = true
				}
			}
		}
		for t := range s.tools {
			if scanCandidateRE.MatchString(t) {
				set[t] = true
			}
		}
		for id := range set {
			caps = append(caps, id)
		}
		sort.Strings(caps)
		s.capsAreCandidates = len(caps) > 0
		s.caps = caps
	}
	for _, id := range caps {
		forms := []string{strings.ToLower(id), strings.ToLower(strings.NewReplacer(".", "-", "_", "-").Replace(id)),
			strings.ToLower(strings.NewReplacer(".", "_", "-", "_").Replace(id)), strings.ToLower(strings.NewReplacer(".", "/", "_", "/").Replace(id)),
			strings.ToLower(strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(id))}
	docs:
		for _, f := range s.files {
			if f.content == nil || !scanIsDocText(f.Path) {
				continue
			}
			for i, l := range scanLines(f.content) {
				ll := strings.ToLower(l)
				for _, form := range forms {
					if strings.Contains(ll, form) {
						s.add("doc.cap", f, i+1, i+1, id)
						s.capDocCovered++
						break docs
					}
				}
			}
		}
		slug := scanSlug(id)
		for _, f := range s.files {
			if f.lines > 0 && (scanInDir(f.Path, "conversation-contracts") || scanInDir(f.Path, "conversation_contracts")) &&
				strings.Contains(scanSlug(strings.TrimSuffix(path.Base(f.Path), path.Ext(f.Path))), slug) {
				s.add("cc.cap", f, 1, 1, id)
				s.capCCCovered++
				break
			}
		}
	}
}
