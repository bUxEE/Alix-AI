import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";
import { nextTick } from "vue";
import type { AlixConfirmation, ChatItem } from "../src/agui/types";
import {
  AlixActionCard,
  AlixAttachmentComposer,
  AlixChartBlock,
  AlixChatPanel,
  AlixCitationList,
  AlixClarificationChoices,
  AlixConfirmationSheet,
  AlixConversationList,
  AlixFilePreview,
  AlixIntegrationAuditPanel,
  AlixLoadable,
  AlixMarkdown,
  AlixMessageStream,
  AlixProvider,
  AlixSettingsPanel,
  AlixSourceHealthPanel,
  AlixTableBlock,
  AlixTranscriptEditor,
  AlixVoiceButton,
} from "../src/index";
import type { Citation, SettingsAuditStatus, SettingsEffective, SourceStatus } from "../src/sdk-types";
import { ev, runFinished, runStarted, scriptedClient, flush } from "./helpers";

const confirmation: AlixConfirmation = {
  preparationId: "prep_1",
  capabilityId: "appointments.book",
  summary: "Book Dr. Rossi on Tuesday at 10:30",
  effects: [
    {
      operation: "create",
      entity_type: "appointment",
      description: "New appointment with Dr. Rossi",
      changes: [{ field: "time", value: { kind: "text", value: "10:30" }, display: "Tuesday 10:30" }],
      external: true,
    },
  ],
  riskLevel: 3,
  expiresAt: "2030-01-01T10:00:00Z",
  mode: "explicit",
};

const citation = (over: Partial<Citation> = {}): Citation => ({
  source_id: "src_1",
  display_name: "Cancellation policy",
  version: "v1",
  locator: { page: 3 },
  availability: "available",
  staleness: "current",
  open_ref: { kind: "file", file_id: "file_1" },
  claims: ["Cancel 24 hours ahead"],
  ...over,
});

describe("AlixConfirmationSheet", () => {
  it("shows the exact effects and risk, and reports approve and decline once each as events without payload", async () => {
    const wrapper = mount(AlixConfirmationSheet, { props: { confirmation } });
    expect(wrapper.text()).toContain("Book Dr. Rossi on Tuesday at 10:30");
    expect(wrapper.text()).toContain("New appointment with Dr. Rossi");
    expect(wrapper.text()).toContain("Tuesday 10:30");
    expect(wrapper.text()).toContain("Risk level 3");
    expect(wrapper.text()).toContain("Leaves this system");
    await wrapper.findAll("button").find((button) => button.text() === "Confirm")?.trigger("click");
    expect(wrapper.emitted("approve")).toEqual([[]]);
    const other = mount(AlixConfirmationSheet, { props: { confirmation } });
    await other.findAll("button").find((button) => button.text() === "Decline")?.trigger("click");
    expect(other.emitted("decline")).toEqual([[]]);
  });

  it("disables approval once the request has expired, and when a step-up is required offers only the hand-off", async () => {
    const expired = mount(AlixConfirmationSheet, { props: { confirmation: { ...confirmation, expiresAt: "2000-01-01T00:00:00Z" } } });
    expect(expired.text()).toContain("This request has expired");
    expect(expired.findAll("button").find((button) => button.text() === "Confirm")?.attributes("disabled")).toBeDefined();

    const stepUp = mount(AlixConfirmationSheet, { props: { confirmation: { ...confirmation, mode: "step_up" } } });
    expect(stepUp.findAll("button").map((button) => button.text())).toEqual(["Continue in the application"]);
    await stepUp.find("button").trigger("click");
    expect(stepUp.emitted("continue-in-app")).toHaveLength(1);
    expect(stepUp.emitted("approve")).toBeUndefined();
  });

  it("replaces the buttons by the outcome after an answer, and traps focus and dismisses on Escape as a modal", async () => {
    const done = mount(AlixConfirmationSheet, { props: { confirmation, status: "approved" } });
    expect(done.findAll("button")).toHaveLength(0);
    expect(done.text()).toContain("Confirmed");

    const modal = mount(AlixConfirmationSheet, { props: { confirmation, modal: true }, attachTo: document.body });
    expect(modal.find("section").attributes("role")).toBe("dialog");
    expect(modal.find("section").attributes("aria-modal")).toBe("true");
    await modal.find("section").trigger("keydown", { key: "Escape" });
    expect(modal.emitted("dismiss")).toHaveLength(1);
    modal.unmount();
  });
});

