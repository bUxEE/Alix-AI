// RAG client surface (sources, indexing, search, grounded answers). src/index.ts re-exports this file as
// the `rag` namespace, so everything below is `rag.<name>` to a host.
//
//   const client = rag.createRagClient({ baseUrl, getContextToken });
//   const answer = await client.answer({ question: "How long is the warranty?" });
//   if (rag.isAnswered(answer)) show(answer.claims, answer.citations);
//   else show(answer.reason); // no_supported_evidence: never an uncited answer

// The client types, and only those: the internal evidence records are not exported.
export type {
  AnswerRequest,
  AnswerResult,
  GroundedAnswer,
  IndexGeneration,
  NoSupportedEvidence,
  SearchHit,
  SearchRequest,
  SearchResponse,
  SourceStatus,
} from "./types.js";
// The wire types the OpenAPI document defines for this area beyond those.
export type { CitationResolution, GenerationList, ReauditReport, Source, SourceList, SyncAccepted } from "./types.js";
export { isAnswered, isNoSupportedEvidence } from "./types.js";

export { createRagClient } from "./client.js";
export type { RagClient, RagClientOptions, RagContextTokenProvider, RagContextTokenRequest } from "./client.js";
export { RAG_OPERATIONS } from "./operations.js";
export type { RagOperationId, RagOperationSpec } from "./operations.js";
