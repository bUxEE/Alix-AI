package meta

import (
	_ "embed"
)

// profileJSON is the provider profile this adapter ships as a starting point (a
// "candidate"): Meta's own Model API (https://api.meta.ai/v1) with its standard
// tier model muse-spark-1.3. Terms, region and compliance members are "unknown",
// the terms URL is null and the terms hash is a placeholder until a human records
// them (task X-H01); no price or limit in it is measured (task X-H02: prices and
// rate limits are the figures the documentation summary gave on 2026-10-08, the
// output cap and the tool maximum are conservative defaults). Its billing notice
// states that Meta consumer AI access is not API access (additive plan 4.1). A
// hosted-Llama deployment (DialectOpenAICompat) copies this profile and replaces
// the endpoint, model id, limits, prices, terms and billing notice with the
// chosen host's. The profile registry copies it, mints the real profile id and
// records the operator's choices; a report is for the exact profile, model id and
// model version.
//
//go:embed profile.json
var profileJSON []byte

// ProfileJSON returns a copy of the shipped starting profile.
func ProfileJSON() []byte { return append([]byte(nil), profileJSON...) }
