// Package scaffold writes a DRAFT application-package skeleton from the verified findings of a readiness report
// (master plan 9.5, ADR 0037 item 10). It is the optional --scaffold of `alixai readiness scan`.
//
// What it is. A starting point for the application owner, nothing more. Every file it writes is marked DRAFT, carries no
// signature (there is no signatures/ content at all), is refused by the package loader as unsigned (apppkg reports
// signature.missing) and would be quarantined if anyone installed it. It never certifies, signs or activates anything, and it never
// says a capability is safe.
//
// What goes in. Only verified facts:
//
//   - A finding is used only when it is found or partial, comes from a rule or from a model whose evidence the verifier confirmed
//     (never model_unverified, never unverified, missing or contradicted), cites evidence, and every cited span still holds when
//     this package re-reads the file and re-hashes it. A finding that fails any of these is not used, and nothing it says appears in
//     any output file.
//   - A capability id, tool or operation name from the scan inventory becomes a field only when it occurs in a file that a used
//     finding cites (the file is re-hashed first). The file, lines and hash are written beside it as a comment. Any other name
//     is not written anywhere.
//   - The text of a finding (summary, remediation) is never copied. Documentation files are stubs: the verified references and
//     TODO markers, never generated prose. Prose documentation is the application owner's signed work.
//   - Risk levels, effect classes and permissions are not written at all (TODO). Confirmation defaults to the strictest allowed
//     mode (step_up); the data of a RAG source defaults to restricted and local_only; the package forbids external providers.
//
// Where it writes. Only inside OUTDIR, which must not exist: the directory is created exclusively and every file is created
// exclusively under an os.Root, so a symlink, a junction or a hostile name cannot send a write elsewhere. OUTDIR is never inside
// the scanned directory. File names are fixed (cap-001.yaml, src-001.yaml...), never derived from project text.
package scaffold
