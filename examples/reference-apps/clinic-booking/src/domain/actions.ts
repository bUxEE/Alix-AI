import { createHash } from "node:crypto";

import { resolveCaller, type Caller, type Permission } from "../access.ts";
import { toTimestamp, type Clock } from "../clock.ts";
import { inTransaction, type Database } from "../db.ts";
import { isRefusal, refuse, type Refusal } from "../errors.ts";
import { IDEMPOTENCY_KEY_PATTERN } from "../ids.ts";
import {
  okResult,
  rejectedResult,
  snapshotVersion,
  type DataVersion,
  type Effect,
  type Fact,
  type FollowUp,
  type HostResult,
  type NextAction,
  type RecordRef,
} from "../results.ts";
import { IDEMPOTENCY_WINDOW_SECONDS } from "./rules.ts";
import { parseFields, type RawFields } from "./fields.ts";

// The prepare / validate / commit / status protocol every consequential
// capability shares (ADR 0006). A capability supplies what is particular to
// it; this module supplies what must never differ between them:
//
//   * the host reauthorises every call from the identity it receives;
//   * validate and prepare run the same checks and report the same snapshot
//     version on unchanged data;
//   * commit applies exactly once per idempotency key, answers a repeat with
//     the original reply, refuses a preparation whose data moved (`stale`) or
//     whose time ran out (`expired`), and re-checks every rule before it writes;
//   * status says what was recorded for a key, and "nothing" in the one shape
//     that proves nothing was applied.

export type Env = { db: Database; clock: Clock };

export type Loaded = {
  // The records the action relied on, each with its own revision. Their
  // snapshot is the `freshness.version` of validate, prepare and commit.
  versions: DataVersion[];
};

export type Description = {
  values: Fact[];
  summary: string;
  effects: Effect[];
  nextActionsWhenPrepared: NextAction[];
};

export type Applied = {
  identifiers: RecordRef[];
  values: Fact[];
  // The same records after the write, for the receipt's freshness.
  versionsAfter: DataVersion[];
  followUp?: FollowUp;
  nextActions?: NextAction[];
};

export type ActionDefinition<L extends Loaded> = {
  capabilityId: string;
  permission: Permission;
  fieldNames: readonly string[];
  declaredRiskLevel: 1 | 2 | 3;
  validForSeconds: number;
  // Parse the candidate fields and read the records they name, inside the
  // caller's scope. Missing fields, malformed values and records the caller
  // may not see are refused here.
  load: (env: Env, caller: Caller, fields: RawFields) => L;
  // The business rules. They run on validate, prepare and again on commit.
  check: (env: Env, caller: Caller, loaded: L) => void;
  describe: (env: Env, caller: Caller, loaded: L) => Description;
  // The candidate fields the exact effects of a prepared action stand for.
  fieldsFromEffects: (effects: PreparedEffect[]) => RawFields;
  // Write. Runs inside the commit transaction, after every check passed.
  apply: (env: Env, caller: Caller, loaded: L) => Applied;
};

// ---- the prepared action the core hands back at commit ----------------------

export type PreparedEffect = {
  operation: string;
  entity_type: string;
  entity_id?: string;
  changes: Record<string, string | number | boolean>;
};

export type PreparedInput = {
  effects: PreparedEffect[];
  dataVersions: DataVersion[];
  expiresAt?: string;
  capabilityId?: string;
};

const isObject = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const bad = (message: string) => refuse("validation_failed", message, { field: "prepared" });

// The core sends the whole PreparedAction. The host reads only what it needs
// and treats all of it as untrusted: it re-derives the fields from the effects
// and runs every check again.
export const parsePrepared = (raw: unknown): PreparedInput => {
  if (!isObject(raw)) {
    throw bad("the prepared action is missing");
  }
  const effectsRaw = raw.effects;
  const versionsRaw = raw.data_versions;
  if (!Array.isArray(effectsRaw) || effectsRaw.length === 0 || !Array.isArray(versionsRaw)) {
    throw bad("the prepared action is incomplete");
  }
  const effects = effectsRaw.map((item): PreparedEffect => {
    if (!isObject(item) || typeof item.operation !== "string" || typeof item.entity_type !== "string") {
      throw bad("an effect of the prepared action is malformed");
    }
    const changes: PreparedEffect["changes"] = {};
    if (Array.isArray(item.changes)) {
      for (const change of item.changes) {
        if (isObject(change) && typeof change.field === "string" && isObject(change.value)) {
          const value = change.value.value;
          if (typeof value === "string" || typeof value === "number" || typeof value === "boolean") {
            changes[change.field] = value;
          }
        }
      }
    }
    return {
      operation: item.operation,
      entity_type: item.entity_type,
      ...(typeof item.entity_id === "string" ? { entity_id: item.entity_id } : {}),
      changes,
    };
  });
  const dataVersions = versionsRaw.map((item): DataVersion => {
    if (
      !isObject(item) ||
      typeof item.entity_type !== "string" ||
      typeof item.entity_id !== "string" ||
      typeof item.version !== "string"
    ) {
      throw bad("a data version of the prepared action is malformed");
    }
    return { entity_type: item.entity_type, entity_id: item.entity_id, version: item.version };
  });
  return {
    effects,
    dataVersions,
    ...(typeof raw.expires_at === "string" ? { expiresAt: raw.expires_at } : {}),
    ...(typeof raw.capability_id === "string" ? { capabilityId: raw.capability_id } : {}),
  };
};

