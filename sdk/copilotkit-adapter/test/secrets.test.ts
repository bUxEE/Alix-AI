import { inspect } from "node:util";
import { describe, expect, it } from "vitest";
import { createMemorySecretStore } from "../src/secrets.js";

const key = { threadId: "ses_1", preparationId: "prep_1" };

describe("the in-memory confirmation secret store", () => {
  it("keeps a secret per thread and preparation and deletes it on use", async () => {
    const store = createMemorySecretStore();
    await store.put(key, "cfm_a");
    await store.put({ threadId: "ses_2", preparationId: "prep_1" }, "cfm_b");
    expect(await store.get(key)).toBe("cfm_a");
    expect(await store.get({ threadId: "ses_2", preparationId: "prep_1" })).toBe("cfm_b");
    expect(await store.get({ threadId: "ses_1", preparationId: "prep_2" })).toBeUndefined();
    await store.delete(key);
    expect(await store.get(key)).toBeUndefined();
  });

  it("never lets two keys collide through the way they are joined", async () => {
    const store = createMemorySecretStore();
    await store.put({ threadId: "a", preparationId: "bc" }, "one");
    await store.put({ threadId: "ab", preparationId: "c" }, "two");
    expect(await store.get({ threadId: "a", preparationId: "bc" })).toBe("one");
    expect(await store.get({ threadId: "ab", preparationId: "c" })).toBe("two");
  });

  it("forgets a secret after its time limit and drops the oldest beyond its size limit", async () => {
    let now = 1000;
    const store = createMemorySecretStore({ ttlMs: 100, maxEntries: 2, now: () => now });
    await store.put(key, "x");
    now += 101;
    expect(await store.get(key)).toBeUndefined();
    await store.put({ threadId: "t", preparationId: "1" }, "1");
    await store.put({ threadId: "t", preparationId: "2" }, "2");
    await store.put({ threadId: "t", preparationId: "3" }, "3");
    expect(await store.get({ threadId: "t", preparationId: "1" })).toBeUndefined();
    expect(await store.get({ threadId: "t", preparationId: "3" })).toBe("3");
  });

  it("prints and serialises without the secrets", async () => {
    const store = createMemorySecretStore();
    await store.put(key, "cfm_do_not_print");
    expect(JSON.stringify(store)).not.toContain("cfm_do_not_print");
    expect(inspect(store, { depth: 5, showHidden: true })).not.toContain("cfm_do_not_print");
    expect(String(store)).not.toContain("cfm_do_not_print");
  });
});
