import { refuse } from "../errors.ts";
import { isCalendarDate } from "../time.ts";

// Candidate field values arrive from the model through AlixAI, so they are
// untrusted: every one is checked here, and a value that is absent, of the
// wrong type or outside its range is a typed failure naming the field, never a
// coerced guess.

export type RawFields = Record<string, unknown>;

// Control characters have no place in any field value.
// eslint-disable-next-line no-control-regex
const CONTROL_CHARACTERS = /[\u0000-\u001f\u007f]/u;

export const parseFields = (raw: unknown, allowed: readonly string[]): RawFields => {
  if (raw === undefined || raw === null) {
    return {};
  }
  if (typeof raw !== "object" || Array.isArray(raw)) {
    throw refuse("validation_failed", "fields must be an object");
  }
  const fields = raw as RawFields;
  for (const key of Object.keys(fields)) {
    if (!allowed.includes(key)) {
      throw refuse("validation_failed", `${key} is not a field of this capability`, { field: key });
    }
  }
  return fields;
};

const isAbsent = (value: unknown): boolean => value === undefined || value === null;

type StringRule = { min?: number; max?: number };

export const requiredString = (fields: RawFields, name: string, rule: StringRule = {}): string => {
  const value = fields[name];
  if (isAbsent(value) || (typeof value === "string" && value.trim() === "")) {
    throw refuse("missing_fields", `${name} is required`, { field: name });
  }
  return checkedString(name, value, rule);
};

export const optionalString = (fields: RawFields, name: string, rule: StringRule = {}): string | undefined => {
  const value = fields[name];
  if (isAbsent(value) || (typeof value === "string" && value.trim() === "")) {
    return undefined;
  }
  return checkedString(name, value, rule);
};

const checkedString = (name: string, value: unknown, rule: StringRule): string => {
  if (typeof value !== "string") {
    throw refuse("validation_failed", `${name} must be text`, { field: name });
  }
  const trimmed = value.trim();
  if (CONTROL_CHARACTERS.test(trimmed)) {
    throw refuse("validation_failed", `${name} contains control characters`, { field: name });
  }
  const length = [...trimmed].length;
  if (rule.min !== undefined && length < rule.min) {
    throw refuse("validation_failed", `${name} must have at least ${rule.min} characters`, { field: name });
  }
  if (rule.max !== undefined && length > rule.max) {
    throw refuse("validation_failed", `${name} must have at most ${rule.max} characters`, { field: name });
  }
  return trimmed;
};

export const optionalInteger = (
  fields: RawFields,
  name: string,
  rule: { min: number; max: number; fallback: number },
): number => {
  const value = fields[name];
  if (isAbsent(value)) {
    return rule.fallback;
  }
  if (typeof value !== "number" || !Number.isInteger(value)) {
    throw refuse("validation_failed", `${name} must be a whole number`, { field: name });
  }
  if (value < rule.min || value > rule.max) {
    throw refuse("validation_failed", `${name} must be between ${rule.min} and ${rule.max}`, { field: name });
  }
  return value;
};

export const optionalDate = (fields: RawFields, name: string): string | undefined => {
  const value = fields[name];
  if (isAbsent(value)) {
    return undefined;
  }
  if (typeof value !== "string" || !isCalendarDate(value)) {
    throw refuse("validation_failed", `${name} must be a calendar date written YYYY-MM-DD`, { field: name });
  }
  return value;
};

export const optionalEnum = <T extends string>(fields: RawFields, name: string, values: readonly T[]): T | undefined => {
  const value = fields[name];
  if (isAbsent(value)) {
    return undefined;
  }
  if (typeof value !== "string" || !values.includes(value as T)) {
    throw refuse("validation_failed", `${name} must be one of: ${values.join(", ")}`, { field: name });
  }
  return value as T;
};

// An entity reference is an identifier of this application. The model may only
// propose the value; the lookup that follows decides whether it names anything
// the caller may see.
export const requiredReference = (fields: RawFields, name: string): string => {
  const value = requiredString(fields, name, { max: 128 });
  if (/[\s*]/u.test(value)) {
    throw refuse("validation_failed", `${name} must be a record identifier`, { field: name });
  }
  return value;
};

export const optionalReference = (fields: RawFields, name: string): string | undefined => {
  const value = optionalString(fields, name, { max: 128 });
  if (value !== undefined && /[\s*]/u.test(value)) {
    throw refuse("validation_failed", `${name} must be a record identifier`, { field: name });
  }
  return value;
};
