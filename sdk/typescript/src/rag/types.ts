import type { SourceID } from "../gen/actions/shared.js";
import type { Citation } from "../gen/conversation/citation.js";
import type { GroundedAnswer } from "../gen/rag/grounded-answer.js";
import type { NoSupportedEvidence } from "../gen/rag/no-supported-evidence.js";
import type { AnswerResult } from "../gen/rag/answer-result.js";
import type { IndexGeneration } from "../gen/rag/index-generation.js";
import type { SourceStatus } from "../gen/rag/source-status.js";

// The client types of the RAG area (master plan 14.2 "RAG", 12.4, 12.5). Only these seven come from
// the schemas under schemas/rag: SearchRequest, SearchResponse, SearchHit, AnswerRequest, AnswerResult,
// SourceStatus and IndexGeneration. The internal records of the retrieval pipeline (EvidenceItem,
// EvidenceCitation, EvidencePackage) carry each source's data-egress mode and classification, are not
// part of any client type and are deliberately not exported: the citation a client holds is the
// conversation Citation, which has no member for either.
export type { AnswerRequest } from "../gen/rag/answer-request.js";
export type { AnswerResult } from "../gen/rag/answer-result.js";
export type { IndexGeneration } from "../gen/rag/index-generation.js";
export type { SearchHit } from "../gen/rag/search-hit.js";
export type { SearchRequest } from "../gen/rag/search-request.js";
export type { SearchResponse } from "../gen/rag/search-response.js";
export type { SourceStatus } from "../gen/rag/source-status.js";

// The two members of an AnswerResult, named so a host can narrow without importing from the generated tree.
export type { GroundedAnswer } from "../gen/rag/grounded-answer.js";
export type { NoSupportedEvidence } from "../gen/rag/no-supported-evidence.js";

/** One registered source: the declaration of the active package it was made from, and its status. */
export interface Source {
  /** The name the application's active package declares the source under. */
  declaration: string;
  /** The version of the package the registration was last bound to. */
  package_version: string;
  /**
   * True while the source is bound to the active package and may be read. A source registered under a
   * package that was replaced is false until it is re-bound, and so is one whose removal began.
   */
  serving: boolean;
  /** Lifecycle, availability, staleness, counts and the generation that serves it. */
  status: SourceStatus;
}

export interface SourceList {
  sources: Source[];
}

/** What a re-audit did, counted. */
export interface ReauditReport {
  checked: number;
  rebound: number;
  queued: number;
  removed: number;
  failed: number;
}

/** An accepted sync or rebuild: the durable job, whose progress is the status of the source. */
export interface SyncAccepted {
  source_id: SourceID;
  job_id: string;
}

export interface GenerationList {
  generations: IndexGeneration[];
}

/**
 * What the reference of a citation resolves to, read at the moment it is resolved: the state of the
 * source now, the passage that was cited and the way to open the source itself.
 */
export interface CitationResolution {
  source_id: Citation["source_id"];
  display_name: string;
  version: Citation["version"];
  locator?: Citation["locator"];
  /** The source's availability now, not at retrieval. */
  availability: Citation["availability"];
  /** The source's staleness now, not at retrieval. */
  staleness: Citation["staleness"];
  /** The way to open the source itself: the host's own record or file reference, or the source by id. */
  open_ref: Citation["open_ref"];
  /** The passage the citation was grounded on, as it was cited; absent when previews are off. */
  preview?: string;
  /** When the reference stops resolving (RFC 3339, UTC). */
  expires_at: string;
}

/** An answer that is grounded: a citation for every claim. Narrowing helper for AnswerResult. */
export const isAnswered = (result: AnswerResult): result is GroundedAnswer => result.outcome === "answered";

/** The typed absence of an answer: no authorised evidence supports one. Narrowing helper for AnswerResult. */
export const isNoSupportedEvidence = (result: AnswerResult): result is NoSupportedEvidence =>
  result.outcome === "no_supported_evidence";