// ---- helpers -----------------------------------------------------------------

const freshnessOf = (env: Env, versions: DataVersion[]) => ({
  as_of: toTimestamp(env.clock.now()),
  version: snapshotVersion(versions),
});

// Once the records were read, every refusal carries the snapshot version the
// host saw, so a caller can tell "the data moved" from "the rule failed".
const withFreshness = (env: Env, versions: DataVersion[], error: Refusal): Refusal => {
  if (error.refusal.freshness === undefined) {
    error.refusal.freshness = freshnessOf(env, versions);
  }
  return error;
};

const checked = <L extends Loaded>(def: ActionDefinition<L>, env: Env, caller: Caller, loaded: L): void => {
  try {
    def.check(env, caller, loaded);
  } catch (error) {
    throw isRefusal(error) ? withFreshness(env, loaded.versions, error) : error;
  }
};

const canonicalJson = (value: unknown): string => {
  if (Array.isArray(value)) {
    return `[${value.map(canonicalJson).join(",")}]`;
  }
  if (isObject(value)) {
    return `{${Object.keys(value)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${canonicalJson(value[key])}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
};

const fingerprintOf = (capabilityId: string, caller: Caller, fields: RawFields): string =>
  createHash("sha256")
    .update(canonicalJson({ capability: capabilityId, principal: caller.principalId, fields }))
    .digest("hex");

const requireKey = (key: unknown): string => {
  if (typeof key !== "string" || !IDEMPOTENCY_KEY_PATTERN.test(key)) {
    throw refuse("validation_failed", "the idempotency key must be 16 to 128 URL-safe characters", {
      field: "idempotency_key",
    });
  }
  return key;
};

type IdempotencyRow = {
  capability_id: string;
  principal_id: string;
  fingerprint: string;
  response_json: string;
};

const storedKey = (env: Env, caller: Caller, key: string): IdempotencyRow | undefined =>
  env.db
    .prepare(
      "SELECT capability_id, principal_id, fingerprint, response_json FROM idempotency WHERE tenant_id = ? AND site_id = ? AND key = ?",
    )
    .get(caller.tenantId, caller.siteId, key) as IdempotencyRow | undefined;

// ---- the four operations -----------------------------------------------------

const evaluate = <L extends Loaded>(
  def: ActionDefinition<L>,
  env: Env,
  identity: unknown,
  rawFields: unknown,
): { caller: Caller; loaded: L; description: Description } => {
  const caller = resolveCaller(env.db, identity, def.permission);
  const fields = parseFields(rawFields, def.fieldNames);
  const loaded = def.load(env, caller, fields);
  checked(def, env, caller, loaded);
  return { caller, loaded, description: def.describe(env, caller, loaded) };
};

// Validate checks candidate values and stages nothing. On unchanged data its
// freshness.version is exactly the one Prepare stored: both are the snapshot of
// the same records computed by the same function, never a response ETag or a
// counter shared by the site.
export const validateAction = <L extends Loaded>(
  def: ActionDefinition<L>,
  env: Env,
  identity: unknown,
  rawFields: unknown,
): HostResult => {
  const { loaded, description } = evaluate(def, env, identity, rawFields);
  return okResult({ values: description.values, freshness: freshnessOf(env, loaded.versions) });
};

export const prepareAction = <L extends Loaded>(
  def: ActionDefinition<L>,
  env: Env,
  identity: unknown,
  rawFields: unknown,
): HostResult => {
  const { loaded, description } = evaluate(def, env, identity, rawFields);
  return okResult({
    values: description.values,
    freshness: freshnessOf(env, loaded.versions),
    allowed_next_actions: description.nextActionsWhenPrepared,
    preparation: {
      summary: description.summary,
      effects: description.effects,
      data_versions: loaded.versions,
      declared_risk_level: def.declaredRiskLevel,
      valid_for_seconds: def.validForSeconds,
    },
  });
};

export const commitAction = <L extends Loaded>(
  def: ActionDefinition<L>,
  env: Env,
  identity: unknown,
  args: { idempotency_key: unknown; prepared: unknown },
): HostResult => {
  const caller = resolveCaller(env.db, identity, def.permission);
  const key = requireKey(args.idempotency_key);
  sweepIdempotency(env);
  const prepared = parsePrepared(args.prepared);
  if (prepared.capabilityId !== undefined && prepared.capabilityId !== def.capabilityId) {
    throw refuse("validation_failed", "the prepared action is for another capability", { field: "prepared" });
  }
  const fields = def.fieldsFromEffects(prepared.effects);
  const fingerprint = fingerprintOf(def.capabilityId, caller, fields);

  // A repeat answers with the original reply and changes nothing. The same key
  // for anything else is a conflict, never a second effect.
  const previous = storedKey(env, caller, key);
  if (previous !== undefined) {
    if (
      previous.capability_id !== def.capabilityId ||
      previous.principal_id !== caller.principalId ||
      previous.fingerprint !== fingerprint
    ) {
      throw refuse("conflict", "the idempotency key was already used for a different action", {
        field: "idempotency_key",
      });
    }
    return JSON.parse(previous.response_json) as HostResult;
  }

  const now = env.clock.now();
  if (prepared.expiresAt !== undefined && now.getTime() >= Date.parse(prepared.expiresAt)) {
    throw refuse("expired", "the prepared action expired before it was committed", { field: "prepared" });
  }

  return inTransaction(env.db, () => {
    const loaded = def.load(env, caller, fields);
    // First the data the user confirmed, then the rules: a record that moved
    // is `stale`, whatever else changed with it.
    for (const current of loaded.versions) {
      const seen = prepared.dataVersions.find(
        (v) => v.entity_type === current.entity_type && v.entity_id === current.entity_id,
      );
      if (seen === undefined) {
        throw refuse("validation_failed", "the prepared action does not record the data it relied on", {
          field: "prepared",
        });
      }
      if (seen.version !== current.version) {
        throw refuse("stale", "a record changed after the action was prepared", {
          freshness: freshnessOf(env, loaded.versions),
        });
      }
    }
    checked(def, env, caller, loaded);
    const applied = def.apply(env, caller, loaded);
    const result = okResult({
      identifiers: applied.identifiers,
      values: applied.values,
      freshness: freshnessOf(env, applied.versionsAfter),
      ...(applied.followUp === undefined ? {} : { follow_up: applied.followUp }),
      ...(applied.nextActions === undefined ? {} : { allowed_next_actions: applied.nextActions }),
    });
    env.db
      .prepare(
        `INSERT INTO idempotency (tenant_id, site_id, key, capability_id, principal_id, fingerprint, response_json, created_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
      )
      .run(
        caller.tenantId,
        caller.siteId,
        key,
        def.capabilityId,
        caller.principalId,
        fingerprint,
        JSON.stringify(result),
        toTimestamp(now),
      );
    return result;
  });
};

