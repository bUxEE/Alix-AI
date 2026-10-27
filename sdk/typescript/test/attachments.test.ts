import { describe, expect, it } from "vitest";

import { createClient, isAlixAIError } from "../src/index.js";
import type { AlixAIError, FileUpload } from "../src/index.js";
import { reply } from "../src/testing/index.js";
import { createHost } from "./helpers/host.js";

// Files in a turn (M3-20 gateway, M3-29 SDK): `TurnRequest.attachments` names up to four uploaded file ids. The SDK checks the list before
// a context token is spent, the fake resolves the ids under the scope before any handler runs, and every reference that does not resolve
// for the caller is the same `scope_violation`.

const caught = async (promise: Promise<unknown>): Promise<AlixAIError> => {
  try {
    await promise;
  } catch (error) {
    expect(isAlixAIError(error), `not an AlixAIError: ${String(error)}`).toBe(true);
    return error as AlixAIError;
  }
  throw new Error("expected a failure");
};

const PNG = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3, 4]);
const png = (name = "a.png"): FileUpload => ({ data: PNG, name, mimeType: "image/png" });
const UNKNOWN = "fil_0123456789ABCDEFGHJKMNPQRS";

type Host = ReturnType<typeof createHost>;

/** A client of the same fake for another caller: another principal, tenant or session. */
const asOther = (host: Host, claims: Parameters<Host["fake"]["issueToken"]>[0]) =>
  createClient({ baseUrl: host.fake.baseUrl, fetch: host.fake.fetch, getContextToken: () => host.fake.issueToken(claims) });

describe("sendTurn with attachments", () => {
  it("sends the file ids with the text, and the turn handler sees them in order", async () => {
    const host = createHost();
    const seen: unknown[] = [];
    host.fake.onTurn((turn) => {
      seen.push(turn.input);
      return reply("ok");
    });
    const first = await host.client.files.upload(png("one.png"));
    const second = await host.client.files.upload(png("two.png"));
    const result = await host.client.sendTurn("ses-1", { text: "read these", language: "en", attachments: [second.file_id, first.file_id] }).result();
    expect(result.events.at(-1)).toMatchObject({ type: "done", finish_reason: "completed" });
    expect(host.fake.requests.at(-1)?.body).toMatchObject({ text: "read these", language: "en", attachments: [second.file_id, first.file_id], client_capabilities: expect.objectContaining({ contract: "1.5" }) });
    expect(seen).toEqual([{ text: "read these", attachments: [second.file_id, first.file_id] }]);
  });

  it("an empty list is left out of the request, and sendTurnJson takes the same member", async () => {
    const host = createHost();
    await host.client.sendTurn("ses-1", { text: "hi", attachments: [] }).result();
    expect(host.fake.requests.at(-1)?.body).toMatchObject({ text: "hi", client_capabilities: expect.objectContaining({ contract: "1.5" }) });
    const ref = await host.client.files.upload(png());
    const turn = await host.client.sendTurnJson("ses-1", { text: "hi", attachments: [ref.file_id] });
    expect(turn.events.at(-1)?.type).toBe("done");
    expect(host.fake.requests.at(-1)?.body).toMatchObject({ text: "hi", attachments: [ref.file_id], client_capabilities: expect.objectContaining({ contract: "1.5" }) });
  });

  it("checks the list before a context token is spent", async () => {
    const host = createHost();
    const ref = await host.client.files.upload(png());
    host.tokenRequests.length = 0;
    const ok = ref.file_id;
    const attempts: Array<[string, unknown]> = [
      ["more than four", { text: "x", attachments: [ok, ok, ok, ok, ok] }],
      ["not a file id", { text: "x", attachments: ["../../etc/passwd"] }],
      ["a number", { text: "x", attachments: [7] }],
      ["not a list", { text: "x", attachments: ok }],
      ["with a choice", { choice_id: "c1", attachments: [ok] }],
      ["with a confirmation", { confirmation: { preparation_id: "prep_1", token: "cfm_" + "A".repeat(43) }, attachments: [ok] }],
      ["with a rejection", { reject: { preparation_id: "prep_1" }, attachments: [ok] }],
    ];
    for (const [label, input] of attempts) {
      expect((await caught(host.client.sendTurn("ses-1", input as never).result())).code, label).toBe("validation_failed");
      expect((await caught(host.client.sendTurnJson("ses-1", input as never))).code, label).toBe("validation_failed");
    }
    expect(host.tokenRequests).toHaveLength(0);
    expect(host.fake.requests.filter((r) => r.operation === "sendTurn")).toHaveLength(0);
  });

  it("four files, and a file named twice, are accepted", async () => {
    const host = createHost();
    const ids: string[] = [];
    for (let i = 0; i < 4; i += 1) ids.push((await host.client.files.upload(png(i + ".png"))).file_id);
    await host.client.sendTurn("ses-1", { text: "all", attachments: ids }).result();
    await host.client.sendTurn("ses-1", { text: "twice", attachments: [ids[0]!, ids[0]!] }).result();
  });
});

