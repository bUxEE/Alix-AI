import type { AlixAIBlock, AlixAIInterrupt } from "@alixai/copilotkit-adapter";
import type { ActionReceipt, Citation } from "@alixai/sdk";

// Fixed data for the blocks gallery, in the shapes of the frozen schemas, about the same fictional company as the mock (Northwind
// Facilities). Dates are relative to now so the "valid until" lines stay in the future.

const inMinutes = (minutes: number): string => new Date(Date.now() + minutes * 60_000).toISOString();

export const tableBlock = (): AlixAIBlock<"table"> =>
  ({
    type: "table",
    caption: "Open work orders",
    columns: [
      { key: "id", label: "Work order", kind: "text" },
      { key: "site", label: "Site", kind: "text" },
      { key: "hours", label: "Hours", kind: "number" },
      { key: "due", label: "Due", kind: "date" },
      { key: "overdue", label: "Overdue", kind: "boolean" },
    ],
    rows: [
      ["WO-2291", "Harbor Point Office Tower", 3.5, "2026-10-14", false],
      ["WO-2294", "Riverside Logistics Depot", 6, "2026-10-03", true],
      ["WO-2301", "Westfield Data Hall", 8, "2026-10-06", true],
      ["WO-2312", "Westfield Data Hall", 5, "2026-10-15", false],
    ],
  }) as unknown as AlixAIBlock<"table">;

export type Mark = "bar" | "line" | "area" | "point" | "pie";

export const chartBlock = (mark: Mark): AlixAIBlock<"chart"> => {
  if (mark === "pie")
    return {
      type: "chart",
      title: "Work orders by category, Q3",
      description: "Work orders by category in Q3: HVAC 38, electrical 27, plumbing 19, fire safety 14, lifts 9.",
      mark: "pie",
      data: [
        { category: "HVAC", orders: 38 },
        { category: "Electrical", orders: 27 },
        { category: "Plumbing", orders: 19 },
        { category: "Fire safety", orders: 14 },
        { category: "Lifts", orders: 9 },
      ],
      encoding: { color: { field: "category", type: "nominal" }, theta: { field: "orders", type: "quantitative" } },
    } as unknown as AlixAIBlock<"chart">;
  const months = ["Apr", "May", "Jun", "Jul", "Aug", "Sep"];
  const harbor = [412, 398, 455, 503, 496, 430];
  const westfield = [688, 702, 741, 797, 802, 735];
  if (mark === "bar")
    return {
      type: "chart",
      title: "Open work orders per site",
      description: "Open work orders per site, urgent and planned. Westfield has the most urgent ones (3).",
      mark: "bar",
      data: [
        { site: "Harbor Point", kind: "Urgent", orders: 1 },
        { site: "Harbor Point", kind: "Planned", orders: 4 },
        { site: "Riverside", kind: "Urgent", orders: 2 },
        { site: "Riverside", kind: "Planned", orders: 3 },
        { site: "Elm Street", kind: "Urgent", orders: 0 },
        { site: "Elm Street", kind: "Planned", orders: 2 },
        { site: "Westfield", kind: "Urgent", orders: 3 },
        { site: "Westfield", kind: "Planned", orders: 2 },
      ],
      encoding: { x: { field: "site", type: "nominal" }, y: { field: "orders", type: "quantitative" }, color: { field: "kind", type: "nominal" } },
    } as unknown as AlixAIBlock<"chart">;
  return {
    type: "chart",
    title: "Monthly energy use (MWh)",
    description: "Monthly energy use, April to September 2026: Westfield peaks at 802 MWh in August, Harbor Point at 503 MWh in July.",
    mark,
    data: months.flatMap((month, index) => [
      { month, site: "Harbor Point", mwh: harbor[index] },
      { month, site: "Westfield", mwh: westfield[index] },
    ]),
    encoding: { x: { field: "month", type: "ordinal" }, y: { field: "mwh", type: "quantitative" }, color: { field: "site", type: "nominal" } },
  } as unknown as AlixAIBlock<"chart">;
};

export const diagramBlock = (): AlixAIBlock<"diagram"> =>
  ({
    type: "diagram",
    language: "mermaid",
    requires_host_renderer: true,
    title: "Work-order process",
    description: "A request is triaged, scheduled, done by a contractor and closed after a check.",
    source: "flowchart LR\n  A[Request] --> B{Triage}\n  B -->|P1| C[Dispatch now]\n  B -->|P2/P3| D[Schedule]\n  C --> E[Contractor on site]\n  D --> E\n  E --> F[Check] --> G[Closed]",
  }) as unknown as AlixAIBlock<"diagram">;

export const citations = (): Citation[] =>
  [
    {
      source_id: "src_policy",
      display_name: "Preventive maintenance policy",
      version: "v12",
      locator: { section: "4.2 HVAC filters", page: 7 },
      availability: "available",
      staleness: "current",
      open_ref: { kind: "file" },
      claims: ["Filters are replaced every 90 days, or sooner when the pressure drop exceeds 250 Pa."],
    },
    {
      source_id: "src_contract",
      display_name: "Coolwave HVAC service contract",
      version: "2026-01",
      locator: { page: 3 },
      availability: "available",
      staleness: "stale",
      open_ref: { kind: "file" },
      claims: ["Filter replacement is covered by the service contract."],
    },
    {
      source_id: "src_wiki",
      display_name: "Old facilities wiki",
      version: "r88",
      availability: "removed",
      staleness: "unknown",
      open_ref: { kind: "url" },
      claims: [],
    },
  ] as unknown as Citation[];

