import { randomBytes } from "node:crypto";

// Host-chosen identifiers: a short type prefix and random lower-case hex. They
// are opaque and unguessable, so an identifier from another scope tells an
// attacker nothing and cannot be enumerated.
export const newId = (prefix: string): string => `${prefix}_${randomBytes(10).toString("hex")}`;

// Crockford base32 without I, L, O and U: readable aloud at a desk.
const REFERENCE_ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

// A short reference a patient can quote ("APT-7K3Q9X").
export const newReference = (): string => {
  const bytes = randomBytes(6);
  let out = "";
  for (const byte of bytes) {
    out += REFERENCE_ALPHABET[byte % REFERENCE_ALPHABET.length];
  }
  return `APT-${out}`;
};

const IDENTIFIER_PATTERN = /^[^\u0000-\u0020\u007f-\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000*]+$/u;

// The common identifier grammar of the AlixAI schemas: 1 to 128 characters, no
// whitespace, no control characters and no wildcard.
export const isIdentifier = (value: unknown): value is string =>
  typeof value === "string" && value.length >= 1 && [...value].length <= 128 && IDENTIFIER_PATTERN.test(value);

export const IDEMPOTENCY_KEY_PATTERN = /^[A-Za-z0-9_-]{16,128}$/;
