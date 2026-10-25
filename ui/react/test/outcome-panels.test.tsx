// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import type { ReactElement } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ALIXAI_ACTIVITY, blockActivityType } from "@alixai/copilotkit-adapter";
import type { AlixAIBlock } from "@alixai/copilotkit-adapter";
import type { ActionReceipt, SettingsAuditStatus, SettingsProviderHealth, types } from "@alixai/sdk";
import {
  AlixConfirmationOutcome,
  AlixConfirmationPanel,
  AlixContinueInApp,
  AlixIntegrationAuditPanel,
  AlixProvider,
  AlixReceiptCard,
  AlixSettingsPanel,
  AlixSourceHealthPanel,
  parseContinueInApp,
  parseReceipt,
} from "../src/index.js";
import { alixKitComponents, alixProviderProps } from "../src/copilotkit.js";

afterEach(cleanup);

// Fixtures in the shapes of the frozen schemas: fictional records, no secrets.

const receipt = (overrides: Partial<ActionReceipt> = {}): ActionReceipt =>
  ({
    receipt_id: "rcpt_01J00000000000000000000001",
    preparation_id: "prep_01J00000000000000000000001",
    capability_id: "work_orders.create",
    status: "committed",
    records: [{ entity_type: "work_order", entity_id: "WO-2318" }],
    facts: [
      { field: "work_order", value: { kind: "identifier", value: "WO-2318" } },
      { field: "scheduled_for", value: { kind: "date", value: "2026-10-14" }, display: "Wed 14 Oct" },
    ],
    errors: [],
    audit_ref: "aud_01J00000000000000000000001",
    issued_at: "2026-10-11T09:30:00Z",
    ...overrides,
  }) as ActionReceipt;

const panelBlock = (mode: "explicit" | "step_up" = "explicit"): AlixAIBlock<"confirmation_panel"> =>
  ({
    type: "confirmation_panel",
    preparation_id: "prep_01J00000000000000000000002",
    summary: "End the cleaning contract with Brightline Services on 31 January 2027",
    effects: [{ operation: "update", entity_type: "contract", description: "Give notice on contract CTR-0087.", changes: [{ field: "status", value: { kind: "text", value: "notice_given" } }], external: true }],
    risk_level: mode === "step_up" ? 3 : 2,
    expires_at: "2026-10-11T09:40:00Z",
    confirmation: { mode, bindings: [] },
  }) as unknown as AlixAIBlock<"confirmation_panel">;

const audit = (): SettingsAuditStatus => ({
  application_id: "app_northwind",
  state: "active" as SettingsAuditStatus["state"],
  package_version: "northwind-facilities@3.2.0",
  audited: true,
  outcome: "needs_acceptance",
  counts: { critical: 1, warnings: 1, info: 1, unverified: 0 },
  findings: [
    { key: "f3", check_id: "c3", severity: "info", message: "A capability has no description in Italian.", remediation: "Add it.", status: "informational" },
    { key: "f1", check_id: "c1", severity: "critical", capability: "contracts.terminate", message: "No step-up for a risk 3 action.", remediation: "Declare step_up.", status: "open" },
    { key: "f2", check_id: "c2", severity: "warning", message: "A field is free text.", remediation: "Use an enum.", status: "accepted" },
  ],
});

const providers = (): SettingsProviderHealth => ({
  status: "degraded",
  roles: [
    { role: "chat", configured: true, status: "ok" },
    { role: "speech", configured: true, status: "degraded", effect: "slow" },
    { role: "vision", configured: false, status: "unavailable" },
  ],
});

