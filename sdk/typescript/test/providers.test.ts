import { inspect } from "node:util";

import { describe, expect, it } from "vitest";

import * as sdk from "../src/index.js";
import { isAlixAIError } from "../src/index.js";
import type { AlixAIError } from "../src/index.js";
import type * as P from "../src/providers/index.js";
import { describe as explain, loadSpec } from "./helpers/openapi.js";

// The providers namespace of the SDK (task X-14): its operation table against schemas/api/paths/providers.yaml and oauth.yaml, the client against
// the in-process fake with every answer validated against the document, and the rules that make this area what it is: operator-only, off by default,
// write-only credentials, fail-closed.

const { providers } = sdk;

const failure = async (call: Promise<unknown>): Promise<AlixAIError> => {
  try {
    await call;
  } catch (error) {
    if (isAlixAIError(error)) return error;
    throw error;
  }
  throw new Error("the call did not fail");
};

const KEY = "sk-test-0123456789abcdef-NEVER-ECHO";

const setup = (options: P.FakeProvidersServerOptions = {}) => {
  const fake = providers.createFakeProvidersServer({ config: { ...providers.defaultProvidersConfig(), tenantAdmins: ["admin-2"] }, ...options });
  const tokens: Array<{ operation: string; provider?: string }> = [];
  const client = providers.createProvidersClient({
    baseUrl: fake.baseUrl,
    fetch: fake.fetch,
    getContextToken: (request) => {
      tokens.push(request);
      return fake.issueToken();
    },
  });
  return { fake, client, tokens };
};

const clientOf = (fake: P.FakeProvidersServer, claims: P.FakeProvidersClaims) =>
  providers.createProvidersClient({ baseUrl: fake.baseUrl, fetch: fake.fetch, getContextToken: () => fake.issueToken(claims) });

describe("the operation table", () => {
  it("matches the OpenAPI document, which this SDK speaks in full for the providers and oauth areas", () => {
    const spec = loadSpec();
    const documented = spec.operations.filter((o) => o.area === "providers" || o.area === "oauth");
    expect(Object.keys(providers.PROVIDERS_OPERATIONS).sort()).toEqual(documented.map((o) => o.operationId).sort());
    expect(documented).toHaveLength(16);
    for (const o of documented) {
      const row = providers.PROVIDERS_OPERATIONS[o.operationId as P.ProvidersOperationId];
      expect({ method: row.method, path: row.path, safe: row.safe, authenticated: o.authenticated }, o.operationId).toEqual({
        method: o.method,
        path: o.path,
        safe: o.method === "GET",
        authenticated: true,
      });
    }
  });

  it("names the methods after the operations, the same in every SDK", () => {
    const { client } = setup();
    expect(Object.keys(client).sort()).toEqual(
      [
        "list", "submitCredential", "disconnect", "acknowledgeBillingNotice", "getDisclosure", "getMode", "setMode", "getEgressPolicy", "setEgressPolicy",
        "getBudgets", "setBudget", "exportUsage", "getKillSwitch", "setKillSwitch", "startOAuth", "completeOAuth",
      ].sort(),
    );
  });
});