describe("AlixConfirmationSheet answers once", () => {
  it("emits approve once however often it is clicked, and again only after the host reports the answer failed", async () => {
    const wrapper = mount(AlixConfirmationSheet, { props: { confirmation } });
    const confirm = (): ReturnType<typeof wrapper.find> => wrapper.findAll("button").find((button) => button.text() === "Confirm") as ReturnType<typeof wrapper.find>;
    await confirm().trigger("click");
    await confirm().trigger("click");
    await wrapper.findAll("button").find((button) => button.text() === "Decline")?.trigger("click");
    expect(wrapper.emitted("approve")).toHaveLength(1);
    expect(wrapper.emitted("decline")).toBeUndefined();
    await wrapper.setProps({ status: "failed" });
    await confirm().trigger("click");
    expect(wrapper.emitted("approve")).toHaveLength(2);
  });
});

describe("AlixCitationList", () => {
  it("reports open-citation with the citation itself, marks stale and removed sources, and never hides one", async () => {
    const wrapper = mount(AlixCitationList, {
      props: { citations: [citation(), citation({ source_id: "src_2", display_name: "Old handbook", staleness: "stale" }), citation({ source_id: "src_3", display_name: "Gone", availability: "removed" })] },
    });
    expect(wrapper.text()).toContain("page 3");
    expect(wrapper.text()).toContain("May be out of date");
    expect(wrapper.text()).toContain("Removed");
    expect(wrapper.text()).toContain("3 sources");
    expect(wrapper.findAll("li > div button")).toHaveLength(2);
    await wrapper.find("button").trigger("click");
    expect(wrapper.emitted("open-citation")).toEqual([[expect.objectContaining({ source_id: "src_1" })]]);
  });
});

describe("AlixClarificationChoices", () => {
  const block = {
    type: "clarification_choices" as const,
    field: "doctor",
    answer_kind: "choice" as const,
    allow_free_text: true,
    choices: [{ choice_id: "dr_rossi", label: "Dr. Rossi", value: { kind: "text" as const, value: "rossi" } }],
  };

  it("reports the choice id, and typed text as free-text", async () => {
    const wrapper = mount(AlixClarificationChoices, { props: { block } });
    await wrapper.find("button.alix-chip").trigger("click");
    expect(wrapper.emitted("choose")).toEqual([["dr_rossi"]]);
    await wrapper.find("input").setValue("  someone else  ");
    await wrapper.find("form").trigger("submit");
    expect(wrapper.emitted("free-text")).toEqual([["someone else"]]);
  });

  it("disables the choices once answered", () => {
    const wrapper = mount(AlixClarificationChoices, { props: { block, answered: true } });
    expect(wrapper.find("button.alix-chip").attributes("disabled")).toBeDefined();
    expect(wrapper.find("form").exists()).toBe(false);
  });
});

describe("AlixActionCard", () => {
  it("shows the host's facts as text and reports the chosen action", async () => {
    const wrapper = mount(AlixActionCard, {
      props: {
        block: {
          type: "action_card",
          title: "Appointment",
          facts: [{ field: "doctor", value: { kind: "text", value: "Dr. Rossi" } }],
          actions: [{ action: "continue_in_app" }, { action: "invoke_capability", label: "Reschedule" }],
        },
      },
    });
    expect(wrapper.text()).toContain("Dr. Rossi");
    await wrapper.findAll("button")[1]?.trigger("click");
    expect(wrapper.emitted("action")).toEqual([[{ action: "invoke_capability", label: "Reschedule" }]]);
    expect(wrapper.findAll("button")[0]?.text()).toBe("Continue in the application");
  });
});