const effective = (overrides: Partial<Record<string, unknown>> = {}): types.settings.EffectiveSettings =>
  ({
    levels: { deployment: 1, tenant: 1, user: 1 },
    language: "en",
    model_profile: "balanced",
    voice: { allowed: false, enabled: false, speech_rate_pct: 100, auto_play: false, barge_in: false },
    history: { retention_days: 90, auto_cleanup: true, export_allowed: false },
    privacy: { store_transcript: true },
    attachments: { allowed: true, enabled: true, max_file_mb: 8 },
    sources: { include_attachments: false, citation_display: "footnote" },
    notifications: { allowed: true, enabled: false, events: [] },
    streaming: { enabled: true, unit: "sentence" },
    accessibility: { reduced_motion: false, high_contrast: false, text_scale_pct: 100, screen_reader: false, captions: false },
    capabilities: { disabled: [] },
    confirmation: {},
    decided_by: {},
    ...overrides,
  }) as unknown as types.settings.EffectiveSettings;

describe("the receipt", () => {
  it("reads a receipt from a stream and refuses anything else", () => {
    expect(parseReceipt(receipt())?.receipt_id).toBe("rcpt_01J00000000000000000000001");
    expect(parseReceipt({ ...receipt(), errors: undefined })?.errors).toEqual([]);
    expect(parseReceipt({ ...receipt(), status: "approved" })).toBeUndefined();
    expect(parseReceipt({ ...receipt(), facts: "WO-2318" })).toBeUndefined();
    expect(parseReceipt({ status: "committed" })).toBeUndefined();
    expect(parseReceipt("committed")).toBeUndefined();
  });

  it("shows the status in words, the application's facts and the follow-up, in Italian too", () => {
    const { unmount } = render(<AlixReceiptCard receipt={receipt({ status: "queued", follow_up: { reference: "job_1", state: "running", as_of: "2026-10-11T09:30:00Z" } } as Partial<ActionReceipt>)} />);
    const card = screen.getByRole("region", { name: "Result" });
    expect(within(card).getByText("Queued").getAttribute("data-tone")).toBe("accent");
    expect(within(card).getByText("WO-2318")).toBeTruthy();
    expect(within(card).getByText("Wed 14 Oct")).toBeTruthy();
    expect(within(card).getByText("Follow-up: running")).toBeTruthy();
    unmount();
    render(
      <AlixProvider locale="it">
        <AlixReceiptCard receipt={receipt({ status: "partial", errors: [{ code: "partial", message: "The e-mail was not sent." }] } as unknown as Partial<ActionReceipt>)} />
      </AlixProvider>,
    );
    const italian = screen.getByRole("region", { name: "Esito" });
    expect(within(italian).getByText("Eseguita in parte")).toBeTruthy();
    expect(within(italian).getByRole("list", { name: "Problemi" }).textContent).toBe("The e-mail was not sent.");
  });

  it("says in one line how a confirmation ended without a receipt", () => {
    render(<AlixConfirmationOutcome status="rejected" />);
    expect(screen.getByRole("status").textContent).toBe("Declined. Nothing was changed.");
  });
});

describe("the prepared action and the step-up", () => {
  it("keeps the prepared action in the transcript, with its effects folded and what happens next", () => {
    render(<AlixConfirmationPanel block={panelBlock("explicit")} />);
    const panel = screen.getByRole("region", { name: "Prepared action" });
    expect(panel.getAttribute("data-mode")).toBe("explicit");
    expect(within(panel).getByText(/Risk level: Consequential/)).toBeTruthy();
    expect(within(panel).getByText("The changes (1)")).toBeTruthy();
    expect(within(panel).getByText("Give notice on contract CTR-0087.")).toBeTruthy();
    expect(within(panel).getByText(/Nothing happens until you approve it/)).toBeTruthy();
    expect(within(panel).queryByRole("button")).toBeNull();
  });

  it("names a step-up action as one for the application", () => {
    render(<AlixConfirmationPanel block={panelBlock("step_up")} />);
    const panel = screen.getByRole("region", { name: "Needs the application" });
    expect(within(panel).getByText(/Risk level: High risk/)).toBeTruthy();
    expect(within(panel).getByText(/continues in the application/)).toBeTruthy();
  });

  it("hands the step-up call to the host, and only explains without a handler", () => {
    const request = parseContinueInApp({ preparationId: "prep_2", capabilityId: "contracts.terminate", summary: "End the contract", secret: "never kept" });
    expect(request).toEqual({ preparationId: "prep_2", capabilityId: "contracts.terminate", summary: "End the contract" });
    expect(parseContinueInApp("nonsense")).toEqual({});
    const onContinue = vi.fn();
    const { unmount } = render(<AlixContinueInApp request={request} onContinue={onContinue} />);
    fireEvent.click(screen.getByRole("button", { name: "Continue in the application" }));
    expect(onContinue).toHaveBeenCalledWith(request);
    unmount();
    render(<AlixContinueInApp request={request} />);
    expect(screen.getByRole("region", { name: "Continue in the application" })).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });
});