describe("every operation against the fake, every answer validated against the document", () => {
  it("drives all sixteen and checks the status and the body of each", async () => {
    const spec = loadSpec();
    const fake = providers.createFakeProvidersServer({ config: { ...providers.defaultProvidersConfig(), tenantAdmins: ["admin-2"] } });
    const violations: string[] = [];
    const checked = new Set<string>();
    const checkingFetch: typeof fake.fetch = async (input, init) => {
      const before = fake.requests.length;
      const response = await fake.fetch(input, init);
      const recorded = fake.requests[before];
      const operation = recorded?.operation ?? "";
      checked.add(operation);
      if (!spec.declaredStatuses(operation).includes(response.status)) violations.push(`${operation}: undeclared status ${response.status}`);
      const validate = spec.responseValidator(operation, response.status);
      if (validate === undefined) violations.push(`${operation} ${response.status}: no schema`);
      else if (!validate(await response.clone().json())) violations.push(`${operation} ${response.status}: ${explain(validate)}`);
      return response;
    };
    const operator = providers.createProvidersClient({ baseUrl: fake.baseUrl, fetch: checkingFetch, getContextToken: () => fake.issueToken() });

    await operator.list();
    await operator.acknowledgeBillingNotice("openai", { notice_version: "2026-10-02.1", consumer_subscription_is_not_api_access: true, separate_api_billing_understood: true });
    await failure(operator.acknowledgeBillingNotice("openai", { notice_version: "old", consumer_subscription_is_not_api_access: true, separate_api_billing_understood: true }));
    await operator.submitCredential("acme", { key: providers.providerSecret(KEY), label: "main", scopes: ["model.request"] });
    await failure(operator.submitCredential("acme", { key: "bad-key-value" }));
    await operator.disconnect("acme");
    const started = await operator.startOAuth("openai", { capabilities: ["chat"], redirect_uri: "http://127.0.0.1:8123/callback" });
    expect(isManualFallbackOf(await failure(operator.startOAuth("acme", { capabilities: ["chat"], redirect_uri: "http://127.0.0.1:8123/callback" })))).toBe(true);
    const state = new URL(started.authorization_url).searchParams.get("state");
    await operator.completeOAuth({ redirect_uri: "http://127.0.0.1:8123/callback", query: `?code=c0de&state=${state}` });
    await failure(operator.completeOAuth({ redirect_uri: "http://127.0.0.1:8123/callback", query: `code=c0de&state=${state}` }));
    await operator.getMode();
    await operator.setMode("deployment", "external");
    await operator.getDisclosure("openai", { source: ["src_a"], estimated_toks_in: 100 });
    await operator.getEgressPolicy("deployment");
    await operator.setEgressPolicy({
      level: "deployment", expected_revision: 0, enabled: true, source_mode: "local_only", allowed_providers: ["openai"],
      allowed_models: [{ provider_id: "openai", model_id: "gpt-4.1-mini" }], allowed_classes: ["public"], allowed_regions: ["us"],
      files: { enabled: true, max_class: "internal" }, audio: { enabled: false, max_class: "public" },
    });
    await failure(operator.setEgressPolicy({ level: "deployment", expected_revision: 0, enabled: true, source_mode: "local_only" }));
    const created = await operator.setBudget({
      subject: { kind: "tenant", application_id: "fake-app", tenant_id: "tenant-1" }, channel: "interactive", currency: "EUR",
      limits: { daily_micro_units: 5_000_000, monthly_micro_units: 100_000_000 }, warn_at_percent: 80,
    });
    expect([created.revision, created.budget_id.startsWith("bud_")]).toEqual([1, true]);
    await operator.setBudget({ ...created, limits: { daily_micro_units: 9_000_000, monthly_micro_units: 100_000_000 } });
    await failure(operator.setBudget(created));
    await operator.getBudgets();
    await operator.exportUsage({ provider: "openai", scope: "application" });
    await operator.getKillSwitch();
    await operator.setKillSwitch(true, "incident");
    await operator.setKillSwitch(false);
    await failure(clientOf(fake, { principalId: "nobody" }).list());
    await failure(clientOf(fake, { principalId: "admin-2" }).getKillSwitch());
    fake.setUnavailable();
    await failure(operator.list());

    expect(violations).toEqual([]);
    for (const operation of Object.keys(providers.PROVIDERS_OPERATIONS)) expect(checked.has(operation), `${operation} was never called`).toBe(true);
  });
});

const isManualFallbackOf = (error: AlixAIError): boolean => providers.isManualFallback(error);

