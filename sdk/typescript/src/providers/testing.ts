import { PROVIDERS_OPERATIONS } from "./operations.js";
import type { ProvidersOperationId } from "./operations.js";
import type { ProvidersContextTokenProvider } from "./client.js";

// An in-process fake of the providers and OAuth areas (External Provider Mode, milestone X), for the tests of a host application: provider
// connections, the mode, the data-egress policy, the egress disclosure, budgets, usage, the kill switch and the browser OAuth flow. It is a
// `fetch` replacement: no port, no network, no process. Pass `server.fetch` and `server.getContextToken` to `createProvidersClient`.
//
// The area is OFF until the fake is given a configuration, like a deployment without external mode: every route then answers 503 `unavailable`,
// never an empty result. Every route needs an OPERATOR, decided from the configuration (`tenantAdmins` and `deploymentOperators`, by principal id),
// never from a role or capability a token asserts; with nobody configured nobody passes, and the decision is made before the body is read. A token is
// SINGLE USE (a second presentation is 401 `replayed`).
//
// The fake keeps no secret: a manual credential is looked at (its length and a prefix) and dropped, never stored, echoed or logged, and the request
// log holds no body. Money is integer micro-units.

/** One provider profile the fake offers. */
export interface ProviderSeed {
  /** The provider id, for example "openai". */
  id: string;
  model: string;
  modelVersion: string;
  /** The current billing notice version of the profile. */
  noticeVersion: string;
  /** Whether the provider offers browser OAuth for API access. Without it startOAuth answers `validation_failed` with `details.reason` `manual_fallback`. */
  oauth: boolean;
}

/** Switches the providers area on. */
export interface ProvidersFixtureConfig {
  /** The principals that administer their own tenant. */
  tenantAdmins?: string[];
  /** The principals that also manage what concerns more than one tenant: the kill switch, the application and deployment mode and egress policy, provider-wide budgets, the whole application's usage. */
  deploymentOperators?: string[];
  providers?: ProviderSeed[];
}

/** A deployment whose only operator is the default principal ("principal-1"), offering an OAuth-capable provider "openai" and a manual-key provider "acme". */
export const defaultProvidersConfig = (): Required<ProvidersFixtureConfig> => ({
  tenantAdmins: [],
  deploymentOperators: ["principal-1"],
  providers: [
    { id: "openai", model: "gpt-4.1-mini", modelVersion: "2025-04-14", noticeVersion: "2026-10-02.1", oauth: true },
    { id: "acme", model: "acme-small", modelVersion: "2026-01-01", noticeVersion: "2026-10-02.1", oauth: false },
  ],
});

/** What a context token carries: the verified scope of the caller. */
export interface FakeProvidersClaims {
  principalId?: string;
  tenantId?: string;
  applicationId?: string;
  workspaceId?: string;
}

/** One request the fake received, for assertions. The body is never recorded. */
export interface RecordedProvidersRequest {
  /** The operationId, "" when no operation matched. */
  operation: string;
  method: string;
  /** The path, without the query. */
  path: string;
  /** The query string, without the "?". */
  query: string;
  /** True when the request carried a bearer token. */
  authenticated: boolean;
  /** The status the fake answered with. */
  status: number;
}

export interface FakeProvidersServerOptions {
  /** The base URL the fake answers for. Default "https://alixai.test". */
  baseUrl?: string;
  /** The configuration. Without it the area is off and every route answers `unavailable`. */
  config?: ProvidersFixtureConfig;
  /** The claims of the tokens `getContextToken` issues. Default: principal-1 of tenant-1 in fake-app. */
  claims?: FakeProvidersClaims;
  /** The clock, in milliseconds since the epoch. Default `Date.now`. */
  now?: () => number;
}

export interface FakeProvidersServer {
  readonly baseUrl: string;
  /** A `fetch` that routes to the fake. Pass it as the `fetch` option of the client. */
  readonly fetch: (input: string, init?: RequestInit) => Promise<Response>;
  /** A token provider that issues a fresh single-use token on every call. Pass it as `getContextToken`. */
  readonly getContextToken: ProvidersContextTokenProvider;
  /** Every request received so far, oldest first. */
  readonly requests: readonly RecordedProvidersRequest[];
  /** Switches the area on with this configuration (and resets its state). */
  configure: (config: ProvidersFixtureConfig) => void;
  /** Makes every route answer 503 `unavailable`, as a server without the providers service does, or restores them. */
  setUnavailable: (unavailable?: boolean) => void;
  /** Sets the settled spend of a budget's daily and monthly windows (micro-units), so a test can see a warning or an exhausted budget. */
  setBudgetSpend: (budgetId: string, dailyMicroUnits: number, monthlyMicroUnits: number) => void;
  /** Sets the document the usage export answers with (records and totals). */
  setUsage: (usage: Record<string, unknown>) => void;
  /** Issues one single-use token with these claims (the defaults for what is left out). */
  issueToken: (claims?: FakeProvidersClaims) => string;
}

