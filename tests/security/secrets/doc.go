// Package secretssec is the secret-handling security suite of master plan 16.2 and 16.4 b13:
// secrets must not reach a log, a panic message, the audit log or the repository.
//
// # Tests and the backlog (docs/security/test-backlog.md)
//
//	SEC-082  TestSecLogInspectionNoCanary
//	SEC-082  TestSecLogScannerSeesCanariesInSinksFilesAndDirectories (the control of the log scanner)
//	SEC-084  TestSecRepoSecretScan
//	SEC-084  TestSecSecretDetectorsFlagPlantedSecretsAndOnlyThose (the control of the repository scan)
//	SEC-084  TestSecSecretScanSeesTheRealTestKeyOnlyThroughItsMarker (the scan on the repository's own key)
//
// # Log inspection (SEC-082)
//
// An in-process AlixAI runs: a real store, audit log, policy gateway and action coordinator over
// the in-memory host, identity verification with a generated issuer key, the gateway serving on a
// loopback port, and an application adapter that echoes the credential it was given in its error. Every
// logger is internal/log's (the production redaction layer, with two
// operator-configured keys) and writes to one sink that also writes a log file. Random canaries (never
// literals) go down every path that touches a secret: context tokens in Authorization (accepted,
// replayed, expired, wrong audience, garbage), a cookie, a password and configured fields in request
// bodies, nested and listed values, free text with "Authorization: Bearer ...", "password=..." and URL
// userinfo, API keys resolved from env: and file: secret references and then logged, a key pasted where a reference
// belongs, a credential echoed by the application's adapter, an audit event written
// with sensitive fields, and a recovered handler panic.
//
// The product's /v1 route areas need the whole dialogue stack to mount, so the stack carries the
// credentials through the real identity middleware and the real action coordinator by two routes of its own
// under /v1 (a read and a strict confirm), plus an echo route and a panic route that stand for application
// code that logs what it receives and code that fails.
//
// The scan reads the sink, the log file and the audit events. Controls: the same canaries written without
// the redaction layer are found by the same scanner; the paths that log request data do emit their
// canaries in an unredacted copy of the logs, so redaction is what removed them; with the redaction layer
// taken out of the stack the test reports the leaks; and the scanner fails when it reads nothing.
// The redaction helper is also run on every sensitive key spelling and on values nested in maps,
// slices, groups, structs and errors.
//
// # Repository scan (SEC-084)
//
// secutil.RepoFiles lists the tree; every text file up to 4 MiB is read (larger files and binaries are
// counted and logged, never silently dropped) and the scan fails if it reads fewer than 200 files.
// Detectors: private key blocks (unless TEST-ONLY stands in the content within 15 lines before the key or in
// the first 15 lines of the file; a path never clears a key), AWS key ids, GitHub tokens, JWTs with a real
// signature, bearer literals, high-entropy literals assigned to secret-named keys, long hex or base64
// under such keys, and tracked environment files (only *.env.example may be tracked). A value that
// calls itself a secret or a fixture, counts (0123456789) or reads as a phrase is not a credential, and
// placeholders (change-me, ${...}, <...>, env:, file:, REPLACE, example) are ignored. The TEST-ONLY marker
// also clears a JWT, bearer or assignment finding of a fixture that carries it. Every detector
// is run on planted secrets assembled at run time (the sources hold no secret-shaped literal, so the
// scan does not flag itself) and on clean look-alikes. The history and built images are scanned by CI.
package secretssec
