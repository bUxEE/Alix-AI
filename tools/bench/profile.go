package main

import (
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/internal/models"
)

// profileSpec is what a hardware profile stands for. The numbers are the
// caps in deploy/container/localai/compose.<profile>.yaml (master plan 7.1,
// docs/decisions/0020-4gb-profile.md).
type profileSpec struct {
	id    models.Profile
	memMB int
	cpus  float64
}

var profileSpecs = []profileSpec{
	{models.ProfileLite, 8192, 4},
	{models.ProfileBalanced, 16384, 8},
	{models.ProfileExperimental4G, 4096, 4},
}

// ParseProfile maps the command-line spelling to the registry's profile enum:
// lite, balanced and experimental-4g, with 4g as the short form of the last.
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

func specFor(p models.Profile) (profileSpec, bool) {
	for _, s := range profileSpecs {
		if s.id == p {
			return s, true
		}
	}
	return profileSpec{}, false
}

// IsEmulated reports whether results under profile are emulated. Every
// profile the tool knows is a resource-limited one: lite, balanced and
// experimental-4g are cgroup caps on a machine that is larger than the
// profile, so what they measure is evidence for qualified-emulated, never for
// certified (master plan 7.4; the registry refuses a certified entry that
// rests on an emulated measurement). There is deliberately no way to run the
// tool with a profile that claims otherwise; a certified-hardware profile
// needs a new, reviewed value here.
func IsEmulated(p models.Profile) bool {
	_, known := specFor(p)
	return known
}

// LimitsFor returns the container limits the profile stands for.
func LimitsFor(p models.Profile) (ContainerLimits, bool) {
	s, ok := specFor(p)
	if !ok {
		return ContainerLimits{}, false
	}
	return ContainerLimits{MemMB: s.memMB, CPUs: s.cpus, Source: "profile-definition"}, true
}