export const actionCardBlock = (): AlixAIBlock<"action_card"> =>
  ({
    type: "action_card",
    title: "Work order ready to submit",
    capability_id: "work_orders.create",
    facts: [
      { field: "site", value: { kind: "text", value: "Harbor Point Office Tower" } },
      { field: "priority", value: { kind: "text", value: "P2" } },
      { field: "contractor", value: { kind: "text", value: "Coolwave Ltd" } },
      { field: "estimated_cost", value: { kind: "money", value: "420.00", currency: "EUR" }, display: "EUR 420.00" },
    ],
    actions: [{ action: "modify", label: "Change details" }, { action: "refresh" }],
  }) as unknown as AlixAIBlock<"action_card">;

export const choicesBlock = (): AlixAIBlock<"clarification_choices"> =>
  ({
    type: "clarification_choices",
    field: "slot",
    prompt: "Which slot suits the fire-safety inspection at Riverside?",
    answer_kind: "choice",
    allow_free_text: true,
    choices: [
      { choice_id: "slot_tue", label: "Tue 08:30", description: "Fiamma Safety Srl", value: { kind: "text", value: "tue-0830" } },
      { choice_id: "slot_wed", label: "Wed 14:00", value: { kind: "text", value: "wed-1400" } },
      { choice_id: "slot_fri", label: "Fri 09:00", value: { kind: "text", value: "fri-0900" } },
    ],
  }) as unknown as AlixAIBlock<"clarification_choices">;

const effects = [
  {
    operation: "create",
    entity_type: "work_order",
    description: "Create work order WO-2318 (P2, HVAC) at Harbor Point Office Tower.",
    changes: [
      { field: "task", value: { kind: "text", value: "Replace the filters of AHU-3A and AHU-3B" } },
      { field: "scheduled_for", value: { kind: "date", value: "2026-10-14" }, display: "Wed 14 Oct" },
    ],
    external: false,
  },
  {
    operation: "send",
    entity_type: "notification",
    description: "E-mail the job to Coolwave Ltd, the HVAC contractor.",
    changes: [{ field: "recipient", value: { kind: "text", value: "service@coolwave.example" } }],
    external: true,
  },
];

export const interrupt = (): AlixAIInterrupt => ({
  id: "prep_gallery_1",
  reason: "confirmation_required",
  metadata: {
    preparationId: "prep_gallery_1",
    capabilityId: "work_orders.create",
    summary: "Create a P2 work order at Harbor Point Office Tower: replace the HVAC filters on level 3 on Wed 14 Oct",
    effects,
    riskLevel: 2,
    expiresAt: inMinutes(10),
    confirmation: { mode: "explicit" },
  },
});

export const panelBlock = (mode: "explicit" | "step_up"): AlixAIBlock<"confirmation_panel"> =>
  ({
    type: "confirmation_panel",
    preparation_id: `prep_gallery_${mode}`,
    summary:
      mode === "explicit"
        ? "Create a P2 work order at Harbor Point Office Tower: replace the HVAC filters on level 3 on Wed 14 Oct"
        : "End the cleaning contract with Brightline Services at the end of the notice period (31 January 2027)",
    effects:
      mode === "explicit"
        ? effects
        : [
            {
              operation: "update",
              entity_type: "contract",
              entity_id: "CTR-0087",
              description: "Give notice on contract CTR-0087 (Brightline Services, cleaning).",
              changes: [{ field: "effective_date", value: { kind: "date", value: "2027-01-31" } }],
              external: true,
            },
          ],
    risk_level: mode === "explicit" ? 2 : 3,
    expires_at: inMinutes(10),
    confirmation: { mode, bindings: [] },
  }) as unknown as AlixAIBlock<"confirmation_panel">;

export const receipt = (status: "committed" | "queued" | "partial"): ActionReceipt =>
  ({
    receipt_id: `rcpt_gallery_${status}`,
    preparation_id: "prep_gallery_1",
    capability_id: status === "queued" ? "reports.export" : "work_orders.create",
    status,
    records: [{ entity_type: "work_order", entity_id: "WO-2318" }],
    facts:
      status === "queued"
        ? [
            { field: "report", value: { kind: "text", value: "Maintenance report Q3 2026" } },
            { field: "format", value: { kind: "text", value: "PDF" } },
            { field: "pages", value: { kind: "integer", value: 18 } },
          ]
        : [
            { field: "work_order", value: { kind: "identifier", value: "WO-2318" } },
            { field: "site", value: { kind: "text", value: "Harbor Point Office Tower" } },
            { field: "scheduled_for", value: { kind: "date", value: "2026-10-14" }, display: "Wed 14 Oct" },
          ],
    errors: status === "partial" ? [{ code: "upstream_unavailable", message: "The e-mail to Coolwave Ltd could not be sent. The work order exists." }] : [],
    ...(status === "queued" && { follow_up: { reference: "job_q3_export", state: "running", as_of: new Date().toISOString() } }),
    audit_ref: "aud_gallery",
    issued_at: new Date(Date.now() - 4 * 60_000).toISOString(),
  }) as unknown as ActionReceipt;
