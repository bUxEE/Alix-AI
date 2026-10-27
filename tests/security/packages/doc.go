// Package packagessec is the package-supply-chain security suite of master plan
// 16.2 and 16.4 (b12): what the package loader and the trust manifest builder do
// with an application package archive that is hostile.
//
// A package archive is attacker-controlled bytes. Loading one must never write a
// file outside the install directory, never create a link or a special file, and
// must refuse an archive that is too big, too deep or too compressible before it
// has been read to the end. Each test here drives the production loader
// (internal/apppkg: Load, FromArchive, FromArchiveFile and the Installer that
// writes into quarantine) and the production manifest builder (internal/trust),
// over archives built at run time with archive/tar and compress/gzip. Nothing
// hostile is committed to the repository.
//
// # Tests and the backlog (docs/security/test-backlog.md)
//
//	SEC-059  TestSecPackageArchiveTraversal
//
// # How it proves "nothing was written"
//
// Every case runs in a temporary tree that holds the data directory the
// installer writes to and a sentinel directory beside it that a traversal would
// hit. The whole tree is walked before and after the load, and the two walks
// must be identical (same paths and modes; sizes and content digests of regular
// files); the walk also
// fails on any symbolic link or irregular file anywhere in the tree. Absolute,
// drive-letter, UNC and \\?\ names are aimed at the real path of the sentinel
// file, so an extractor that honoured them would overwrite it.
//
// # Controls
//
// Every "nothing happened" assertion has a control that it would have been seen:
//
//   - a deliberately naive extractor (joins the entry name onto a directory and
//     writes) is run over the very same hostile archive and the tree walk must
//     show the escape, so the walk is able to see an escape and the archive is
//     able to cause one;
//   - the walk is run over a planted extra file, a planted overwrite of the
//     sentinel and a planted symbolic link (when the platform lets the test
//     create one) and must flag each; its link classifier is also run over every
//     file mode;
//   - the benign archive installs and the install directory holds exactly the
//     regular files of the archive;
//   - the size, depth, file-count and ratio refusals are paired with the same
//     archive under generous limits, which is not refused, so the refusal is the
//     limit and nothing else;
//   - bounded memory is measured: the compressed bytes a counting reader served
//     and the bytes the process allocated while loading a bomb are compared with
//     the same load under a deliberately weakened ratio limit.
package packagessec
