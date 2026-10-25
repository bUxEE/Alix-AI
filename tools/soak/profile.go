package main

import (
	"fmt"
	"math"
	"strings"

	"github.com/buxee/alix-ai/internal/models"
)

// profileSpec is what a hardware profile stands for: the caps in
// deploy/container/stack/compose.<profile>.yaml (master plan 7.1, ADR 0020).
// Same table as tools/bench and tools/rageval.
type profileSpec struct {
	id     models.Profile
	memMiB float64
	cpus   float64
}

var profileSpecs = []profileSpec{
	{models.ProfileLite, 8192, 4},
	{models.ProfileBalanced, 16384, 8},
	{models.ProfileExperimental4G, 4096, 4},
}

// ParseProfile maps the command-line spelling to the registry's profile enum:
// lite, balanced and experimental-4g (4g for short).
func ParseProfile(s string) (models.Profile, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "4g" {
		return models.ProfileExperimental4G, nil
	}
	for _, p := range profileSpecs {
		if string(p.id) == s {
			return p.id, nil
		}
	}
	return "", fmt.Errorf("unknown profile %q (use lite, balanced or 4g)", s)
}

// profileMemMiB is the memory the profile stands for.
func profileMemMiB(p models.Profile) (float64, bool) {
	for _, s := range profileSpecs {
		if s.id == p {
			return s.memMiB, true
		}
	}
	return 0, false
}

// limitsFor returns the container limits the profile stands for.
func limitsFor(p models.Profile) (ContainerLimits, bool) {
	for _, s := range profileSpecs {
		if s.id == p {
			return ContainerLimits{MemMB: int(s.memMiB), CPUs: s.cpus, Source: "profile-definition"}, true
		}
	}
	return ContainerLimits{}, false
}

// headroomFloorMiB is the absolute floor of the section 7.4 rule: 1 GB.
const headroomFloorMiB = 1024

// headroomFraction is the relative part of the section 7.4 rule: 15 percent.
const headroomFraction = 0.15

// RequiredHeadroomMiB is the memory a certified profile must keep free in
// steady state (master plan 7.4): at least 15 percent of system memory or
// 1 GB, whichever is greater. limitMiB is the memory the profile stands for,
// never one container's cap: the AlixAI container alone is capped at 512 MB
// and its share of a 1 GB floor would fail every sample.
func RequiredHeadroomMiB(limitMiB float64) float64 {
	return math.Round(math.Max(headroomFraction*limitMiB, headroomFloorMiB)*1000) / 1000
}

// Decision-layer axis (milestone D, optional scorer, served as the router role).
const (
	DecisionOff = "off"
	DecisionOn  = "on"
)

// checkDecisionAxis refuses an axis the profile does not allow. The scorer is
// optional and never required by the smallest profile (docs/architecture/decision-layer.md
// section 7, docs/operations/resource-budgets.md R2): experimental-4g declares no scorer, lite
// admits one only when a qualified sub-1B scorer is asserted, balanced may run one.
func checkDecisionAxis(p models.Profile, axis string, qualified bool) error {
	switch axis {
	case DecisionOff:
		return nil
	case DecisionOn:
	default:
		return fmt.Errorf("--decision %q: use off or on", axis)
	}
	switch p {
	case models.ProfileBalanced:
		return nil
	case models.ProfileLite:
		if !qualified {
			return fmt.Errorf("--decision on under lite needs --decision-qualified: the scorer is admitted there only when a qualified sub-1B scorer is configured (resource-budgets R2)")
		}
		return nil
	default:
		return fmt.Errorf("--decision on is not allowed under %s: that profile declares no decision scorer (resource-budgets R2)", p)
	}
}