describe("AlixAttachmentComposer", () => {
  it("sends on Enter, not on Shift+Enter or while composing, and clears the box", async () => {
    const wrapper = mount(AlixAttachmentComposer, { props: { modelValue: "hello", "onUpdate:modelValue": (value: string) => wrapper.setProps({ modelValue: value }) } });
    const field = wrapper.find("textarea");
    await field.trigger("keydown", { key: "Enter", shiftKey: true });
    await field.trigger("keydown", { key: "Enter", isComposing: true });
    expect(wrapper.emitted("send")).toBeUndefined();
    await field.trigger("keydown", { key: "Enter" });
    expect(wrapper.emitted("send")).toEqual([["hello"]]);
    expect((field.element as HTMLTextAreaElement).value).toBe("");
  });

  it("turns Send into Stop while a run is in flight, and waits for uploads and for text", async () => {
    const running = mount(AlixAttachmentComposer, { props: { modelValue: "x", running: true } });
    await running.find("button[aria-label='Stop']").trigger("click");
    expect(running.emitted("stop")).toHaveLength(1);

    const uploading = mount(AlixAttachmentComposer, {
      props: { modelValue: "x", files: [{ fileId: "pending-1", name: "a.png", mimeType: "image/png", sizeBytes: 3, previewable: false, status: "uploading" }] },
    });
    expect(uploading.find("button[type='submit']").attributes("disabled")).toBeDefined();
    const empty = mount(AlixAttachmentComposer, { props: { modelValue: "  " } });
    expect(empty.find("button[type='submit']").attributes("disabled")).toBeDefined();
  });

  it("reports add-files within the size limit, refuses the rest with a notice, and reports remove-file with the id", async () => {
    const wrapper = mount(AlixAttachmentComposer, {
      props: { modelValue: "", maxFileMb: 1, files: [{ fileId: "file_1", name: "a.png", mimeType: "image/png", sizeBytes: 3, previewable: true, status: "ready" }] },
    });
    const input = wrapper.find("input[type='file']");
    const small = new File(["x"], "ok.txt");
    const big = new File([new Uint8Array(2 * 1024 * 1024)], "big.bin");
    Object.defineProperty(input.element, "files", { value: [small, big], configurable: true });
    await input.trigger("change");
    expect(wrapper.emitted("add-files")).toEqual([[[small]]]);
    expect(wrapper.text()).toContain("big.bin is larger than 1 MB");
    await wrapper.find("button[aria-label='Remove a.png']").trigger("click");
    expect(wrapper.emitted("remove-file")).toEqual([["file_1"]]);
  });
});

describe("AlixConversationList", () => {
  it("reports select and new, and moves focus with the arrow keys", async () => {
    const wrapper = mount(AlixConversationList, {
      props: { items: [{ id: "ses_1", title: "First" }, { id: "ses_2", title: "Second" }], activeId: "ses_2" },
      attachTo: document.body,
    });
    const rows = wrapper.findAll("[data-row]");
    expect(rows[1]?.attributes("aria-current")).toBe("true");
    (rows[0]?.element as HTMLElement).focus();
    await wrapper.find("ul").trigger("keydown", { key: "ArrowDown" });
    expect(document.activeElement).toBe(rows[1]?.element);
    await rows[0]?.trigger("click");
    expect(wrapper.emitted("select")).toEqual([["ses_1"]]);
    await wrapper.find("header button").trigger("click");
    expect(wrapper.emitted("new")).toHaveLength(1);
    wrapper.unmount();
  });

  it("shows a skeleton on first load and the overlay over the rows on refresh", async () => {
    const wrapper = mount(AlixConversationList, { props: { loading: true } });
    expect(wrapper.findAll(".alix-skeleton").length).toBeGreaterThan(0);
    expect(wrapper.find(".alix-section-loader").exists()).toBe(false);
    await wrapper.setProps({ items: [{ id: "ses_1", title: "First" }], loading: false });
    expect(wrapper.findAll(".alix-skeleton")).toHaveLength(0);
    await wrapper.setProps({ loading: true });
    expect(wrapper.text()).toContain("First");
    expect(wrapper.find(".alix-section-loader").exists()).toBe(true);
    expect(wrapper.find(".alix-position-relative").exists()).toBe(true);
  });
});

