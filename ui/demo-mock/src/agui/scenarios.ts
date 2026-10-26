import type { ActionReceipt, Choice, Citation, Effect, ErrorCode, Fact, FileRef, RichBlock } from "@alixai/sdk";
import { ENERGY_MWH, OPEN_BY_SITE, OPEN_WORK_ORDERS, Q3_BY_SITE, Q3_SPEND, SITES, filterCitations, reportCitations, t } from "../data/northwind";
import { SEEDED_FILES, type FileStore } from "../files/store";
import { isoAt, newId } from "../ids";
import type { MeMemoryHandler } from "../me-memory";
import { demoPrompt, demoPrompts, type DemoLocale, type DemoScenarioId, type DemoText } from "../prompts";
import type { Clarification, Pending, PreparedAction, Thread } from "./threads";
import type { AguiEvent, TurnBuilder } from "./turn";

// The scripted turns. Each scenario writes the AG-UI events the gateway would map from the engine's TurnEvents for a request of
// that kind (pipeline steps, the directive, blocks as `alixai.block.<type>` activities, citations, the confirmation tool call and
// its interrupt, text in fragments) and says how the run ends and what the thread's pending goal becomes. The blocks have the
// shapes of schemas/conversation/rich-block.schema.json; the suite validates every one of them.

export interface Suggestion {
  readonly title: string;
  readonly message: string;
  readonly capabilityId?: string;
}

export type Outcome =
  | { readonly type: "success"; readonly finishReason: "completed" | "awaiting_input"; readonly suggestions?: readonly Suggestion[] }
  | { readonly type: "interrupt"; readonly interrupt: Record<string, unknown> }
  | { readonly type: "cancelled" }
  | { readonly type: "error"; readonly message: string; readonly code: ErrorCode };

export interface ScenarioResult {
  readonly outcome: Outcome;
  /** The thread's pending goal at the end of the run (`state.alixai.pending`). */
  readonly pending: Pending | null;
}

export interface ScenarioContext {
  readonly turn: TurnBuilder;
  readonly locale: DemoLocale;
  readonly thread: Thread;
  readonly turnId: string;
  readonly now: number;
  readonly files: FileStore;
  readonly userText: string;
  readonly meMemory?: MeMemoryHandler;
}

const MINUTE = 60 * 1000;

const msgId = (ctx: ScenarioContext, n: number | string): string => `msg_${ctx.turnId}_${n}`;
const actId = (ctx: ScenarioContext, kind: string): string => `act_${ctx.turnId}_${kind}`;
const tr = (ctx: ScenarioContext, text: DemoText): string => t(ctx.locale, text);

const STEP_DELAY: Record<string, number> = { interpreting: 260, validating: 160, tool_call: 420, retrieving: 520, generating: 300, verifying: 140, committing: 380 };

/** The pipeline stages of a turn; the tool call stage also reports the capability it ran. */
const pipeline = (ctx: ScenarioContext, capabilityId: string | undefined, stages: readonly string[], toolState = "completed"): void => {
  for (const stage of stages) {
    ctx.turn.step(stage, STEP_DELAY[stage] ?? 200, () => {
      if (stage === "tool_call" && capabilityId !== undefined) {
        ctx.turn.add({ type: "ACTIVITY_SNAPSHOT", messageId: actId(ctx, "tool_call"), activityType: "alixai.progress", content: { capabilityId, stage: "tool_call", state: toolState } });
      }
    });
  }
};

const READ_STAGES = ["interpreting", "validating", "tool_call", "generating", "verifying"] as const;

const CONFIRM_BINDINGS = ["preparation_id", "principal", "session", "data_versions"] as const;

interface DirectiveInit {
  readonly capabilityId: string | null;
  readonly act: string;
  readonly facts?: readonly Fact[];
  readonly mustState?: readonly string[];
  readonly missing?: readonly string[];
  readonly nextQuestion?: Record<string, unknown>;
  readonly confirmation?: "none" | "explicit" | "step_up";
  readonly next?: readonly Record<string, unknown>[];
  readonly sources?: readonly Citation[];
  readonly preparationId?: string;
  readonly receiptId?: string;
  readonly reasonCode?: ErrorCode;
}

/** The `alixai.directive` custom event: what the core decided the reply may say (schemas/conversation/dialogue-directive.schema.json). */
const directive = (ctx: ScenarioContext, init: DirectiveInit): void => {
  const mode = init.confirmation ?? "none";
  ctx.turn.add({
    type: "CUSTOM",
    name: "alixai.directive",
    value: {
      turn_id: ctx.turnId,
      capability_id: init.capabilityId,
      act: init.act,
      facts: init.facts ?? [],
      must_state: init.mustState ?? [],
      missing_fields: init.missing ?? [],
      invalid_fields: [],
      ...(init.nextQuestion !== undefined && { next_question: init.nextQuestion }),
      allowed_suggestions: [],
      confirmation: { mode, bindings: mode === "none" ? [] : [...CONFIRM_BINDINGS] },
      permitted_next_actions: init.next ?? [],
      sources: init.sources ?? [],
      ...(init.preparationId !== undefined && { preparation_id: init.preparationId }),
      ...(init.receiptId !== undefined && { receipt_id: init.receiptId }),
      ...(init.reasonCode !== undefined && { reason_code: init.reasonCode }),
    },
  });
  ctx.turn.commit(60);
};

const block = (ctx: ScenarioContext, n: number, value: RichBlock, delayMs = 160): void => {
  ctx.turn.activity(actId(ctx, `b${n}`), `alixai.block.${value.type}`, value, delayMs);
};

/** Citations as the gateway sends them: one snapshot with the first, then one `add /citations/-` delta per further citation. */
const citations = (ctx: ScenarioContext, list: readonly Citation[]): void => {
  const id = actId(ctx, "citations");
  const [first, ...rest] = list;
  if (first === undefined) return;
  ctx.turn.activity(id, "alixai.citations", { citations: [first] }, 140);
  for (const citation of rest) {
    ctx.turn.add({ type: "ACTIVITY_DELTA", messageId: id, activityType: "alixai.citations", patch: [{ op: "add", path: "/citations/-", value: citation }] });
    ctx.turn.commit(110);
  }
};

const goal = (ctx: ScenarioContext, capabilityId: string, collected: readonly string[]): Pending => ({
  kind: "goal",
  capabilityId,
  phase: "success",
  collectedFields: collected.map((name) => ({ name, origin: "user_turn" })),
  missingFields: [],
  expiresAt: isoAt(ctx.now + 30 * MINUTE).replace(/\.\d{3}Z$/, "Z"),
});

/** Follow-up prompts, as `alixai.suggestions` carries them: picking one sends its message. */
export const suggestionsFor = (locale: DemoLocale, ids: readonly DemoScenarioId[]): Suggestion[] =>
  ids.map((id) => {
    const prompt = demoPrompt(id);
    return { title: prompt.title[locale], message: prompt.prompt[locale] };
  });

const text = (value: string): Fact["value"] => ({ kind: "text", value });
const fact = (field: string, value: Fact["value"], display?: string): Fact => ({ field, value, ...(display !== undefined && { display }) });

const dayLabel = (ms: number, locale: DemoLocale, withTime: boolean): string =>
  new Intl.DateTimeFormat(locale === "it" ? "it-IT" : "en-GB", {
    weekday: "short",
    day: "numeric",
    month: "short",
    ...(withTime && { hour: "2-digit", minute: "2-digit" }),
    timeZone: "Europe/Rome",
  }).format(new Date(ms));

/** The next working day after `from` (Monday to Friday), at the given UTC hour and minute. */
const workingDay = (from: number, offset: number, hourUtc: number, minute: number): number => {
  const day = new Date(from);
  let added = 0;
  while (added < offset) {
    day.setUTCDate(day.getUTCDate() + 1);
    const weekday = day.getUTCDay();
    if (weekday !== 0 && weekday !== 6) added += 1;
  }
  day.setUTCHours(hourUtc, minute, 0, 0);
  return day.getTime();
};

