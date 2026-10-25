import type { Citation } from "@alixai/sdk";
import type { AlixAIAttachment, AlixAIBlock, AlixAIInterrupt } from "@alixai/copilotkit-adapter";

// Hand-written fixtures in the shapes of the frozen schemas (schemas/_frozen/v1): no real names, numbers or secrets.

export const interrupt = (overrides: { riskLevel?: number; operation?: string; metadata?: unknown } = {}): AlixAIInterrupt => ({
  id: "prep_01J00000000000000000000001",
  reason: "confirmation_required",
  metadata: overrides.metadata ?? {
    preparationId: "prep_01J00000000000000000000001",
    capabilityId: "calendar.book_visit",
    summary: "Book a 30 minute visit for Ilaria Bellandi on 14 March at 09:30.",
    effects: [
      {
        operation: overrides.operation ?? "create",
        entity_type: "visit",
        description: "Create a visit",
        external: false,
        changes: [
          { field: "patient", value: { kind: "text", value: "Ilaria Bellandi" } },
          { field: "fee", value: { kind: "money", value: "48.50", currency: "EUR" }, display: "EUR 48,50" },
        ],
      },
      { operation: "send", entity_type: "email", description: "Send a reminder", external: true, changes: [] },
    ],
    riskLevel: overrides.riskLevel ?? 2,
    expiresAt: "2026-03-14T08:45:00Z",
    confirmation: { mode: "explicit" },
  },
});

export const citation = (overrides: Partial<Citation> = {}): Citation =>
  ({
    source_id: "src_01J00000000000000000000001",
    display_name: "Treatment protocol",
    version: "v7",
    locator: { section: "3.2", page: 14 },
    availability: "available",
    staleness: "current",
    open_ref: { kind: "source" },
    claims: ["Visits last 30 minutes."],
    ...overrides,
  }) as Citation;

export const attachment = (overrides: Partial<AlixAIAttachment> = {}): AlixAIAttachment => ({
  fileId: "file_01J00000000000000000000001",
  name: "scan-0317.png",
  mimeType: "image/png",
  sizeBytes: 182_400,
  previewable: true,
  ...overrides,
});

export const tableBlock = (): AlixAIBlock<"table"> =>
  ({
    type: "table",
    caption: "Open visits",
    columns: [
      { key: "who", label: "Patient", kind: "text" },
      { key: "minutes", label: "Minutes", kind: "number" },
      { key: "paid", label: "Paid", kind: "boolean" },
    ],
    rows: [
      ["Ilaria Bellandi", 12500, true],
      ["<img src=x onerror=alert(1)>", 45, false],
    ],
  }) as AlixAIBlock<"table">;

export const chartBlock = (mark: "bar" | "line" | "area" | "point" | "pie" = "bar"): AlixAIBlock<"chart"> =>
  ({
    type: "chart",
    title: "Visits per week",
    description: "Visits per week, rising from 12 to 31.",
    mark,
    data: [
      { week: "W10", visits: 12, clinic: "North" },
      { week: "W11", visits: 19, clinic: "North" },
      { week: "W12", visits: 31, clinic: "North" },
      { week: "W10", visits: 7, clinic: "South" },
    ],
    encoding: {
      x: { field: "week", type: "ordinal" },
      y: { field: "visits", type: "quantitative" },
      color: { field: "clinic", type: "nominal" },
      theta: { field: "visits", type: "quantitative" },
    },
  }) as AlixAIBlock<"chart">;

export const choicesBlock = (allowFreeText = true): AlixAIBlock<"clarification_choices"> =>
  ({
    type: "clarification_choices",
    field: "slot",
    prompt: "Which slot suits you?",
    answer_kind: "choice",
    allow_free_text: allowFreeText,
    choices: [
      { choice_id: "slot_a", label: "09:30", value: { kind: "text", value: "09:30" } },
      { choice_id: "slot_b", label: "11:00", value: { kind: "text", value: "11:00" } },
    ],
  }) as AlixAIBlock<"clarification_choices">;

export const actionCardBlock = (): AlixAIBlock<"action_card"> =>
  ({
    type: "action_card",
    title: "Visit booked",
    facts: [
      { field: "when", value: { kind: "text", value: "14 March, 09:30" } },
      { field: "fee", value: { kind: "money", value: "48.50", currency: "EUR" } },
    ],
    actions: [],
  }) as AlixAIBlock<"action_card">;

export const diagramBlock = (): AlixAIBlock<"diagram"> =>
  ({
    type: "diagram",
    language: "mermaid",
    requires_host_renderer: true,
    description: "Booking flow",
    source: "graph TD; A-->B",
  }) as AlixAIBlock<"diagram">;