describe("AlixLoadable", () => {
  it("keeps a failed first load a first load", () => {
    const wrapper = mount(AlixLoadable, { props: { loading: true, loaded: false }, slots: { skeleton: "<i class='sk'/>", default: "<b class='content'/>" } });
    expect(wrapper.find(".sk").exists()).toBe(true);
    expect(wrapper.find(".alix-section-loader").exists()).toBe(false);
  });
});

describe("text blocks render as text", () => {
  it("shows markup in markdown as text and keeps only safe links", () => {
    const wrapper = mount(AlixMarkdown, {
      props: { source: "# Title\n\n<img src=x onerror=alert(1)> and **bold** [ok](https://example.com/a) [bad](javascript:alert(1))\n\n```\n<script>alert(1)</script>\n```" },
    });
    expect(wrapper.find("img").exists()).toBe(false);
    expect(wrapper.find("script").exists()).toBe(false);
    expect(wrapper.text()).toContain("<img src=x onerror=alert(1)>");
    expect(wrapper.find("strong").text()).toBe("bold");
    const links = wrapper.findAll("a");
    expect(links).toHaveLength(1);
    expect(links[0]?.attributes("href")).toBe("https://example.com/a");
    expect(links[0]?.attributes("rel")).toContain("noopener");
    expect(wrapper.text()).toContain("bad");
    expect(wrapper.html()).not.toContain("javascript:");
    // The form the core's sanitiser writes: the address in angle brackets, the label escaped.
    const sanitised = mount(AlixMarkdown, { props: { source: "see [snake\\_case docs](<https://example.com/a>)" } });
    expect(sanitised.find("a").attributes("href")).toBe("https://example.com/a");
    expect(sanitised.text()).toBe("see snake_case docs");
  });

  it("renders table cells as text, booleans as words, and a missing value as a dash read as no value", () => {
    const wrapper = mount(AlixTableBlock, {
      props: { block: { type: "table", caption: "Visits", columns: [{ key: "n", label: "Name" }, { key: "ok", label: "Done", kind: "boolean" }], rows: [["<b>x</b>", true], [null, false]] } },
    });
    expect(wrapper.find("b").exists()).toBe(false);
    expect(wrapper.text()).toContain("<b>x</b>");
    expect(wrapper.text()).toContain("Yes");
    expect(wrapper.text()).toContain("no value");
    expect(wrapper.find("caption").text()).toBe("Visits");
  });

  it("draws a chart with its text alternative and the data as a table, and falls back to the description when the data does not fit the encoding", () => {
    const base = { type: "chart" as const, description: "Visits per day", mark: "bar" as const };
    const ok = mount(AlixChartBlock, {
      props: { block: { ...base, data: [{ day: "Mon", n: 4 }, { day: "Tue", n: 9 }], encoding: { x: { field: "day", type: "nominal" as const }, y: { field: "n", type: "quantitative" as const } } } },
    });
    expect(ok.find("svg").attributes("aria-label")).toBe("Visits per day");
    expect(ok.findAll("rect")).toHaveLength(2);
    expect(ok.find("details table").exists()).toBe(true);
    const broken = mount(AlixChartBlock, {
      props: { block: { ...base, data: [{ day: "Mon" }], encoding: { x: { field: "day", type: "nominal" as const }, y: { field: "missing", type: "quantitative" as const } } } },
    });
    expect(broken.find("svg").exists()).toBe(false);
    expect(broken.text()).toContain("Visits per day");
  });
});