type Json = Record<string, unknown>;

interface Conn {
  state: string;
  kind: string;
  account: Json;
  connectedAt: string;
  expiresAt: string | null;
  warnings: string[];
}

interface Flow {
  provider: string;
  tenant: string;
  principal: string;
  redirect: string;
  expires: number;
}

interface PopupFlow {
  provider: string;
  tenant: string;
  principal: string;
  application: string;
  workspace: string;
  status: string;
  updated: number;
}

interface Claims {
  principalId: string;
  tenantId: string;
  applicationId: string;
  workspaceId: string;
}

interface Route {
  operation: ProvidersOperationId;
  method: string;
  pattern: RegExp;
}

const ROUTES: Route[] = (Object.entries(PROVIDERS_OPERATIONS) as [ProvidersOperationId, { method: string; path: string }][]).map(([operation, spec]) => ({
  operation,
  method: spec.method,
  pattern: new RegExp(`^${spec.path.replace("{provider}", "([^/]+)").replace("{flow_id}", "([^/]+)")}$`),
}));

const ROLES = ["conversational_controller", "language_realization", "vision", "image_generation", "embedding", "stt", "tts"];
const CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";
const MODE_RANK: Record<string, number> = { local: 0, hybrid: 1, external: 2 };
const PROFILE_ULID = "01J0000000000000000000000P";
const SOURCE_ID = /^src_[0-9A-HJKMNP-TV-Z]{26}$/;

const problem = (status: number, code: string, message: string, details?: Json): Response =>
  new Response(JSON.stringify({ code, message, retryable: status === 503, ...(details !== undefined && { details }) }), {
    status,
    headers: { "Content-Type": "application/json", "X-Request-ID": "req_fake", ...(status === 503 && { "Retry-After": "1" }) },
  });

const ok = (body: unknown): Response =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json", "X-Request-ID": "req_fake" } });