describe("the credential is write-only", () => {
  it("prints as a placeholder everywhere, and the fake keeps no copy", async () => {
    const secret = providers.providerSecret(KEY);
    const shown = [String(secret), `${secret}`, JSON.stringify({ key: secret }), inspect(secret), inspect({ nested: { key: secret } }, { depth: 5 }), JSON.stringify(secret)].join("|");
    expect(shown).not.toContain(KEY);
    expect(shown).toContain("[redacted]");

    const { fake, client } = setup();
    const status = await client.submitCredential("acme", { key: secret, label: "main", expires_at: "2027-01-01T00:00:00Z", scopes: ["model.request"] });
    expect(status).toMatchObject({ state: "connected", kind: "api_key", account: { organization_id: "org-fake" } });
    expect(JSON.stringify(status)).not.toContain(KEY);
    expect(JSON.stringify(fake.requests)).not.toContain(KEY);
    expect(fake.requests.at(-1)).toMatchObject({ operation: "putProviderCredential", method: "PUT", path: "/v1/providers/acme/credential", query: "" });
    const list = await client.list();
    expect(JSON.stringify(list)).not.toContain(KEY);
  });

  it("a plain string key works, a refused key is a fixed message, a broad key is quarantined", async () => {
    const { client } = setup();
    expect(await client.submitCredential("acme", { key: KEY })).toMatchObject({ state: "connected" });
    const refused = await failure(client.submitCredential("acme", { key: `bad-${KEY}` }));
    expect([refused.code, refused.message.includes(KEY), JSON.stringify(refused.details ?? {}).includes(KEY)]).toEqual(["validation_failed", false, false]);
    expect(await client.submitCredential("acme", { key: `broad-${KEY}` })).toMatchObject({ state: "quarantined", warnings: ["excessive_permissions_reported"] });
    expect(await client.disconnect("acme")).toMatchObject({ state: "not_connected", kind: null });
  });

  it("refuses an answer that repeats the key, and strips the key from an error that quotes it", async () => {
    const echoing = providers.createProvidersClient({
      baseUrl: "https://alixai.test",
      getContextToken: () => "tok",
      fetch: async (_url, init) => {
        const key = (JSON.parse(String(init?.body)) as { key: string }).key;
        const status = {
          as_of: "2026-10-02T09:00:00Z", provider: { profile_id: "pprf_01J0000000000000000000000P", provider_id: "acme", model_id: "m", model_version: "1" },
          lifecycle: "qualified", state: "connected", kind: "api_key", account: { account_id: null, organization_id: key, project_id: null },
          connected_at: "2026-10-02T08:00:05Z", expires_at: null, warnings: [], health: "healthy", last_error_kind: null, budget_state: null, billing_notice_acknowledged: true,
        };
        return new Response(JSON.stringify(status), { status: 200, headers: { "Content-Type": "application/json" } });
      },
    });
    const echoed = await failure(echoing.submitCredential("acme", { key: KEY }));
    expect([echoed.code, echoed.details?.["reason"], JSON.stringify(echoed).includes(KEY), echoed.message.includes(KEY)]).toEqual(["internal", "echoed_secret", false, false]);

    const quoting = providers.createProvidersClient({
      baseUrl: "https://alixai.test",
      getContextToken: () => "tok",
      fetch: async (_url, init) => {
        const key = (JSON.parse(String(init?.body)) as { key: string }).key;
        return new Response(JSON.stringify({ code: "validation_failed", message: `the key ${key} is bad`, retryable: false, details: { key } }), {
          status: 400,
          headers: { "Content-Type": "application/json" },
        });
      },
    });
    const quoted = await failure(quoting.submitCredential("acme", { key: KEY }));
    expect([quoted.code, quoted.message.includes(KEY), JSON.stringify(quoted.details ?? {}).includes(KEY), quoted.status]).toEqual(["validation_failed", false, false, 400]);
  });

  it("refuses a malformed key locally, before a token is spent, without naming it", async () => {
    const { client, tokens, fake } = setup();
    for (const key of ["", "a\nb", ` ${KEY}`, "x".repeat(4097), 42 as unknown as string]) {
      const error = await failure(client.submitCredential("acme", { key }));
      expect([error.code, String(error.message).includes(KEY)]).toEqual(["validation_failed", false]);
    }
    expect(tokens).toEqual([]);
    expect(fake.requests).toEqual([]);
  });

  it("a failed PUT is an uncertain outcome, a failed GET is unavailable", async () => {
    const down = providers.createProvidersClient({
      baseUrl: "https://alixai.test",
      getContextToken: () => "tok",
      fetch: async () => {
        throw new TypeError("connection reset");
      },
    });
    expect((await failure(down.submitCredential("acme", { key: KEY }))).code).toBe("uncertain_outcome");
    expect((await failure(down.list())).code).toBe("unavailable");
  });
});

