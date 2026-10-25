//go:build !live

package main

import (
	"fmt"

	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	suite "github.com/buxee/alix-ai/tests/conformance/providers"
)

// liveBuild is false: this binary has no live support compiled in.
const liveBuild = false

func liveSubject(string, pv.ProviderProfile, func(string) string) (suite.Subject, func(), error) {
	return suite.Subject{}, nil, fmt.Errorf("built without live support (-tags live)")
}
