import type { Choice, Effect, Fact } from "@alixai/sdk";
import { fixedId } from "../ids";
import type { DemoLocale } from "../prompts";
import type { Frame } from "./turn";

// The conversations the mock remembers: each thread's history (what a connect run replays), its pending goal (the client-safe
// projection `state.alixai.pending`), the actions it prepared and the clarification it asked, and every run's frames, so a run can
// be read again from a cursor or replayed for the same run id.

export interface HistoryMessage {
  readonly id: string;
  readonly role: "user" | "assistant";
  readonly content: string;
}

export interface Pending {
  readonly kind: "confirmation" | "clarification" | "goal";
  readonly capabilityId: string;
  readonly phase: string;
  readonly collectedFields: readonly { readonly name: string; readonly origin: string }[];
  readonly missingFields: readonly string[];
  readonly question?: { readonly field: string; readonly answerKind: string; readonly choices?: readonly { readonly id: string; readonly label: string }[] };
  readonly preparationId?: string;
  readonly expiresAt: string;
}

export interface PreparedAction {
  readonly preparationId: string;
  readonly capabilityId: string;
  readonly summary: string;
  readonly effects: readonly Effect[];
  readonly riskLevel: 2;
  readonly expiresAt: string;
  readonly locale: DemoLocale;
  /** The record the commit creates, and the facts its receipt states. */
  readonly record: { readonly entityType: string; readonly entityId: string };
  readonly receiptFacts: readonly Fact[];
  readonly collected: readonly { readonly name: string; readonly origin: string }[];
  status: "pending" | "approved" | "declined";
}

export interface Clarification {
  readonly field: string;
  readonly capabilityId: string;
  readonly choices: readonly Choice[];
  readonly locale: DemoLocale;
}

export interface RecordedRun {
  readonly runId: string;
  readonly frames: Frame[];
  /** The live request writing it, while one is. */
  writer: AbortController | undefined;
  /** How many frames have been written to anyone. */
  written: number;
  finished: boolean;
  /** Set by a cancel: the writer ends the run with the cancelled outcome. */
  cancelRequested: boolean;
  /** Frames after which the first delivery is cut (the reconnect scenario). */
  cutAfter: number | undefined;
}

export interface Thread {
  readonly id: string;
  title: string;
  updatedAt: string;
  history: HistoryMessage[];
  pending: Pending | null;
  locale: DemoLocale;
  readonly prepared: Map<string, PreparedAction>;
  clarification: Clarification | undefined;
  readonly runs: Map<string, RecordedRun>;
}

export interface ConversationSummary {
  readonly id: string;
  readonly title?: string;
  readonly preview?: string;
  readonly updatedAt?: string;
}

const daysAgo = (days: number, hour: number): string => {
  const at = new Date(Date.now() - days * 24 * 60 * 60 * 1000);
  at.setUTCHours(hour, 12, 0, 0);
  return at.toISOString();
};

const seedThread = (id: string, title: string, updatedAt: string, pairs: readonly (readonly [string, string])[]): Thread => {
  const turn = fixedId("trn", id.length * 31 + title.length);
  return {
    id,
    title,
    updatedAt,
    history: pairs.flatMap(([user, assistant], index): HistoryMessage[] => [
      { id: `msg_${turn}_u${index + 1}`, role: "user", content: user },
      { id: `msg_${turn}_${index + 1}`, role: "assistant", content: assistant },
    ]),
    pending: null,
    locale: "en",
    prepared: new Map(),
    clarification: undefined,
    runs: new Map(),
  };
};

export interface ThreadStore {
  get: (id: string) => Thread;
  find: (id: string) => Thread | undefined;
  list: () => ConversationSummary[];
  create: () => Thread;
  remove: (id: string) => boolean;
}

export const createThreadStore = (welcomeId: string): ThreadStore => {
  const threads = new Map<string, Thread>();
  const seeds = [
    seedThread(welcomeId, "HVAC filters at Harbor Point", daysAgo(0, 8), [
      [
        "When were the HVAC filters at Harbor Point last replaced?",
        "The filters of both air handling units on level 3 of **Harbor Point Office Tower** were replaced on **14 July 2026** (work order `WO-2140`).\n\nUnder the preventive maintenance policy the next replacement is due by **14 October 2026**.",
      ],
    ]),
    seedThread("ses_demo_q3_board", "Q3 summary for the board", daysAgo(1, 15), [
      [
        "Summarise Q3 for the board in three bullet points",
        "- **201** work orders opened and **184** closed across the four sites.\n- **6** were overdue at the end of the quarter, 3 of them at Westfield Data Hall.\n- Maintenance spend was **€441k**, 41% of it on HVAC.",
      ],
      ["Which site needs attention?", "**Westfield Data Hall**: it has the most overdue work orders (3) and the longest mean time to close among the large sites (3.1 days)."],
    ]),
    seedThread("ses_demo_riverside", "Riverside fire inspection", daysAgo(3, 10), [
      [
        "Is Riverside Depot compliant with the fire-safety schedule?",
        "Not yet. The Q3 inspection tagged a **sprinkler valve** for follow-up (work order `WO-2294`, overdue since 3 October), and the full Q4 inspection is due by **31 October**.",
      ],
    ]),
    seedThread("ses_demo_energy_it", "Consumi estivi", daysAgo(9, 17), [
      [
        "Quanto abbiamo consumato a luglio al Westfield Data Hall?",
        "A luglio 2026 il **Westfield Data Hall** ha consumato **797 MWh**, il 7,6% in più di giugno, per il carico di raffreddamento.",
      ],
    ]),
  ];
  seeds[3]!.locale = "it";
  for (const thread of seeds) threads.set(thread.id, thread);

  const blank = (id: string): Thread => ({
    id,
    title: "",
    updatedAt: new Date().toISOString(),
    history: [],
    pending: null,
    locale: "en",
    prepared: new Map(),
    clarification: undefined,
    runs: new Map(),
  });

  let counter = 0;
  return {
    get: (id) => {
      const found = threads.get(id);
      if (found !== undefined) return found;
      const created = blank(id);
      threads.set(id, created);
      return created;
    },
    find: (id) => threads.get(id),
    list: () =>
      [...threads.values()]
        .filter((thread) => thread.history.length > 0 || thread.title !== "")
        .sort((a, b) => b.updatedAt.localeCompare(a.updatedAt))
        .map((thread) => {
          const last = thread.history[thread.history.length - 1];
          const firstUser = thread.history.find((message) => message.role === "user");
          const preview = last === undefined ? undefined : last.content.replace(/[*_`#>\\-]/g, "").replace(/\s+/g, " ").trim().slice(0, 120);
          const title = thread.title !== "" ? thread.title : firstUser?.content.slice(0, 60);
          return { id: thread.id, ...(title !== undefined && { title }), ...(preview !== undefined && { preview }), updatedAt: thread.updatedAt };
        }),
    create: () => {
      counter += 1;
      const thread = blank(`ses_demo_new_${Date.now().toString(36)}${counter}`);
      thread.title = "New conversation";
      threads.set(thread.id, thread);
      return thread;
    },
    remove: (id) => threads.delete(id),
  };
};