describe("External Provider Mode is off by default and operator-only", () => {
  it("answers unavailable without the service, never an empty result", async () => {
    const fake = providers.createFakeProvidersServer();
    const client = providers.createProvidersClient({ baseUrl: fake.baseUrl, fetch: fake.fetch, getContextToken: fake.getContextToken });
    for (const call of [() => client.list(), () => client.getMode(), () => client.getKillSwitch(), () => client.startOAuth("openai", { capabilities: ["chat"], redirect_uri: "http://127.0.0.1/cb" })]) {
      const error = await failure(call());
      expect([error.code, error.retryable, error.status]).toEqual(["unavailable", true, 503]);
    }
    fake.configure({ deploymentOperators: ["principal-1"] });
    expect((await client.list()).providers).toHaveLength(2);
  });

  it("decides the operator from the configuration, before the body is read, and the level decides which one", async () => {
    const { fake } = setup();
    expect((await failure(clientOf(fake, { principalId: "nobody" }).list())).code).toBe("permission_denied");
    expect((await failure(clientOf(fake, { principalId: "nobody" }).submitCredential("acme", { key: KEY }))).code).toBe("permission_denied");
    const admin = clientOf(fake, { principalId: "admin-2" });
    expect((await admin.list()).providers).toHaveLength(2);
    expect((await admin.setMode("tenant", "hybrid")).effective).toBe("hybrid");
    for (const call of [
      () => admin.setMode("deployment", "external"),
      () => admin.setMode("application", "external"),
      () => admin.getKillSwitch(),
      () => admin.setKillSwitch(true, "incident"),
      () => admin.exportUsage({ scope: "application" }),
      () => admin.setEgressPolicy({ level: "application", expected_revision: 0, enabled: true, source_mode: "local_only" }),
    ]) {
      expect((await failure(call())).code).toBe("permission_denied");
    }
  });

  it("refuses a token twice", async () => {
    const { fake } = setup();
    const token = fake.issueToken();
    const client = providers.createProvidersClient({ baseUrl: fake.baseUrl, fetch: fake.fetch, getContextToken: () => token });
    await client.list();
    expect((await failure(client.list())).code).toBe("replayed");
  });

  it("validates every argument before a token is minted", async () => {
    const { client, tokens, fake } = setup();
    const bad: Array<[string, () => Promise<unknown>]> = [
      ["a path-shaped provider id", () => client.disconnect("../etc")],
      ["an unknown mode", () => client.setMode("tenant", "turbo" as "local")],
      ["an unknown level", () => client.setMode("global" as "tenant", "local")],
      ["a source level with no id", () => client.getEgressPolicy("source")],
      ["an id for a tenant level", () => client.getEgressPolicy("tenant", "src_1")],
      ["an unconfirmed notice", () => client.acknowledgeBillingNotice("acme", { notice_version: "1", consumer_subscription_is_not_api_access: false as true, separate_api_billing_understood: true })],
      ["a negative estimate", () => client.getDisclosure("acme", { estimated_toks_in: -1 })],
      ["a float estimate", () => client.getDisclosure("acme", { max_output_toks: 1.5 })],
      ["a free-text kill reason", () => client.setKillSwitch(true, "because I said so")],
      ["no capabilities", () => client.startOAuth("openai", { capabilities: [], redirect_uri: "http://127.0.0.1/cb" })],
      ["an empty callback query", () => client.completeOAuth({ redirect_uri: "http://127.0.0.1/cb", query: "?" })],
      ["a float limit", () => client.setBudget({ subject: { kind: "provider", provider_id: "openai" }, channel: "interactive", currency: "EUR", limits: { daily_micro_units: 1.5, monthly_micro_units: 1 }, warn_at_percent: 80 })],
      ["a budget that exists with no revision", () => client.setBudget({ budget_id: "bud_01J0000000000000000000000B", subject: { kind: "provider", provider_id: "openai" }, channel: "interactive", currency: "EUR", limits: { daily_micro_units: 1, monthly_micro_units: 1 }, warn_at_percent: 80 })],
      ["a usage scope", () => client.exportUsage({ scope: "world" as "tenant" })],
      ["a restricted files class", () => client.setEgressPolicy({ level: "tenant", expected_revision: 0, enabled: true, source_mode: "local_only", files: { enabled: true, max_class: "restricted" as "public" } })],
      ["an audio setting with no enabled", () => client.setEgressPolicy({ level: "tenant", expected_revision: 0, enabled: true, source_mode: "local_only", audio: { max_class: "public" } as P.EgressPolicyContentEgress })],
      ["a files setting with another member", () => client.setEgressPolicy({ level: "tenant", expected_revision: 0, enabled: true, source_mode: "local_only", files: { enabled: true, max_class: "public", max_bytes: 1 } as P.EgressPolicyContentEgress })],
    ];
    for (const [name, call] of bad) expect([name, (await failure(call())).code]).toEqual([name, "validation_failed"]);
    expect(tokens).toEqual([]);
    expect(fake.requests).toEqual([]);
  });

  it("hands the token provider the operation and the provider the path names", async () => {
    const { client, tokens } = setup();
    await client.getMode();
    await client.disconnect("acme");
    expect(tokens).toEqual([{ operation: "getProviderMode" }, { operation: "deleteProviderConnection", provider: "acme" }]);
  });
});