type Renderer = { activityType: string; content: { "~standard": { validate: (value: unknown) => { value?: unknown; issues?: unknown } } }; render: (props: { content: unknown; agent: undefined }) => ReactElement | null };

describe("the kit's renderers for what follows a prepared action", () => {
  const renderers = (props: object = {}, options = {}): Renderer[] => (alixProviderProps(props, options) as unknown as { renderActivityMessages: Renderer[] }).renderActivityMessages;

  it("registers the receipt activity and the confirmation panel block, and keeps the host's own renderers", () => {
    const own = { activityType: "host.thing", content: {}, render: () => null };
    const types = renderers({ renderActivityMessages: [own] }).map((renderer) => renderer.activityType);
    expect(types).toContain(ALIXAI_ACTIVITY.receipt);
    expect(types).toContain(blockActivityType("confirmation_panel"));
    expect(types).toContain("host.thing");
  });

  it("draws a valid receipt and refuses one that is not", () => {
    const renderer = renderers().find((entry) => entry.activityType === ALIXAI_ACTIVITY.receipt)!;
    expect("issues" in (renderer.content["~standard"].validate({ receipt_id: 1 }) as object)).toBe(true);
    const parsed = renderer.content["~standard"].validate(receipt()) as { value: unknown };
    const { container } = render(<>{renderer.render({ content: parsed.value, agent: undefined })}</>);
    expect(container.querySelector(".alix-receipt")).not.toBeNull();
    const panel = renderers().find((entry) => entry.activityType === blockActivityType("confirmation_panel"))!;
    const drawn = render(<>{panel.render({ content: panel.content["~standard"].validate(panelBlock()).value, agent: undefined })}</>);
    expect(drawn.container.querySelector(".alix-prepared")).not.toBeNull();
  });

  it("lets a host component win for the receipt", () => {
    const own = () => <p>own</p>;
    expect(alixKitComponents({ components: { [ALIXAI_ACTIVITY.receipt]: own as never } })[ALIXAI_ACTIVITY.receipt]).toBe(own);
  });
});

describe("the integration audit panel", () => {
  it("orders the findings critical first, with remediation, and shows each role's health in words", () => {
    render(<AlixIntegrationAuditPanel audit={audit()} providers={providers()} />);
    const panel = screen.getByRole("region", { name: "Integration audit" });
    expect(within(panel).getByText("Needs acceptance").getAttribute("data-tone")).toBe("warn");
    expect(within(panel).getByText("northwind-facilities@3.2.0")).toBeTruthy();
    const findings = within(within(panel).getByRole("list", { name: "Findings" })).getAllByRole("listitem");
    expect(findings.map((item) => item.getAttribute("data-severity"))).toEqual(["critical", "warning", "info"]);
    expect(within(findings[0]!).getByText("What to do: Declare step_up.")).toBeTruthy();
    const roles = within(within(panel).getByRole("region", { name: "Model and service health" })).getAllByRole("listitem");
    expect(roles.map((role) => role.textContent)).toEqual(["chatWorking", "speechDegraded", "visionNot set up"]);
    expect(within(panel).queryByRole("button")).toBeNull();
  });

  it("shows a skeleton on the first load, the error with a retry after a failed one, and refreshes on request", () => {
    const onRefresh = vi.fn();
    const { rerender, container } = render(<AlixIntegrationAuditPanel loading onRefresh={onRefresh} />);
    expect(container.querySelector(".alix-skeleton")).not.toBeNull();
    rerender(<AlixIntegrationAuditPanel failed onRefresh={onRefresh} />);
    expect(screen.getByRole("alert").textContent).toContain("The audit could not be loaded.");
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    rerender(<AlixIntegrationAuditPanel audit={audit()} onRefresh={onRefresh} />);
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(onRefresh).toHaveBeenCalledTimes(2);
  });

  it("says when the package was never audited", () => {
    render(<AlixIntegrationAuditPanel audit={{ ...audit(), audited: false, outcome: undefined, findings: [] } as SettingsAuditStatus} />);
    expect(screen.getAllByText("Not audited yet").length).toBe(2);
  });
});