// ---- help and the default reply ----

export const help = (ctx: ScenarioContext, unknown: boolean): ScenarioResult => {
  directive(ctx, { capabilityId: null, act: "offer_help" });
  const intro = unknown
    ? tr(ctx, {
        en: "I don't have a scripted answer for that one. This is the AlixAI demo backend: every reply is scripted, and nothing leaves your machine. Try one of these:",
        it: "Per questa richiesta non ho una risposta preparata. Questo è il backend dimostrativo di AlixAI: ogni risposta è scritta in anticipo e nulla esce dal tuo computer. Prova una di queste:",
      })
    : tr(ctx, {
        en: "I'm the Northwind Facilities assistant, running on the AlixAI demo backend: every reply is scripted, and nothing leaves your machine. Each of these shows a different part of the UI kit:",
        it: "Sono l'assistente di Northwind Facilities, sul backend dimostrativo di AlixAI: ogni risposta è scritta in anticipo e nulla esce dal tuo computer. Ognuna di queste mostra una parte diversa del kit:",
      });
  const lines = demoPrompts
    .filter((prompt) => prompt.id !== "help")
    .map((prompt) => `- **${prompt.title[ctx.locale]}**: ${prompt.prompt[ctx.locale]}${prompt.kits !== undefined ? ` (${prompt.kits.map((kit) => (kit === "vue" ? "Vue" : "React")).join(", ")})` : ""}`);
  ctx.turn.text(msgId(ctx, 1), `${intro}\n\n${lines.join("\n")}`);
  return { outcome: { type: "success", finishReason: "completed", suggestions: suggestionsFor(ctx.locale, ["markdown", "confirmation", "citations", "long"]) }, pending: ctx.thread.pending };
};

// ---- plain streamed Markdown ----

export const markdown = (ctx: ScenarioContext): ScenarioResult => {
  pipeline(ctx, "work_orders.list", READ_STAGES);
  directive(ctx, { capabilityId: "work_orders.list", act: "present_result", facts: [fact("open", { kind: "integer", value: 7 }), fact("overdue", { kind: "integer", value: 2 })], mustState: ["open", "overdue"] });
  const body = tr(ctx, {
    en: [
      "## Maintenance at a glance",
      "",
      "Northwind Facilities looks after **4 sites** and **312 maintained assets**. This week:",
      "",
      "- **7** open work orders, **2** of them overdue (`WO-2294` and `WO-2301`)",
      "- one planned shutdown: *Westfield Data Hall*, Saturday 06:00 to 10:00",
      "- **96%** of P1 calls were answered within 4 hours in Q3",
      "",
      "### What the team covers",
      "",
      "1. **HVAC**: filters, belts and coils on the planned schedule",
      "2. **Fire safety**: sprinklers, alarms and extinguishers",
      "3. **Electrical and lifts**: inspections and reactive repairs",
      "",
      "> Report anything urgent by phone: a chat request is not an emergency call.",
      "",
      "Response times by priority:",
      "",
      "```text",
      "P1  urgent    4 hours",
      "P2  high      1 business day",
      "P3  planned   5 business days",
      "```",
      "",
      "The full rules are in the [preventive maintenance policy](<https://northwind.example/policies/preventive-maintenance>).",
    ].join("\n"),
    it: [
      "## La manutenzione in sintesi",
      "",
      "Northwind Facilities gestisce **4 sedi** e **312 impianti in manutenzione**. Questa settimana:",
      "",
      "- **7** ordini di lavoro aperti, **2** in ritardo (`WO-2294` e `WO-2301`)",
      "- un fermo programmato: *Westfield Data Hall*, sabato dalle 06:00 alle 10:00",
      "- il **96%** delle chiamate P1 ha avuto risposta entro 4 ore nel terzo trimestre",
      "",
      "### Di cosa si occupa il team",
      "",
      "1. **HVAC**: filtri, cinghie e batterie secondo il piano",
      "2. **Antincendio**: sprinkler, allarmi ed estintori",
      "3. **Impianti elettrici e ascensori**: verifiche e interventi su guasto",
      "",
      "> Per le urgenze telefona: una richiesta in chat non è una chiamata di emergenza.",
      "",
      "Tempi di risposta per priorità:",
      "",
      "```text",
      "P1  urgente      4 ore",
      "P2  alta         1 giorno lavorativo",
      "P3  pianificata  5 giorni lavorativi",
      "```",
      "",
      "Le regole complete sono nella [politica di manutenzione preventiva](<https://northwind.example/policies/preventive-maintenance>).",
    ].join("\n"),
  });
  ctx.turn.text(msgId(ctx, 1), body);
  return {
    outcome: { type: "success", finishReason: "completed", suggestions: suggestionsFor(ctx.locale, ["table", "chart_bar", "citations"]) },
    pending: goal(ctx, "work_orders.list", ["status"]),
  };
};

// ---- charts, table, diagram ----

const openBySiteChart = (ctx: ScenarioContext): RichBlock => {
  const urgent = tr(ctx, { en: "Urgent (P1)", it: "Urgenti (P1)" });
  const planned = tr(ctx, { en: "Planned (P2, P3)", it: "Pianificati (P2, P3)" });
  return {
    type: "chart",
    title: tr(ctx, { en: "Open work orders per site", it: "Ordini di lavoro aperti per sede" }),
    description: tr(ctx, {
      en: "Grouped bars of open work orders per site. Harbor Point: 1 urgent, 4 planned. Riverside: 2 urgent, 3 planned. Elm Street: 0 urgent, 2 planned. Westfield: 3 urgent, 2 planned.",
      it: "Barre raggruppate degli ordini aperti per sede. Harbor Point: 1 urgente, 4 pianificati. Riverside: 2 urgenti, 3 pianificati. Elm Street: 0 urgenti, 2 pianificati. Westfield: 3 urgenti, 2 pianificati.",
    }),
    mark: "bar",
    data: OPEN_BY_SITE.flatMap((row) => [
      { site: row.site, kind: urgent, open: row.urgent },
      { site: row.site, kind: planned, open: row.planned },
    ]),
    encoding: {
      x: { field: "site", type: "nominal", title: tr(ctx, { en: "Site", it: "Sede" }) },
      y: { field: "open", type: "quantitative", title: tr(ctx, { en: "Open work orders", it: "Ordini aperti" }) },
      color: { field: "kind", type: "nominal", title: tr(ctx, { en: "Priority", it: "Priorità" }) },
    },
  };
};

const MONTHS: Record<DemoLocale, readonly string[]> = {
  en: ["Apr", "May", "Jun", "Jul", "Aug", "Sep"],
  it: ["apr", "mag", "giu", "lug", "ago", "set"],
};

const energyChart = (ctx: ScenarioContext): RichBlock => ({
  type: "chart",
  title: tr(ctx, { en: "Monthly energy use, April to September 2026", it: "Consumi energetici mensili, aprile-settembre 2026" }),
  description: tr(ctx, {
    en: "Lines of monthly energy use in MWh. Harbor Point: 412, 398, 455, 503, 496, 430. Westfield Data Hall: 688, 702, 741, 797, 802, 735. Both peak in July and August.",
    it: "Linee dei consumi mensili in MWh. Harbor Point: 412, 398, 455, 503, 496, 430. Westfield Data Hall: 688, 702, 741, 797, 802, 735. Entrambi hanno il picco a luglio e agosto.",
  }),
  mark: "line",
  data: ENERGY_MWH.flatMap((row, index) => [
    { month: MONTHS[ctx.locale][index] as string, site: "Harbor Point", mwh: row.harbor },
    { month: MONTHS[ctx.locale][index] as string, site: "Westfield", mwh: row.westfield },
  ]),
  encoding: {
    x: { field: "month", type: "ordinal", title: tr(ctx, { en: "Month", it: "Mese" }) },
    y: { field: "mwh", type: "quantitative", title: "MWh" },
    color: { field: "site", type: "nominal", title: tr(ctx, { en: "Site", it: "Sede" }) },
  },
});

