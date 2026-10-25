package localaisec

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ---- the exceptions table (checklist section 6) ----

// exception is one row of the exceptions table: it relaxes exactly the checklist row it names.
type exception struct {
	ID, Row, Reason, Owner string
	Expires                time.Time
}

var (
	exceptionRowRE = regexp.MustCompile(`^EX-\d{2}$`)
	hcRowRE        = regexp.MustCompile(`^HC-\d{2}$`)
	isoDateRE      = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
)

// parseExceptions reads the table of section 6 out of the checklist text, keyed by the HC row each entry relaxes. A row
// that cannot be read (no row id, no reason, no owner, no expiry date) is returned in problems and does not relax anything.
func parseExceptions(doc string) (map[string]exception, []string) {
	out := map[string]exception{}
	var problems []string
	in := false
	for _, line := range strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "## ") {
			in = strings.HasPrefix(trim, "## 6.")
			continue
		}
		if !in || !strings.HasPrefix(trim, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(trim, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if len(cells) < 5 || !exceptionRowRE.MatchString(cells[0]) {
			continue
		}
		e := exception{ID: cells[0], Row: cells[1], Reason: cells[2], Owner: cells[3]}
		if !hcRowRE.MatchString(e.Row) {
			problems = append(problems, fmt.Sprintf("%s: the row %q is not an HC-NN id", e.ID, e.Row))
			continue
		}
		if len(e.Reason) < 20 || e.Owner == "" {
			problems = append(problems, fmt.Sprintf("%s: an exception needs a stated reason and an owner", e.ID))
			continue
		}
		d := isoDateRE.FindString(cells[4])
		exp, err := time.Parse("2006-01-02", d)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: no expiry date in %q", e.ID, cells[4]))
			continue
		}
		e.Expires = exp
		out[e.Row] = e
	}
	return out, problems
}

// exceptions is what the rules may relax, at one moment in time.
type exceptions struct {
	byRow map[string]exception
	now   time.Time
}

// allows reports whether an exception for the HC row is in the table and has not expired. An expired exception
// relaxes nothing: the date is the day the entry must be re-examined.
func (ex exceptions) allows(row string) (exception, bool) {
	e, ok := ex.byRow[row]
	if !ok {
		return exception{}, false
	}
	return e, !ex.now.After(e.Expires.Add(24 * time.Hour))
}

// ---- findings ----

type finding struct {
	Rule string // HC-NN, or STRUCT for the structure rules
	Msg  string
}

func (f finding) String() string { return f.Rule + ": " + f.Msg }

// ruleSet is the machine-checkable rules of section 4 of the checklist, with the profile minimums of HC-29.
var (
	requireTrue = []struct{ row, name string }{
		{"HC-07", "LOCALAI_SUBTLE_KEY_COMPARISON"},
		{"HC-12", "LOCALAI_DISABLE_WEBUI"},
		{"HC-13", "LOCALAI_DISABLE_RUNTIME_SETTINGS"},
		{"HC-14", "LOCALAI_DISABLE_GALLERY_ENDPOINT"},
		{"HC-15", "LOCALAI_DISABLE_AGENTS"},
		{"HC-16", "LOCALAI_DISABLE_MCP"},
	}
	requireFalse = []struct{ row, name string }{
		{"HC-14", "LOCALAI_AUTOLOAD_GALLERIES"},
		{"HC-14", "LOCALAI_AUTOLOAD_BACKEND_GALLERIES"},
	}
	// excepted: required to be true unless the exception row relaxes it; then absent or false is allowed too.
	requireTrueUnlessExcepted = []struct{ row, name string }{
		{"HC-20", "LOCALAI_OPAQUE_ERRORS"},
		{"HC-21", "LOCALAI_DISABLE_METRICS_ENDPOINT"},
	}
	forbidden = []struct{ row, name string }{
		{"HC-08", "LOCALAI_DISABLE_API_KEY_REQUIREMENT_FOR_HTTP_GET"},
		{"HC-08", "LOCALAI_HTTP_GET_EXEMPTED_ENDPOINTS"},
		{"HC-09", "LOCALAI_AUTH"},
		{"HC-09", "GITHUB_CLIENT_ID"},
		{"HC-09", "GITHUB_CLIENT_SECRET"},
		{"HC-09", "LOCALAI_OIDC_ISSUER"},
		{"HC-09", "LOCALAI_OIDC_CLIENT_ID"},
		{"HC-09", "LOCALAI_OIDC_CLIENT_SECRET"},
		{"HC-09", "LOCALAI_REGISTRATION_MODE"},
		{"HC-17", "LOCALAI_P2P"},
		{"HC-17", "LOCALAI_FEDERATED"},
		{"HC-17", "LOCALAI_P2P_TOKEN"},
		{"HC-17", "LOCALAI_P2P_NETWORK_ID"},
		{"HC-18", "LOCALAI_EXTERNAL_GRPC_BACKENDS"},
		{"HC-19", "LOCALAI_CORS"},
		{"HC-19", "LOCALAI_CORS_ALLOW_ORIGINS"},
		{"HC-19", "LOCALAI_DISABLE_CSRF"},
		{"HC-22", "LOCALAI_DISABLE_PREDOWNLOAD_SCAN"},
		{"HC-23", "LOCALAI_MACHINE_TAG"},
		{"HC-34", "LOCALAI_DEBUG"},
		{"HC-34", "DEBUG"},
		{"HC-34", "ALIXAI_ALLOW_VERBOSE_LOG"},
	}
	profileResident = map[string]int{"lite": 2, "balanced": 4, "4g": 1}
)

