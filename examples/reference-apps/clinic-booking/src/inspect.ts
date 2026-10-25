import type { Database } from "./db.ts";
import { refuse } from "./errors.ts";
import { bool, integer, text, type Fact } from "./results.ts";
import { IDEMPOTENCY_WINDOW_SECONDS, MAX_AHEAD_DAYS, MAX_BODY_BYTES, MIN_LEAD_MINUTES } from "./domain/rules.ts";
import { MAX_LIFETIME_SECONDS } from "./context-token.ts";
import { ROLES } from "./access.ts";

// The inspection document for the runtime and system half of the integration
// audit (master plan 4.5, "Audit targets"). It follows the shape the AlixAI
// adapters read: one report per requested aspect, in the canonical order, each
// either `reported` with at least one fact or `unsupported` with none. This
// application reports all nine aspects, so none is left unverified.
//
// Nothing here is a secret. Credentials are described by reference and by
// whether they are required, never by value; the database is described by
// kind and location; counts say how much exists, not what it holds.

export const ASPECTS = [
  "versions",
  "modules",
  "dependencies",
  "health",
  "scopes",
  "credentials",
  "limits",
  "storage",
  "network",
] as const;

export type Aspect = (typeof ASPECTS)[number];

export type AspectReport = { aspect: Aspect; state: "reported" | "unsupported"; facts: Fact[] };

export type Inspection = { reports: AspectReport[] };

export const APPLICATION_VERSION = "1.0.0";
export const CONTRACT_VERSION = 1;

// What the running process knows about itself; the HTTP layer fills it in once
// the socket is bound.
export type RuntimeInfo = {
  host: string;
  port: number;
  databasePath: string;
  testSupport: boolean;
  credentialRequired: boolean;
  tokenKeyId: string | null;
};

export const parseAspects = (raw: unknown): Aspect[] => {
  if (raw === undefined || raw === null) {
    return [...ASPECTS];
  }
  if (!Array.isArray(raw)) {
    throw refuse("validation_failed", "aspects must be a list", { field: "aspects" });
  }
  const seen = new Set<string>();
  for (const item of raw) {
    if (typeof item !== "string" || !(ASPECTS as readonly string[]).includes(item) || seen.has(item)) {
      throw refuse("validation_failed", "aspects must be known aspect names, each at most once", { field: "aspects" });
    }
    seen.add(item);
  }
  // Empty means all, as the adapter contract says.
  return raw.length === 0 ? [...ASPECTS] : ASPECTS.filter((a) => seen.has(a));
};

const count = (db: Database, sql: string): number => Number((db.prepare(sql).get() as { n: number }).n);

export const inspect = (db: Database, runtime: RuntimeInfo, aspects: Aspect[]): Inspection => {
  let databaseOk = true;
  try {
    db.prepare("SELECT 1 AS ok").get();
  } catch {
    databaseOk = false;
  }
  const sqliteVersion = databaseOk
    ? String((db.prepare("SELECT sqlite_version() AS v").get() as { v: string }).v)
    : "unknown";
  const facts = (aspect: Aspect): Fact[] => {
    switch (aspect) {
      case "versions":
        return [
          text("application_version", APPLICATION_VERSION),
          integer("contract_version", CONTRACT_VERSION),
          text("node_version", process.version),
          text("sqlite_version", sqliteVersion),
        ];
      case "modules":
        return [
          text("mcp_server", "enabled"),
          text("http_inspection", "enabled"),
          text("test_support", runtime.testSupport ? "enabled" : "disabled"),
          text("context_token_minting", runtime.tokenKeyId === null ? "disabled" : "enabled"),
        ];
      case "dependencies":
        return [
          text("database", databaseOk ? "available" : "unavailable"),
          text("notification_outbox", "synthetic outbox table; nothing leaves the process"),
          text("external_services", "none"),
        ];
      case "health":
        return [text("status", databaseOk ? "ok" : "unavailable")];
      case "scopes":
        return [
          text("tenant_meaning", "clinic group"),
          text("workspace_meaning", "clinic site"),
          text("roles", ROLES.join(", ")),
          integer("tenant_count", databaseOk ? count(db, "SELECT COUNT(*) AS n FROM tenants") : 0),
          integer(
            "disposable_tenant_count",
            databaseOk ? count(db, "SELECT COUNT(*) AS n FROM tenants WHERE disposable = 1") : 0,
          ),
        ];
      case "credentials":
        return [
          text("mcp_credential_ref", "cred:clinic-mcp"),
          bool("mcp_credential_required", runtime.credentialRequired),
          text("context_token_key_id", runtime.tokenKeyId ?? "none"),
        ];
      case "limits":
        return [
          integer("max_request_bytes", MAX_BODY_BYTES),
          integer("max_ahead_days", MAX_AHEAD_DAYS),
          integer("min_lead_minutes", MIN_LEAD_MINUTES),
          integer("idempotency_window_seconds", IDEMPOTENCY_WINDOW_SECONDS),
          integer("token_max_lifetime_seconds", MAX_LIFETIME_SECONDS),
        ];
      case "storage":
        return [
          text("database_kind", runtime.databasePath === ":memory:" ? "memory" : "file"),
          text("database_location", runtime.databasePath),
        ];
      case "network":
        return [
          text("bind_address", runtime.host),
          integer("listen_port", runtime.port),
          text("outbound_connections", "none"),
        ];
    }
  };
  return { reports: aspects.map((aspect) => ({ aspect, state: "reported", facts: facts(aspect) })) };
};