export const chartBar = (ctx: ScenarioContext): ScenarioResult => {
  pipeline(ctx, "work_orders.list", READ_STAGES);
  directive(ctx, { capabilityId: "work_orders.list", act: "present_result" });
  block(ctx, 1, openBySiteChart(ctx));
  ctx.turn.text(
    msgId(ctx, 1),
    tr(ctx, {
      en: "**Westfield Data Hall** has the most urgent work orders (3), followed by Riverside Logistics Depot (2). Elm Street Medical Center has none.",
      it: "Il **Westfield Data Hall** ha più ordini urgenti (3), seguito dal Riverside Logistics Depot (2). L'Elm Street Medical Center non ne ha.",
    }),
  );
  return { outcome: { type: "success", finishReason: "completed", suggestions: suggestionsFor(ctx.locale, ["table", "chart_line"]) }, pending: goal(ctx, "work_orders.list", ["site"]) };
};

export const chartLine = (ctx: ScenarioContext): ScenarioResult => {
  pipeline(ctx, "energy.read", READ_STAGES);
  directive(ctx, { capabilityId: "energy.read", act: "present_result" });
  block(ctx, 1, energyChart(ctx));
  ctx.turn.text(
    msgId(ctx, 1),
    tr(ctx, {
      en: "Both sites peak in **July and August** with the cooling load. Westfield Data Hall uses about **1.7 times** as much energy as Harbor Point, and its September figure (735 MWh) is still 7% above April.",
      it: "Entrambe le sedi hanno il picco a **luglio e agosto** per il carico di raffreddamento. Il Westfield Data Hall consuma circa **1,7 volte** l'energia di Harbor Point, e a settembre (735 MWh) è ancora del 7% sopra aprile.",
    }),
  );
  return { outcome: { type: "success", finishReason: "completed", suggestions: suggestionsFor(ctx.locale, ["chart_bar", "long"]) }, pending: goal(ctx, "energy.read", ["site", "period"]) };
};

const openOrdersTable = (ctx: ScenarioContext): RichBlock => ({
  type: "table",
  caption: tr(ctx, { en: `Open work orders on ${dayLabel(ctx.now, "en", false)}`, it: `Ordini di lavoro aperti al ${dayLabel(ctx.now, "it", false)}` }),
  columns: [
    { key: "work_order", label: tr(ctx, { en: "Work order", it: "Ordine" }), kind: "text" },
    { key: "site", label: tr(ctx, { en: "Site", it: "Sede" }), kind: "text" },
    { key: "category", label: tr(ctx, { en: "Category", it: "Categoria" }), kind: "text" },
    { key: "priority", label: tr(ctx, { en: "Priority", it: "Priorità" }), kind: "text" },
    { key: "due", label: tr(ctx, { en: "Due", it: "Scadenza" }), kind: "date" },
    { key: "overdue", label: tr(ctx, { en: "Overdue", it: "In ritardo" }), kind: "boolean" },
    { key: "hours", label: tr(ctx, { en: "Est. hours", it: "Ore stimate" }), kind: "number" },
  ],
  rows: OPEN_WORK_ORDERS.map((row) => [row.id, row.site, tr(ctx, row.category), row.priority, row.due, row.overdue, row.hours]),
});

export const table = (ctx: ScenarioContext): ScenarioResult => {
  pipeline(ctx, "work_orders.list", READ_STAGES);
  directive(ctx, { capabilityId: "work_orders.list", act: "present_result" });
  block(ctx, 1, openOrdersTable(ctx));
  ctx.turn.text(
    msgId(ctx, 1),
    tr(ctx, {
      en: "There are **7** open work orders. **2** are overdue: `WO-2294` (fire safety, Riverside) and `WO-2301` (electrical, Westfield), both P1.",
      it: "Ci sono **7** ordini di lavoro aperti. **2** sono in ritardo: `WO-2294` (antincendio, Riverside) e `WO-2301` (elettrico, Westfield), entrambi P1.",
    }),
  );
  return { outcome: { type: "success", finishReason: "completed", suggestions: suggestionsFor(ctx.locale, ["chart_bar", "confirmation"]) }, pending: goal(ctx, "work_orders.list", ["status"]) };
};

export const diagram = (ctx: ScenarioContext): ScenarioResult => {
  pipeline(ctx, undefined, ["interpreting", "generating", "verifying"]);
  directive(ctx, { capabilityId: null, act: "offer_help" });
  const labels =
    ctx.locale === "it"
      ? { req: "Richiesta ricevuta", triage: "Triage", urgent: "Urgente", routine: "Ordinaria", dispatch: "Invio tecnico", plan: "Visita pianificata", work: "Intervento", check: "Controllo qualità", closed: "Chiuso", reopen: "Non superato" }
      : { req: "Request received", triage: "Triage", urgent: "Urgent", routine: "Routine", dispatch: "Dispatch technician", plan: "Schedule visit", work: "Work carried out", check: "Quality check", closed: "Closed", reopen: "Failed" };
  block(ctx, 1, {
    type: "diagram",
    language: "mermaid",
    requires_host_renderer: true,
    title: tr(ctx, { en: "Work-order lifecycle", it: "Ciclo di vita di un ordine di lavoro" }),
    description: tr(ctx, {
      en: "A request is triaged. Urgent requests dispatch a technician, routine ones are scheduled. After the work, a quality check either closes the order or sends it back to the work step.",
      it: "Una richiesta passa dal triage. Quelle urgenti fanno partire un tecnico, quelle ordinarie vengono pianificate. Dopo l'intervento, il controllo qualità chiude l'ordine o lo rimanda all'intervento.",
    }),
    source: [
      "flowchart LR",
      `  A[${labels.req}] --> B{${labels.triage}}`,
      `  B -->|${labels.urgent}| C[${labels.dispatch}]`,
      `  B -->|${labels.routine}| D[${labels.plan}]`,
      `  C --> E[${labels.work}]`,
      "  D --> E",
      `  E --> F{${labels.check}}`,
      `  F -->|OK| G[${labels.closed}]`,
      `  F -->|${labels.reopen}| E`,
    ].join("\n"),
  });
  ctx.turn.text(
    msgId(ctx, 1),
    tr(ctx, {
      en: "Every work order follows the same path: **triage** decides whether a technician goes out now (P1) or a visit is scheduled (P2, P3), and nothing is closed until it passes the **quality check**.",
      it: "Ogni ordine segue lo stesso percorso: il **triage** decide se un tecnico parte subito (P1) o se la visita viene pianificata (P2, P3), e nulla si chiude prima del **controllo qualità**.",
    }),
  );
  return { outcome: { type: "success", finishReason: "completed", suggestions: suggestionsFor(ctx.locale, ["table", "confirmation"]) }, pending: ctx.thread.pending };
};

// ---- files ----

const attachmentOf = (ref: FileRef): Record<string, unknown> => ({
  fileId: ref.file_id,
  name: ref.name,
  mimeType: ref.mime_type,
  sizeBytes: ref.size_bytes,
  previewable: ref.previewable,
  expiresAt: ref.expires_at,
});

/** The `alixai.attachments` activity of a run whose message carried files (sent right after RUN_STARTED). */
export const attachmentsActivity = (ctx: ScenarioContext, refs: readonly FileRef[]): AguiEvent => ({
  type: "ACTIVITY_SNAPSHOT",
  messageId: actId(ctx, "attachments"),
  activityType: "alixai.attachments",
  content: { files: refs.map(attachmentOf) },
});