const (
	keyVar         = "LOCALAI_API_KEY"
	maxActiveVar   = "LOCALAI_MAX_ACTIVE_BACKENDS"
	galleriesVar   = "LOCALAI_GALLERIES"
	backendGalVar  = "LOCALAI_BACKEND_GALLERIES"
	externalBeVar  = "LOCALAI_EXTERNAL_BACKENDS"
	addressVar     = "LOCALAI_ADDRESS"
	logLevelVar    = "LOCALAI_LOG_LEVEL"
	placeholderKey = "change-me"
)

var (
	digestRE      = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	backendNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

func isFalseValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "false", "0", "no", "off":
		return true
	}
	return false
}

// checkCtx carries what the rules need beyond the launcher itself: the exceptions, and the repository tree for the
// backend catalogue that EX-01 and EX-02 point at.
type checkCtx struct {
	ex exceptions
	tr tree
}

// check applies the rules to one launcher.
func (c checkCtx) check(l *launcher) []finding {
	var fs []finding
	add := func(rule, f string, a ...any) {
		fs = append(fs, finding{Rule: rule, Msg: l.Name + ": " + fmt.Sprintf(f, a...)})
	}

	// values returns the candidate values of a variable and whether it is set at all.
	values := func(name string) ([]string, *setting) {
		s := l.Env[name]
		if s == nil {
			return nil, nil
		}
		return s.Values, s
	}
	// pinned reports why a value cannot be judged: it is left to the operator or to the host environment.
	unpinned := func(rule, name string, s *setting) bool {
		if s.External {
			add(rule, "%s is taken from the host environment; the file must pin it", name)
			return true
		}
		for _, v := range s.Values {
			if v == requiredValue {
				add(rule, "%s is a required substitution (${%s:?}): the operator picks the value, the file must pin it", name, name)
				return true
			}
		}
		return false
	}

	for _, r := range requireTrue {
		c.requireValue(l, &fs, r.row, r.name, "true", false)
	}
	for _, r := range requireFalse {
		c.requireValue(l, &fs, r.row, r.name, "false", false)
	}
	for _, r := range requireTrueUnlessExcepted {
		_, excepted := c.ex.allows(r.row)
		c.requireValue(l, &fs, r.row, r.name, "true", excepted)
	}
	for _, r := range forbidden {
		vs, s := values(r.name)
		if s == nil || unpinned(r.row, r.name, s) {
			continue
		}
		for _, v := range vs {
			if !isFalseValue(v) {
				add(r.row, "%s=%q must be absent, empty or false", r.name, v)
			}
		}
	}

	// HC-34 log level.
	if vs, s := values(logLevelVar); s != nil && !unpinned("HC-34", logLevelVar, s) {
		for _, v := range vs {
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "", "info", "warn", "warning", "error", "fatal":
			default:
				add("HC-34", "%s=%q: debug and trace put every model completion in the container log", logLevelVar, v)
			}
		}
	}

	// HC-06 API key.
	c.checkAPIKey(l, &fs)

	// HC-14 galleries: the lists are unset or [], except the one local catalogue of EX-01.
	c.checkGalleries(l, &fs)

	// HC-18 external backends: absent, except the catalogue names of EX-02.
	c.checkExternalBackends(l, &fs)

	// HC-29 resident backends.
	c.checkMaxActive(l, &fs)

	if l.EnvOnly {
		return fs
	}

	// HC-01 address, native installs: loopback.
	if vs, s := values(addressVar); s != nil && !l.Container {
		for _, v := range vs {
			host, _, err := net.SplitHostPort(v)
			if err != nil || !(host == "127.0.0.1" || host == "::1" || host == "localhost") {
				add("HC-01", "%s=%q: a native LocalAI listens on a loopback address, never on all interfaces", addressVar, v)
			}
		}
	} else if s == nil && !l.Container {
		add("HC-01", "%s is not set: LocalAI's default :8080 is every interface", addressVar)
	}

	if !l.Container {
		c.checkNativeStructure(l, &fs)
		return fs
	}

	// Structure rules.
	if len(l.Images) == 0 {
		add("HC-27", "no image")
	}
	for _, img := range l.Images {
		if !digestRE.MatchString(img) {
			add("HC-27", "image %q is not pinned by digest (…@sha256:<64 hex>)", img)
		}
	}
	for _, p := range l.Ports {
		if !strings.HasPrefix(p, "127.0.0.1:") {
			add("HC-02", "published port %q does not start with 127.0.0.1:", p)
		}
	}
	for _, m := range l.Mounts {
		switch m.Target {
		case "/models", "/backends", "/data", "/configuration":
			if m.Named {
				add("HC-24", "%s is a named or anonymous volume (%q): models, backends, data and configuration are bind mounts", m.Target, m.Raw)
			}
		}
	}
	if l.MemLimit != "" || l.MemSwap != "" {
		if l.MemLimit == "" || l.MemSwap != l.MemLimit {
			add("HC-33", "memswap_limit %q must equal mem_limit %q (no swap)", l.MemSwap, l.MemLimit)
		}
	}
	if l.Profile != "" && l.MemLimit == "" {
		add("HC-33", "profile %s sets no mem_limit", l.Profile)
	}
	if !contains(l.CapDrop, "ALL") {
		add("HC-33", "cap_drop does not contain ALL (%v)", l.CapDrop)
	}
	if l.Privileged {
		add("HC-33", "privileged: true defeats cap_drop ALL")
	}
	if strings.EqualFold(strings.TrimSpace(l.NetworkMode), "host") {
		add("HC-02", "network_mode host puts LocalAI on the host's interfaces")
	}
	for _, n := range l.Names {
		if strings.Contains(strings.ToLower(n), "init") {
			add("STRUCT", "service %q: an init container is not allowed; setup runs in the main service's entrypoint (M0-08 rule)", n)
		}
	}
	return fs
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// requireValue judges a variable that must be set to want. excepted lets it be absent or false instead (the exception
// row of the checklist relaxes exactly that).
func (c checkCtx) requireValue(l *launcher, fs *[]finding, row, name, want string, excepted bool) {
	add := func(f string, a ...any) {
		*fs = append(*fs, finding{Rule: row, Msg: l.Name + ": " + fmt.Sprintf(f, a...)})
	}
	s := l.Env[name]
	if s == nil {
		if !l.EnvOnly && !excepted {
			add("%s is not set; it must be %s", name, want)
		}
		return
	}
	if s.External {
		add("%s is taken from the host environment; the file must pin it to %s", name, want)
		return
	}
	for _, v := range s.Values {
		switch {
		case v == requiredValue:
			add("%s is a required substitution: the file must pin it to %s", name, want)
		case strings.EqualFold(strings.TrimSpace(v), want):
		case excepted && isFalseValue(v):
		default:
			add("%s=%q; it must be %s", name, v, want)
		}
	}
}