describe("AlixFilePreview", () => {
  const file = { fileId: "file_1", name: "scan.png", mimeType: "image/png", sizeBytes: 2048, previewable: true };

  it("accepts only addresses that cannot run anything", () => {
    expect(mount(AlixFilePreview, { props: { file, src: "blob:http://x/1" } }).find("img").exists()).toBe(true);
    expect(mount(AlixFilePreview, { props: { file, src: "javascript:alert(1)" } }).find("img").exists()).toBe(false);
    expect(mount(AlixFilePreview, { props: { file, src: "data:image/svg+xml;base64,AAAA" } }).find("img").exists()).toBe(false);
    expect(mount(AlixFilePreview, { props: { file, src: "http://insecure/x.png" } }).find("img").exists()).toBe(false);
  });

  it("reports download, open and remove with the file", async () => {
    const wrapper = mount(AlixFilePreview, { props: { file, removable: true } });
    await wrapper.find("button[aria-label='Open scan.png']").trigger("click");
    await wrapper.find("button[aria-label='Download scan.png']").trigger("click");
    await wrapper.find("button[aria-label='Remove scan.png']").trigger("click");
    expect(wrapper.emitted("open")).toEqual([[file]]);
    expect(wrapper.emitted("download")).toEqual([[file]]);
    expect(wrapper.emitted("remove")).toEqual([["file_1"]]);
    expect(wrapper.text()).toContain("2 KB");
  });
});

describe("AlixMessageStream", () => {
  const items: ChatItem[] = [
    { kind: "message", id: "u1", role: "user", text: "book it", streaming: false },
    { kind: "message", id: "a1", role: "assistant", text: "Sure **thing**", streaming: false },
    { kind: "confirmation", id: "c1", confirmation, interruptId: "prep_1", status: "pending" },
    { kind: "error", id: "e1", message: "It failed", code: "unavailable" },
  ];

  it("is a labelled log that is busy while a reply streams, and reports answers with the item id", async () => {
    const wrapper = mount(AlixMessageStream, { props: { items, running: true } });
    const log = wrapper.find("[role='log']");
    expect(log.attributes("aria-busy")).toBe("true");
    expect(log.attributes("aria-label")).toBe("Conversation");
    expect(wrapper.find("strong").text()).toBe("thing");
    await wrapper.setProps({ running: false });
    await wrapper.findAll("button").find((button) => button.text() === "Confirm")?.trigger("click");
    await wrapper.findAll("button").find((button) => button.text() === "Try again")?.trigger("click");
    expect(wrapper.emitted("approve")).toEqual([["c1"]]);
    const declined = mount(AlixMessageStream, { props: { items } });
    await declined.findAll("button").find((button) => button.text() === "Decline")?.trigger("click");
    expect(declined.emitted("decline")).toEqual([["c1"]]);
    expect(wrapper.emitted("retry")).toHaveLength(1);
  });

  it("offers no answer to a confirmation while another run holds the thread, and takes one once it ends", async () => {
    // The controller refuses an answer while a run is in flight; a click then would leave the sheet answered for good.
    const wrapper = mount(AlixMessageStream, { props: { items, running: true } });
    const button = (label: string): ReturnType<typeof wrapper.find> | undefined => wrapper.findAll("button").find((found) => found.text() === label);
    expect(button("Confirm")?.attributes("disabled")).toBeDefined();
    expect(button("Decline")?.attributes("disabled")).toBeDefined();
    await button("Confirm")?.trigger("click");
    expect(wrapper.emitted("approve")).toBeUndefined();
    await wrapper.setProps({ running: false });
    await button("Confirm")?.trigger("click");
    expect(wrapper.emitted("approve")).toEqual([["c1"]]);
  });

  it("lets a consumer replace one kind of entry through a slot without touching the component", () => {
    const wrapper = mount(AlixMessageStream, { props: { items }, slots: { error: "<p class='mine'>custom error</p>" } });
    expect(wrapper.find(".mine").exists()).toBe(true);
    expect(wrapper.text()).not.toContain("It failed");
  });

  it("shows an empty state in a slot with a default", () => {
    expect(mount(AlixMessageStream, { props: { items: [] } }).text()).toContain("How can I help?");
  });

  it("brings a reader who scrolled up to the message they just sent, and only offers a jump for the replies", async () => {
    const answered: ChatItem[] = [
      { kind: "message", id: "u1", role: "user", text: "first", streaming: false },
      { kind: "message", id: "a1", role: "assistant", text: "an answer", streaming: false },
    ];
    const wrapper = mount(AlixMessageStream, { props: { items: answered }, attachTo: document.body });
    const log = wrapper.find("[role='log']").element as HTMLElement;
    // jsdom lays nothing out: make the log tall and scrolled away from the end.
    Object.defineProperty(log, "scrollHeight", { configurable: true, value: 1000 });
    Object.defineProperty(log, "clientHeight", { configurable: true, value: 200 });
    const scrolled: number[] = [];
    log.scrollTo = ((options: ScrollToOptions) => scrolled.push(options.top ?? 0)) as typeof log.scrollTo;
    log.scrollTop = 100;
    await wrapper.find("[role='log']").trigger("scroll");

    await wrapper.setProps({ items: [...answered, { kind: "message", id: "a2", role: "assistant", text: "more", streaming: false }] });
    await flushPromises();
    expect(scrolled).toEqual([]);
    expect(wrapper.text()).toContain("Jump to latest");

    await wrapper.setProps({
      items: [...answered, { kind: "message", id: "a2", role: "assistant", text: "more", streaming: false }, { kind: "message", id: "u2", role: "user", text: "next", streaming: false }],
    });
    await flushPromises();
    expect(scrolled).toEqual([1000]);
    wrapper.unmount();
  });
});