/** Creates the in-process fake of the providers and OAuth areas. */
export const createFakeProvidersServer = (options: FakeProvidersServerOptions = {}): FakeProvidersServer => {
  const baseUrl = (options.baseUrl ?? "https://alixai.test").replace(/\/+$/, "");
  const now = options.now ?? (() => Date.now());
  const defaultClaims: Claims = {
    principalId: options.claims?.principalId ?? "principal-1",
    tenantId: options.claims?.tenantId ?? "tenant-1",
    applicationId: options.claims?.applicationId ?? "fake-app",
    workspaceId: options.claims?.workspaceId ?? "workspace-1",
  };
  const tokens = new Map<string, Claims>();
  const used = new Set<string>();
  const requests: RecordedProvidersRequest[] = [];
  let counter = 0;

  // ---- the state of the area ----
  let config: Required<ProvidersFixtureConfig> | undefined;
  let unavailable = false;
  let conns = new Map<string, Conn>();
  let acks = new Set<string>();
  let modes = new Map<string, string>();
  let kill: Json = { engaged: false };
  let policies = new Map<string, Json>();
  let budgets: Json[] = [];
  let spent = new Map<string, [number, number]>();
  let flows = new Map<string, Flow>();
  let popupFlows = new Map<string, PopupFlow>();
  let usage: Json = { records: [], totals: [] };

  const configure = (next: ProvidersFixtureConfig): void => {
    const base = defaultProvidersConfig();
    config = {
      tenantAdmins: next.tenantAdmins ?? [],
      deploymentOperators: next.deploymentOperators ?? [],
      providers: next.providers ?? base.providers,
    };
    conns = new Map();
    acks = new Set();
    modes = new Map();
    kill = { engaged: false };
    policies = new Map();
    budgets = [];
    spent = new Map();
    flows = new Map();
    popupFlows = new Map();
    usage = { records: [], totals: [] };
  };
  if (options.config !== undefined) configure(options.config);

  const random = (length: number): string => Array.from({ length }, () => CROCKFORD[Math.floor(Math.random() * 32)]).join("");
  const typedId = (prefix: string): string => `${prefix}_${random(26)}`;
  const stamp = (ms = now()): string => new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");

  const issueToken = (claims: FakeProvidersClaims = {}): string => {
    counter += 1;
    const token = `fake.${counter}.${random(10)}`;
    tokens.set(token, { ...defaultClaims, ...(Object.fromEntries(Object.entries(claims).filter(([, v]) => v !== undefined)) as Partial<Claims>) });
    return token;
  };

  // ---- the handlers ----

  const seedOf = (id: string) => config?.providers.find((s) => s.id === id);
  const providerRef = (s: ProviderSeed): Json => ({
    profile_id: `pprf_${PROFILE_ULID}`,
    provider_id: s.id,
    model_id: s.model,
    model_version: s.modelVersion,
  });
  const connKey = (c: Claims, provider: string): string => `${c.tenantId}/${provider}`;
  const modeKey = (c: Claims, level: string): string =>
    level === "application" ? `application/${c.applicationId}` : level === "tenant" ? `tenant/${c.applicationId}/${c.tenantId}` : "deployment";
  const policyKey = (c: Claims, level: string, source: string): string => {
    if (level === "deployment") return "deployment";
    if (level === "application") return `application/${c.applicationId}`;
    if (level === "tenant") return `tenant/${c.applicationId}/${c.tenantId}`;
    if (level === "workspace") return `workspace/${c.applicationId}/${c.tenantId}/${c.workspaceId}`;
    return `source/${c.applicationId}/${c.tenantId}/${c.workspaceId}/${source}`;
  };

  const budgetState = (b: Json): Json => {
    const id = String(b["budget_id"]);
    const limits = b["limits"] as { daily_micro_units: number; monthly_micro_units: number };
    const warn = b["warn_at_percent"] as number;
    const [daySpent, monthSpent] = spent.get(id) ?? [0, 0];
    const at = new Date(now());
    const day = Date.UTC(at.getUTCFullYear(), at.getUTCMonth(), at.getUTCDate());
    const month = Date.UTC(at.getUTCFullYear(), at.getUTCMonth(), 1);
    const window = (name: string, from: number, to: number, limit: number, spentNow: number): Json => {
      let remaining = limit - spentNow;
      let state = "ok";
      if (remaining <= 0) [remaining, state] = [0, "exhausted"];
      else if (spentNow * 100 >= limit * warn) state = "warning";
      return {
        window: name, starts_at: stamp(from), ends_at: stamp(to), limit_micro_units: limit, spent_micro_units: spentNow, reserved_micro_units: 0,
        remaining_micro_units: remaining, state,
      };
    };
    const daily = window("daily", day, day + 86_400_000, limits.daily_micro_units, daySpent);
    const monthly = window("monthly", month, Date.UTC(at.getUTCFullYear(), at.getUTCMonth() + 1, 1), limits.monthly_micro_units, monthSpent);
    const states = [daily["state"], monthly["state"]];
    const worst = states.includes("exhausted") ? "exhausted" : states.includes("warning") ? "warning" : "ok";
    return {
      budget_id: id, subject: b["subject"], channel: b["channel"], currency: b["currency"], as_of: stamp(), state: worst, windows: [daily, monthly],
      local_fallback_available: false,
    };
  };

  const budgetStatusFor = (provider: string): string | null => {
    let worst: string | null = null;
    for (const b of budgets) {
      const subject = b["subject"] as Json;
      if (typeof subject["provider_id"] === "string" && subject["provider_id"] !== provider) continue;
      const state = String(budgetState(b)["state"]);
      if (state === "exhausted" || (state === "warning" && worst !== "exhausted") || worst === null) worst = state;
    }
    return worst;
  };

  const connectionStatus = (c: Claims, s: ProviderSeed): Json => {
    const conn = conns.get(connKey(c, s.id));
    return {
      as_of: stamp(), provider: providerRef(s), lifecycle: "qualified",
      state: conn?.state ?? "not_connected", kind: conn?.kind ?? null,
      account: conn?.account ?? { account_id: null, organization_id: null, project_id: null },
      connected_at: conn?.connectedAt ?? null, expires_at: conn?.expiresAt ?? null, warnings: [...(conn?.warnings ?? [])],
      health: conn?.state === "connected" ? "healthy" : "unknown", last_error_kind: null, budget_state: budgetStatusFor(s.id),
      billing_notice_acknowledged: acks.has(connKey(c, s.id)),
    };
  };

  const effectiveMode = (c: Claims): [string, string] => {
    let mode = "local";
    let decidedBy = "default";
    let first = true;
    for (const level of ["deployment", "application", "tenant"]) {
      const m = modes.get(modeKey(c, level));
      if (m === undefined) continue;
      if (first || (MODE_RANK[m] ?? 0) < (MODE_RANK[mode] ?? 0)) [mode, decidedBy] = [m, level];
      first = false;
    }
    return [mode, decidedBy];
  };

  const modeView = (c: Claims): Json => {
    const levels: Json = {};
    for (const level of ["deployment", "application", "tenant"]) {
      const m = modes.get(modeKey(c, level));
      if (m !== undefined) levels[level] = m;
    }
    const [configured, decided] = effectiveMode(c);
    const killed = kill["engaged"] === true;
    return { effective: killed ? "local" : configured, configured, decided_by: killed ? "kill_switch" : decided, kill_switch: killed, levels };
  };

  const isDeploymentOperator = (c: Claims): boolean => config?.deploymentOperators.includes(c.principalId) === true;

  const readBody = (init: RequestInit | undefined): Json | undefined => {
    if (typeof init?.body !== "string") return undefined;
    try {
      const parsed: unknown = JSON.parse(init.body);
      return typeof parsed === "object" && parsed !== null && !Array.isArray(parsed) ? (parsed as Json) : undefined;
    } catch {
      return undefined;
    }
  };

  const closedBy = (body: Json, known: readonly string[]): boolean => Object.keys(body).every((k) => known.includes(k));
  const bad = (message = "the request body is not valid for this route"): Response => problem(400, "validation_failed", message);
  const denied = (message = "the caller is not an operator"): Response => problem(403, "permission_denied", message);

  interface Ctx {
    route: Route;
    provider: string;
    flowId?: string;
    claims: Claims;
    query: URLSearchParams;
    body: Json | undefined;
    hasBody: boolean;
  }

  const handlers: Record<ProvidersOperationId, (ctx: Ctx) => Response> = {
    listProviders: (ctx) => ok({ providers: (config?.providers ?? []).map((s) => connectionStatus(ctx.claims, s)) }),

    putProviderCredential: (ctx) => {
      const seed = seedOf(ctx.provider);
      if (seed === undefined) return problem(400, "validation_failed", "no such provider", { reason: "not_found" });
      const body = ctx.body;
      if (body === undefined || !closedBy(body, ["key", "label", "expires_at", "scopes"])) return bad();
      // The key is looked at and dropped. Nothing below keeps, logs or echoes it, and every failure has a fixed message.
      const key = body["key"];
      if (typeof key !== "string" || key.length === 0 || key.length > 4096 || key.startsWith("bad")) return problem(400, "validation_failed", "the credential was not accepted");
      const conn: Conn = {
        state: "connected", kind: "api_key", connectedAt: stamp(), expiresAt: typeof body["expires_at"] === "string" ? body["expires_at"] : null,
        account: { account_id: null, organization_id: "org-fake", project_id: "proj-fake" }, warnings: ["broader_than_oauth_scopes"],
      };
      if (key.startsWith("broad")) [conn.state, conn.warnings] = ["quarantined", ["excessive_permissions_reported"]];
      conns.set(connKey(ctx.claims, seed.id), conn);
      return ok(connectionStatus(ctx.claims, seed));
    },

    deleteProviderConnection: (ctx) => {
      const seed = seedOf(ctx.provider);
      if (seed === undefined) return problem(400, "validation_failed", "no such provider", { reason: "not_found" });
      conns.delete(connKey(ctx.claims, seed.id));
      return ok(connectionStatus(ctx.claims, seed));
    },

    ackProviderBillingNotice: (ctx) => {
      const seed = seedOf(ctx.provider);
      if (seed === undefined) return problem(400, "validation_failed", "no such provider", { reason: "not_found" });
      const body = ctx.body;
      if (body === undefined || !closedBy(body, ["notice_version", "consumer_subscription_is_not_api_access", "separate_api_billing_understood"])) return bad();
      if (typeof body["notice_version"] !== "string" || body["notice_version"] === "" || body["consumer_subscription_is_not_api_access"] !== true || body["separate_api_billing_understood"] !== true) {
        return bad("the notice version and both confirmations are required");
      }
      if (body["notice_version"] !== seed.noticeVersion) return problem(409, "conflict", "the notice version is not the current one", { reason: "stale_notice" });
      acks.add(connKey(ctx.claims, seed.id));
      return ok({
        ack_id: typedId("ack"), profile_id: `pprf_${PROFILE_ULID}`, provider_id: seed.id, notice_version: seed.noticeVersion, notice_digest: `sha256:${"c".repeat(64)}`,
        scope: { deployment_id: "dep_fake", application_id: ctx.claims.applicationId, tenant_id: ctx.claims.tenantId },
        confirmations: { consumer_subscription_is_not_api_access: true, separate_api_billing_understood: true },
        acknowledged_by: ctx.claims.principalId, acknowledged_at: stamp(),
      });
    },

    getProviderDisclosure: (ctx) => {
      const seed = seedOf(ctx.provider);
      if (seed === undefined) return problem(400, "validation_failed", "no such provider", { reason: "not_found" });
      const role = ctx.query.get("role") ?? "conversational_controller";
      if (!ROLES.includes(role)) return bad("role is not a known role");
      for (const name of ["estimated_toks_in", "max_output_toks"]) {
        const v = ctx.query.get(name);
        if (v !== null && !/^\d+$/.test(v)) return bad(`${name} is not a valid count`);
      }
      const conn = conns.get(connKey(ctx.claims, seed.id));
      const enabled = modeView(ctx.claims)["effective"] !== "local" && conn?.state === "connected" && acks.has(connKey(ctx.claims, seed.id));
      const withSources = ctx.query.getAll("source").length > 0;
      return ok({
        generated_at: stamp(), provider: providerRef(seed), role, external_enabled: enabled,
        data_classes: enabled ? ["public"] : [],
        data_categories: enabled ? ["user_message", "structured_state", "deterministic_fact", ...(withSources ? ["rag_excerpt"] : [])] : [],
        rag_excerpts_included: enabled && withSources, regions: ["us"],
        terms: { retention: "limited", retention_days: 30, training_use: "never", residency: "selectable" },
        cost: { currency: "EUR", estimate_upper_micro_units: 4200, budget_state: budgetStatusFor(seed.id) ?? "ok" },
        on_unavailable: { behavior: "fail", fallback_profile_id: null },
      });
    },

    getProviderMode: (ctx) => ok(modeView(ctx.claims)),

    putProviderMode: (ctx) => {
      const body = ctx.body;
      if (body === undefined || !closedBy(body, ["level", "mode"])) return bad();
      const { level, mode } = body;
      if (typeof level !== "string" || !["deployment", "application", "tenant"].includes(level)) return bad("level is deployment, application or tenant");
      if (typeof mode !== "string" || !(mode in MODE_RANK)) return bad("mode is local, hybrid or external");
      if (level !== "tenant" && !isDeploymentOperator(ctx.claims)) return denied("this level needs a deployment operator");
      modes.set(modeKey(ctx.claims, level), mode);
      return ok(modeView(ctx.claims));
    },

    getProviderEgressPolicy: (ctx) => {
      const level = ctx.query.get("level") ?? "";
      const source = ctx.query.get("source_id") ?? "";
      if (!["deployment", "application", "tenant", "workspace", "source"].includes(level) || (level === "source") !== (source !== "") || (source !== "" && !SOURCE_ID.test(source))) {
        return bad("level is required; source_id is for the source level and only for it");
      }
      return ok({ policy: policies.get(policyKey(ctx.claims, level, source)) ?? null });
    },

    putProviderEgressPolicy: (ctx) => {
      const body = ctx.body;
      const known = [
        "level", "source_id", "expected_revision", "enabled", "source_mode", "source_class", "allowed_providers", "allowed_models", "allowed_classes", "allowed_regions",
        "files", "audio",
      ];
      if (body === undefined || !closedBy(body, known)) return bad();
      const level = body["level"];
      const source = typeof body["source_id"] === "string" ? body["source_id"] : "";
      const sourceClass = body["source_class"];
      const expected = body["expected_revision"];
      if (
        typeof level !== "string" || !["deployment", "application", "tenant", "workspace", "source"].includes(level) || (level === "source") !== (source !== "") ||
        (source !== "" && !SOURCE_ID.test(source)) || typeof body["enabled"] !== "boolean" || typeof expected !== "number" || !Number.isInteger(expected) || expected < 0 ||
        !["local_only", "allow_excerpt", "allow_metadata", "deny"].includes(String(body["source_mode"])) || (level === "source") !== (typeof sourceClass === "string")
      ) {
        return bad("the policy is not valid");
      }
      // The files and audio settings: { enabled, max_class }, never restricted, and not at source level (uploads and raw audio have no source).
      const content: Record<string, Json> = {};
      for (const name of ["files", "audio"]) {
        const setting = body[name];
        if (setting === undefined) continue;
        if (
          level === "source" || typeof setting !== "object" || setting === null || Array.isArray(setting) || !closedBy(setting as Json, ["enabled", "max_class"]) ||
          typeof (setting as Json)["enabled"] !== "boolean" || !["public", "internal", "confidential"].includes(String((setting as Json)["max_class"]))
        ) {
          return bad(`the ${name} setting is not valid`);
        }
        content[name] = { enabled: (setting as Json)["enabled"], max_class: (setting as Json)["max_class"] };
      }
      const abovePublic = Object.values(content).some((s) => s["enabled"] === true && s["max_class"] !== "public");
      if ((level === "deployment" || level === "application") && !isDeploymentOperator(ctx.claims)) return denied("this level needs a deployment operator");
      const key = policyKey(ctx.claims, level, source);
      const current = policies.get(key);
      const revision = current === undefined ? 0 : (current["revision"] as number);
      if (expected !== revision) return problem(409, "conflict", "the policy changed since it was read", { revision });
      const at = stamp();
      const c = ctx.claims;
      const target: Json = { deployment_id: "dep_fake" };
      if (level !== "deployment") target["application_id"] = c.applicationId;
      if (["tenant", "workspace", "source"].includes(level)) target["tenant_id"] = c.tenantId;
      if (["workspace", "source"].includes(level)) target["workspace_id"] = c.workspaceId;
      if (level === "source") target["source_id"] = source;
      const classes = Array.isArray(body["allowed_classes"]) ? (body["allowed_classes"] as string[]) : [];
      const policy: Json = {
        policy_id: current?.["policy_id"] ?? typedId("epol"), revision: revision + 1, level, target, enabled: body["enabled"], source_mode: body["source_mode"],
        allowed_providers: body["allowed_providers"] ?? [], allowed_models: body["allowed_models"] ?? [], allowed_classes: classes, allowed_regions: body["allowed_regions"] ?? [],
        updated_at: at, updated_by: c.principalId,
        ...(typeof sourceClass === "string" && { source_class: sourceClass }),
        ...content,
        ...((classes.includes("internal") || classes.includes("confidential") || abovePublic) && { approval: { approved_by: c.principalId, approved_at: at } }),
      };
      policies.set(key, policy);
      return ok(policy);
    },

    getProviderBudgets: () => ok({ budgets: [...budgets], states: budgets.map(budgetState) }),

    putProviderBudget: (ctx) => {
      const body = ctx.body;
      const known = ["budget_id", "revision", "subject", "channel", "currency", "limits", "warn_at_percent", "quotas", "rate_limits", "updated_at", "updated_by"];
      if (body === undefined || !closedBy(body, known)) return bad();
      const subject = body["subject"] as Json | undefined;
      const limits = body["limits"] as Json | undefined;
      const warn = body["warn_at_percent"];
      const money = (v: unknown): boolean => typeof v === "number" && Number.isInteger(v) && v >= 0;
      if (
        subject === undefined || typeof subject !== "object" || !["provider", "tenant", "workspace", "principal"].includes(String(subject["kind"])) ||
        !["interactive", "background"].includes(String(body["channel"])) || typeof body["currency"] !== "string" || body["currency"] === "" ||
        limits === undefined || !money(limits["daily_micro_units"]) || !money(limits["monthly_micro_units"]) || typeof warn !== "number" || warn < 1 || warn > 99
      ) {
        return bad("the budget is not valid");
      }
      if (subject["kind"] === "provider" && !isDeploymentOperator(ctx.claims)) return denied("a provider-wide budget needs a deployment operator");
      const doc: Json = {
        subject, channel: body["channel"], currency: body["currency"], limits: { daily_micro_units: limits["daily_micro_units"], monthly_micro_units: limits["monthly_micro_units"] },
        warn_at_percent: warn, updated_at: stamp(), updated_by: ctx.claims.principalId,
        ...(body["quotas"] !== undefined && { quotas: body["quotas"] }),
        ...(body["rate_limits"] !== undefined && { rate_limits: body["rate_limits"] }),
      };
      const id = body["budget_id"];
      if (id === undefined || id === "") {
        const created = { budget_id: typedId("bud"), revision: 1, ...doc };
        budgets.push(created);
        return ok(created);
      }
      const index = budgets.findIndex((b) => b["budget_id"] === id);
      if (index < 0) return problem(400, "validation_failed", "no such budget", { reason: "unknown_budget" });
      const existing = budgets[index] as Json;
      if (existing["revision"] !== body["revision"]) return problem(409, "conflict", "the budget changed since it was read", { revision: existing["revision"] });
      if (JSON.stringify(existing["subject"]) !== JSON.stringify(subject) || existing["channel"] !== body["channel"] || existing["currency"] !== body["currency"]) {
        return bad("the subject, channel and currency of a budget cannot change");
      }
      const changed = { budget_id: id, revision: (existing["revision"] as number) + 1, ...doc };
      budgets[index] = changed;
      return ok(changed);
    },

    exportProviderUsage: (ctx) => {
      for (const name of ["from", "to"]) {
        const v = ctx.query.get(name);
        if (v !== null && Number.isNaN(Date.parse(v))) return bad(`${name} is not an RFC 3339 time`);
      }
      const channel = ctx.query.get("channel");
      if (channel !== null && channel !== "interactive" && channel !== "background") return bad("channel is not a known channel");
      const format = ctx.query.get("format");
      if (format !== null && format !== "json") return bad("this fake renders json");
      const scope = ctx.query.get("scope");
      if (scope !== null && scope !== "tenant" && scope !== "application") return bad("scope is tenant or application");
      if (scope === "application" && !isDeploymentOperator(ctx.claims)) return denied("the whole application needs a deployment operator");
      return ok(usage);
    },

    getProviderKillSwitch: () => ok(kill),

    putProviderKillSwitch: (ctx) => {
      const body = ctx.body;
      if (body === undefined || !closedBy(body, ["engaged", "reason"])) return bad();
      const reason = body["reason"];
      if (typeof body["engaged"] !== "boolean" || (reason !== undefined && (typeof reason !== "string" || !/^[a-z][a-z0-9_]{0,63}$/.test(reason)))) {
        return bad("engaged is required; reason is a fixed code");
      }
      kill = { engaged: body["engaged"], ...(reason !== undefined && { reason }), changed_by: ctx.claims.principalId, changed_at: stamp() };
      return ok(kill);
    },

    startProviderOAuth: (ctx) => {
      const seed = seedOf(ctx.provider);
      if (seed === undefined) return problem(400, "validation_failed", "no such provider", { reason: "not_found" });
      const body = ctx.body;
      if (body === undefined || !closedBy(body, ["capabilities", "redirect_uri", "reauthorize", "switch_account", "label", "presentation", "callback", "opener_origin"])) return bad();
      const caps = body["capabilities"];
      const redirect = body["redirect_uri"];
      if (!Array.isArray(caps) || caps.length === 0 || caps.length > 64 || typeof redirect !== "string" || redirect === "" || redirect.length > 512) {
        return bad("capabilities and redirect_uri are required and bounded");
      }
      if (!seed.oauth) return problem(400, "validation_failed", "this provider has no OAuth for API access: use a manual credential", { reason: "manual_fallback" });
      const state = `st${random(20)}`;
      const expires = now() + 10 * 60_000;
      flows.set(state, { provider: seed.id, tenant: ctx.claims.tenantId, principal: ctx.claims.principalId, redirect, expires });
      const authorization = new URLSearchParams({
        response_type: "code", state, code_challenge: `ch${random(20)}`, code_challenge_method: "S256", nonce: `no${random(8)}`, redirect_uri: redirect,
      });
      const out: Record<string, unknown> = {
        authorization_url: `https://auth.fake.test/authorize?${authorization.toString()}`, redirect_uri: redirect, expires_at: stamp(expires),
        consent: {
          provider_name: seed.id, account: { account_id: null, organization_id: null, project_id: null },
          scopes: [{ name: "model.request", description: "Send model requests", data_access: "the permitted request content" }],
          data_access: ["the permitted request content"], reauthorization: typeof body["reauthorize"] === "string" && body["reauthorize"] !== "", switch_account: body["switch_account"] === true,
        },
      };
      if (body["presentation"] === "popup") {
        const flowId = `oaf_${"01J0000000000000000000000T"}`;
        popupFlows.set(flowId, {
          provider: seed.id, tenant: ctx.claims.tenantId, principal: ctx.claims.principalId,
          application: ctx.claims.applicationId, workspace: ctx.claims.workspaceId, status: "pending", updated: now(),
        });
        out["flow_id"] = flowId;
        out["poll_interval_ms"] = 1000;
      }
      return ok(out);
    },

    getProviderOAuthFlow: (ctx) => {
      const id = ctx.flowId;
      const fl = id === undefined ? undefined : popupFlows.get(id);
      if (fl === undefined || fl.principal !== ctx.claims.principalId || fl.tenant !== ctx.claims.tenantId ||
        fl.application !== ctx.claims.applicationId || fl.workspace !== ctx.claims.workspaceId) {
        return problem(404, "validation_failed", "no such flow", { reason: "not_found" });
      }
      return ok({ flow_id: id, provider: fl.provider, status: fl.status, updated_at: stamp(fl.updated) });
    },

    cancelProviderOAuthFlow: (ctx) => {
      const id = ctx.flowId;
      const fl = id === undefined ? undefined : popupFlows.get(id);
      if (fl === undefined || fl.principal !== ctx.claims.principalId || fl.tenant !== ctx.claims.tenantId ||
        fl.application !== ctx.claims.applicationId || fl.workspace !== ctx.claims.workspaceId) {
        return problem(404, "validation_failed", "no such flow", { reason: "not_found" });
      }
      if (fl.status === "pending") {
        fl.status = "cancelled";
        fl.updated = now();
      }
      return new Response(null, { status: 204 });
    },

    completeProviderOAuth: (ctx) => {
      const body = ctx.body;
      if (body === undefined || !closedBy(body, ["redirect_uri", "query"])) return bad();
      const { redirect_uri: redirect, query } = body;
      if (typeof redirect !== "string" || redirect === "" || redirect.length > 512 || typeof query !== "string" || query === "" || query.length > 4096) {
        return bad("redirect_uri and query are required and bounded");
      }
      const params = new URLSearchParams(query.replace(/^\?/, ""));
      const state = params.get("state");
      if (state === null || state === "") return bad("the callback query has no usable state");
      // The state is single use: unknown, replayed, expired or issued to another scope is refused, and spent. The message is fixed.
      const refuse = (): Response => problem(409, "conflict", "the authorization could not be completed");
      const flow = flows.get(state);
      if (flow === undefined) return refuse();
      flows.delete(state);
      if (now() > flow.expires || flow.tenant !== ctx.claims.tenantId || flow.principal !== ctx.claims.principalId || flow.redirect !== redirect || params.has("error") || !params.get("code")) {
        return refuse();
      }
      const seed = seedOf(flow.provider);
      if (seed === undefined) return refuse();
      conns.set(connKey(ctx.claims, seed.id), {
        state: "connected", kind: "oauth", connectedAt: stamp(), expiresAt: stamp(now() + 3_600_000),
        account: { account_id: "sub-fake", organization_id: null, project_id: null }, warnings: [],
      });
      return ok(connectionStatus(ctx.claims, seed));
    },
  };

  // ---- the request ----

  const deploymentOnly: ReadonlySet<ProvidersOperationId> = new Set(["getProviderKillSwitch", "putProviderKillSwitch"]);
  const queryOf: Partial<Record<ProvidersOperationId, readonly string[]>> = {
    getProviderDisclosure: ["role", "source", "estimated_toks_in", "max_output_toks"],
    getProviderEgressPolicy: ["level", "source_id"],
    exportProviderUsage: ["from", "to", "provider", "channel", "scope", "format"],
  };

  const answer = (route: Route | undefined, provider: string, flowId: string, url: URL, init: RequestInit | undefined, authorization: string | null): Response => {
    if (route === undefined) return problem(404, "validation_failed", "no such route");
    const token = authorization?.startsWith("Bearer ") === true ? authorization.slice("Bearer ".length) : undefined;
    const claims = token === undefined ? undefined : tokens.get(token);
    if (token === undefined || claims === undefined) return problem(401, "unauthenticated", "the context token is missing or unknown");
    if (used.has(token)) return problem(401, "replayed", "the context token was already used: a token is single use");
    used.add(token);
    if (config === undefined || unavailable) return problem(503, "unavailable", "the server has no providers service");
    // The operator decision is made from the configuration, before the query or the body is looked at.
    const isOp = config.deploymentOperators.includes(claims.principalId);
    if (!isOp && (deploymentOnly.has(route.operation) || !config.tenantAdmins.includes(claims.principalId))) return denied();
    const known = queryOf[route.operation] ?? [];
    for (const name of url.searchParams.keys()) if (!known.includes(name)) return bad("unknown query parameter");
    const hasBody = init?.body !== undefined;
    const body = hasBody ? readBody(init) : undefined;
    if (hasBody && new Headers(init?.headers).get("Content-Type")?.startsWith("application/json") !== true) {
      return problem(415, "validation_failed", "Content-Type must be application/json");
    }
    return handlers[route.operation]({
      route, provider, claims, query: url.searchParams, body, hasBody,
      ...(flowId !== "" && { flowId }),
    });
  };

  const fetchFake = async (input: string, init?: RequestInit): Promise<Response> => {
    if (!input.startsWith(baseUrl)) throw new TypeError(`${input} is not the fake server (${baseUrl})`);
    const url = new URL(input);
    const prefix = new URL(baseUrl).pathname.replace(/\/+$/, "");
    const path = url.pathname.slice(prefix.length);
    const method = init?.method ?? "GET";
    let provider = "";
    let flowId = "";
    const route = ROUTES.find((r) => {
      const match = r.method === method ? r.pattern.exec(path) : null;
      if (match !== null) {
        const captured = decodeURIComponent(match[1] ?? "");
        if (r.operation === "getProviderOAuthFlow" || r.operation === "cancelProviderOAuthFlow") flowId = captured;
        else provider = captured;
      }
      return match !== null;
    });
    const response = answer(route, provider, flowId, url, init, new Headers(init?.headers).get("Authorization"));
    requests.push({
      operation: route?.operation ?? "", method, path, query: url.search.replace(/^\?/, ""), authenticated: new Headers(init?.headers).has("Authorization"),
      status: response.status,
    });
    return response;
  };

  return {
    baseUrl,
    fetch: fetchFake,
    getContextToken: () => issueToken(),
    requests,
    configure,
    setUnavailable: (value = true) => {
      unavailable = value;
    },
    setBudgetSpend: (budgetId, daily, monthly) => {
      spent.set(budgetId, [daily, monthly]);
    },
    setUsage: (next) => {
      usage = next;
    },
    issueToken,
  };
};