describe("a reference that does not resolve is one scope_violation, with no turn run", () => {
  const refused = async (setup: (host: Host) => Promise<string>, label: string) => {
    const host = createHost();
    let handled = 0;
    host.fake.onTurn(() => {
      handled += 1;
      return reply("ok");
    });
    const id = await setup(host);
    const before = (await host.client.getSession("ses-1")).turn_count;
    const error = await caught(host.client.sendTurn("ses-1", { text: "x", attachments: [id] }).result());
    expect(error.code, label).toBe("scope_violation");
    expect(error.status, label).toBe(403);
    expect(handled, label).toBe(0);
    expect((await host.client.getSession("ses-1")).turn_count, label).toBe(before);
    return error;
  };

  it("another principal's file, another tenant's, another session's, a deleted one, an expired one and one that never existed look the same", async () => {
    const errors = [
      await refused(async (host) => (await asOther(host, { session_id: "ses-1", principal_id: "user-2" }).files.upload(png())).file_id, "another principal"),
      await refused(async (host) => (await asOther(host, { session_id: "ses-1", tenant_id: "tenant-2" }).files.upload(png())).file_id, "another tenant"),
      await refused(async (host) => (await asOther(host, { session_id: "ses-9" }).files.upload(png())).file_id, "another session"),
      await refused(async (host) => {
        const ref = await host.client.files.upload(png());
        await host.client.files.delete(ref.file_id);
        return ref.file_id;
      }, "deleted"),
      await refused(async (host) => {
        const ref = await host.client.files.upload(png());
        host.fake.advanceTime(60 * 60 * 1000);
        return ref.file_id;
      }, "expired"),
      await refused(async () => UNKNOWN, "unknown"),
    ];
    // Nothing tells the cases apart: same status, same message, no reason, no level.
    const shapes = new Set(errors.map((e) => JSON.stringify([e.code, e.status, e.message, e.details?.["reason"], e.details?.["levels"]])));
    expect(shapes.size).toBe(1);
  });

  it("one bad reference among good ones refuses the whole turn", async () => {
    const host = createHost();
    let handled = 0;
    host.fake.onTurn(() => {
      handled += 1;
      return reply("ok");
    });
    const good = await host.client.files.upload(png());
    const error = await caught(host.client.sendTurn("ses-1", { text: "x", attachments: [good.file_id, UNKNOWN] }).result());
    expect(error.code).toBe("scope_violation");
    expect(handled).toBe(0);
  });
});

describe("sendMessage names the files it uploaded", () => {
  it("one turn, with the ids of the uploads in order, and the files resolve for the turn", async () => {
    const host = createHost();
    const seen: unknown[] = [];
    host.fake.onTurn((turn) => {
      seen.push(turn.input);
      return reply("ok");
    });
    const stream = host.client.sendMessage("ses-1", { text: "look", attachments: [png("a.png"), png("b.png")] });
    await stream.result();
    const refs = await stream.attachments;
    expect(seen).toEqual([{ text: "look", attachments: refs.map((r) => r.file_id) }]);
    expect(host.fake.requests.filter((r) => r.operation === "sendTurn")).toHaveLength(1);
  });

  it("when the core refuses the turn the typed error surfaces and the references are still reported", async () => {
    const host = createHost();
    host.fake.failNext({ operation: "sendTurn", status: 403, error: { code: "scope_violation", message: "the file reference does not resolve for this caller", retryable: false } });
    const stream = host.client.sendMessage("ses-1", { text: "x", attachments: [png()] });
    expect((await caught(stream.result())).code).toBe("scope_violation");
    expect(await stream.attachments).toHaveLength(1);
  });
});
