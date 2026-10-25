import type { ActionReceipt, PreparedAction } from "./gen/actions/index.js";
import type {
  ActionOutcome,
  Application,
  CapabilityList,
  Health,
  Liveness,
  ReadyReport,
  Registration,
  Session,
  SessionEnded,
} from "./api-types.js";

// Shallow response guards. The hand-written response types of api-types.ts are a promise about the
// server's JSON; these check that the members each type requires are present with the right JSON
// type before the data is handed to the host, so a proxy's `{}` or an HTML page that parsed as
// something is a typed error instead of an `undefined` member three calls later. They are
// deliberately not a validator: unknown members are tolerated (additive server changes), and the
// schemas under schemas/ stay the authority for the deep structure.

export type Check = (value: unknown) => boolean;

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

export const string: Check = (v) => typeof v === "string";
export const boolean: Check = (v) => typeof v === "boolean";
export const integer: Check = (v) => typeof v === "number" && Number.isInteger(v);
export const array: Check = (v) => Array.isArray(v);
export const object: Check = (v) => isRecord(v);
export const oneOf =
  (...values: readonly string[]): Check =>
  (v) =>
    typeof v === "string" && values.includes(v);
export const nullable =
  (check: Check): Check =>
  (v) =>
    v === null || check(v);
export const arrayOf =
  (check: Check): Check =>
  (v) =>
    Array.isArray(v) && v.every(check);

/** An object with every `required` member passing its check and every `optional` member passing when present. */
export const shape =
  (required: Record<string, Check>, optional: Record<string, Check> = {}): Check =>
  (value) => {
    if (!isRecord(value)) return false;
    for (const [key, check] of Object.entries(required)) if (!check(value[key])) return false;
    for (const [key, check] of Object.entries(optional)) if (value[key] !== undefined && !check(value[key])) return false;
    return true;
  };

const applicationStatus = oneOf("active", "quarantined", "rejected", "superseded", "not_installed");
const readiness = oneOf("ok", "degraded", "unavailable");

const pendingState = shape(
  { kind: oneOf("confirmation", "clarification", "goal"), capability_id: string, phase: string, collected_fields: array },
  { missing_fields: array, question: object, preparation: object, expires_at: string },
);

const packageRecord = shape({
  manifest_hash: string,
  package_version: string,
  state: oneOf("quarantined", "active", "rejected", "superseded"),
  warnings: integer,
  installed_at: string,
  status_changed_at: string,
});

const readyReport = shape({ status: readiness, ready: boolean, roles: array, checks: array });

const preparedAction = shape({ preparation_id: string, capability_id: string, state: string, expires_at: string, confirmation: object });

const checks = {
  session: shape({
    session_id: string,
    status: oneOf("active", "ended"),
    turn_count: integer,
    created_at: string,
    updated_at: string,
    pending: nullable(pendingState),
  }),
  sessionEnded: shape({ session_id: string, status: oneOf("ended") }),
  pending: shape({ pending: nullable(pendingState) }),
  preparedAction,
  receipt: shape({ receipt_id: string, preparation_id: string, capability_id: string, status: string, audit_ref: string }),
  // A 200 without a receipt is not a success: the receipt is the durable record of the commit.
  actionOutcome: shape({ receipt: object }, { preparation: preparedAction }),
  application: shape({ application_id: string, status: applicationStatus, packages: arrayOf(packageRecord) }),
  registration: shape({ application_id: string, package: packageRecord, created: boolean, findings: array }),
  capabilityList: shape({ capabilities: arrayOf(shape({ id: string, risk_level: integer, requires_confirmation: boolean, requires_step_up: boolean, fields: array })) }),
  readyReport,
  health: shape({
    status: readiness,
    ready: boolean,
    roles: array,
    checks: array,
    application: shape({ application_id: string, status: applicationStatus }),
  }),
  liveness: shape({ status: oneOf("ok") }),
};

export const isSession = checks.session as (v: unknown) => v is Session;
export const isSessionEnded = checks.sessionEnded as (v: unknown) => v is SessionEnded;
export const isPendingResponse = checks.pending as (v: unknown) => v is { pending: Session["pending"] };
export const isPreparedAction = checks.preparedAction as (v: unknown) => v is PreparedAction;
export const isActionReceipt = checks.receipt as (v: unknown) => v is ActionReceipt;
export const isActionOutcome = checks.actionOutcome as (v: unknown) => v is ActionOutcome;
export const isApplication = checks.application as (v: unknown) => v is Application;
export const isRegistration = checks.registration as (v: unknown) => v is Registration;
export const isCapabilityList = checks.capabilityList as (v: unknown) => v is CapabilityList;
export const isReadyReport = checks.readyReport as (v: unknown) => v is ReadyReport;
export const isHealth = checks.health as (v: unknown) => v is Health;
export const isLiveness = checks.liveness as (v: unknown) => v is Liveness;
