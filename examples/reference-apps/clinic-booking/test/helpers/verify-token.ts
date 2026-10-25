import { createPublicKey, verify } from "node:crypto";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { REPO_ROOT } from "./schemas.ts";

// An independent strict reader of the ADR 0013 token profile, written from the
// ADR and not from the minting code, so a mistake in one does not hide in the
// other. It accepts exactly what internal/identity accepts and nothing else.

export const DEV_PUBLIC_KEY_FILE = join(REPO_ROOT, "tools", "devkeys", "test-only-publisher.pub");

export const devPublicKeyBase64 = (): string => readFileSync(DEV_PUBLIC_KEY_FILE, "utf8").trim();

const RAW_ED25519_SPKI_PREFIX = Buffer.from("302a300506032b6570032100", "hex");

export const publicKeyFromRaw = (rawBase64: string) =>
  createPublicKey({
    key: Buffer.concat([RAW_ED25519_SPKI_PREFIX, Buffer.from(rawBase64, "base64")]),
    format: "der",
    type: "spki",
  });

const HEADER_MEMBERS = ["alg", "kid", "typ"];
const CLAIM_MEMBERS = [
  "application_id",
  "aud",
  "capability_scopes",
  "exp",
  "iat",
  "iss",
  "jti",
  "nbf",
  "roles",
  "session_id",
  "sub",
  "tenant_id",
  "workspace_id",
];

export type VerifiedToken = { header: Record<string, unknown>; claims: Record<string, unknown> };

const strictBase64url = (segment: string): Buffer => {
  if (!/^[A-Za-z0-9_-]+$/u.test(segment)) {
    throw new Error("segment is not unpadded base64url");
  }
  const bytes = Buffer.from(segment, "base64url");
  if (bytes.toString("base64url") !== segment) {
    throw new Error("segment is not the canonical encoding");
  }
  return bytes;
};

const parseObject = (bytes: Buffer): Record<string, unknown> => {
  const parsed: unknown = JSON.parse(bytes.toString("utf8"));
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    throw new Error("not an object");
  }
  return parsed as Record<string, unknown>;
};

export const verifyContextToken = (
  token: string,
  expected: { publicKeyBase64: string; issuer: string; audience: string; applicationId: string; keyId: string; now: Date },
): VerifiedToken => {
  if (Buffer.byteLength(token) > 8 * 1024) {
    throw new Error("token is larger than 8 KiB");
  }
  const segments = token.split(".");
  if (segments.length !== 3) {
    throw new Error("not a compact JWS");
  }
  const [headerB64, payloadB64, signatureB64] = segments as [string, string, string];
  const header = parseObject(strictBase64url(headerB64));
  const claims = parseObject(strictBase64url(payloadB64));
  const signature = strictBase64url(signatureB64);
  if (signature.length !== 64) {
    throw new Error("signature is not 64 bytes");
  }

  if (JSON.stringify(Object.keys(header).sort()) !== JSON.stringify(HEADER_MEMBERS)) {
    throw new Error("header must hold exactly alg, typ and kid");
  }
  if (header.alg !== "EdDSA" || header.typ !== "alixai-context+jwt" || header.kid !== expected.keyId) {
    throw new Error("header values are wrong");
  }
  if (!/^[A-Za-z0-9._-]{1,64}$/u.test(String(header.kid))) {
    throw new Error("kid has an invalid form");
  }

  const ok = verify(
    null,
    Buffer.from(`${headerB64}.${payloadB64}`, "ascii"),
    publicKeyFromRaw(expected.publicKeyBase64),
    signature,
  );
  if (!ok) {
    throw new Error("signature does not verify");
  }

  if (JSON.stringify(Object.keys(claims).sort()) !== JSON.stringify(CLAIM_MEMBERS)) {
    throw new Error(`claims must be exactly ${CLAIM_MEMBERS.join(", ")} (got ${Object.keys(claims).sort().join(", ")})`);
  }
  if ("deployment_id" in claims) {
    throw new Error("a token must not carry deployment_id");
  }
  if (claims.iss !== expected.issuer || claims.aud !== expected.audience || claims.application_id !== expected.applicationId) {
    throw new Error("issuer, audience or application is wrong");
  }
  for (const name of ["sub", "tenant_id", "workspace_id", "session_id"] as const) {
    const value = claims[name];
    if (typeof value !== "string" || value === "" || value.length > 128 || /[\s*]/u.test(value)) {
      throw new Error(`${name} is not a valid scope identifier`);
    }
  }
  for (const [name, max] of [
    ["roles", 64],
    ["capability_scopes", 256],
  ] as const) {
    const value = claims[name];
    if (!Array.isArray(value) || value.length === 0 || value.length > max || new Set(value).size !== value.length) {
      throw new Error(`${name} must be a non-empty set of at most ${max}`);
    }
    if (value.some((v) => typeof v !== "string" || v === "" || /[\s*]/u.test(v))) {
      throw new Error(`${name} holds an invalid entry`);
    }
  }
  for (const name of ["iat", "nbf", "exp"] as const) {
    const value = claims[name];
    if (typeof value !== "number" || !Number.isInteger(value) || value < 0 || String(value).length > 12) {
      throw new Error(`${name} must be a plain non-negative integer`);
    }
  }
  const [iat, nbf, exp] = [claims.iat as number, claims.nbf as number, claims.exp as number];
  if (exp - Math.min(iat, nbf) > 15 * 60) {
    throw new Error("the validity window exceeds 15 minutes");
  }
  const now = Math.floor(expected.now.getTime() / 1000);
  if (iat > now + 30 || nbf > now + 30) {
    throw new Error("not yet valid");
  }
  if (now >= exp + 30) {
    throw new Error("expired");
  }
  if (typeof claims.jti !== "string" || !/^[A-Za-z0-9._-]{16,128}$/u.test(claims.jti)) {
    throw new Error("jti is not 16 to 128 characters of the token alphabet");
  }
  return { header, claims };
};
