import { describe, expect, it } from "vitest";
import { createClient } from "../src/client.js";
import { privateLlmGuards } from "../src/ops/client.js";
import { createFakeServer } from "../src/testing/fake-server.js";

describe("private LLM 1.4 compatibility", () => {
  it("accepts a server response with extra members", () => {
    const body = {
      endpoint_id: "ple_01J0000000000000000000000A",
      origin: "http://127.0.0.1:8080",
      api_dialect: "openai-compatible",
      model_ids: ["local-model"],
      roles: ["conversational_controller"],
      owner_kind: "shared",
      lifecycle: "ready",
      updated_at: "2026-10-27T12:00:00Z",
      future_optional: "ok",
    };
    expect(privateLlmGuards.isPrivateLlmEndpoint(body)).toBe(true);
    expect(
      privateLlmGuards.isPrivateLlmTenantPin({
        tenant_id: "ten-1",
        endpoint_id: body.endpoint_id,
        updated_at: body.updated_at,
        extra: 1,
      }),
    ).toBe(true);
    expect(
      privateLlmGuards.isPrivateLlmPrincipalBinding({
        principal_id: "usr_alice",
        endpoint_id: body.endpoint_id,
        roles: ["conversational_controller"],
        updated_at: body.updated_at,
        note: "x",
      }),
    ).toBe(true);
  });

  it("accepts a 1.4 Record body at runtime", async () => {
    const fake = createFakeServer();
    const client = createClient({
      baseUrl: fake.baseUrl,
      fetch: fake.fetch,
      getContextToken: () => fake.issueToken({ session_id: "ses-1" }),
    });
    const body: Record<string, unknown> = {
      origin: "http://127.0.0.1:8080",
      api_dialect: "openai-compatible",
      model_ids: ["local-model"],
      roles: ["conversational_controller"],
      owner_kind: "shared",
    };
    const ep = await client.ops.privateLlm.putEndpoint("ple_01J0000000000000000000000A", body);
    expect(ep.endpoint_id).toBe("ple_01J0000000000000000000000A");
  });
});

/** Type-level: a 1.4-shaped Record call must type-check (tsc --noEmit). */
export const _privateLlmV14BodyTypecheck = async () => {
  const fake = createFakeServer();
  const client = createClient({
    baseUrl: fake.baseUrl,
    fetch: fake.fetch,
    getContextToken: () => fake.issueToken({ session_id: "ses-1" }),
  });
  const body: Record<string, unknown> = {
    origin: "http://127.0.0.1:8080",
    api_dialect: "openai-compatible",
    model_ids: ["m"],
    roles: ["conversational_controller"],
    owner_kind: "shared",
  };
  await client.ops.privateLlm.putEndpoint("ple_01J0000000000000000000000A", body);
  await client.ops.privateLlm.putTenantPin("ten-1", { endpoint_id: "ple_01J0000000000000000000000A" } as Record<string, unknown>);
  await client.ops.privateLlm.putPrincipalBinding("usr_a", {
    endpoint_id: "ple_01J0000000000000000000000A",
    roles: ["conversational_controller"],
  } as Record<string, unknown>);
};