// checkAPIKey is HC-06: the key is set from a required substitution (or an env file placeholder, or a credential
// reference), never a literal or a default.
func (c checkCtx) checkAPIKey(l *launcher, fs *[]finding) {
	add := func(f string, a ...any) {
		*fs = append(*fs, finding{Rule: "HC-06", Msg: l.Name + ": " + fmt.Sprintf(f, a...)})
	}
	s := l.Env[keyVar]
	if s == nil {
		if !l.EnvOnly {
			add("%s is not set: LocalAI would run without authentication", keyVar)
		}
		return
	}
	if s.Ref || s.External {
		return
	}
	for _, v := range s.Values {
		switch {
		case v == requiredValue:
		case v == "" && s.FromFile && !l.EnvOnly:
			add("%s is empty in %s: LocalAI would run without authentication", keyVar, s.From)
		case l.EnvOnly || s.FromFile:
			// An env file carries a placeholder only; a real key never goes in a tracked file.
			if v != "" && !strings.HasPrefix(strings.ToLower(v), placeholderKey) {
				add("%s in %s is %d characters of something that is not the %q placeholder: a key never goes in a tracked file", keyVar, s.From, len(v), placeholderKey)
			}
		case v == "":
			add("%s is empty: LocalAI would run without authentication", keyVar)
		default:
			add("%s has a default or literal value (%d characters): it must be a required substitution ${%s:?...}", keyVar, len(v), keyVar)
		}
	}
}