export const image = (ctx: ScenarioContext): ScenarioResult => {
  pipeline(ctx, "work_orders.get", READ_STAGES);
  directive(ctx, { capabilityId: "work_orders.get", act: "present_result" });
  const refs = [ctx.files.get(SEEDED_FILES.floorPlan), ctx.files.get(SEEDED_FILES.energyChart)].filter((ref): ref is FileRef => ref !== undefined);
  ctx.turn.activity(actId(ctx, "files"), "alixai.attachments", { files: refs.map(attachmentOf) }, 200);
  ctx.turn.text(
    msgId(ctx, 1),
    tr(ctx, {
      en: "Here is the **level 3 floor plan** of Harbor Point Office Tower. The HVAC plant room is the amber area in the south-east corner; the red dot marks the filter bank of air handling units AHU-3A and AHU-3B. I also attached the Q3 energy chart of the building.",
      it: "Ecco la **planimetria del piano 3** di Harbor Point Office Tower. La centrale HVAC è l'area ambra nell'angolo sud-est; il punto rosso indica il banco filtri delle unità AHU-3A e AHU-3B. Ho allegato anche il grafico dei consumi del terzo trimestre.",
    }),
  );
  return { outcome: { type: "success", finishReason: "completed", suggestions: suggestionsFor(ctx.locale, ["confirmation", "chart_line"]) }, pending: goal(ctx, "work_orders.get", ["site"]) };
};

/** The reply to a message that carried files and asked for nothing the demo scripts. */
export const filesReceived = (ctx: ScenarioContext, refs: readonly FileRef[]): ScenarioResult => {
  pipeline(ctx, undefined, ["interpreting", "generating", "verifying"]);
  directive(ctx, { capabilityId: null, act: "offer_help" });
  const list = refs.map((ref) => `- **${ref.name}** (${ref.mime_type}, ${Math.max(1, Math.round(ref.size_bytes / 1024))} KB)`).join("\n");
  ctx.turn.text(
    msgId(ctx, 1),
    tr(ctx, {
      en: `I've got ${refs.length === 1 ? "your file" : `your ${refs.length} files`}:\n\n${list}\n\nThey stay with this conversation for two hours and only you can preview them. Ask me to create a work order and I'll attach them to it.`,
      it: `Ho ricevuto ${refs.length === 1 ? "il tuo file" : `i tuoi ${refs.length} file`}:\n\n${list}\n\nRestano in questa conversazione per due ore e solo tu puoi vederne l'anteprima. Chiedimi di creare un ordine di lavoro e li allegherò.`,
    }),
  );
  return { outcome: { type: "success", finishReason: "completed", suggestions: suggestionsFor(ctx.locale, ["confirmation"]) }, pending: ctx.thread.pending };
};

// ---- an answer with sources ----

export const grounded = (ctx: ScenarioContext): ScenarioResult => {
  pipeline(ctx, undefined, ["interpreting", "retrieving"]);
  const list = filterCitations(ctx.locale);
  citations(ctx, list);
  pipeline(ctx, undefined, ["generating", "verifying"]);
  directive(ctx, { capabilityId: null, act: "present_result", sources: list });
  ctx.turn.text(
    msgId(ctx, 1),
    tr(ctx, {
      en: [
        "Here is what the sources say about HVAC filters:",
        "",
        "- Filters of air handling units are replaced **every 3 months**, or earlier when the pressure drop exceeds **250 Pa**. \\[1\\]",
        "- Offices use **ePM1 55% (F7)** filters; clinical areas use **ePM1 80% (F9)**. \\[1\\]",
        "- Coolwave supplies the filters and up to **4 replacements per unit per year** are included in the fee. \\[2\\]",
        "- At Elm Street Medical Center the pressure drop is checked **monthly**. \\[3\\] This document has not been re-indexed for a while, so check it before relying on it.",
        "",
        "Sources:",
        "",
        "- \\[1\\] Preventive Maintenance Policy, page 4 (section 3.2)",
        "- \\[2\\] HVAC Service Contract (Coolwave Ltd), page 11 (Schedule B)",
        "- \\[3\\] Elm Street ventilation requirements, page 2 (out of date)",
      ].join("\n"),
      it: [
        "Ecco cosa dicono le fonti sui filtri HVAC:",
        "",
        "- I filtri delle unità di trattamento aria si sostituiscono **ogni 3 mesi**, o prima se la perdita di carico supera **250 Pa**. \\[1\\]",
        "- Gli uffici usano filtri **ePM1 55% (F7)**; le aree cliniche **ePM1 80% (F9)**. \\[1\\]",
        "- Coolwave fornisce i filtri e il canone include fino a **4 sostituzioni per unità all'anno**. \\[2\\]",
        "- All'Elm Street Medical Center la perdita di carico si controlla **ogni mese**. \\[3\\] Il documento non è stato reindicizzato da tempo: verificalo prima di farci affidamento.",
        "",
        "Fonti:",
        "",
        "- \\[1\\] Politica di manutenzione preventiva, pagina 4 (sezione 3.2)",
        "- \\[2\\] Contratto di servizio HVAC (Coolwave Ltd), pagina 11 (Allegato B)",
        "- \\[3\\] Requisiti di ventilazione Elm Street, pagina 2 (non aggiornato)",
      ].join("\n"),
    }),
  );
  return { outcome: { type: "success", finishReason: "completed", suggestions: suggestionsFor(ctx.locale, ["confirmation", "long"]) }, pending: ctx.thread.pending };
};

// ---- clarification, confirmation, step up ----

const inspectionChoices = (ctx: ScenarioContext): Choice[] =>
  [
    [1, 6, 30],
    [1, 9, 0],
    [2, 12, 30],
  ].map(([offset, hour, minute], index) => {
    const at = workingDay(ctx.now, offset as number, hour as number, minute as number);
    return {
      choice_id: `slot_${index + 1}`,
      label: dayLabel(at, ctx.locale, true),
      value: { kind: "datetime", value: isoAt(at), zone: "Europe/Rome" },
      description: tr(ctx, { en: "Inspector: Fiamma Safety Srl", it: "Ispettore: Fiamma Safety Srl" }),
    };
  });

export const clarification = (ctx: ScenarioContext): ScenarioResult => {
  const capabilityId = "inspections.book";
  pipeline(ctx, undefined, ["interpreting", "validating"]);
  const choices = inspectionChoices(ctx);
  directive(ctx, {
    capabilityId,
    act: "ask_missing",
    missing: ["slot"],
    nextQuestion: { field: "slot", answer_kind: "choice", choices },
    facts: [fact("site", text(SITES.riverside))],
  });
  const prompt = tr(ctx, {
    en: "Which slot should I book for the fire-safety inspection at Riverside Logistics Depot?",
    it: "Quale fascia prenoto per l'ispezione antincendio al Riverside Logistics Depot?",
  });
  block(ctx, 1, { type: "clarification_choices", field: "slot", prompt, answer_kind: "choice", allow_free_text: true, choices });
  ctx.turn.text(
    msgId(ctx, 1),
    tr(ctx, {
      en: "Fiamma Safety has three openings for the **fire-safety inspection** at Riverside Logistics Depot. Which one should I book?",
      it: "Fiamma Safety ha tre disponibilità per l'**ispezione antincendio** al Riverside Logistics Depot. Quale prenoto?",
    }),
  );
  const clarify: Clarification = { field: "slot", capabilityId, choices, locale: ctx.locale };
  ctx.thread.clarification = clarify;
  return {
    outcome: { type: "success", finishReason: "awaiting_input" },
    pending: {
      kind: "clarification",
      capabilityId,
      phase: "",
      collectedFields: [
        { name: "site", origin: "user_turn" },
        { name: "kind", origin: "user_turn" },
      ],
      missingFields: ["slot"],
      question: { field: "slot", answerKind: "choice", choices: choices.map((choice) => ({ id: choice.choice_id, label: choice.label })) },
      expiresAt: isoAt(ctx.now + 30 * MINUTE).replace(/\.\d{3}Z$/, "Z"),
    },
  };
};

