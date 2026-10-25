import type {
  ActionCardBlock,
  ActionReceipt,
  AlixConfirmation,
  ChartBlock,
  Citation,
  ClarificationChoicesBlock,
  DiagramBlock,
  TableBlock,
} from "@alixai/vue";
import type { DemoLocale } from "../../demo-mock/src/index";

// Sample data for the blocks gallery, in the shapes of the v1 schemas, about the mock's fictional company (Northwind
// Facilities). Each block is shown standalone, the way a host renders one outside the chat.

const pick = (locale: DemoLocale, en: string, it: string): string => (locale === "it" ? it : en);
const inTenMinutes = (): string => new Date(Date.now() + 10 * 60 * 1000).toISOString();

export const sampleMarkdown = (locale: DemoLocale): string =>
  locale === "it"
    ? "### Manutenzione preventiva\n\nI filtri HVAC si sostituiscono **ogni 90 giorni** negli uffici e **ogni 60** nelle sale dati.\n\n- Harbor Point: prossima visita il 14 ottobre\n- Westfield Data Hall: 3 ordini in ritardo\n\n> Le ispezioni antincendio restano trimestrali.\n\nIl codice dell'ordine è `WO-2291`."
    : "### Preventive maintenance\n\nHVAC filters are replaced **every 90 days** in offices and **every 60** in data halls.\n\n- Harbor Point: next visit on 14 October\n- Westfield Data Hall: 3 overdue work orders\n\n> Fire-safety inspections stay quarterly.\n\nThe work order code is `WO-2291`.";

export const sampleBar = (locale: DemoLocale): ChartBlock => ({
  type: "chart",
  title: pick(locale, "Open work orders per site", "Ordini di lavoro aperti per sede"),
  description: pick(locale, "Open and overdue work orders at five sites. Westfield has the most overdue.", "Ordini aperti e in ritardo in cinque sedi. Westfield ha più ritardi."),
  mark: "bar",
  data: [
    { site: "Harbor Point", status: pick(locale, "Open", "Aperti"), count: 14 },
    { site: "Harbor Point", status: pick(locale, "Overdue", "In ritardo"), count: 2 },
    { site: "Westfield", status: pick(locale, "Open", "Aperti"), count: 11 },
    { site: "Westfield", status: pick(locale, "Overdue", "In ritardo"), count: 3 },
    { site: "Riverside", status: pick(locale, "Open", "Aperti"), count: 7 },
    { site: "Riverside", status: pick(locale, "Overdue", "In ritardo"), count: 1 },
    { site: "Elm Street", status: pick(locale, "Open", "Aperti"), count: 5 },
    { site: "Elm Street", status: pick(locale, "Overdue", "In ritardo"), count: 0 },
  ],
  encoding: {
    x: { field: "site", type: "nominal", title: pick(locale, "Site", "Sede") },
    y: { field: "count", type: "quantitative", title: pick(locale, "Work orders", "Ordini") },
    color: { field: "status", type: "nominal" },
  },
});

export const sampleLine = (locale: DemoLocale): ChartBlock => ({
  type: "chart",
  title: pick(locale, "Monthly energy use (MWh)", "Consumo energetico mensile (MWh)"),
  description: pick(locale, "Energy use from April to September 2026 at two sites; Westfield peaks in July at 797 MWh.", "Consumi da aprile a settembre 2026 in due sedi; Westfield tocca il massimo a luglio con 797 MWh."),
  mark: "line",
  data: [
    ["2026-04-01", 412, 655],
    ["2026-05-01", 398, 688],
    ["2026-06-01", 431, 741],
    ["2026-07-01", 467, 797],
    ["2026-08-01", 452, 783],
    ["2026-09-01", 419, 702],
  ].flatMap(([month, harbor, westfield]) => [
    { month, site: "Harbor Point", mwh: harbor },
    { month, site: "Westfield", mwh: westfield },
  ]) as ChartBlock["data"],
  encoding: {
    x: { field: "month", type: "temporal", title: pick(locale, "Month", "Mese") },
    y: { field: "mwh", type: "quantitative", title: "MWh" },
    color: { field: "site", type: "nominal" },
  },
});

