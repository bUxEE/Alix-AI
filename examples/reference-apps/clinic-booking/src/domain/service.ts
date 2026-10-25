import type { Clock } from "../clock.ts";
import type { Database } from "../db.ts";
import { isRefusal } from "../errors.ts";
import { rejectedResult, unavailableResult, type HostResult } from "../results.ts";
import {
  commitAction,
  prepareAction,
  statusAction,
  validateAction,
  type ActionDefinition,
  type Env,
  type Loaded,
} from "./actions.ts";
import { bookAppointment, cancelAppointment, updateContact } from "./capabilities.ts";
import {
  availabilityList,
  patientsFind,
  resolveAppointment,
  resolveDoctor,
  resolvePatient,
  resolveSlot,
} from "./read.ts";

// The one place a thrown refusal becomes a typed result. Domain code throws a
// refusal to stop early; nothing but a DeterministicResult leaves the
// application. A failure the host cannot attribute to the request is reported
// as unavailable, with a generic message: a stack trace or a SQL error is
// never part of a response. Every write runs inside one transaction, so by the
// time this catches anything, nothing was applied.
const guard = (operation: () => HostResult, onInternal: (error: unknown) => void): HostResult => {
  try {
    return operation();
  } catch (error) {
    if (isRefusal(error)) {
      const { code, message, field, freshness } = error.refusal;
      return rejectedResult(code, message, {
        ...(field === undefined ? {} : { field }),
        ...(freshness === undefined ? {} : { freshness }),
      });
    }
    onInternal(error);
    return unavailableResult("internal", "the clinic application could not complete the request");
  }
};

export type ActionOperations = {
  validate: (identity: unknown, fields: unknown) => HostResult;
  prepare: (identity: unknown, fields: unknown) => HostResult;
  commit: (identity: unknown, idempotencyKey: unknown, prepared: unknown) => HostResult;
  status: (identity: unknown, idempotencyKey: unknown) => HostResult;
};

export type ClinicService = {
  env: Env;
  availabilityList: (identity: unknown, fields: unknown) => HostResult;
  patientsFind: (identity: unknown, fields: unknown) => HostResult;
  resolvePatient: (identity: unknown, fields: unknown) => HostResult;
  resolveDoctor: (identity: unknown, fields: unknown) => HostResult;
  resolveSlot: (identity: unknown, fields: unknown) => HostResult;
  resolveAppointment: (identity: unknown, fields: unknown) => HostResult;
  book: ActionOperations;
  cancel: ActionOperations;
  contact: ActionOperations;
};

export type ServiceOptions = {
  db: Database;
  clock: Clock;
  // Receives failures the host could not attribute to the request (never
  // refusals). The default writes one redacted line to stderr.
  onInternalError?: (error: unknown) => void;
};

const defaultOnInternal = (error: unknown): void => {
  const name = error instanceof Error ? error.name : "unknown";
  process.stderr.write(`${JSON.stringify({ level: "error", event: "internal_error", error: name })}\n`);
};

export const createClinicService = (options: ServiceOptions): ClinicService => {
  const env: Env = { db: options.db, clock: options.clock };
  const onInternal = options.onInternalError ?? defaultOnInternal;
  const run = (operation: () => HostResult): HostResult => guard(operation, onInternal);

  const actions = <L extends Loaded>(def: ActionDefinition<L>): ActionOperations => ({
    validate: (identity, fields) => run(() => validateAction(def, env, identity, fields)),
    prepare: (identity, fields) => run(() => prepareAction(def, env, identity, fields)),
    commit: (identity, idempotencyKey, prepared) =>
      run(() => commitAction(def, env, identity, { idempotency_key: idempotencyKey, prepared })),
    status: (identity, idempotencyKey) =>
      run(() => statusAction(def, env, identity, { idempotency_key: idempotencyKey })),
  });

  return {
    env,
    availabilityList: (identity, fields) => run(() => availabilityList(env.db, env.clock, identity, fields)),
    patientsFind: (identity, fields) => run(() => patientsFind(env.db, env.clock, identity, fields)),
    resolvePatient: (identity, fields) => run(() => resolvePatient(env.db, env.clock, identity, fields)),
    resolveDoctor: (identity, fields) => run(() => resolveDoctor(env.db, env.clock, identity, fields)),
    resolveSlot: (identity, fields) => run(() => resolveSlot(env.db, env.clock, identity, fields)),
    resolveAppointment: (identity, fields) => run(() => resolveAppointment(env.db, env.clock, identity, fields)),
    book: actions(bookAppointment),
    cancel: actions(cancelAppointment),
    contact: actions(updateContact),
  };
};
