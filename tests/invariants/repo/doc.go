// Package repo holds repository-wide invariants of AlixAI, read from the checkout on disk. It holds tests only
// and fails closed: a scan that finds nothing is an error, never a pass.
//
//   - TestNoLocalAIImportsOutsideAdapterAndCompositionRoots: LocalAI code is imported only by adapters/localai
//     and the composition roots (cmd/, internal/cli/, tools/). TestPkgContractsImportsNothingInternal:
//     pkg/contracts depends on the standard library, third parties and its own subpackages only.
//   - TestComposeLoopbackOnly, TestComposeImagesPinnedByDigest and TestLocalAIAdminSurfaceStaysOff: every
//     published container port binds to the loopback, every image is pinned by digest, and LocalAI's web UI,
//     gallery, settings, MCP, agents and peer networking stay off with its API key required.
//   - TestIDPrefixesMatchSchemaGrammar: every id prefix minted in internal/ fits the typedId grammar of
//     schemas/common/ids.schema.json.
//   - TestGoSourcesAreGofmtFormatted and TestCIActionsPinnedBySHA: gofmt cleanliness, and every GitHub Action
//     pinned by commit SHA with a version comment.
//
// Every detector has a table test on inline sources, so a rule cannot pass by finding nothing.
package repo
