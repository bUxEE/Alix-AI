package gemini

import (
	_ "embed"
)

// profileJSON is the provider profile this adapter ships as a starting point
// (a "candidate": terms, region and compliance members are "unknown" and the
// terms hash is a placeholder until a human records them, task X-H01; no price
// or limit in it is measured, task X-H02). It describes the cheapest current
// tier the adapter's table knows, gemini-3.5-flash-lite, and its billing notice
// states that Gemini API access is tied to a Google Cloud project's quota and
// billing and that a consumer subscription is not API access (additive plan
// 4.1). The profile registry copies it, mints the real profile id and records
// the operator's choices; a report is for the exact profile, model id and model
// version.
//
//go:embed profile.json
var profileJSON []byte

// ProfileJSON returns a copy of the shipped starting profile.
func ProfileJSON() []byte { return append([]byte(nil), profileJSON...) }