describe("panels", () => {
  const status = (over: Partial<SourceStatus> = {}): SourceStatus => ({
    source_id: "src_1",
    display_name: "Clinic handbook",
    state: "ready",
    availability: "available",
    staleness: "current",
    last_attempt_at: "2030-01-01T08:00:00Z",
    last_success_at: "2030-01-01T08:00:00Z",
    counts: { discovered: 12, indexed: 10, skipped: 1, failed: 1, tombstoned: 0 },
    active_generation_id: "gen_1",
    ...over,
  });

  it("source health lists each source with its state, counts and typed error, and asks the host to refresh", async () => {
    const wrapper = mount(AlixSourceHealthPanel, { props: { sources: [status(), status({ source_id: "src_2", display_name: "Old", state: "paused", staleness: "stale", last_success_at: null })] } });
    expect(wrapper.text()).toContain("Clinic handbook");
    expect(wrapper.text()).toContain("10 indexed, 1 failed, 1 skipped");
    expect(wrapper.text()).toContain("Never indexed");
    expect(wrapper.text()).toContain("Paused");
    expect(wrapper.text()).toContain("May be out of date");
    await wrapper.find("button[aria-label='Refresh']").trigger("click");
    expect(wrapper.emitted("refresh")).toHaveLength(1);
    expect(mount(AlixSourceHealthPanel, { props: { sources: [] } }).text()).toContain("No sources are connected.");
  });

  const audit: SettingsAuditStatus = {
    application_id: "app_1",
    state: "active",
    audited: true,
    outcome: "needs_acceptance",
    counts: { critical: 1, warnings: 1, info: 0, unverified: 0 },
    findings: [
      { key: "k2", check_id: "c2", severity: "warning", message: "Broad scope", remediation: "Narrow it", status: "open" },
      { key: "k1", check_id: "c1", severity: "critical", message: "Writes without confirmation", remediation: "Require confirmation", status: "open", capability: "records.delete" },
    ],
  };

  it("integration audit orders findings by severity and shows providers", () => {
    const wrapper = mount(AlixIntegrationAuditPanel, {
      props: { audit, providers: { status: "degraded", roles: [{ role: "chat", configured: true, status: "degraded" }, { role: "voice", configured: false, status: "unavailable" }] } },
    });
    const messages = wrapper.findAll("li p.alix-break-word:not(.alix-text-muted)").map((node) => node.text());
    expect(messages).toEqual(["Writes without confirmation", "Broad scope"]);
    expect(wrapper.text()).toContain("Needs acceptance");
    expect(wrapper.text()).toContain("Not set up");
    expect(wrapper.text()).toContain("Degraded");
  });

  const effective: SettingsEffective = {
    levels: { deployment: 1, tenant: 1, user: 1 },
    language: "en",
    model_profile: "balanced",
    voice: { allowed: false, enabled: false, speech_rate_pct: 100, auto_play: false, barge_in: false },
    history: { retention_days: 30, auto_cleanup: true, export_allowed: true },
    privacy: { store_transcript: true },
    attachments: { allowed: true, enabled: true, max_file_mb: 10 },
    sources: { include_attachments: false, citation_display: "panel" },
    notifications: { allowed: true, enabled: false, events: [] },
    streaming: { enabled: true, unit: "token" },
    accessibility: { reduced_motion: false, high_contrast: false, text_scale_pct: 100, screen_reader: false, captions: false },
    capabilities: { disabled: [] },
    confirmation: {},
    decided_by: {},
  };

  it("settings is a read-only view of the effective values without a user document", () => {
    const wrapper = mount(AlixSettingsPanel, { props: { settings: effective } });
    expect(wrapper.find("input").exists()).toBe(false);
    expect(wrapper.text()).toContain("History is kept for 30 days.");
    expect(wrapper.text()).toContain("Turned off by your organisation");
  });

  it("settings edits only the user's own document, disables what policy forbids, and saves the whole document with the version it replaces", async () => {
    const wrapper = mount(AlixSettingsPanel, { props: { settings: effective, user: { streaming: { enabled: false } }, version: 4 } });
    const switches = wrapper.findAll("input[role='switch']");
    expect(switches.length).toBeGreaterThan(4);
    const voice = wrapper.findAll("input[role='switch']").filter((node) => node.attributes("disabled") !== undefined);
    expect(voice).toHaveLength(3);
    expect(wrapper.find("button[type='submit']").attributes("disabled")).toBeDefined();
    const transcript = switches.find((node) => node.element.closest(".alix-switch")?.textContent?.includes("Keep my conversation history"));
    await transcript?.setValue(false);
    await wrapper.find("form").trigger("submit");
    expect(wrapper.emitted("save")).toEqual([[{ streaming: { enabled: false }, privacy: { store_transcript: false } }, 4]]);
    expect(wrapper.text()).not.toMatch(/lower|disable confirmation/i);
  });
});