// galleryEntry is one element of LOCALAI_BACKEND_GALLERIES.
type galleryEntry struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// checkGalleries is the gallery half of HC-14: LOCALAI_GALLERIES is unset or [], LOCALAI_BACKEND_GALLERIES likewise,
// except for the single local, read-only, digest-pinned catalogue that EX-01 allows.
func (c checkCtx) checkGalleries(l *launcher, fs *[]finding) {
	add := func(f string, a ...any) {
		*fs = append(*fs, finding{Rule: "HC-14", Msg: l.Name + ": " + fmt.Sprintf(f, a...)})
	}
	empty := func(v string) bool {
		v = strings.TrimSpace(v)
		return v == "" || v == "[]"
	}
	if s := l.Env[galleriesVar]; s != nil {
		if s.External {
			add("%s is taken from the host environment", galleriesVar)
		}
		for _, v := range s.Values {
			if !empty(v) {
				add("%s=%q: the model gallery must be unset or []", galleriesVar, v)
			}
		}
	}
	s := l.Env[backendGalVar]
	if s == nil {
		return
	}
	if s.External {
		add("%s is taken from the host environment", backendGalVar)
		return
	}
	for _, v := range s.Values {
		if empty(v) {
			continue
		}
		e, ok := c.ex.allows("HC-14")
		if !ok {
			add("%s=%q: a backend gallery is configured and no unexpired exception (EX-01) allows it", backendGalVar, v)
			continue
		}
		var entries []galleryEntry
		if err := json.Unmarshal([]byte(v), &entries); err != nil || len(entries) != 1 {
			add("%s: %s allows exactly one gallery, got %q", backendGalVar, e.ID, v)
			continue
		}
		u, err := url.Parse(entries[0].URL)
		if err != nil || u.Scheme != "file" || !strings.HasPrefix(u.Path, "/backends/") {
			add("%s: %s allows only a local file:///backends/... catalogue, got %q", backendGalVar, e.ID, entries[0].URL)
			continue
		}
		// The catalogue is mounted read-only, from a file that pins every backend by digest.
		if !l.EnvOnly && l.Container {
			ro := false
			for _, m := range l.Mounts {
				if m.Target == u.Path && m.ReadOnly {
					ro = true
				}
			}
			if !ro {
				add("%s: the catalogue %s is not mounted read-only (%s)", backendGalVar, u.Path, e.ID)
			}
		}
	}
}

// catalogueNames returns the backend names and aliases of every shipped catalogue, and the entries that are not
// pinned by digest.
func (c checkCtx) catalogue() (names map[string]bool, unpinned []string, found int) {
	names = map[string]bool{}
	for _, p := range c.tr.sorted() {
		if path.Base(p) != "backend-gallery.yaml" {
			continue
		}
		var entries []struct {
			Name    string   `yaml:"name"`
			Alias   string   `yaml:"alias"`
			URI     string   `yaml:"uri"`
			Mirrors []string `yaml:"mirrors"`
		}
		if err := yaml.Unmarshal([]byte(c.tr[p]), &entries); err != nil {
			unpinned = append(unpinned, p+": not a list of backends: "+err.Error())
			continue
		}
		found++
		for _, e := range entries {
			names[e.Name] = true
			if e.Alias != "" {
				names[e.Alias] = true
			}
			for _, u := range append([]string{e.URI}, e.Mirrors...) {
				if !digestRE.MatchString(u) {
					unpinned = append(unpinned, fmt.Sprintf("%s: backend %q source %q is not pinned by digest", p, e.Name, u))
				}
			}
		}
	}
	return names, unpinned, found
}

