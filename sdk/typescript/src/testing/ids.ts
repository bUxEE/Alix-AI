// Identifier and secret generation for the fake server, from the platform's CSPRNG (Web Crypto,
// present in Node 20+ and browsers). No key material and no fixed secret lives in this package.

const CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

const randomBytes = (length: number): Uint8Array => globalThis.crypto.getRandomValues(new Uint8Array(length));

/** A 26-character ULID (Crockford base32), time first, as the core mints them. */
export const ulid = (now: number): string => {
  let time = "";
  let t = Math.floor(now);
  for (let i = 0; i < 10; i++) {
    time = CROCKFORD.charAt(t % 32) + time;
    t = Math.floor(t / 32);
  }
  let random = "";
  for (const byte of randomBytes(16)) random += CROCKFORD.charAt(byte & 31);
  return time + random;
};

export const typedId = (prefix: string, now: number): string => `${prefix}_${ulid(now)}`;

export const base64url = (bytes: Uint8Array): string => {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
};

export const base64urlText = (text: string): string => base64url(new TextEncoder().encode(text));

export const decodeBase64urlText = (segment: string): string => {
  const padded = segment.replace(/-/g, "+").replace(/_/g, "/") + "=".repeat((4 - (segment.length % 4)) % 4);
  const binary = atob(padded);
  return new TextDecoder().decode(Uint8Array.from(binary, (c) => c.charCodeAt(0)));
};

/** A confirmation secret: "cfm_" and 43 base64url characters (256 random bits). */
export const confirmationSecret = (): string => `cfm_${base64url(randomBytes(32))}`;

/** An idempotency key: 16 to 128 URL-safe characters. */
export const idempotencyKey = (): string => base64url(randomBytes(24));

export const randomSegment = (bytes: number): string => base64url(randomBytes(bytes));