describe("the source health panel, refetching", () => {
  const source = { source_id: "src_1", display_name: "Maintenance policy", state: "ready", availability: "available", staleness: "current", last_attempt_at: null, last_success_at: null, counts: { discovered: 4, indexed: 4, skipped: 0, failed: 0, tombstoned: 0 }, active_generation_id: null } as unknown as types.rag.SourceStatus;

  it("shows a skeleton on the first load and keeps the list under a scrim on a refetch", () => {
    const onRefresh = vi.fn();
    const { container, rerender } = render(<AlixSourceHealthPanel loading onRefresh={onRefresh} />);
    expect(container.querySelector(".alix-skeleton")).not.toBeNull();
    expect(screen.queryByText("Maintenance policy")).toBeNull();
    rerender(<AlixSourceHealthPanel sources={[source]} onRefresh={onRefresh} />);
    expect(screen.getByText("1 of 1 ready")).toBeTruthy();
    rerender(<AlixSourceHealthPanel sources={[source]} loading onRefresh={onRefresh} />);
    expect(container.querySelector(".alix-skeleton")).toBeNull();
    expect(screen.getByText("Maintenance policy")).toBeTruthy();
    expect(container.querySelector(".alix-loadable__overlay")).not.toBeNull();
    expect((screen.getByRole("button", { name: "Refresh" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("offers a retry after a failed first load", () => {
    const onRefresh = vi.fn();
    render(<AlixSourceHealthPanel failed onRefresh={onRefresh} />);
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });
});

describe("the settings panel as an editor", () => {
  it("edits the user's own document and reports it with the version it replaces", () => {
    const onSave = vi.fn();
    const user = { streaming: { enabled: true } };
    render(<AlixSettingsPanel settings={effective()} user={user} version={3} onSave={onSave} onClearHistory={vi.fn()} />);
    const save = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.click(screen.getByRole("switch", { name: /Show replies as they are written/ }));
    fireEvent.change(screen.getByRole("combobox", { name: /^Ask me to confirm more actions/ }), { target: { value: "2" } });
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    expect(onSave).toHaveBeenCalledWith({ streaming: { enabled: false }, confirmation: { extra_from_risk_level: 2 } }, 3);
  });

  it("shows a feature the policy forbids switched off with the reason, never hidden", () => {
    render(<AlixSettingsPanel settings={effective()} user={{}} onSave={vi.fn()} />);
    const voice = screen.getByRole("switch", { name: /^Voice/ }) as HTMLInputElement;
    expect(voice.disabled).toBe(true);
    expect(screen.getAllByText("Turned off by your organisation").length).toBe(3);
  });

  it("puts a discarded draft back, and clears the history only on request", () => {
    const onClearHistory = vi.fn();
    render(<AlixSettingsPanel settings={effective()} user={{}} onSave={vi.fn()} onClearHistory={onClearHistory} />);
    const transcript = screen.getByRole("switch", { name: /Keep my conversation history/ }) as HTMLInputElement;
    expect(transcript.checked).toBe(true);
    fireEvent.click(transcript);
    expect(transcript.checked).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    expect(transcript.checked).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Clear this conversation's history" }));
    expect(onClearHistory).toHaveBeenCalledTimes(1);
  });

  it("shows the error with a retry when the settings never loaded", () => {
    const onRetry = vi.fn();
    render(<AlixSettingsPanel failed onRetry={onRetry} />);
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });
});