export const sampleTable = (locale: DemoLocale): TableBlock => ({
  type: "table",
  caption: pick(locale, "Open work orders", "Ordini di lavoro aperti"),
  columns: [
    { key: "id", label: pick(locale, "Work order", "Ordine"), kind: "text" },
    { key: "site", label: pick(locale, "Site", "Sede"), kind: "text" },
    { key: "due", label: pick(locale, "Due", "Scadenza"), kind: "date" },
    { key: "cost", label: pick(locale, "Estimate (EUR)", "Stima (EUR)"), kind: "number" },
    { key: "overdue", label: pick(locale, "Overdue", "In ritardo"), kind: "boolean" },
  ],
  rows: [
    ["WO-2291", "Harbor Point", "2026-10-14", 1840.5, false],
    ["WO-2294", "Riverside", "2026-10-03", 612, true],
    ["WO-2301", "Westfield", "2026-10-09", 4275.25, true],
    ["WO-2306", "Elm Street", "2026-10-21", 389.9, false],
  ],
});

export const sampleDiagram = (locale: DemoLocale): DiagramBlock => ({
  type: "diagram",
  language: "mermaid",
  requires_host_renderer: true,
  title: pick(locale, "Work-order process", "Processo degli ordini di lavoro"),
  description: pick(
    locale,
    "A request is triaged: urgent ones are dispatched, routine ones planned; the work is checked, and closed or reopened.",
    "Una richiesta viene smistata: le urgenti partono subito, le ordinarie si pianificano; il lavoro si verifica e si chiude o si riapre.",
  ),
  source: [
    "flowchart LR",
    `  A[${pick(locale, "Request", "Richiesta")}] --> B{${pick(locale, "Triage", "Smistamento")}}`,
    `  B -->|P1| C[${pick(locale, "Dispatch", "Intervento")}]`,
    `  B -->|P2, P3| D[${pick(locale, "Plan visit", "Pianifica")}]`,
    `  C --> E[${pick(locale, "Work", "Lavoro")}]`,
    "  D --> E",
    `  E --> F{${pick(locale, "Check", "Verifica")}}`,
    `  F -->|OK| G[${pick(locale, "Closed", "Chiuso")}]`,
    `  F -->|${pick(locale, "Reopen", "Riapri")}| E`,
  ].join("\n"),
});

export const sampleCard = (locale: DemoLocale): ActionCardBlock => ({
  type: "action_card",
  title: pick(locale, "Replace HVAC filters, Harbor Point level 3", "Sostituzione filtri HVAC, Harbor Point piano 3"),
  capability_id: "workorders.create",
  facts: [
    { field: "site", value: { kind: "text", value: "Harbor Point Office Tower" } },
    { field: "priority", value: { kind: "text", value: "P2" } },
    { field: "scheduled", value: { kind: "date", value: "2026-10-14" }, display: pick(locale, "Wednesday 14 October", "mercoledì 14 ottobre") },
    { field: "estimate", value: { kind: "money", value: "1840.50", currency: "EUR" } },
  ],
  actions: [
    { action: "modify", label: pick(locale, "Change details", "Modifica i dettagli") },
    { action: "refresh" },
  ],
  preparation_id: "prep_demo_gallery",
});

export const sampleChoices = (locale: DemoLocale): ClarificationChoicesBlock => ({
  type: "clarification_choices",
  field: "slot",
  prompt: pick(locale, "When should the inspection take place?", "Quando deve svolgersi l'ispezione?"),
  answer_kind: "choice",
  allow_free_text: true,
  choices: [
    { choice_id: "slot_tue", label: pick(locale, "Tuesday 8:30", "Martedì 8:30"), value: { kind: "datetime", value: "2026-10-13T08:30:00+02:00" } },
    { choice_id: "slot_wed", label: pick(locale, "Wednesday 14:00", "Mercoledì 14:00"), value: { kind: "datetime", value: "2026-10-14T14:00:00+02:00" } },
    { choice_id: "slot_fri", label: pick(locale, "Friday 10:00", "Venerdì 10:00"), value: { kind: "datetime", value: "2026-10-16T10:00:00+02:00" }, description: pick(locale, "Inspector on site all day", "Ispettore in sede tutto il giorno") },
  ],
});