describe("modes, kill switch, disclosure, policy, budgets, OAuth", () => {
  it("local is the default, the most restrictive level decides, the kill switch wins", async () => {
    const { client } = setup();
    expect(await client.getMode()).toEqual({ effective: "local", configured: "local", decided_by: "default", kill_switch: false, levels: {} });
    await client.setMode("deployment", "external");
    const mode = await client.setMode("tenant", "hybrid");
    expect(mode).toMatchObject({ effective: "hybrid", decided_by: "tenant", levels: { deployment: "external", tenant: "hybrid" } });
    expect(await client.setKillSwitch(true, "incident_42")).toMatchObject({ engaged: true, reason: "incident_42", changed_by: "principal-1" });
    expect(await client.getMode()).toMatchObject({ effective: "local", configured: "hybrid", decided_by: "kill_switch", kill_switch: true });
    expect((await client.setKillSwitch(false)).engaged).toBe(false);
    expect((await client.getKillSwitch()).engaged).toBe(false);
  });

  it("discloses what may leave, and nothing when the request stays local", async () => {
    const { client, fake } = setup();
    const local = await client.getDisclosure("openai");
    expect(local).toMatchObject({ external_enabled: false, data_classes: [], data_categories: [], on_unavailable: { behavior: "fail" } });
    await client.acknowledgeBillingNotice("openai", { notice_version: "2026-10-02.1", consumer_subscription_is_not_api_access: true, separate_api_billing_understood: true });
    await client.submitCredential("openai", { key: KEY });
    await client.setMode("deployment", "external");
    const external = await client.getDisclosure("openai", { role: "conversational_controller", source: ["src_a", "src_b"], estimated_toks_in: 1200 });
    expect(external).toMatchObject({ external_enabled: true, rag_excerpts_included: true, provider: { model_id: "gpt-4.1-mini" }, terms: { training_use: "never" } });
    expect(Number.isInteger(external.cost.estimate_upper_micro_units)).toBe(true);
    const query = new URLSearchParams(fake.requests.at(-1)?.query);
    expect([query.get("role"), query.getAll("source"), query.get("estimated_toks_in")]).toEqual(["conversational_controller", ["src_a", "src_b"], "1200"]);
    await client.setKillSwitch(true, "stop");
    expect((await client.getDisclosure("openai")).external_enabled).toBe(false);
  });

  it("keeps revisions of an egress policy, and an approval where internal data may leave", async () => {
    const { client } = setup();
    expect(await client.getEgressPolicy("deployment")).toEqual({ policy: null });
    const base: Omit<P.EgressPolicyUpdate, "expected_revision"> = { level: "deployment", enabled: true, source_mode: "local_only", allowed_providers: ["openai"], allowed_classes: ["public"], allowed_regions: ["us"] };
    const first = await client.setEgressPolicy({ ...base, expected_revision: 0 });
    expect([first.revision, "approval" in first]).toEqual([1, false]);
    expect((await failure(client.setEgressPolicy({ ...base, expected_revision: 0 }))).code).toBe("conflict");
    const second = await client.setEgressPolicy({ ...base, expected_revision: 1, allowed_classes: ["public", "internal"] });
    expect([second.revision, second.approval?.approved_by]).toEqual([2, "principal-1"]);
    expect((await client.getEgressPolicy("deployment")).policy?.revision).toBe(2);
    const source = await client.setEgressPolicy({ level: "source", source_id: "src_01J0000000000000000000000S", source_class: "confidential", expected_revision: 0, enabled: true, source_mode: "allow_excerpt" });
    expect(source.target.source_id).toBe("src_01J0000000000000000000000S");
  });

  it("sets the files and audio settings, sent as the document's update body and left out when unset", async () => {
    const fake = providers.createFakeProvidersServer({ config: providers.defaultProvidersConfig() });
    const bodies: unknown[] = [];
    const client = providers.createProvidersClient({
      baseUrl: fake.baseUrl,
      getContextToken: () => fake.issueToken(),
      fetch: async (input, init) => {
        if (typeof init?.body === "string") bodies.push(JSON.parse(init.body));
        return fake.fetch(input, init);
      },
    });
    const validate = loadSpec().componentValidator("EgressPolicyUpdate");
    const base: P.EgressPolicyUpdate = { level: "workspace", expected_revision: 0, enabled: true, source_mode: "local_only", allowed_classes: ["public"] };
    const plain = await client.setEgressPolicy(base);
    expect(["files" in plain, "audio" in plain, Object.keys(bodies[0] as object).sort()]).toEqual([false, false, ["allowed_classes", "enabled", "expected_revision", "level", "source_mode"]]);

    const files: P.EgressPolicyContentEgress = { enabled: true, max_class: "confidential" };
    const audio: P.EgressPolicyContentEgress = { enabled: true, max_class: "public" };
    const stored = await client.setEgressPolicy({ ...base, expected_revision: 1, files, audio });
    expect(bodies[1]).toEqual({ ...base, expected_revision: 1, files, audio });
    for (const body of bodies) expect(validate(body), explain(validate)).toBe(true);
    expect([stored.files, stored.audio, stored.approval?.approved_by]).toEqual([files, audio, "principal-1"]);
    expect((await client.getEgressPolicy("workspace")).policy).toMatchObject({ revision: 2, files, audio });

    // Uploaded files and raw audio have no source: a source policy has neither setting.
    const source = { level: "source", source_id: "src_01J0000000000000000000000S", source_class: "public", expected_revision: 0, enabled: true, source_mode: "allow_excerpt" } as const;
    expect((await failure(client.setEgressPolicy({ ...source, audio }))).code).toBe("validation_failed");
    expect((await failure(client.setEgressPolicy({ ...source, files }))).code).toBe("validation_failed");
    expect((await client.setEgressPolicy(source)).files).toBeUndefined();
  });

  it("creates, changes and reads budgets in integer micro-units, and shows the state", async () => {
    const { client, fake } = setup();
    const input = {
      subject: { kind: "provider", provider_id: "openai" }, channel: "interactive", currency: "EUR", limits: { daily_micro_units: 1000, monthly_micro_units: 5000 }, warn_at_percent: 80,
    } as const;
    const created = await client.setBudget(input);
    expect([created.revision, created.limits.daily_micro_units]).toEqual([1, 1000]);
    expect((await failure(client.setBudget({ ...created, revision: 7 }))).code).toBe("conflict");
    const changed = await client.setBudget({ ...created, limits: { daily_micro_units: 2000, monthly_micro_units: 5000 } });
    expect(changed.revision).toBe(2);
    expect((await failure(client.setBudget({ ...changed, subject: { kind: "provider", provider_id: "acme" } }))).code).toBe("validation_failed");
    fake.setBudgetSpend(changed.budget_id, 1700, 0);
    expect((await client.getBudgets()).states[0]).toMatchObject({ state: "warning" });
    fake.setBudgetSpend(changed.budget_id, 2000, 0);
    const view = await client.getBudgets();
    expect(view.states[0]).toMatchObject({ state: "exhausted", windows: [{ window: "daily", remaining_micro_units: 0 }, { window: "monthly" }] });
    expect((await client.list()).providers.find((p) => p.provider.provider_id === "openai")?.budget_state).toBe("exhausted");
  });

  it("exports usage with the selection in the query", async () => {
    const { client, fake } = setup();
    fake.setUsage({ records: [{ provider_id: "openai", toks_in: 12 }], totals: [] });
    expect(await client.exportUsage({ from: "2026-10-01T00:00:00Z", provider: "openai", channel: "background", scope: "application" })).toEqual({ records: [{ provider_id: "openai", toks_in: 12 }], totals: [] });
    const query = new URLSearchParams(fake.requests.at(-1)?.query);
    expect(Object.fromEntries(query)).toEqual({ from: "2026-10-01T00:00:00Z", provider: "openai", channel: "background", scope: "application", format: "json" });
  });

  it("runs the OAuth flow, with the manual fallback for a provider without it, and a state that is single use", async () => {
    const { client } = setup();
    const input = { capabilities: ["chat"], redirect_uri: "http://127.0.0.1:8123/callback", label: "main" };
    const fallback = await failure(client.startOAuth("acme", input));
    expect([fallback.code, providers.isManualFallback(fallback), providers.isManualFallback(new Error("x"))]).toEqual(["validation_failed", true, false]);
    const started = await client.startOAuth("openai", input);
    expect(started.consent).toMatchObject({ provider_name: "openai", reauthorization: false });
    expect(started.authorization_url).not.toContain("verifier");
    const state = new URL(started.authorization_url).searchParams.get("state") as string;
    const connected = await client.completeOAuth({ redirect_uri: input.redirect_uri, query: `code=abc123&state=${state}` });
    expect(connected).toMatchObject({ state: "connected", kind: "oauth", account: { account_id: "sub-fake" } });
    const replay = await failure(client.completeOAuth({ redirect_uri: input.redirect_uri, query: `code=abc123&state=${state}` }));
    expect([replay.code, replay.message.includes(state), replay.message.includes("abc123")]).toEqual(["conflict", false, false]);
    const other = await client.startOAuth("openai", input);
    const wrong = await failure(client.completeOAuth({ redirect_uri: "http://127.0.0.1:1/other", query: `code=x&state=${new URL(other.authorization_url).searchParams.get("state")}` }));
    expect(wrong.code).toBe("conflict");
  });
});

