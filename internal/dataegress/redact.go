package dataegress

import (
	"net/url"
	"regexp"
	"strings"

	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Placeholders that stand where blocked content was. They carry no part of the
// removed text.
const (
	placeholderSecret = alog.Redacted
	placeholderPath   = "[path removed]"
	placeholderDB     = "[database handle removed]"
	placeholderMCP    = "[mcp url removed]"
)

// The patterns below are deterministic text rules, applied to every piece of
// text that is about to leave (additive plan 6.2 step 4: "remove secrets"; and
// 6.2: "the provider never receives a raw filesystem path, database connection,
// reusable application credential or unrestricted MCP URL"). They are a net under
// the typed categories a candidate declares, not a substitute for them: content a
// caller declares as a credential, raw database content or a local-only field never
// reaches this code, it is removed whole.

const urlTail = `[^\s"'<>)\]]*`

var (
	// MCP endpoints: the mcp scheme family, and any http(s) URL whose host or path
	// names mcp (see isMCPURL).
	mcpSchemeRE = regexp.MustCompile(`(?i)\bmcp(?:\+[a-z0-9]+)?://` + urlTail)
	httpURLRE   = regexp.MustCompile(`(?i)\bhttps?://` + urlTail)

	// Database handles: connection URLs and JDBC / ADO-style connection strings.
	dbURLRE = regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|mysql|mariadb|mongodb(?:\+srv)?|redis|rediss|amqps?|sqlserver|mssql|oracle|cockroachdb|clickhouse|sqlite3?|libsql)://` + urlTail +
		`|\bjdbc:[a-z0-9]+:` + urlTail)
	dbConnStrRE = regexp.MustCompile(`(?i)\b(?:server|data source|host|database|initial catalog|dbname|user id|uid)\s*=\s*[^;\r\n]+(?:;\s*[A-Za-z][A-Za-z _]*\s*=\s*[^;\r\n]*)+;?`)

	// Filesystem paths: file URLs, Windows drive and UNC paths, absolute and
	// home-relative POSIX paths, and parent-directory traversal. An absolute path
	// is at least two segments, or one segment under a well-known system root, and
	// must start where a word could start so "and/or" and "2026/10/02" and the path
	// part of an https URL are left alone.
	fileURLRE = regexp.MustCompile(`(?i)\bfile://` + urlTail)
	winPathRE = regexp.MustCompile(`\b[A-Za-z]:[\\/][^\s"'<>|*?]*`)
	uncPathRE = regexp.MustCompile(`\\\\[A-Za-z0-9_.$-]+\\[^\s"'<>|*?]+`)
	posixRE   = regexp.MustCompile(`(^|[\s"'(\[=,;<>])((?:/(?:etc|home|usr|var|tmp|opt|root|mnt|proc|sys|srv|dev|bin|sbin|lib|Users|Library|Volumes|private)(?:/[^\s"'<>)\]|,;]*)?)|(?:/[A-Za-z0-9._~@%+-]+(?:/[A-Za-z0-9._~@%+-]+)+/?))`)
	homeRE    = regexp.MustCompile(`(^|[\s"'(\[=,;])~(?:/[^\s"'<>)\]|,;]*)+`)
	parentRE  = regexp.MustCompile(`(?:\.\.[\\/])+[^\s"'<>]*`)

	// Credentials with a recognisable shape, and the opaque ids of credential
	// references (a reusable application credential is never sent, not even its handle).
	secretShapeRE = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|AKIA[0-9A-Z]{16}|(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AIza[0-9A-Za-z_-]{30,}|glpat-[A-Za-z0-9_-]{16,}|cred_[0-9A-HJKMNP-TV-Z]{26})\b`)
)

// scrub removes blocked content from s and reports which blocked categories it
// found. withPaths turns the filesystem-path rules on; they are left off for tool
// schemas, whose strings legitimately hold JSON pointers ("#/$defs/x") and patterns.
// Text without blocked content is returned unchanged. The result never contains
// what was removed, and a second pass changes nothing.
func scrub(s string, withPaths bool) (string, []providers.BlockedCategory) {
	if s == "" {
		return s, nil
	}
	var found []providers.BlockedCategory
	note := func(c providers.BlockedCategory) {
		for _, f := range found {
			if f == c {
				return
			}
		}
		found = append(found, c)
	}
	sub := func(re *regexp.Regexp, repl string, c providers.BlockedCategory) {
		if re.MatchString(s) {
			s = re.ReplaceAllString(s, repl)
			note(c)
		}
	}
	// URLs first, whole, so their userinfo and path never reach the other rules.
	sub(mcpSchemeRE, placeholderMCP, providers.BlockedCategoryMCPURL)
	if httpURLRE.MatchString(s) {
		replaced := httpURLRE.ReplaceAllStringFunc(s, func(u string) string {
			if isMCPURL(u) {
				note(providers.BlockedCategoryMCPURL)
				return placeholderMCP
			}
			return u
		})
		s = replaced
	}
	sub(dbURLRE, placeholderDB, providers.BlockedCategoryDatabaseHandle)
	sub(dbConnStrRE, placeholderDB, providers.BlockedCategoryDatabaseHandle)
	if withPaths {
		sub(fileURLRE, placeholderPath, providers.BlockedCategoryFilesystemPath)
		sub(uncPathRE, placeholderPath, providers.BlockedCategoryFilesystemPath)
		sub(winPathRE, placeholderPath, providers.BlockedCategoryFilesystemPath)
		sub(posixRE, "${1}"+placeholderPath, providers.BlockedCategoryFilesystemPath)
		sub(homeRE, "${1}"+placeholderPath, providers.BlockedCategoryFilesystemPath)
		sub(parentRE, placeholderPath, providers.BlockedCategoryFilesystemPath)
	}
	sub(secretShapeRE, placeholderSecret, providers.BlockedCategoryCredential)
	if r := alog.RedactString(s); r != s {
		s = r
		note(providers.BlockedCategoryCredential)
	}
	return s, found
}

// scrubValue applies scrub to every string of a decoded JSON value (a tool
// schema), in place of the strings, and returns the categories found.
func scrubValue(v any) (any, []providers.BlockedCategory) {
	var found []providers.BlockedCategory
	merge := func(c []providers.BlockedCategory) {
		for _, x := range c {
			dup := false
			for _, f := range found {
				if f == x {
					dup = true
				}
			}
			if !dup {
				found = append(found, x)
			}
		}
	}
	switch t := v.(type) {
	case string:
		s, c := scrub(t, false)
		return s, c
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			nx, c := scrubValue(x)
			merge(c)
			out[k] = nx
		}
		return out, found
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			nx, c := scrubValue(x)
			merge(c)
			out[i] = nx
		}
		return out, found
	}
	return v, nil
}

// isMCPURL reports whether an http(s) URL addresses an MCP endpoint: a host label
// or a path segment that is "mcp" (https://mcp.example.com/, https://app.example.com/v1/mcp/sse).
func isMCPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		// An unparsable URL is judged on its text: fail closed.
		return strings.Contains(strings.ToLower(raw), "mcp")
	}
	for _, label := range strings.Split(strings.ToLower(u.Hostname()), ".") {
		if label == "mcp" {
			return true
		}
	}
	for _, seg := range strings.Split(strings.ToLower(u.Path), "/") {
		if seg == "mcp" {
			return true
		}
	}
	return false
}
