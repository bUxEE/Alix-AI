package assist

import (
	"regexp"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// assistInjectionPatterns is the closed list of shapes that make a project line read as an
// instruction to a model rather than as documentation. It is deliberately small and blunt: a line
// that matches is withheld from the prompt (its number stays, its text does not) and can never be
// cited, so a false positive costs a little model help and a false negative is still caught by
// the verifier, the rule statuses and the template. Matching is on lower-cased, whitespace-
// collapsed text.
var assistInjectionPatterns = []*regexp.Regexp{
	// "ignore all previous instructions", "disregard the rules above"
	regexp.MustCompile(`\b(ignore|disregard|forget|override|bypass|discard)\b.{0,40}\b(previous|prior|above|earlier|preceding|all|any|these|your|the|system)\b.{0,30}\b(instructions?|rules?|prompts?|polic(y|ies)|guidelines?|constraints?|restrictions?)\b`),
	// "mark every requirement as found"
	regexp.MustCompile(`\b(mark|set|report|answer|respond|output|label|flag|treat|consider|declare|return)\b.{0,60}\b(every|all|each|any)\b.{0,40}\b(requirements?|findings?|items?|checks?|statuses|status)\b`),
	// "you must answer ..." addressed to the reader
	regexp.MustCompile(`\b(you|assistant|the model|llm|ai)\b.{0,25}\b(must|should|shall|will|are to|need to|have to)\b.{0,60}\b(mark|answer|respond|output|report|say|reply|ignore|return|claim|state)\b`),
	// talk about the prompt itself
	regexp.MustCompile(`\b(system prompt|developer message|developer prompt|new instructions?|updated instructions?|hidden instructions?|jailbreak|prompt injection)\b`),
	regexp.MustCompile(`\b(you are now|from now on,? you|pretend (to be|you)|as an (ai|llm|language model)\b)`),
	// do not tell the operator
	regexp.MustCompile(`\b(do not|don'?t|never)\b.{0,30}\b(mention|report|reveal|flag|tell|disclose|list)\b.{0,30}\b(this|these|the operator|the user|anyone)\b`),
	// status words used as a command
	regexp.MustCompile(`\b(status|result)\b\s*[:=]\s*"?(found|met|covered|satisfied|verified|certified|approved|passed)\b`),
	// role and chat-template markers at the start of a line
	regexp.MustCompile(`^\s*(system|assistant|user|developer|tool)\s*[:>]`),
	regexp.MustCompile(`^\s*#{1,6}\s*(system|instructions?)\b`),
}

// assistFlagLine reports whether the line reads as an instruction or carries a fence or role
// marker. hidden says the line held characters that are invisible to a reader (zero-width,
// bidirectional or tag characters); such a line is flagged whatever it says, because hidden text
// is what a prompt injection hides in (master plan 12.6).
func assistFlagLine(line string, hidden bool) bool {
	if hidden {
		return true
	}
	if assistFenceBreak.MatchString(line) {
		return true
	}
	norm := strings.Join(strings.Fields(strings.ToLower(line)), " ")
	for _, re := range assistInjectionPatterns {
		if re.MatchString(norm) {
			return true
		}
	}
	return false
}

// assistCategoryTerms are the words a quote must mention at least one of to count as evidence for
// a requirement of the category. It is a plausibility check, not proof: the verifier already
// proves the quote is in the cited lines; this refuses a proposal whose cited lines do not even
// talk about the subject, so a model cannot justify "found" with an unrelated sentence.
var assistCategoryTerms = map[readiness.Category]*regexp.Regexp{
	readiness.CategoryMCP:                  assistTerms("mcp", "model context protocol", "tool", "tools", "server", "stdio", "sse"),
	readiness.CategoryOpenAPI:              assistTerms("openapi", "swagger", "api", "apis", "endpoint", "endpoints", "get", "post", "put", "patch", "delete", "route", "routes", "rest", "operation", "operations", "paths", "graphql", "grpc"),
	readiness.CategoryNativeAdapter:        assistTerms("adapter", "adapters", "sdk", "operation", "operations", "alixai", "native", "plugin", "connector"),
	readiness.CategoryModelRuntime:         assistTerms("model", "models", "llm", "runtime", "inference", "gpu", "ollama", "localai", "embedding", "embeddings", "external", "provider", "providers", "openai", "anthropic"),
	readiness.CategoryDeterministicData:    assistTerms("read", "reads", "validate", "validation", "prepare", "commit", "receipt", "idempotent", "idempotency", "transaction", "typed", "result", "results", "query", "write", "writes", "create", "update", "get", "post"),
	readiness.CategoryDocumentation:        assistTerms("document", "documents", "documentation", "docs", "description", "field", "fields", "validation", "error", "errors", "risk", "confirm", "confirmation", "capability", "capabilities", "endpoint", "operation", "parameter", "parameters"),
	readiness.CategoryConversationContract: assistTerms("conversation", "intent", "intents", "utterance", "utterances", "dialogue", "dialog", "contract", "contracts", "terminology", "glossary", "synonym", "synonyms", "term", "terms", "language", "languages"),
	readiness.CategoryRAGSource:            assistTerms("rag", "document", "documents", "knowledge", "folder", "folders", "volume", "volumes", "index", "source", "sources", "corpus", "files", "wiki", "search", "citation", "citations"),
	readiness.CategoryScopeAndAuth:         assistTerms("tenant", "tenants", "workspace", "workspaces", "role", "roles", "principal", "principals", "user", "users", "scope", "auth", "authentication", "identity", "login", "jwt", "oauth", "session", "sessions", "organization", "organisation"),
	readiness.CategoryPermissions:          assistTerms("permission", "permissions", "role", "roles", "rbac", "acl", "policy", "policies", "access", "allow", "allowed", "authorize", "authorization", "admin", "scopes"),
	readiness.CategoryDeployment:           assistTerms("docker", "compose", "container", "containers", "port", "ports", "migration", "migrations", "deploy", "deployment", "manifest", "signature", "signatures", "package", "health", "helm", "kubernetes", "environment", "smoke", "inspection"),
}

func assistTerms(words ...string) *regexp.Regexp {
	q := make([]string, len(words))
	for i, w := range words {
		q[i] = regexp.QuoteMeta(w)
	}
	return regexp.MustCompile(`\b(` + strings.Join(q, "|") + `)\b`)
}

// assistPlausible reports whether at least one quote mentions a term of the category. A category
// with no terms listed refuses, so adding a category forces someone to list its words.
func assistPlausible(cat readiness.Category, quotes []string) bool {
	re, ok := assistCategoryTerms[cat]
	if !ok {
		return false
	}
	for _, q := range quotes {
		if re.MatchString(strings.ToLower(q)) {
			return true
		}
	}
	return false
}
