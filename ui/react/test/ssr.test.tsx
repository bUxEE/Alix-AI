import { renderToString } from "react-dom/server";
import { describe, expect, it } from "vitest";
import {
  AlixActionCard,
  AlixAttachmentComposer,
  AlixChartBlock,
  AlixClarificationForm,
  AlixCitationList,
  AlixConfirmationSheet,
  AlixConversationList,
  AlixDiagramBlock,
  AlixFileList,
  AlixProgressLine,
  AlixProvider,
  AlixSettingsPanel,
  AlixSourceHealthPanel,
  AlixTableBlock,
} from "../src/index.js";
import { actionCardBlock, attachment, chartBlock, choicesBlock, citation, diagramBlock, interrupt, tableBlock } from "./fixtures.js";

// This file runs in the node environment: there is no window, no document and no layout. Everything the root entry exports
// has to render to a string there (server rendering) without touching a browser API.

describe("server rendering", () => {
  it("has no window", () => {
    expect(typeof window).toBe("undefined");
    expect(typeof document).toBe("undefined");
  });

  it("renders every component to a string", () => {
    const noop = (): void => undefined;
    const html = renderToString(
      <AlixProvider locale="it" timeZone="UTC">
        <AlixConfirmationSheet interrupt={interrupt()} onApprove={noop} onDecline={noop} />
        <AlixTableBlock block={tableBlock()} />
        <AlixChartBlock block={chartBlock("bar")} />
        <AlixChartBlock block={chartBlock("pie")} />
        <AlixDiagramBlock block={diagramBlock()} />
        <AlixActionCard block={actionCardBlock()} />
        <AlixClarificationForm block={choicesBlock()} onChoose={noop} onFreeText={noop} />
        <AlixCitationList citations={[citation()]} onOpenCitation={noop} />
        <AlixFileList files={[attachment()]} />
        <AlixAttachmentComposer files={[attachment()]} onAddFiles={noop} onRemoveFile={noop} maxFileMb={10} />
        <AlixConversationList items={[{ id: "a", title: "Prima", updatedAt: "2026-03-10T08:00:00Z" }]} onSelect={noop} />
        <AlixProgressLine progress={{ stage: "Reading", percent: 40 }} />
        <AlixSourceHealthPanel sources={[]} />
      </AlixProvider>,
    );
    expect(html).toContain("Approva");
    expect(html).toContain("alix-chart__mark");
    expect(html).toContain("Fonti");
    // A fixed zone makes the server and the browser print the same time.
    expect(html).toContain("10 mar 2026");
  });

  it("renders the settings panel to a string", () => {
    const settings = {
      levels: { deployment: 0, tenant: 0, user: 0 },
      model_profile: "balanced",
      voice: { allowed: false, enabled: false },
      history: { retention_days: 30, auto_cleanup: false, export_allowed: false },
      privacy: { store_transcript: false },
      attachments: { allowed: false, enabled: false, max_file_mb: 5 },
      sources: { include_attachments: false, citation_display: "panel" },
      notifications: { allowed: false, enabled: false, events: [] },
      streaming: { enabled: true, unit: "token" },
      accessibility: { reduced_motion: false, high_contrast: false, text_scale_pct: 100, screen_reader: false, captions: false },
      capabilities: { disabled: [] },
      confirmation: {},
      decided_by: {},
    };
    expect(renderToString(<AlixSettingsPanel settings={settings as never} />)).toContain("alix-settings");
  });
});
