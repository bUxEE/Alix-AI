import type {
  AnswerResult,
  CitationResolution,
  GenerationList,
  IndexGeneration,
  ReauditReport,
  SearchResponse,
  Source,
  SourceList,
  SyncAccepted,
} from "./types.js";

// Response guards of the RAG area, in the style of src/guards.ts: they check that the members each
// type requires are present with the right JSON type before the data is handed to the host, so a
// proxy's `{}` or an HTML page that parsed as something is a typed error and never an `undefined`
// member three calls later. They are not a validator (unknown members are tolerated, so additive
// server changes do not break a client; the schemas under schemas/rag stay the authority for the deep
// structure), with three exceptions that are the point of this area and are checked strictly:
//
//   * provenance: an answer has at least one claim and one citation, every claim cites at least one
//     citation that exists, and every citation lists the claims it supports. A document answer without
//     a source is not an answer, so it is refused whatever the server says (master plan 3 rule 9);
//   * the closed result: an AnswerResult is `answered` or `no_supported_evidence` and nothing else;
//   * internal metadata: a response that carries a source's data-egress mode or classification anywhere
//     is refused. Those members are internal to the core and not part of any client type, so a server
//     that sends them is not the server this SDK speaks to, and the host never sees them.

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

type Check = (value: unknown) => boolean;

const string: Check = (v) => typeof v === "string";
const nonEmptyString: Check = (v) => typeof v === "string" && v.length > 0;
const boolean: Check = (v) => typeof v === "boolean";
const integer: Check = (v) => typeof v === "number" && Number.isInteger(v);
const count: Check = (v) => typeof v === "number" && Number.isInteger(v) && v >= 0;
const object: Check = (v) => isRecord(v);
const oneOf =
  (...values: readonly string[]): Check =>
  (v) =>
    typeof v === "string" && values.includes(v);
const nullable =
  (check: Check): Check =>
  (v) =>
    v === null || check(v);
const arrayOf =
  (check: Check): Check =>
  (v) =>
    Array.isArray(v) && v.every(check);

const shape =
  (required: Record<string, Check>, optional: Record<string, Check> = {}): Check =>
  (value) => {
    if (!isRecord(value)) return false;
    for (const [key, check] of Object.entries(required)) if (!check(value[key])) return false;
    for (const [key, check] of Object.entries(optional)) if (value[key] !== undefined && !check(value[key])) return false;
    return true;
  };

/** True when a member that is internal to the core appears anywhere in the value. */
const INTERNAL_MEMBERS: readonly string[] = ["source_egress", "source_classification"];

export const carriesInternalMetadata = (value: unknown, depth = 0): boolean => {
  if (depth > 12) return true; // nothing the API answers is this deep: refuse rather than walk it
  if (Array.isArray(value)) return value.some((item) => carriesInternalMetadata(item, depth + 1));
  if (!isRecord(value)) return false;
  return Object.entries(value).some(([key, item]) => INTERNAL_MEMBERS.includes(key) || carriesInternalMetadata(item, depth + 1));
};

const availability = oneOf("available", "unavailable", "removed");
const staleness = oneOf("current", "stale", "unknown");
const retrievalNotices = arrayOf(oneOf("reduced_semantic_recall", "query_rewritten"));
const generationState = oneOf("building", "validating", "active", "superseded", "failed", "invalidated");
const openRef: Check = (v) => isRecord(v) && oneOf("file", "record", "source")(v["kind"]);

const sourceStatus = shape(
  {
    source_id: nonEmptyString,
    display_name: nonEmptyString,
    state: oneOf("registered", "indexing", "ready", "paused", "disabled", "removing"),
    availability,
    staleness,
    last_attempt_at: nullable(string),
    last_success_at: nullable(string),
    counts: shape({ discovered: count, indexed: count, skipped: count, failed: count, tombstoned: count }),
    active_generation_id: nullable(string),
  },
  { error: object },
);

const source = shape({ declaration: string, package_version: string, serving: boolean, status: sourceStatus });

const indexGeneration = shape(
  {
    generation_id: nonEmptyString,
    source_id: nonEmptyString,
    state: generationState,
    components: object,
    counts: shape({ documents: count, chunks: count, embedded_chunks: count }),
    created_at: string,
    activated_at: nullable(string),
    retired_at: nullable(string),
  },
  { error: object },
);

const searchHit = shape(
  {
    evidence_id: nonEmptyString,
    rank: (v) => integer(v) && (v as number) >= 1,
    source_id: nonEmptyString,
    display_name: nonEmptyString,
    version: nonEmptyString,
    availability,
    staleness,
    open_ref: openRef,
    excerpt: nonEmptyString,
  },
  { locator: object },
);

const citation = shape(
  {
    source_id: nonEmptyString,
    display_name: nonEmptyString,
    version: nonEmptyString,
    availability,
    staleness,
    open_ref: openRef,
    claims: (v) => Array.isArray(v) && v.length > 0 && v.every(nonEmptyString),
  },
  { locator: object },
);

// Every guard of this area refuses a response that carries internal metadata, whatever its shape.
const guardCheck =
  <T>(check: Check) =>
  (value: unknown): value is T =>
    !carriesInternalMetadata(value) && check(value);

export const isSource = guardCheck<Source>(source);

export const isSourceList = guardCheck<SourceList>(shape({ sources: arrayOf(source) }));

export const isReauditReport = guardCheck<ReauditReport>(
  shape({ checked: count, rebound: count, queued: count, removed: count, failed: count }),
);

export const isSyncAccepted = guardCheck<SyncAccepted>(shape({ source_id: nonEmptyString, job_id: nonEmptyString }));

export const isIndexGeneration = guardCheck<IndexGeneration>(indexGeneration);

export const isGenerationList = guardCheck<GenerationList>(shape({ generations: arrayOf(indexGeneration) }));

export const isCitationResolution = guardCheck<CitationResolution>(
  shape(
    {
      source_id: nonEmptyString,
      display_name: nonEmptyString,
      version: nonEmptyString,
      availability,
      staleness,
      open_ref: openRef,
      expires_at: nonEmptyString,
    },
    { locator: object, preview: string },
  ),
);

export const isSearchResponse = guardCheck<SearchResponse>(shape({ results: arrayOf(searchHit), notices: retrievalNotices }));

/** The provenance rule: every claim cites a citation that exists, and the answer has claims and citations. */
const groundedAnswer = (value: unknown): boolean => {
  if (!shape({ outcome: oneOf("answered"), claims: Array.isArray, citations: Array.isArray, notices: retrievalNotices })(value)) return false;
  const answer = value as { claims: unknown[]; citations: unknown[] };
  if (answer.claims.length === 0 || answer.citations.length === 0 || !answer.citations.every(citation)) return false;
  return answer.claims.every((claim) => {
    if (!shape({ text: nonEmptyString, citation_indexes: Array.isArray })(claim)) return false;
    const indexes = (claim as { citation_indexes: unknown[] }).citation_indexes;
    return indexes.length > 0 && indexes.every((i) => integer(i) && (i as number) >= 0 && (i as number) < answer.citations.length);
  });
};

const noSupportedEvidence = shape({
  outcome: oneOf("no_supported_evidence"),
  reason: oneOf("no_match", "claims_unsupported", "evidence_unavailable", "egress_restricted"),
  notices: retrievalNotices,
});

export const isAnswerResult = guardCheck<AnswerResult>((value) => groundedAnswer(value) || noSupportedEvidence(value));