export const sampleCitations = (locale: DemoLocale): Citation[] => [
  {
    source_id: "src_policy",
    display_name: pick(locale, "Preventive Maintenance Policy", "Politica di manutenzione preventiva"),
    version: "v12",
    locator: { page: 4 },
    availability: "available",
    staleness: "current",
    open_ref: { kind: "file", file_id: "fil_01JNWDEM0F1XTVRE0000000001" },
    claims: [pick(locale, "Filters are replaced every 90 days in offices.", "I filtri si sostituiscono ogni 90 giorni negli uffici.")],
  },
  {
    source_id: "src_contract",
    display_name: pick(locale, "Coolwave HVAC service contract", "Contratto di servizio HVAC Coolwave"),
    version: "2025-11",
    locator: { section: "4.2" },
    availability: "available",
    staleness: "stale",
    open_ref: { kind: "file", file_id: "fil_01JNWDEM0F1XTVRE0000000002" },
    claims: [pick(locale, "Coolwave replaces filters within five working days.", "Coolwave sostituisce i filtri entro cinque giorni lavorativi.")],
  },
  {
    source_id: "src_wo",
    display_name: "WO-2291",
    version: "7",
    availability: "unavailable",
    staleness: "unknown",
    open_ref: { kind: "record", entity_type: "work_order", entity_id: "WO-2291" },
    claims: [],
  },
];

export const sampleReceipt = (locale: DemoLocale, status: ActionReceipt["status"] = "committed"): ActionReceipt => ({
  receipt_id: `rcpt_demo_${status}`,
  preparation_id: "prep_demo_gallery",
  capability_id: "workorders.create",
  status,
  records: [{ entity_type: "work_order", entity_id: "WO-2318" }],
  facts: [
    { field: "work_order", value: { kind: "identifier", value: "WO-2318" } },
    { field: "scheduled", value: { kind: "date", value: "2026-10-14" }, display: pick(locale, "Wednesday 14 October", "mercoledì 14 ottobre") },
  ],
  errors: status === "rejected" ? [{ code: "conflict", message: pick(locale, "The slot was taken in the meantime.", "Nel frattempo la fascia è stata occupata.") }] : [],
  ...(status === "queued" && { follow_up: { reference: "job_export_q3", state: "queued" as const } }),
  audit_ref: "aud_01JNWDEM0F1XTVRE0000000042",
  issued_at: "2026-10-11T08:12:00Z",
});

export const sampleConfirmation = (locale: DemoLocale, mode: "explicit" | "step_up" = "explicit"): AlixConfirmation => ({
  preparationId: `prep_demo_${mode}`,
  capabilityId: mode === "explicit" ? "workorders.create" : "contracts.terminate",
  summary:
    mode === "explicit"
      ? pick(locale, "Create a P2 work order at Harbor Point Office Tower: replace the HVAC filters on level 3", "Crea un ordine P2 a Harbor Point Office Tower: sostituire i filtri HVAC al piano 3")
      : pick(locale, "End the cleaning contract with Brightline Services at the end of the notice period", "Recedere dal contratto di pulizia con Brightline Services alla fine del preavviso"),
  effects: [
    mode === "explicit"
      ? {
          operation: "create",
          entity_type: "work_order",
          description: pick(locale, "New work order for Coolwave HVAC Services", "Nuovo ordine per Coolwave HVAC Services"),
          changes: [
            { field: "priority", value: { kind: "text", value: "P2" } },
            { field: "scheduled", value: { kind: "date", value: "2026-10-14" }, display: pick(locale, "Wednesday 14 October", "mercoledì 14 ottobre") },
          ],
          external: false,
        }
      : {
          operation: "update",
          entity_type: "contract",
          entity_id: "CTR-0087",
          description: pick(locale, "Give notice on contract CTR-0087", "Comunica il recesso dal contratto CTR-0087"),
          changes: [{ field: "status", value: { kind: "text", value: "notice_given" } }],
          external: true,
        },
  ],
  riskLevel: mode === "explicit" ? 2 : 3,
  expiresAt: inTenMinutes(),
  mode,
});