describe("voice shells", () => {
  it("the voice button toggles, or holds to talk with pointer and keyboard", async () => {
    const toggle = mount(AlixVoiceButton);
    await toggle.trigger("click");
    expect(toggle.emitted("start")).toHaveLength(1);
    await toggle.setProps({ state: "listening" });
    expect(toggle.attributes("aria-pressed")).toBe("true");
    await toggle.trigger("click");
    expect(toggle.emitted("stop")).toHaveLength(1);

    const hold = mount(AlixVoiceButton, { props: { mode: "push-to-talk" } });
    await hold.trigger("keydown", { key: " " });
    expect(hold.emitted("start")).toHaveLength(1);
    await hold.setProps({ state: "listening" });
    await hold.trigger("keyup", { key: " " });
    expect(hold.emitted("stop")).toHaveLength(1);

    const off = mount(AlixVoiceButton, { props: { state: "unavailable" } });
    await off.trigger("click");
    expect(off.emitted("start")).toBeUndefined();
  });

  it("the transcript editor sends only the corrected text", async () => {
    const wrapper = mount(AlixTranscriptEditor, { props: { modelValue: "book dr rosy" } });
    await wrapper.find("textarea").setValue("  book Dr. Rossi  ");
    await wrapper.find("form").trigger("submit");
    expect(wrapper.emitted("confirm")).toEqual([["book Dr. Rossi"]]);
    await wrapper.findAll("button")[0]?.trigger("click");
    expect(wrapper.emitted("discard")).toHaveLength(1);
  });
});

describe("AlixProvider", () => {
  it("switches the kit to Italian and sets the theming attributes", () => {
    const wrapper = mount(AlixProvider, {
      props: { locale: "it", theme: "dark", contrast: "more", reducedMotion: true, textScalePct: 150 },
      slots: { default: () => "x" },
    });
    expect(wrapper.attributes("lang")).toBe("it");
    expect(wrapper.attributes("data-theme")).toBe("dark");
    expect(wrapper.attributes("data-contrast")).toBe("more");
    expect(wrapper.attributes("data-reduced-motion")).toBe("true");
    expect(wrapper.attributes("style")).toContain("--alix-text-scale: 1.5");
    const inside = mount(
      { components: { AlixProvider, AlixTableBlock }, template: "<AlixProvider locale='it'><AlixTableBlock :block='block'/></AlixProvider>", data: () => ({ block: { type: "table", columns: [], rows: [] } }) },
    );
    expect(inside.text()).toContain("Nessuna riga.");
  });
});