interface PrepareInit {
  readonly capabilityId: string;
  readonly summary: string;
  readonly effects: Effect[];
  readonly cardTitle: string;
  readonly cardFacts: Fact[];
  readonly prompt: string;
  readonly record: { readonly entityType: string; readonly entityId: string };
  readonly receiptFacts: Fact[];
  readonly collected: readonly { readonly name: string; readonly origin: string }[];
}

/** A prepared action of risk level 2: the card, the confirmation tool call, the confirmation panel, the text and the interrupt. */
const prepare = (ctx: ScenarioContext, init: PrepareInit): ScenarioResult => {
  const preparationId = newId("prep", ctx.now);
  const expiresAt = isoAt(ctx.now + 10 * MINUTE);
  const toolCallId = `alixai_confirm_${preparationId}`;
  const confirmation = { mode: "explicit", bindings: [...CONFIRM_BINDINGS] };
  pipeline(ctx, init.capabilityId, READ_STAGES);
  directive(ctx, {
    capabilityId: init.capabilityId,
    act: "request_confirmation",
    facts: [fact("summary", text(init.summary)), ...init.cardFacts],
    mustState: ["summary"],
    confirmation: "explicit",
    next: [{ action: "confirm" }, { action: "modify" }, { action: "reject" }],
    preparationId,
  });
  block(ctx, 1, {
    type: "action_card",
    title: init.cardTitle,
    capability_id: init.capabilityId,
    facts: init.cardFacts,
    actions: [{ action: "modify", label: tr(ctx, { en: "Change details", it: "Modifica i dettagli" }) }],
    preparation_id: preparationId,
  });
  const args = { preparationId, capabilityId: init.capabilityId, summary: init.summary, effects: init.effects, riskLevel: 2, expiresAt, confirmation };
  ctx.turn.add(
    { type: "TOOL_CALL_START", toolCallId, toolCallName: "alixai_confirm_action" },
    { type: "TOOL_CALL_ARGS", toolCallId, delta: JSON.stringify(args) },
    { type: "TOOL_CALL_END", toolCallId },
  );
  ctx.turn.commit(120);
  block(ctx, 2, { type: "confirmation_panel", preparation_id: preparationId, summary: init.summary, effects: init.effects, risk_level: 2, expires_at: expiresAt, confirmation: { mode: "explicit", bindings: [...CONFIRM_BINDINGS] } }, 80);
  ctx.turn.text(msgId(ctx, 1), init.prompt);
  const prepared: PreparedAction = {
    preparationId,
    capabilityId: init.capabilityId,
    summary: init.summary,
    effects: init.effects,
    riskLevel: 2,
    expiresAt,
    locale: ctx.locale,
    record: init.record,
    receiptFacts: init.receiptFacts,
    collected: init.collected,
    status: "pending",
  };
  ctx.thread.prepared.set(preparationId, prepared);
  ctx.thread.clarification = undefined;
  return {
    outcome: {
      type: "interrupt",
      interrupt: {
        id: preparationId,
        reason: "confirmation_required",
        message: init.summary,
        toolCallId,
        responseSchema: { additionalProperties: false, properties: { approved: { type: "boolean" } }, required: ["approved"], type: "object" },
        expiresAt,
        metadata: { preparationId, capabilityId: init.capabilityId, summary: init.summary, effects: init.effects, riskLevel: 2, expiresAt, confirmation },
      },
    },
    pending: {
      kind: "confirmation",
      capabilityId: init.capabilityId,
      phase: "prepared",
      collectedFields: init.collected,
      missingFields: [],
      preparationId,
      expiresAt: expiresAt.replace(/\.\d{3}Z$/, "Z"),
    },
  };
};

export const confirmation = (ctx: ScenarioContext): ScenarioResult => {
  const scheduled = workingDay(ctx.now, 2, 7, 0);
  const day = isoAt(scheduled).slice(0, 10);
  const dayText = dayLabel(scheduled, ctx.locale, false);
  const workOrder = `WO-${2318 + (ctx.thread.prepared.size % 50)}`;
  const task = tr(ctx, { en: "Replace the filters of AHU-3A and AHU-3B (ePM1 55%)", it: "Sostituire i filtri di AHU-3A e AHU-3B (ePM1 55%)" });
  const summary = tr(ctx, {
    en: `Create a P2 work order at Harbor Point Office Tower: replace the HVAC filters on level 3 on ${dayText}`,
    it: `Creare un ordine P2 a Harbor Point Office Tower: sostituzione dei filtri HVAC al piano 3, ${dayText}`,
  });
  const cardFacts: Fact[] = [
    fact("site", text(SITES.harbor)),
    fact("task", text(task)),
    fact("priority", text("P2")),
    fact("scheduled_for", { kind: "date", value: day }, dayText),
    fact("contractor", text("Coolwave Ltd")),
    fact("estimated_cost", { kind: "money", value: "420.00", currency: "EUR" }, "€420.00"),
  ];
  return prepare(ctx, {
    capabilityId: "work_orders.create",
    summary,
    effects: [
      {
        operation: "create",
        entity_type: "work_order",
        description: tr(ctx, { en: `Create work order ${workOrder} (P2, HVAC) at Harbor Point Office Tower.`, it: `Crea l'ordine ${workOrder} (P2, HVAC) a Harbor Point Office Tower.` }),
        changes: [fact("task", text(task)), fact("priority", text("P2")), fact("scheduled_for", { kind: "date", value: day }, dayText)],
        external: false,
      },
      {
        operation: "send",
        entity_type: "notification",
        description: tr(ctx, { en: "E-mail the job to Coolwave Ltd, the HVAC contractor.", it: "Invia l'incarico per e-mail a Coolwave Ltd, il fornitore HVAC." }),
        changes: [fact("recipient", text("service@coolwave.example"))],
        external: true,
      },
    ],
    cardTitle: tr(ctx, { en: "Work order ready to submit", it: "Ordine di lavoro pronto da inviare" }),
    cardFacts,
    prompt: tr(ctx, {
      en: `Please confirm: I'll create a **P2 work order** to replace the HVAC filters on level 3 of Harbor Point Office Tower on **${dayText}**, and send it to Coolwave Ltd. Estimated cost **€420.00**, covered by the service contract.`,
      it: `Conferma: creo un **ordine P2** per sostituire i filtri HVAC al piano 3 di Harbor Point Office Tower **${dayText}** e lo invio a Coolwave Ltd. Costo stimato **420,00 €**, coperto dal contratto di servizio.`,
    }),
    record: { entityType: "work_order", entityId: workOrder },
    receiptFacts: [fact("work_order", { kind: "identifier", value: workOrder }), fact("site", text(SITES.harbor)), fact("scheduled_for", { kind: "date", value: day }, dayText), fact("contractor", text("Coolwave Ltd"))],
    collected: [
      { name: "site", origin: "user_turn" },
      { name: "task", origin: "user_turn" },
      { name: "priority", origin: "default" },
      { name: "scheduled_for", origin: "host" },
    ],
  });
};

