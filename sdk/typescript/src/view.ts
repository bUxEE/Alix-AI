import type { ActionReceipt, PreparedAction } from "./gen/actions/index.js";
import type { Error as ErrorBody } from "./gen/common/index.js";
import type {
  Citation,
  CitationLocator,
  ClarificationChoicesBlock,
  ConfirmationPanelBlock,
  DialogueDirective,
  DoneEvent,
  ProgressEvent,
  RichBlock,
  TurnEvent,
} from "./gen/conversation/index.js";

// What a UI needs from the events of a turn, folded into one object (master plan 15.1 and 15.2): the text, the typed blocks (Markdown,
// code, table, chart, diagram, citations, action cards), the clarification choices, the prepared action with its confirmation panel,
// the receipt, the directive, the citations with their source-link data, and how the turn ended. The SDK renders nothing: an
// integrating application keeps its visual design, routing and accessibility; this is the data to render from.

/** The state of one turn as far as its events go. A partial turn (a stream in progress) is a valid input. */
export interface TurnView {
  /** The turn's id, from its first event. */
  turnId: string | undefined;
  /**
   * The model's text, the `token` events joined in order. It is Markdown sanitised as a whole by the core and then cut into
   * fragments, so render this joined text and never a fragment alone.
   */
  text: string;
  /** The typed blocks, in order (every `block` event, and the clarification as a block too). */
  blocks: RichBlock[];
  /** The citations of the answer, in order, from `citation` events: one source each, with the claims it supports. */
  citations: Citation[];
  /** The clarification the turn asks, with its choices (answer with `choice_id`). */
  clarification: ClarificationChoicesBlock | undefined;
  /** The action the turn prepared. Consequential: show its `confirmationPanel` and let the user confirm or reject, with the secret that came with the turn. */
  preparedAction: PreparedAction | undefined;
  /** The confirmation panel block of the prepared action, when the core sent one. */
  confirmationPanel: ConfirmationPanelBlock | undefined;
  /** The receipt of an action the turn committed (the durable record of the outcome). */
  receipt: ActionReceipt | undefined;
  /** The act the dialogue engine decided (`present_result`, `request_confirmation`, `disambiguate`, `report_failure`, ...). */
  directive: DialogueDirective | undefined;
  /** Where the turn is: the latest progress event of each stage, in the order the stages first appeared. */
  progress: ProgressEvent[];
  /** The typed errors of the turn (a refusal or a failure is an `error` event before `done`, never a hidden state). */
  errors: ErrorBody[];
  /** The `done` event, when the turn has ended. */
  done: DoneEvent | undefined;
  /** How the turn ended: `completed`, `awaiting_input`, `awaiting_confirmation`, `cancelled` or `failed`; undefined while it runs. */
  finishReason: DoneEvent["finish_reason"] | undefined;
}

/** Folds events (all of a turn, or the first few of a stream in progress) into a `TurnView`. */
export const viewTurn = (events: readonly TurnEvent[]): TurnView => {
  const view: TurnView = {
    turnId: undefined,
    text: "",
    blocks: [],
    citations: [],
    clarification: undefined,
    preparedAction: undefined,
    confirmationPanel: undefined,
    receipt: undefined,
    directive: undefined,
    progress: [],
    errors: [],
    done: undefined,
    finishReason: undefined,
  };
  const stages = new Map<string, number>();
  for (const event of events) {
    view.turnId ??= event.turn_id;
    switch (event.type) {
      case "token":
        view.text += event.text;
        break;
      case "block":
        view.blocks.push(event.block);
        if (event.block.type === "confirmation_panel") view.confirmationPanel = event.block;
        break;
      case "clarification":
        view.clarification = event.clarification;
        view.blocks.push(event.clarification);
        break;
      case "citation":
        view.citations.push(event.citation);
        break;
      case "prepared_action":
        view.preparedAction = event.prepared_action;
        break;
      case "receipt":
        view.receipt = event.receipt;
        break;
      case "directive":
        view.directive = event.directive;
        break;
      case "progress": {
        const at = stages.get(event.stage);
        if (at === undefined) {
          stages.set(event.stage, view.progress.length);
          view.progress.push(event);
        } else view.progress[at] = event;
        break;
      }
      case "error":
        view.errors.push(event.error);
        break;
      case "done":
        view.done = event;
        view.finishReason = event.finish_reason;
        break;
    }
  }
  return view;
};

/** The blocks of one type, typed: `blocksOfType(view.blocks, "table")`. */
export const blocksOfType = <T extends RichBlock["type"]>(blocks: readonly RichBlock[], type: T): Array<Extract<RichBlock, { type: T }>> =>
  blocks.filter((block): block is Extract<RichBlock, { type: T }> => block.type === type);

/**
 * The data to render a citation as a source link. `open` says how to open the source and is an opaque reference, never a URL or a
 * path: resolve a `file` reference with `rag.resolveCitation` (which re-checks scope and expiry), or hand a `record` to the host
 * application. A source that is not `available` has nothing to open, and a stale one should say so.
 */
export interface CitationLink {
  /** The source's name as the user is authorised to see it. */
  label: string;
  /** The source id. */
  sourceId: string;
  /** The answer claims this source supports, one short statement each. */
  claims: string[];
  /** Where in the source the evidence sits, when known. */
  locator: CitationLocator | undefined;
  /** A short human-readable place ("page 3", "section Warranty", "record 12", "characters 120 to 340"), or undefined when none is known. */
  place: string | undefined;
  /** The permission-checked way to open the source, or undefined when the source can no longer be opened. */
  open: Citation["open_ref"] | undefined;
  available: boolean;
  /** True when the source changed after the version the answer was grounded on; false for `current` and `unknown`. */
  stale: boolean;
  /** The citation's own statement of staleness, for a UI that wants to tell `unknown` from `current`. */
  staleness: Citation["staleness"];
}

const placeOf = (locator: CitationLocator | undefined): string | undefined => {
  if (locator === undefined) return undefined;
  if (locator.page !== undefined) return `page ${locator.page}`;
  if (locator.section !== undefined) return `section ${locator.section}`;
  if (locator.record !== undefined) return `record ${locator.record}`;
  if (locator.span !== undefined) return "a passage";
  return undefined;
};

/** The rendering data of a citation. */
export const citationLink = (citation: Citation): CitationLink => {
  const available = citation.availability === "available";
  return {
    label: citation.display_name,
    sourceId: citation.source_id,
    claims: citation.claims,
    locator: citation.locator,
    place: placeOf(citation.locator),
    open: available ? citation.open_ref : undefined,
    available,
    stale: citation.staleness === "stale",
    staleness: citation.staleness,
  };
};