describe("AlixChatPanel", () => {
  const client = () =>
    scriptedClient((input) => {
      if (input.messages.length === 0 && input.resume === undefined) {
        return [runStarted(), ev("MESSAGES_SNAPSHOT", { messages: [{ id: "m1", role: "assistant", content: "Welcome back" }] }), runFinished()];
      }
      return [
        runStarted(),
        ev("TEXT_MESSAGE_START", { messageId: "a1" }),
        ev("TEXT_MESSAGE_CONTENT", { messageId: "a1", delta: "Done." }),
        ev("TEXT_MESSAGE_END", { messageId: "a1" }),
        ev("CUSTOM", { name: "alixai.suggestions", value: { suggestions: [{ title: "Book another", message: "Book another visit" }] } }),
        runFinished(),
      ];
    });

  it("loads the history on mount behind a skeleton, then sends a message and offers the computed suggestions", async () => {
    const scripted = client();
    const wrapper = mount(AlixChatPanel, { props: { client: scripted, threadId: "ses_1" }, attachTo: document.body });
    await flushPromises();
    expect(wrapper.text()).toContain("Welcome back");
    await wrapper.find("textarea").setValue("hello");
    await wrapper.find("form").trigger("submit");
    await flushPromises();
    await flush();
    expect(scripted.inputs.map((input) => input.messages.length)).toEqual([0, 1]);
    expect(wrapper.text()).toContain("Done.");
    const suggestion = wrapper.findAll("button.alix-chip").find((button) => button.text() === "Book another");
    expect(suggestion).toBeDefined();
    await suggestion?.trigger("click");
    await flushPromises();
    expect(scripted.inputs[2]?.messages[0]).toMatchObject({ role: "user", content: "Book another visit" });
    wrapper.unmount();
  });

  it("reports a citation for the host to open and shows the offline banner", async () => {
    const scripted = scriptedClient(() => [
      runStarted(),
      ev("ACTIVITY_SNAPSHOT", { messageId: "act_c", activityType: "alixai.citations", content: { citations: [citation()] } }),
      runFinished(),
    ]);
    const wrapper = mount(AlixChatPanel, { props: { client: scripted, threadId: "ses_1" } });
    await flushPromises();
    const open = wrapper.find("button[aria-label='Open Cancellation policy']");
    expect(open.exists()).toBe(true);
    await open.trigger("click");
    expect(wrapper.emitted("open-citation")).toEqual([[expect.objectContaining({ source_id: "src_1" })]]);
    vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
    window.dispatchEvent(new Event("offline"));
    await nextTick();
    expect(wrapper.text()).toContain("You are offline");
    vi.restoreAllMocks();
  });

  it("refuses to be built without a client or a controller", () => {
    expect(() => mount(AlixChatPanel, { props: {} })).toThrow(/client/);
  });

  it("says it waits for the user only while the goal still needs them", async () => {
    const goal = (phase: string) => ({ alixai: { pending: { kind: "goal", capabilityId: "work_orders.list", phase, collectedFields: [], missingFields: [] } } });
    for (const [phase, waits] of [["success", false], ["failure", false], ["", true], ["prepared", true]] as const) {
      const scripted = scriptedClient(() => [runStarted(), ev("STATE_SNAPSHOT", { snapshot: goal(phase) }), runFinished()]);
      const wrapper = mount(AlixChatPanel, { props: { client: scripted, threadId: "ses_1" } });
      await flushPromises();
      expect(wrapper.text().includes("Waiting for you to finish"), `phase ${JSON.stringify(phase)}`).toBe(waits);
      wrapper.unmount();
    }
  });
});