describe("the response guards", () => {
  const good = async () => {
    const { client } = setup();
    return { status: (await client.list()).providers[0], mode: await client.getMode() };
  };

  it("refuse a status that is not the document, and a member the document does not have", async () => {
    const { status } = await good();
    const bad: Record<string, unknown> = {
      "an unknown state": { ...status, state: "mostly_connected" },
      "a credential member": { ...status, api_key: "sk-1" },
      "a missing member": { ...status, health: undefined },
      "not connected but with a kind": { ...status, kind: "api_key" },
      "a connected provider with no time": { ...status, state: "connected", kind: "api_key", connected_at: null },
      "a non-boolean acknowledgement": { ...status, billing_notice_acknowledged: "yes" },
      "an array": [],
    };
    for (const [name, body] of Object.entries(bad)) expect([name, providers.isConnectionStatus(body)]).toEqual([name, false]);
    expect(providers.isConnectionStatus(status)).toBe(true);
  });

  it("refuse an inconsistent mode, a view with no policy member and a float budget", async () => {
    const { mode } = await good();
    expect(providers.isProviderMode({ ...mode, kill_switch: true, effective: "external" })).toBe(false);
    expect(providers.isProviderMode({ ...mode, levels: { galaxy: "local" } })).toBe(false);
    expect(providers.isEgressPolicyView({})).toBe(false);
    expect(providers.isEgressPolicyView({ policy: null })).toBe(true);
    expect(providers.isKillSwitch({ engaged: true, reason: "Free text" })).toBe(false);
    const { client } = setup();
    const budget = await client.setBudget({ subject: { kind: "provider", provider_id: "openai" }, channel: "interactive", currency: "EUR", limits: { daily_micro_units: 1, monthly_micro_units: 2 }, warn_at_percent: 50 });
    expect(providers.isBudget(budget)).toBe(true);
    expect(providers.isBudget({ ...budget, limits: { daily_micro_units: 0.5, monthly_micro_units: 2 } })).toBe(false);
  });

  it("accept a stored policy with the files and audio settings, and refuse a setting of another shape or a member the document does not have", async () => {
    const { client } = setup();
    const policy = await client.setEgressPolicy({ level: "workspace", expected_revision: 0, enabled: true, source_mode: "local_only" });
    const files = { enabled: true, max_class: "confidential" };
    const audio = { enabled: false, max_class: "public" };
    const stored = { ...policy, files, audio, approval: { approved_by: "principal-1", approved_at: policy.updated_at } };
    const validate = loadSpec().responseValidator("putProviderEgressPolicy", 200);
    expect(validate?.(stored), validate === undefined ? "no schema" : explain(validate)).toBe(true);
    expect([providers.isEgressPolicy(stored), providers.isEgressPolicy({ ...policy, audio }), providers.isEgressPolicyView({ policy: stored })]).toEqual([true, true, true]);
    const bad: Record<string, unknown> = {
      "a restricted files class": { ...policy, files: { enabled: true, max_class: "restricted" } },
      "an audio setting with no enabled": { ...policy, audio: { max_class: "public" } },
      "a files setting with no class": { ...policy, files: { enabled: true } },
      "a non-boolean enabled": { ...policy, audio: { enabled: "yes", max_class: "public" } },
      "a member the setting does not have": { ...policy, files: { ...files, max_bytes: 1 } },
      "a setting that is not an object": { ...policy, audio: true },
      "a null setting": { ...policy, files: null },
      "a member the policy does not have": { ...policy, video: audio },
    };
    for (const [name, body] of Object.entries(bad)) expect([name, providers.isEgressPolicy(body)]).toEqual([name, false]);
  });

  it("refuse an answer of the wrong shape as an internal unexpected_shape error", async () => {
    const client = providers.createProvidersClient({
      baseUrl: "https://alixai.test",
      getContextToken: () => "tok",
      fetch: async () => new Response(JSON.stringify({ providers: [{ state: "connected" }] }), { status: 200, headers: { "Content-Type": "application/json" } }),
    });
    const error = await failure(client.list());
    expect([error.code, error.details?.["reason"]]).toEqual(["internal", "unexpected_shape"]);
  });
});

describe("deadlines and cancellation", () => {
  it("honours a signal and a deadline; a write that times out is an uncertain outcome", async () => {
    const hang = (_url: string, init?: RequestInit): Promise<Response> =>
      new Promise((_resolve, reject) => init?.signal?.addEventListener("abort", () => reject(init.signal?.reason)));
    const client = providers.createProvidersClient({ baseUrl: "https://alixai.test", getContextToken: () => "tok", fetch: hang });
    expect((await failure(client.list({ timeoutMs: 10 }))).code).toBe("timeout");
    expect((await failure(client.setKillSwitch(true, "stop", { timeoutMs: 10 }))).code).toBe("uncertain_outcome");
    const controller = new AbortController();
    const pending = failure(client.getMode({ signal: controller.signal }));
    controller.abort(new Error("stop"));
    await expect(pending).rejects.toThrow("stop");
    expect(() => providers.createProvidersClient({ baseUrl: "http://example.com", getContextToken: () => "tok" })).toThrow();
  });
});
