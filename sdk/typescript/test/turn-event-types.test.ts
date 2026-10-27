import { readFileSync } from "node:fs";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

import { TURN_EVENT_TYPES } from "../src/index.js";
import { REPO_ROOT } from "./helpers/repo.js";

describe("TURN_EVENT_TYPES", () => {
  it("equals the TurnEvent type set of schemas/conversation/turn-event", () => {
    const schema = JSON.parse(readFileSync(join(REPO_ROOT, "schemas", "conversation", "turn-event.schema.json"), "utf8")) as {
      oneOf: Array<{ $ref: string }>;
      $defs: Record<string, { properties: { type: { const: string } } }>;
    };
    const want = schema.oneOf.map((item) => {
      const name = item.$ref.replace("#/$defs/", "");
      return schema.$defs[name]!.properties.type.const;
    });
    expect([...TURN_EVENT_TYPES]).toEqual(want);
  });
});
