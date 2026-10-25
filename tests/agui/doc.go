// Package aguigold holds the golden AG-UI streams of the gateway's AG-UI endpoint (master plan 15.6, ADR 0035,
// docs/integration/agui.md) and the test that keeps them true.
//
// A golden stream is a recording of what POST /v1/agui answers (or, for the two block streams below, what the run's
// events route replays), made against the real service: the real session store, dialogue engine, policy gateway, action
// coordinator, files service and stream hub behind the real gateway handlers, with the in-memory host application and a
// scripted model provider. Only the model is scripted. The files are what the TypeScript, Go and Python SDK tests (M3-26,
// M3-27, M3-28) replay, so they are deterministic:
//
//   - the clock is fixed, so every time in a stream (a preparation's expiry, a receipt's time) is the same on every run;
//   - the identifiers the engine mints are random (trn_, prep_, rcpt_, src_, fil_, aud_ and the ids derived from them, such
//     as msg_trn_..._1 and alixai_confirm_prep_...), so each is replaced in the recorded files by a canonical one, by the
//     order of first appearance and per prefix: trn_01J00000000000000000000001, prep_01J00000000000000000000001, ... The
//     same replacement is applied to a scenario's request and its response, so a resume request names the preparation its
//     interrupt named;
//   - the confirmation secret is a response header and is random; it is recorded as a placeholder, and the test fails if a
//     real one appears in any file.
//
// # The files
//
// For each scenario NAME, testdata holds
//
//	NAME.sse    the exact body of the response: `data: <AG-UI event on one line>` frames, with `id: <seq>` on the last event of
//	            each TurnEvent (the resume cursor), a blank line after each frame
//	NAME.json   the scenario: its description, the request (method, path, header names that carry a secret, the RunAgentInput as
//	            sent), the response (status, content type, headers, the ids, the event types, every event)
//
// # Regenerating them
//
//	AGUI_WRITE_GOLDEN=1 go test ./tests/agui -run TestGoldenStreamsAreCurrentAndValid
//
// rewrites every file from a fresh recording; without the variable the test records again and fails, naming the file, if
// the bytes differ from what is checked in (and fails on a file no scenario produces). Review the diff: a change to a golden
// stream is a change to what every client sees.
//
// # What the test checks
//
// Every event of every stream validates against the schemas vendored from the pinned AG-UI release (schemas/agui, through
// internal/agui.Schemas, which also rejects a member the schema does not declare) and parses with agui.ParseEvent; the
// lifecycle rules of @ag-ui/client's event verification hold (RUN_STARTED first, nothing after the end, every step, message
// and tool call opened before use and closed before the run ends); each request validates as a RunAgentInput; the
// confirmation secret is in no file; and each scenario contains the events it exists to show.
//
// # Where each stream comes from
//
// Every scenario but two is recorded from the engine: the model's proposals and wording are scripted, the engine's decisions
// are not. The table and chart streams are recorded through the real stream hub, event validator and mapper with TurnEvents
// published from the published examples of schemas/conversation, because the engine does not yet produce table or chart
// blocks (the sessions route publishes progress, directive, citation, clarification, confirmation panel, receipt, text,
// error and done events); they are read back through GET /v1/agui/threads/{thread_id}/runs/{run_id}/events. When the engine
// produces them, record these two from it.
package aguigold