/** The answer to the clarification: the chosen slot prepares the booking. */
export const chosen = (ctx: ScenarioContext, choice: Choice, origin: "choice" | "user_turn"): ScenarioResult => {
  const at = choice.value.kind === "datetime" ? Date.parse(choice.value.value) : ctx.now;
  const label = dayLabel(at, ctx.locale, true);
  const reference = `INS-${4100 + (ctx.thread.prepared.size % 50)}`;
  const summary = tr(ctx, {
    en: `Book the fire-safety inspection at Riverside Logistics Depot on ${label} with Fiamma Safety Srl`,
    it: `Prenotare l'ispezione antincendio al Riverside Logistics Depot, ${label}, con Fiamma Safety Srl`,
  });
  const slot = fact("slot", choice.value, label);
  return prepare(ctx, {
    capabilityId: "inspections.book",
    summary,
    effects: [
      {
        operation: "create",
        entity_type: "inspection",
        description: tr(ctx, { en: `Book inspection ${reference} at Riverside Logistics Depot.`, it: `Prenota l'ispezione ${reference} al Riverside Logistics Depot.` }),
        changes: [fact("site", text(SITES.riverside)), slot, fact("inspector", text("Fiamma Safety Srl"))],
        external: true,
      },
    ],
    cardTitle: tr(ctx, { en: "Inspection ready to book", it: "Ispezione pronta da prenotare" }),
    cardFacts: [fact("site", text(SITES.riverside)), fact("kind", text(tr(ctx, { en: "Fire safety, full", it: "Antincendio, completa" }))), slot, fact("inspector", text("Fiamma Safety Srl"))],
    prompt: tr(ctx, {
      en: `Please confirm: I'll book the **fire-safety inspection** at Riverside Logistics Depot for **${label}** with Fiamma Safety Srl.`,
      it: `Conferma: prenoto l'**ispezione antincendio** al Riverside Logistics Depot per **${label}** con Fiamma Safety Srl.`,
    }),
    record: { entityType: "inspection", entityId: reference },
    receiptFacts: [fact("inspection", { kind: "identifier", value: reference }), fact("site", text(SITES.riverside)), slot],
    collected: [
      { name: "site", origin: "user_turn" },
      { name: "kind", origin: "user_turn" },
      { name: "slot", origin },
    ],
  });
};

/** The approved action is committed: the receipt as the confirmation call's result, a card of the receipt, the text. */
export const approved = (ctx: ScenarioContext, prepared: PreparedAction): ScenarioResult => {
  const receiptId = newId("rcpt", ctx.now);
  const receipt: ActionReceipt = {
    receipt_id: receiptId,
    preparation_id: prepared.preparationId,
    capability_id: prepared.capabilityId,
    status: "committed",
    records: [{ entity_type: prepared.record.entityType, entity_id: prepared.record.entityId }],
    facts: [...prepared.receiptFacts],
    errors: [],
    audit_ref: newId("aud", ctx.now),
    issued_at: isoAt(ctx.now),
  };
  pipeline(ctx, undefined, ["committing", "generating", "verifying"]);
  directive(ctx, { capabilityId: prepared.capabilityId, act: "report_outcome", facts: [...prepared.receiptFacts], receiptId });
  ctx.turn.add({ type: "TOOL_CALL_RESULT", messageId: msgId(ctx, "r1"), toolCallId: `alixai_confirm_${prepared.preparationId}`, content: JSON.stringify(receipt), role: "tool" });
  ctx.turn.commit(120);
  const isInspection = prepared.record.entityType === "inspection";
  block(ctx, 1, {
    type: "action_card",
    title: isInspection
      ? tr(ctx, { en: `Inspection ${prepared.record.entityId} booked`, it: `Ispezione ${prepared.record.entityId} prenotata` })
      : tr(ctx, { en: `Work order ${prepared.record.entityId} created`, it: `Ordine ${prepared.record.entityId} creato` }),
    capability_id: prepared.capabilityId,
    facts: [...prepared.receiptFacts],
    actions: [
      isInspection
        ? { action: "invoke_capability", capability_id: "inspections.book", label: tr(ctx, { en: "Book another inspection", it: "Prenota un'altra ispezione" }) }
        : { action: "invoke_capability", capability_id: "work_orders.get", label: tr(ctx, { en: `Track ${prepared.record.entityId}`, it: `Segui ${prepared.record.entityId}` }) },
    ],
    receipt_id: receiptId,
  });
  ctx.turn.text(
    msgId(ctx, 1),
    isInspection
      ? tr(ctx, {
          en: `Done. Inspection **${prepared.record.entityId}** is booked and Fiamma Safety Srl has the details.`,
          it: `Fatto. L'ispezione **${prepared.record.entityId}** è prenotata e Fiamma Safety Srl ha ricevuto i dettagli.`,
        })
      : tr(ctx, {
          en: `Done. Work order **${prepared.record.entityId}** was created and sent to Coolwave Ltd. You'll get a note when the technician closes it.`,
          it: `Fatto. L'ordine **${prepared.record.entityId}** è stato creato e inviato a Coolwave Ltd. Riceverai un avviso quando il tecnico lo chiude.`,
        }),
  );
  return {
    outcome: { type: "success", finishReason: "completed", suggestions: suggestionsFor(ctx.locale, ["table", "progress"]) },
    pending: { ...goal(ctx, prepared.capabilityId, []), collectedFields: prepared.collected },
  };
};

/** The action was declined (or the sheet dismissed): nothing changes. */
export const declined = (ctx: ScenarioContext, prepared: PreparedAction): ScenarioResult => {
  pipeline(ctx, undefined, ["verifying"]);
  directive(ctx, { capabilityId: prepared.capabilityId, act: "acknowledge_cancel" });
  ctx.turn.text(msgId(ctx, 1), tr(ctx, { en: "Okay, I've stopped that request. No changes were made.", it: "D'accordo, ho annullato la richiesta. Non è stato modificato nulla." }));
  ctx.turn.add({
    type: "TOOL_CALL_RESULT",
    messageId: msgId(ctx, "r1"),
    toolCallId: `alixai_confirm_${prepared.preparationId}`,
    content: JSON.stringify({ preparationId: prepared.preparationId, status: "rejected" }),
    role: "tool",
  });
  return { outcome: { type: "cancelled" }, pending: null };
};

/** An approval that came after the preparation expired: nothing is committed. */
export const expired = (ctx: ScenarioContext, prepared: PreparedAction): ScenarioResult => {
  const message = tr(ctx, {
    en: "Sorry, this could not be carried out.\nThe confirmation expired before it arrived. Please ask again.",
    it: "Non è stato possibile eseguirla.\nLa conferma è scaduta prima di arrivare. Ripeti la richiesta.",
  });
  ctx.turn.text(msgId(ctx, 1), message);
  ctx.turn.add({
    type: "TOOL_CALL_RESULT",
    messageId: msgId(ctx, "r1"),
    toolCallId: `alixai_confirm_${prepared.preparationId}`,
    content: JSON.stringify({ code: "expired", preparationId: prepared.preparationId, status: "failed" }),
    role: "tool",
  });
  return { outcome: { type: "error", message, code: "expired" }, pending: null };
};

export const stepUp = (ctx: ScenarioContext): ScenarioResult => {
  const capabilityId = "contracts.terminate";
  const preparationId = newId("prep", ctx.now);
  const expiresAt = isoAt(ctx.now + 10 * MINUTE);
  const toolCallId = `alixai_confirm_${preparationId}`;
  const summary = tr(ctx, {
    en: "End the cleaning contract with Brightline Services at the end of the notice period (31 January 2027)",
    it: "Recedere dal contratto di pulizia con Brightline Services alla fine del preavviso (31 gennaio 2027)",
  });
  const effects: Effect[] = [
    {
      operation: "update",
      entity_type: "contract",
      entity_id: "CTR-0087",
      description: tr(ctx, { en: "Give notice on contract CTR-0087 (Brightline Services, cleaning).", it: "Comunica il recesso dal contratto CTR-0087 (Brightline Services, pulizie)." }),
      changes: [fact("status", text("notice_given")), fact("effective_date", { kind: "date", value: "2027-01-31" })],
      external: true,
    },
  ];
  const confirmation = { mode: "step_up", bindings: [...CONFIRM_BINDINGS] };
  pipeline(ctx, capabilityId, READ_STAGES);
  directive(ctx, { capabilityId, act: "request_step_up", confirmation: "step_up", next: [{ action: "continue_in_app" }], preparationId, reasonCode: "step_up_required" });
  ctx.turn.add(
    { type: "TOOL_CALL_START", toolCallId, toolCallName: "alixai_continue_in_app" },
    { type: "TOOL_CALL_ARGS", toolCallId, delta: JSON.stringify({ preparationId, capabilityId, summary, effects, riskLevel: 3, expiresAt, confirmation }) },
    { type: "TOOL_CALL_END", toolCallId },
  );
  ctx.turn.commit(120);
  block(ctx, 1, { type: "confirmation_panel", preparation_id: preparationId, summary, effects, risk_level: 3, expires_at: expiresAt, confirmation: { mode: "step_up", bindings: [...CONFIRM_BINDINGS] } }, 80);
  ctx.turn.text(
    msgId(ctx, 1),
    tr(ctx, {
      en: "Ending a supplier contract is a **risk level 3** action, so I can't complete it in the chat. I've prepared it: continue in the Northwind portal, where you'll be asked to sign in again before anything is sent to Brightline.",
      it: "Recedere da un contratto con un fornitore è un'azione di **rischio 3**, quindi non posso completarla in chat. L'ho preparata: continua nel portale Northwind, dove ti verrà chiesto di autenticarti di nuovo prima che venga inviato qualcosa a Brightline.",
    }),
  );
  return {
    outcome: { type: "success", finishReason: "completed" },
    pending: { kind: "goal", capabilityId, phase: "step_up", collectedFields: [{ name: "contract", origin: "user_turn" }], missingFields: [], preparationId, expiresAt: expiresAt.replace(/\.\d{3}Z$/, "Z") },
  };
};

