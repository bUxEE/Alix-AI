import { randomInt } from "node:crypto";

// Ids of the shapes the contracts ask for: a prefix and 26 characters of Crockford base32 (no I, L, O or U), as the core's
// ULIDs are. The first ten characters carry the time, so ids sort by creation like the real ones.

const CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

const timePart = (ms: number): string => {
  let value = ms;
  let out = "";
  for (let i = 0; i < 10; i += 1) {
    out = (CROCKFORD[value % 32] as string) + out;
    value = Math.floor(value / 32);
  }
  return out;
};

const randomPart = (length: number): string => {
  let out = "";
  for (let i = 0; i < length; i += 1) out += CROCKFORD[randomInt(32)];
  return out;
};

/** A fresh id: `newId("trn")` is `trn_01J…` with 26 characters after the underscore. */
export const newId = (prefix: string, now: number = Date.now()): string => `${prefix}_${timePart(now)}${randomPart(16)}`;

/** A stable id for a fixture: the same `n` always gives the same id. */
export const fixedId = (prefix: string, n: number): string => {
  let value = n;
  let tail = "";
  for (let i = 0; i < 8; i += 1) {
    tail = (CROCKFORD[value % 32] as string) + tail;
    value = Math.floor(value / 32);
  }
  return `${prefix}_01JNWDEM0F1XTVRE${"0".repeat(2)}${tail}`;
};

const BASE64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";

/** A value of the shape of a confirmation secret (`cfm_` and 43 base64url characters). The mock never checks one. */
export const newConfirmationToken = (): string => {
  let out = "cfm_";
  for (let i = 0; i < 43; i += 1) out += BASE64URL[randomInt(64)];
  return out;
};

/** An RFC 3339 UTC timestamp with milliseconds, as the core writes them. */
export const isoAt = (ms: number): string => new Date(ms).toISOString();
