package providerreg

import (
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// ReplyModelMatches reports whether the model name a provider reported in a reply is the model the profile is
// pinned to (SEC-128). It is exact and fails closed: the reported name is the pinned id, or the pinned id with the
// pinned version as its snapshot suffix, either as the version is written or with its dashes dropped
// (claude-x-20250929 for the version 2025-09-29). A longer id (gpt-4.1 against gpt-4.1-mini), a prefix of the id,
// another snapshot of the same model and another model all differ. An empty name is never a match; the caller
// decides what an unreported model means.
func ReplyModelMatches(id providers.ModelID, version providers.ModelVersion, reported string) bool {
	rep, pin := reported, string(id)
	if rep == "" || pin == "" {
		return false
	}
	if rep == pin {
		return true
	}
	v := string(version)
	if v == "" {
		return false
	}
	return rep == pin+"-"+v || rep == pin+"-"+strings.ReplaceAll(v, "-", "")
}
