import { HttpAgent, type BaseEvent } from "@ag-ui/client";
import { ALIXAI_BLOCK_TYPES, parseAttachments, parseBlock, parseCitations, parseConfirmation, parseProgress, parseSuggestions } from "@alixai/copilotkit-adapter";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { blockTypeOfActivity, parseBlock as vueParseBlock, parseReceipt } from "../../vue/src/agui/parse";
import type { AguiEvent } from "../../vue/src/agui/types";
import { demoPrompts, matchScenario, type DemoLocale, type DemoScenarioId } from "../src/index";
import { expectSchema, kindsOf, runVue, SCHEMA, startMock, uid, userMessage, type RunningMock } from "./helpers";

// Every demo prompt, in English and Italian, routes to its own scenario and runs cleanly through both kits' AG-UI paths: the Vue
// kit's client and reducer, and @ag-ui/client's HttpAgent (the React kit's self-managed agent, which verifies the lifecycle and
// parses each event with the @ag-ui/core schemas). Every block, citation, directive and receipt is checked against the
// repository's schemas and read by the kits' parsers.

let mock: RunningMock;
beforeAll(async () => {
  mock = await startMock({ pace: 0 });
});
afterAll(async () => {
  await mock.close();
});

const LOCALES: readonly DemoLocale[] = ["en", "it"];

/** The timeline each scenario leaves in the Vue kit's state (the user's own message is the panel's, not the stream's). */
const EXPECTED: Record<Exclude<DemoScenarioId, "reconnect">, { kinds: string[]; outcome: "success" | "interrupt" | "error" }> = {
  help: { kinds: ["message"], outcome: "success" },
  markdown: { kinds: ["message"], outcome: "success" },
  chart_bar: { kinds: ["block:chart", "message"], outcome: "success" },
  chart_line: { kinds: ["block:chart", "message"], outcome: "success" },
  table: { kinds: ["block:table", "message"], outcome: "success" },
  diagram: { kinds: ["block:diagram", "message"], outcome: "success" },
  image: { kinds: ["attachments", "message"], outcome: "success" },
  citations: { kinds: ["citations", "message"], outcome: "success" },
  clarification: { kinds: ["block:clarification_choices", "message"], outcome: "success" },
  confirmation: { kinds: ["block:action_card", "block:confirmation_panel", "message", "confirmation"], outcome: "interrupt" },
  step_up: { kinds: ["continue", "block:confirmation_panel", "message"], outcome: "success" },
  progress: { kinds: ["receipt", "block:action_card", "message"], outcome: "success" },
  unavailable: { kinds: ["message", "error"], outcome: "error" },
  refusal: { kinds: ["message", "error"], outcome: "error" },
  long: { kinds: ["message", "block:table", "block:chart", "message", "block:chart", "block:chart", "citations", "message"], outcome: "success" },
  // memory_proposal is opt-in on the chat panel (`memory: true`); the default reducer ignores the CUSTOM event.
  memory_proposal: { kinds: ["message"], outcome: "success" },
};

/** Checks every AlixAI payload of a stream against the schemas and the kits' parsers. */
const checkPayloads = (events: readonly AguiEvent[]): void => {
  const activities = new Map<string, unknown>();
  for (const event of events) {
    if (event.type === "ACTIVITY_SNAPSHOT") {
      const type = event["activityType"] as string;
      const content = event["content"];
      activities.set(event["messageId"] as string, content);
      const blockType = blockTypeOfActivity(type);
      if (blockType !== undefined) {
        expectSchema(SCHEMA.richBlock, content);
        expect(vueParseBlock(blockType, content), type).toBeDefined();
        expect(parseBlock(blockType, content), type).toBeDefined();
        expect((ALIXAI_BLOCK_TYPES as readonly string[]).includes(blockType)).toBe(true);
      } else if (type === "alixai.citations") {
        expect(parseCitations(content)).toBeDefined();
        for (const citation of (content as { citations: unknown[] }).citations) expectSchema(SCHEMA.citation, citation);
      } else if (type === "alixai.attachments") {
        expect(parseAttachments(content)).toBeDefined();
      } else if (type === "alixai.progress") {
        expect(parseProgress(content)).toBeDefined();
      } else if (type === "alixai.receipt") {
        expectSchema(SCHEMA.receipt, content);
        expect(parseReceipt(content)).toBeDefined();
      } else {
        throw new Error(`unexpected activity ${type}`);
      }
    } else if (event.type === "ACTIVITY_DELTA") {
      expect(activities.has(event["messageId"] as string)).toBe(true);
      for (const op of event["patch"] as { op: string; path: string; value: unknown }[]) {
        if (op.path === "/citations/-") expectSchema(SCHEMA.citation, op.value);
      }
    } else if (event.type === "CUSTOM" && event["name"] === "alixai.directive") {
      expectSchema(SCHEMA.directive, event["value"]);
    } else if (event.type === "CUSTOM" && event["name"] === "alixai.suggestions") {
      expect(parseSuggestions(event["value"]).length).toBeGreaterThan(0);
    } else if (event.type === "TOOL_CALL_ARGS") {
      JSON.parse(event["delta"] as string);
    } else if (event.type === "TOOL_CALL_RESULT") {
      const content = JSON.parse(event["content"] as string) as Record<string, unknown>;
      if (content["receipt_id"] !== undefined) expectSchema(SCHEMA.receipt, content);
    } else if (event.type === "RUN_FINISHED") {
      const outcome = event["outcome"] as { type: string; interrupts?: unknown[] };
      for (const interrupt of outcome.interrupts ?? []) expect(parseConfirmation(interrupt as never)).toBeDefined();
    } else if (event.type === "TEXT_MESSAGE_CONTENT") {
      expect((event["delta"] as string).length).toBeGreaterThan(0);
    }
  }
};