// ---- an action in progress ----

export const progress = (ctx: ScenarioContext): ScenarioResult => {
  const capabilityId = "reports.export";
  pipeline(ctx, undefined, ["interpreting", "validating"]);
  const id = actId(ctx, "tool_call");
  ctx.turn.add({ type: "STEP_STARTED", stepName: "tool_call" }, { type: "ACTIVITY_SNAPSHOT", messageId: id, activityType: "alixai.progress", content: { capabilityId, stage: "tool_call", state: "running" } });
  ctx.turn.commit(200);
  const patch = (ops: readonly Record<string, unknown>[], delayMs: number): void => {
    ctx.turn.add({ type: "ACTIVITY_DELTA", messageId: id, activityType: "alixai.progress", patch: ops });
    ctx.turn.commit(delayMs);
  };
  patch([{ op: "add", path: "/percent", value: 12 }], 450);
  for (const percent of [31, 54, 78, 100]) patch([{ op: "replace", path: "/percent", value: percent }], 520);
  ctx.turn.add({ type: "ACTIVITY_DELTA", messageId: id, activityType: "alixai.progress", patch: [{ op: "replace", path: "/state", value: "completed" }] }, { type: "STEP_FINISHED", stepName: "tool_call" });
  ctx.turn.commit(200);
  pipeline(ctx, undefined, ["generating", "verifying"]);
  const receiptId = newId("rcpt", ctx.now);
  const readyAt = ctx.now + 2 * MINUTE;
  const facts: Fact[] = [
    fact("report", text(tr(ctx, { en: "Maintenance report Q3 2026", it: "Report di manutenzione T3 2026" }))),
    fact("format", text("PDF")),
    fact("pages", { kind: "integer", value: 18 }),
    fact("ready_by", { kind: "datetime", value: isoAt(readyAt), zone: "Europe/Rome" }, dayLabel(readyAt, ctx.locale, true)),
  ];
  directive(ctx, { capabilityId, act: "report_outcome", facts, receiptId });
  const receipt: ActionReceipt = {
    receipt_id: receiptId,
    preparation_id: newId("prep", ctx.now),
    capability_id: capabilityId,
    status: "queued",
    records: [{ entity_type: "report_export", entity_id: "EXP-0412" }],
    facts,
    errors: [],
    follow_up: { reference: newId("job", ctx.now), state: "running", as_of: isoAt(ctx.now) },
    audit_ref: newId("aud", ctx.now),
    issued_at: isoAt(ctx.now),
  };
  ctx.turn.activity(actId(ctx, "receipt"), "alixai.receipt", receipt, 120);
  block(ctx, 1, {
    type: "action_card",
    title: tr(ctx, { en: "Q3 report is being generated", it: "Il report del T3 è in preparazione" }),
    capability_id: capabilityId,
    facts,
    actions: [{ action: "refresh", label: tr(ctx, { en: "Check again", it: "Ricontrolla" }) }],
    receipt_id: receiptId,
  });
  ctx.turn.text(
    msgId(ctx, 1),
    tr(ctx, {
      en: "I've collected the data and started the export of the **Q3 2026 maintenance report** (PDF, about 18 pages). It finishes in the background: you'll find it under *Reports* in about two minutes.",
      it: "Ho raccolto i dati e avviato l'esportazione del **report di manutenzione del T3 2026** (PDF, circa 18 pagine). Termina in background: lo troverai in *Report* tra circa due minuti.",
    }),
  );
  return { outcome: { type: "success", finishReason: "completed", suggestions: suggestionsFor(ctx.locale, ["long"]) }, pending: goal(ctx, capabilityId, ["report", "period"]) };
};

// ---- failures ----

export const unavailable = (ctx: ScenarioContext): ScenarioResult => {
  pipeline(ctx, "sensors.read_live", ["interpreting", "validating", "tool_call"], "failed");
  directive(ctx, { capabilityId: "sensors.read_live", act: "report_failure", reasonCode: "unavailable" });
  const message = tr(ctx, {
    en: "I couldn't reach the building management system of Westfield Data Hall just now, so I can't show live sensor data. Nothing was changed. Please try again in a few minutes.",
    it: "Al momento non riesco a raggiungere il sistema di gestione dell'edificio del Westfield Data Hall, quindi non posso mostrare i dati live dei sensori. Non è stato modificato nulla. Riprova tra qualche minuto.",
  });
  ctx.turn.text(msgId(ctx, 1), message);
  return { outcome: { type: "error", message, code: "unavailable" }, pending: null };
};

export const refusal = (ctx: ScenarioContext): ScenarioResult => {
  pipeline(ctx, undefined, ["interpreting"]);
  directive(ctx, { capabilityId: null, act: "refuse", reasonCode: "permission_denied" });
  const message = tr(ctx, {
    en: "I'm not able to do that.\nStaff salary data isn't something this assistant can read or export, and I haven't looked anything up. For payroll questions, please contact HR.",
    it: "Non posso farlo.\nI dati sugli stipendi del personale non sono tra quelli che questo assistente può leggere o esportare, e non ho consultato nulla. Per domande sulle buste paga rivolgiti all'ufficio del personale.",
  });
  ctx.turn.text(msgId(ctx, 1), message);
  return { outcome: { type: "error", message, code: "permission_denied" }, pending: ctx.thread.pending };
};

// ---- a long, multi-block answer ----