// checkExternalBackends is HC-18: LOCALAI_EXTERNAL_BACKENDS is absent, except for EX-02, which allows catalogue names
// resolved in the one pinned catalogue of EX-01 and nothing that looks like a location.
func (c checkCtx) checkExternalBackends(l *launcher, fs *[]finding) {
	add := func(f string, a ...any) {
		*fs = append(*fs, finding{Rule: "HC-18", Msg: l.Name + ": " + fmt.Sprintf(f, a...)})
	}
	s := l.Env[externalBeVar]
	if s == nil {
		return
	}
	if s.External {
		add("%s is taken from the host environment", externalBeVar)
		return
	}
	for _, v := range s.Values {
		if strings.TrimSpace(v) == "" {
			continue
		}
		e, ok := c.ex.allows("HC-18")
		if !ok {
			add("%s=%q: external backends are forbidden and no unexpired exception (EX-02) allows them", externalBeVar, v)
			continue
		}
		if _, ok := c.ex.allows("HC-14"); !ok {
			add("%s: %s names catalogues entries, which exist only through EX-01, and that exception is not in force", externalBeVar, e.ID)
		}
		names, unpinned, found := c.catalogue()
		for _, n := range strings.Split(v, ",") {
			n = strings.TrimSpace(n)
			switch {
			case !backendNameRE.MatchString(n):
				add("%s: %q is not a catalogue name (%s allows names, never a URI or a path)", externalBeVar, n, e.ID)
			case found == 0:
				add("%s: no backend catalogue is in the repository to resolve %q", externalBeVar, n)
			case !names[n]:
				add("%s: %q is not in the pinned catalogue", externalBeVar, n)
			}
		}
		for _, u := range unpinned {
			add("%s", u)
		}
	}
}

// checkMaxActive is HC-29: the number of resident backends is set, and not below what the profile keeps resident
// (0 is "unlimited", which is not a bound).
func (c checkCtx) checkMaxActive(l *launcher, fs *[]finding) {
	add := func(f string, a ...any) {
		*fs = append(*fs, finding{Rule: "HC-29", Msg: l.Name + ": " + fmt.Sprintf(f, a...)})
	}
	s := l.Env[maxActiveVar]
	if s == nil {
		if !l.EnvOnly {
			add("%s is not set: the default 0 is unlimited, which is not a bound", maxActiveVar)
		}
		return
	}
	if s.External {
		add("%s is taken from the host environment", maxActiveVar)
		return
	}
	min := 1
	if m, ok := profileResident[l.Profile]; ok {
		min = m
	}
	for _, v := range s.Values {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		switch {
		case v == requiredValue:
			add("%s is a required substitution: the file must set a bound", maxActiveVar)
		case err != nil || n < min:
			add("%s=%q: the profile %q keeps %d models resident, so the bound is at least %d", maxActiveVar, v, l.Profile, min, min)
		}
	}
}

// checkNativeStructure is the structure of section 4 for a service that is not a container: the systemd forms of the
// capability drop and of the no-swap limit.
func (c checkCtx) checkNativeStructure(l *launcher, fs *[]finding) {
	add := func(rule, f string, a ...any) {
		*fs = append(*fs, finding{Rule: rule, Msg: l.Name + ": " + fmt.Sprintf(f, a...)})
	}
	if l.Kind == "supervisor" {
		return
	}
	if !l.CapBoundingSetSet || len(l.CapBoundingSet) != 0 {
		add("HC-33", "CapabilityBoundingSet must be set and empty (the equivalent of cap_drop ALL), got %v", l.CapBoundingSet)
	}
	if l.MemoryMax != "" && l.MemorySwapMax != "0" {
		add("HC-33", "MemorySwapMax=%q: with MemoryMax set the swap cap must be 0 (equal memory and swap limits)", l.MemorySwapMax)
	}
	if l.MemoryMax == "" {
		add("HC-33", "no MemoryMax")
	}
	for _, n := range l.Names {
		if strings.Contains(strings.ToLower(n), "init") {
			add("STRUCT", "unit %q: an init service is not allowed", n)
		}
	}
}

// allRules returns the HC rows this check can report, for the coverage control.
func allRules() []string {
	set := map[string]bool{"HC-01": true, "HC-02": true, "HC-06": true, "HC-14": true, "HC-18": true, "HC-24": true, "HC-27": true, "HC-29": true, "HC-33": true, "HC-34": true, "STRUCT": true}
	for _, r := range requireTrue {
		set[r.row] = true
	}
	for _, r := range requireFalse {
		set[r.row] = true
	}
	for _, r := range requireTrueUnlessExcepted {
		set[r.row] = true
	}
	for _, r := range forbidden {
		set[r.row] = true
	}
	var out []string
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}
