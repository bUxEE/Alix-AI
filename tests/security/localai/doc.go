// Package localaisec is the LocalAI deployment suite of master plan 16.1 and 16.4 (bullets 8, 9 and 10): LocalAI is
// reachable by AlixAI only, with a key; it serves only the surfaces AlixAI uses; it installs nothing; it runs no agent,
// no MCP command and no tool of its own.
//
// # Tests and the backlog (docs/security/test-backlog.md, sections 4.6)
//
//	SEC-066  TestSecLocalAIUnreachableWithoutKey   live only (-tags live, ALIXAI_SEC_LOCALAI_URL)
//	SEC-068  TestSecLocalAIHardeningFlags
//	SEC-069  TestSecNoModelInstallSurface          static, plus a live half behind -tags live
//	SEC-070  TestSecLocalAIAgentsAndMCPDisabled    static, plus a live half behind -tags live
//
// # How SEC-068 reads the deployment
//
// The checklist (docs/security/localai-hardening-checklist.md, section 4) is applied to every file that launches
// LocalAI. The files are found by walking the checkout and judging their structure, not their names: a compose
// service whose image is LocalAI's, merged with each of its override files; a Quadlet container with its drop-ins and
// the env files it names; a systemd unit that runs LocalAI or the supervisor, with the supervisor configuration whose
// child environment it passes; an env file that sets a LOCALAI_ variable. A setting may be an environment entry (map or
// list), an Environment= line, a --flag, or an env-file line; ${VAR:?}, ${VAR:-default} and the values of the shipped
// .env.example are resolved, and every value a setting can take must satisfy its rule, so a default that weakens a rule
// counts as the weakened value. Anything that launches LocalAI outside deploy/ fails the scan, and so does a scan that
// finds nothing or cannot read a file. docs/ and tests/ hold recorded evidence and throw-away harnesses (one of them
// raises the log level to debug on purpose) and are out of scope.
//
// The exceptions of section 6 are read from the checklist itself. Each relaxes exactly its row (EX-01 the one local
// backend catalogue, mounted read-only and pinned by digest; EX-02 catalogue names resolved in that catalogue; EX-03 the
// unset opaque-errors switch; EX-04 the unset metrics switch), only while its entry is in the table with a reason, an
// owner and a date that has not passed.
//
// # Controls
//
// A clean sample deployment is rendered in every file form and must pass. Then dozens of planted violations, one rule
// at a time, are rendered in every form that can carry them and must be reported under the right checklist row; each row
// the detector can report has at least one. Twenty lines of the real shipped files are broken in memory, and the same
// detector must flag each. The exception boundaries are tested the same way: an exception beside its row, a missing,
// expired or unreadable entry, and a neighbouring row all stay flagged.
//
// # Live probes
//
// Files with the build tag `live` probe a running stack and skip unless ALIXAI_SEC_LOCALAI_URL is set (optionally
// ALIXAI_SEC_LOCALAI_KEY and, for the LAN probe, ALIXAI_SEC_LOCALAI_LAN_URL). Each begins with a control: an exempt
// route answers 200, so a refusal means something. Without the tag the live halves are no-ops (live_stub_test.go).
package localaisec