describe("the prompt registry", () => {
  it("routes every demo prompt to its own scenario, in English and in Italian", () => {
    for (const prompt of demoPrompts) {
      for (const locale of LOCALES) {
        const match = matchScenario(prompt.prompt[locale]);
        expect(match.id, `${prompt.id} (${locale}): "${prompt.prompt[locale]}"`).toBe(prompt.id);
        if (match.locale !== undefined) expect(match.locale, `${prompt.id} (${locale})`).toBe(locale);
      }
    }
  });

  it("has a title, a prompt and a caption in both languages for every scenario", () => {
    for (const prompt of demoPrompts) for (const locale of LOCALES) for (const text of [prompt.title, prompt.prompt, prompt.shows]) expect(text[locale].length).toBeGreaterThan(2);
    expect(new Set(demoPrompts.map((prompt) => prompt.id)).size).toBe(demoPrompts.length);
  });
});

describe("every scenario through the Vue kit's client and reducer", () => {
  for (const prompt of demoPrompts) {
    if (prompt.id === "reconnect") continue;
    const expected = EXPECTED[prompt.id as Exclude<DemoScenarioId, "reconnect">];
    for (const locale of LOCALES) {
      it(`${prompt.id} (${locale})`, async () => {
        const threadId = uid("ses_vue");
        const run = await runVue(mock.base, { threadId, messages: [userMessage(prompt.prompt[locale])], forwardedProps: { alixai: { language: locale } } });
        expect(run.result.terminal).toBe(true);
        expect(run.reconnects).toBe(0);
        expect(run.state.outcome).toBe(expected.outcome);
        expect(kindsOf(run.state.items)).toEqual(expected.kinds);
        // The pending goal is read from the state the stream shares.
        expect(run.state.shared).toHaveProperty("alixai");
        // Every event group but the leftovers carries a cursor, and the cursors only grow.
        const numeric = run.cursors.filter((cursor): cursor is string => cursor !== undefined).map(Number);
        expect(numeric).toEqual([...numeric].sort((a, b) => a - b));
        checkPayloads(run.events);
        const text = run.state.items.filter((item) => item.kind === "message").map((item) => (item as { text: string }).text).join("\n");
        expect(text.length).toBeGreaterThan(20);
        if (locale === "it") expect(text).toMatch(/[àèéìòù]|\b(il|la|di|che|per)\b/);
      });
    }
  }
});

describe("every scenario through @ag-ui/client's HttpAgent (the React kit's agent)", () => {
  for (const prompt of demoPrompts) {
    if (prompt.id === "reconnect") continue;
    for (const locale of LOCALES) {
      it(`${prompt.id} (${locale})`, async () => {
        const agent = new HttpAgent({ url: `${mock.base}/v1/agui`, threadId: uid("ses_react") });
        agent.addMessage({ id: uid("msg"), role: "user", content: prompt.prompt[locale] });
        const events: BaseEvent[] = [];
        let failure: unknown;
        try {
          await agent.runAgent({}, { onEvent: ({ event }) => void events.push(event) });
        } catch (error) {
          failure = error;
        }
        const types = events.map((event) => event.type as string);
        expect(types[0]).toBe("RUN_STARTED");
        const expected = EXPECTED[prompt.id as Exclude<DemoScenarioId, "reconnect">];
        if (expected.outcome === "error") {
          expect(types[types.length - 1]).toBe("RUN_ERROR");
        } else {
          expect(failure).toBeUndefined();
          expect(types[types.length - 1]).toBe("RUN_FINISHED");
          // CopilotKit renders the assistant text from the agent's messages and the blocks from its activity messages.
          expect(agent.messages.some((message) => message.role === "assistant" && typeof message.content === "string" && message.content.length > 10)).toBe(true);
          expect((agent.state as { alixai?: unknown }).alixai).toBeDefined();
        }
        checkPayloads(events as unknown as AguiEvent[]);
      });
    }
  }
});