export const long = (ctx: ScenarioContext): ScenarioResult => {
  pipeline(ctx, "work_orders.list", ["interpreting", "validating", "tool_call", "retrieving", "generating", "verifying"]);
  const sources = reportCitations(ctx.locale);
  directive(ctx, { capabilityId: "work_orders.list", act: "present_result", sources });
  ctx.turn.text(
    msgId(ctx, 1),
    tr(ctx, {
      en: "## Q3 2026 facilities report\n\nAcross the four sites the team opened **201** work orders and closed **184**. Six were still overdue on 30 September. Here is the breakdown by site:",
      it: "## Report strutture T3 2026\n\nNelle quattro sedi il team ha aperto **201** ordini di lavoro e ne ha chiusi **184**. Sei erano ancora in ritardo al 30 settembre. Ecco il dettaglio per sede:",
    }),
  );
  block(ctx, 1, {
    type: "table",
    caption: tr(ctx, { en: "Work orders by site, Q3 2026", it: "Ordini di lavoro per sede, T3 2026" }),
    columns: [
      { key: "site", label: tr(ctx, { en: "Site", it: "Sede" }), kind: "text" },
      { key: "opened", label: tr(ctx, { en: "Opened", it: "Aperti" }), kind: "number" },
      { key: "closed", label: tr(ctx, { en: "Closed", it: "Chiusi" }), kind: "number" },
      { key: "overdue", label: tr(ctx, { en: "Overdue on 30 Sep", it: "In ritardo al 30/9" }), kind: "number" },
      { key: "days", label: tr(ctx, { en: "Mean days to close", it: "Giorni medi di chiusura" }), kind: "number" },
    ],
    rows: Q3_BY_SITE.map((row) => [row.site, row.opened, row.closed, row.overdue, row.days]),
  });
  block(ctx, 2, {
    type: "chart",
    title: tr(ctx, { en: "Opened and closed work orders, Q3 2026", it: "Ordini aperti e chiusi, T3 2026" }),
    description: tr(ctx, {
      en: "Grouped bars per site. Harbor Point 64 opened, 61 closed; Riverside 48 and 41; Elm Street 37 and 36; Westfield 52 and 46.",
      it: "Barre raggruppate per sede. Harbor Point 64 aperti, 61 chiusi; Riverside 48 e 41; Elm Street 37 e 36; Westfield 52 e 46.",
    }),
    mark: "bar",
    data: Q3_BY_SITE.flatMap((row) => [
      { site: row.site.split(" ").slice(0, 2).join(" "), series: tr(ctx, { en: "Opened", it: "Aperti" }), count: row.opened },
      { site: row.site.split(" ").slice(0, 2).join(" "), series: tr(ctx, { en: "Closed", it: "Chiusi" }), count: row.closed },
    ]),
    encoding: {
      x: { field: "site", type: "nominal", title: tr(ctx, { en: "Site", it: "Sede" }) },
      y: { field: "count", type: "quantitative", title: tr(ctx, { en: "Work orders", it: "Ordini" }) },
      color: { field: "series", type: "nominal" },
    },
  });
  ctx.turn.text(
    msgId(ctx, 2),
    tr(ctx, {
      en: "Energy use peaked in July and August with the cooling load, and **HVAC** was the largest share of maintenance spend (€182k of €441k):",
      it: "I consumi hanno avuto il picco a luglio e agosto per il raffreddamento, e l'**HVAC** è stata la voce di spesa maggiore (182k€ su 441k€):",
    }),
  );
  block(ctx, 3, energyChart(ctx));
  block(ctx, 4, {
    type: "chart",
    title: tr(ctx, { en: "Maintenance spend by category, Q3 2026 (k€)", it: "Spesa di manutenzione per categoria, T3 2026 (k€)" }),
    description: tr(ctx, {
      en: "Pie of spend in thousands of euros: HVAC 182, Electrical 96, Fire safety 58, Lifts 44, Other 61.",
      it: "Torta della spesa in migliaia di euro: HVAC 182, Elettrico 96, Antincendio 58, Ascensori 44, Altro 61.",
    }),
    mark: "pie",
    data: Q3_SPEND.map((row) => ({ category: tr(ctx, row.category), keur: row.keur })),
    encoding: { theta: { field: "keur", type: "quantitative", title: "k€" }, color: { field: "category", type: "nominal", title: tr(ctx, { en: "Category", it: "Categoria" }) } },
  });
  citations(ctx, sources);
  ctx.turn.text(
    msgId(ctx, 3),
    tr(ctx, {
      en: "### Next steps\n\n1. Close the two overdue P1 orders (`WO-2294`, `WO-2301`); P1 orders are due within 4 hours. \\[2\\]\n2. Book the full fire-safety inspection at Riverside before **31 October**. \\[1\\]\n3. The supplier portal is offline, so the history of `WO-2301` could not be checked. \\[3\\]",
      it: "### Prossimi passi\n\n1. Chiudere i due ordini P1 in ritardo (`WO-2294`, `WO-2301`); gli ordini P1 vanno gestiti entro 4 ore. \\[2\\]\n2. Prenotare l'ispezione antincendio completa a Riverside entro il **31 ottobre**. \\[1\\]\n3. Il portale fornitori è offline, quindi lo storico di `WO-2301` non è stato verificato. \\[3\\]",
    }),
  );
  return { outcome: { type: "success", finishReason: "completed", suggestions: suggestionsFor(ctx.locale, ["clarification", "progress", "confirmation"]) }, pending: goal(ctx, "work_orders.list", ["period"]) };
};

/** A plain answer with a table, long enough that cutting it halfway leaves both halves visible. */
export const reconnect = (ctx: ScenarioContext): ScenarioResult => {
  pipeline(ctx, "work_orders.list", READ_STAGES);
  directive(ctx, { capabilityId: "work_orders.list", act: "present_result" });
  ctx.turn.text(
    msgId(ctx, 1),
    tr(ctx, {
      en: "This answer is cut halfway on purpose: the connection drops once, the client reconnects with the id of the last event it received, and the server sends the rest. Nothing is repeated and nothing is lost. Here are the open work orders while we're at it:",
      it: "Questa risposta viene interrotta a metà di proposito: la connessione cade una volta, il client si riconnette con l'id dell'ultimo evento ricevuto e il server invia il resto. Nulla si ripete e nulla si perde. Intanto, ecco gli ordini di lavoro aperti:",
    }),
  );
  block(ctx, 1, openOrdersTable(ctx));
  ctx.turn.text(
    msgId(ctx, 2),
    tr(ctx, {
      en: "If you saw a short *Reconnecting* notice, that was the cut. The table above arrived after it.",
      it: "Se hai visto per un attimo l'avviso di *riconnessione*, era l'interruzione. La tabella qui sopra è arrivata dopo.",
    }),
  );
  return { outcome: { type: "success", finishReason: "completed", suggestions: suggestionsFor(ctx.locale, ["help"]) }, pending: goal(ctx, "work_orders.list", ["status"]) };
};

/** An assistant-proposed long-term memory awaiting Remember / Don't remember (ADR 0042). */
export const memoryProposal = (ctx: ScenarioContext): ScenarioResult => {
  pipeline(ctx, "memory.propose", ["interpreting", "generating", "verifying"]);
  directive(ctx, { capabilityId: "memory.propose", act: "present_result" });
  const text = tr(ctx, {
    en: "I can remember that you prefer concise answers. Confirm below if you want me to keep that.",
    it: "Posso ricordare che preferisci risposte concise. Conferma qui sotto se vuoi che lo tenga.",
  });
  ctx.turn.text(msgId(ctx, 1), text);
  const proposed =
    ctx.meMemory?.propose(
      tr(ctx, { en: "Prefer concise answers", it: "Preferisci risposte concise" }),
      ctx.now + 10 * MINUTE,
    ) ??
    ({
      id: newId("mem", ctx.now),
      text: tr(ctx, { en: "Prefer concise answers", it: "Preferisci risposte concise" }),
      origin: "assistant_proposed" as const,
      status: "proposed" as const,
      data_class: "internal" as const,
      created_at: isoAt(ctx.now),
      updated_at: isoAt(ctx.now),
      expires_at: isoAt(ctx.now + 10 * MINUTE),
    });
  ctx.turn.add({
    type: "CUSTOM",
    name: "alixai.memory_proposal",
    value: {
      memory: {
        id: proposed.id,
        application_id: "app_demo",
        tenant_id: "ten_demo",
        workspace_id: "ws_demo",
        principal_id: "usr_demo_alex",
        text: proposed.text,
        origin: proposed.origin,
        status: proposed.status,
        data_class: proposed.data_class,
        created_at: proposed.created_at,
        updated_at: proposed.updated_at,
        ...(proposed.expires_at !== undefined && { expires_at: proposed.expires_at }),
      },
    },
  });
  return {
    outcome: { type: "success", finishReason: "awaiting_input", suggestions: suggestionsFor(ctx.locale, ["help", "confirmation"]) },
    pending: null,
  };
};

export const SCENARIOS: Record<Exclude<DemoScenarioId, "help">, (ctx: ScenarioContext) => ScenarioResult> = {
  markdown,
  chart_bar: chartBar,
  chart_line: chartLine,
  table,
  diagram,
  image,
  citations: grounded,
  clarification,
  confirmation,
  step_up: stepUp,
  progress,
  unavailable,
  refusal,
  long,
  reconnect,
  memory_proposal: memoryProposal,
};
