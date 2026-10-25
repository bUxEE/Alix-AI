# 0023 — What Hybrid mode sends by default: structured facts, RAG excerpts, or both

- **Status:** provisional
- **Date:** 2026-10-03
- **Decided in task:** X-01; finalised by X-22

## Context

The open decision asks whether Hybrid sends selected RAG excerpts, only structured facts, or both by default.
Hybrid keeps identity, scope, application state, file indexing and RAG authorisation local, and lets the external
model see "only selected authorized excerpts, structured task state and response requirements" (§2.3). The
contracts that already exist fix much of the answer, so this ADR records the default on top of them
(`docs/integration/provider-schemas.md` §5 to §8, merged in X-02):

- a RAG source has an egress mode, `local_only` (the default for every source), `allow_excerpt`,
  `allow_metadata` or `deny`; a source with no policy is `local_only`;
- the sendable categories of a request envelope are `user_message`, `conversation_history`, `structured_state`,
  `deterministic_fact`, `dialogue_directive`, `action_receipt`, `rag_excerpt`, `rag_metadata`, `file_content`,
  `image_content`, `audio_content`; a `rag_excerpt` part must name its source and that source's mode must be
  `allow_excerpt`, so a `local_only` or `deny` source cannot be built into an envelope at all;
- `restricted` data is never egress-eligible in version 1; credentials, raw application database content,
  filesystem paths, database handles, MCP URLs and fields marked local-only are never sent;
- an `internal` or `confidential` source with `allow_excerpt` or `allow_metadata` needs a recorded operator
  approval; an unknown classification is the most restrictive.

## Options

| Option | Security | Quality and cost | Verdict |
| --- | --- | --- | --- |
| A. Facts only, no excerpts ever in Hybrid | smallest leak surface | an answer that needs document wording cannot be realised externally; more turns fall back to the local model | too restrictive as the only mode |
| B. Facts plus excerpts from every authorised source | every source the principal may read can leave | best answer quality; the largest egress and cost | rejected: contradicts "RAG sources default to `local_only`" |
| C. Facts only by default; excerpts only from sources an operator marked `allow_excerpt` (per source, bounded, approved for internal and confidential) | each excerpt is a deliberate, recorded, per-source operator decision | quality gain only where the operator accepted the exposure | **default** |

## Decision

**The default Hybrid envelope carries structured task state and deterministic facts only** (categories
`user_message`, `structured_state`, `deterministic_fact`, `dialogue_directive`, `action_receipt`). **RAG excerpts
(`rag_excerpt`) enter an envelope only from sources explicitly marked `allow_excerpt`, bounded in size and
authorised for the principal.** `rag_metadata` (titles, ids, status) follows `allow_excerpt` or `allow_metadata`.
`conversation_history` is not part of the default and is included only where a turn needs it and the policy
chain permits it. Because every source defaults to `local_only`, "both" is reached source by source, never as a
deployment-wide switch.

If an answer needs a `local_only` source and the local model cannot supply it, the turn returns the typed result
"selected provider cannot access the required evidence" (X-12) and never sends the source.

## Consequences

- Easier to audit: the egress decision record lists, per source, whether an excerpt or only metadata was
  included, with no content.
- Harder: Hybrid quality on document-heavy questions depends on how many sources the operator opens up. The
  report has to show the quality difference per source setting (below).
- Forbidden: a deployment-level "send all excerpts" switch; widening the envelope after the guard has run;
  sending a `restricted` source under any setting.

## What confirms or overturns this (owed)

- X-22 builds the Hybrid orchestration over this default; X-12 enforces the per-source filter in the real turn
  pipeline (`TestLocalOnlySourceNeverInAnyPrompt`, planned).
- X-23 measures, from recorded fixtures and labelled as such, the quality, bytes sent and cost per turn of
  facts-only against facts plus `allow_excerpt` on the RAG corpus. Before running it, X-22 or X-23 writes the
  pass bar (the quality gain that justifies an operator opening a source). If facts-only passes the corpus,
  option A stays the recommended setting.
- X-H03 reviews which classes and sources may ever be marked `allow_excerpt` for a regulated tenant.

## Reversal plan

The default lives in the envelope builder of `internal/dataegress` (X-04) and the Hybrid orchestration
(`internal/modes/hybrid`, X-22). Tightening it to facts-only everywhere, or loosening it, changes those two
places and this ADR; stored source modes, schemas and decision records do not change.
