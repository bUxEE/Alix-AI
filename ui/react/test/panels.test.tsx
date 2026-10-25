// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { types } from "@alixai/sdk";
import { AlixConversationList, AlixProvider, AlixSettingsPanel, AlixSourceHealthPanel } from "../src/index.js";

afterEach(cleanup);

const source = (overrides: Partial<types.rag.SourceStatus> = {}): types.rag.SourceStatus =>
  ({
    source_id: "src_01J00000000000000000000001",
    display_name: "Clinic handbook",
    state: "ready",
    availability: "available",
    staleness: "current",
    last_attempt_at: "2026-03-10T08:00:00Z",
    last_success_at: "2026-03-10T08:00:00Z",
    counts: { discovered: 1240, indexed: 1236, skipped: 4, failed: 0, tombstoned: 0 },
    active_generation_id: null,
    ...overrides,
  }) as types.rag.SourceStatus;

describe("the source health panel", () => {
  it("shows state, counts and the last success, and flags a failure with its message", () => {
    render(
      <AlixSourceHealthPanel
        sources={[
          source(),
          source({
            source_id: "src_01J00000000000000000000002",
            display_name: "Shared drive",
            state: "indexing",
            last_success_at: null,
            counts: { discovered: 10, indexed: 3, skipped: 0, failed: 2, tombstoned: 0 },
            error: { code: "internal", message: "Two files could not be read.", retryable: true },
          } as Partial<types.rag.SourceStatus>),
        ]}
      />,
    );
    const rows = within(screen.getByRole("region", { name: "Sources" })).getAllByRole("listitem");
    expect(rows.length).toBe(2);
    expect(within(rows[0]!).getByText("Ready").getAttribute("data-tone")).toBe("ok");
    expect(within(rows[0]!).getByText("1,240")).toBeTruthy();
    expect(within(rows[1]!).getByText("Indexing").getAttribute("data-tone")).toBe("danger");
    expect(within(rows[1]!).getByText(/Never/)).toBeTruthy();
    expect(within(rows[1]!).getByRole("alert").textContent).toBe("Two files could not be read.");
  });

  it("says when there are no sources, and speaks Italian", () => {
    render(
      <AlixProvider locale="it">
        <AlixSourceHealthPanel sources={[]} />
      </AlixProvider>,
    );
    expect(screen.getByText("Nessuna fonte registrata.")).toBeTruthy();
  });
});

const settings = (): types.settings.EffectiveSettings =>
  ({
    levels: { deployment: 1, tenant: 0, user: 2 },
    language: "it",
    model_profile: "balanced",
    voice: { allowed: true, enabled: false },
    history: { retention_days: 90, auto_cleanup: true, export_allowed: false },
    privacy: { store_transcript: true },
    attachments: { allowed: true, enabled: true, max_file_mb: 25 },
    sources: { include_attachments: false, citation_display: "footnote" },
    notifications: { allowed: true, enabled: false, events: [] },
    streaming: { enabled: true, unit: "sentence" },
    accessibility: { reduced_motion: true, high_contrast: false, text_scale_pct: 110, screen_reader: false, captions: true },
    capabilities: { disabled: [] },
    confirmation: {},
    decided_by: { language: "user", "history.retention_days": "tenant", "attachments.max_file_mb": "deployment" },
  }) as unknown as types.settings.EffectiveSettings;

describe("the settings panel", () => {
  it("shows the effective settings read only, with the level that decided each value", () => {
    render(
      <AlixSettingsPanel settings={settings()}>
        <button type="button">Edit</button>
      </AlixSettingsPanel>,
    );
    const history = screen.getByRole("region", { name: "History" });
    expect(within(history).getByText("90")).toBeTruthy();
    expect(within(history).getByText("Organisation")).toBeTruthy();
    expect(within(screen.getByRole("region", { name: "Attachments" })).getByText("Deployment")).toBeTruthy();
    expect(within(screen.getByRole("region", { name: "Sources" })).getByText("Footnote")).toBeTruthy();
    // Nothing here changes a setting: the only control is the one the host passed in.
    expect(screen.getAllByRole("button").map((button) => button.textContent)).toEqual(["Edit"]);
  });

  it("speaks Italian", () => {
    render(
      <AlixProvider locale="it">
        <AlixSettingsPanel settings={settings()} />
      </AlixProvider>,
    );
    expect(screen.getByRole("heading", { name: "Impostazioni" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "Cronologia" })).toBeTruthy();
  });
});

describe("the conversation list", () => {
  it("marks the current conversation and reports a pick and a new conversation", () => {
    const onSelect = vi.fn();
    const onNew = vi.fn();
    render(
      <AlixConversationList
        items={[{ id: "ses_a", title: "Rescheduling", updatedAt: "2026-03-10T08:00:00Z" }, { id: "ses_b" }]}
        activeId="ses_a"
        onSelect={onSelect}
        onNew={onNew}
      />,
    );
    expect(screen.getByRole("button", { name: /Rescheduling/ }).getAttribute("aria-current")).toBe("true");
    fireEvent.click(screen.getByRole("button", { name: /Untitled/ }));
    expect(onSelect).toHaveBeenCalledWith("ses_b");
    fireEvent.click(screen.getByRole("button", { name: "New" }));
    expect(onNew).toHaveBeenCalledTimes(1);
  });

  it("says when there are none", () => {
    render(<AlixConversationList items={[]} onSelect={vi.fn()} />);
    expect(screen.getByText("No conversations yet.")).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });
});
