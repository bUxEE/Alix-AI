// Package supplychainsec is the supply-chain part of the security suite (master
// plan 16.3 and 22.5): what may be installed, pulled and run, and how the
// repository proves it checks its own dependencies.
//
//	SEC-063  TestSecModelHashMismatchRefused
//	SEC-064  TestSecAllImagesPinnedByDigest, TestSecImageDetectorsSeePlantedViolations
//	SEC-089  TestSecStaticAnalysisConfigured, TestSecStaticAnalysisDetectorsSeePlantedViolations
//
// SEC-063 drives models.Verify, the function `alixai serve` calls before the
// inference path starts, over every entry of the shipped model registry.
//
// SEC-064 reads every compose file, Quadlet unit, Dockerfile, env example and
// workflow of the checkout and requires each pulled image to end in a sha256
// digest. SEC-089 reads .github/workflows/security.yml and requires govulncheck
// and gosec to gate the build; it also cross-checks that every fuzz target the
// workflow names exists.
//
// Every detector has a planted-violation test on inline text: a scan that
// cannot see its violation would let the real repository pass by finding
// nothing, so each rule is shown to fire first.
package supplychainsec