// What became of the commit sent with this key. Exactly one answer proves that
// nothing was applied: rejected, one validation_failed on idempotency_key, no
// identifiers. Every other answer is a recorded outcome or a refusal, and a
// refusal says nothing about whether the commit happened.
export const statusAction = <L extends Loaded>(
  def: ActionDefinition<L>,
  env: Env,
  identity: unknown,
  args: { idempotency_key: unknown },
): HostResult => {
  const caller = resolveCaller(env.db, identity, def.permission);
  const key = requireKey(args.idempotency_key);
  const stored = storedKey(env, caller, key);
  if (stored === undefined) {
    return rejectedResult("validation_failed", "the application has no record of this idempotency key", {
      field: "idempotency_key",
    });
  }
  if (stored.principal_id !== caller.principalId) {
    throw refuse("permission_denied", "the idempotency key belongs to another principal");
  }
  if (stored.capability_id !== def.capabilityId) {
    throw refuse("conflict", "the idempotency key was used for another capability", { field: "idempotency_key" });
  }
  return JSON.parse(stored.response_json) as HostResult;
};

// The records are remembered for as long as the key is: the window the package
// declares. A sweep removes keys older than that.
export const sweepIdempotency = (env: Env): number => {
  const cutoff = toTimestamp(new Date(env.clock.now().getTime() - IDEMPOTENCY_WINDOW_SECONDS * 1000));
  return Number(env.db.prepare("DELETE FROM idempotency WHERE created_at < ?").run(cutoff).changes);
};
