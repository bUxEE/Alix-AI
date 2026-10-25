import { createPrivateKey, createPublicKey, randomBytes, sign, type KeyObject } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

import type { Clock } from "./clock.ts";
import { isIdentifier } from "./ids.ts";

// The application context token of ADR 0013, minted the way a host application
// mints it: JWS compact serialisation, Ed25519, `node:crypto` only (a host never
// needs an AlixAI SDK to be integrated).
//
// The profile is normative and closed:
//   header   exactly alg, typ, kid
//   claims   exactly iss, aud, sub, application_id, tenant_id, workspace_id,
//            roles, capability_scopes, session_id, iat, nbf, exp, jti
//   no deployment_id claim (the deployment level comes from AlixAI's own
//   configuration), a lifetime of at most 15 minutes, a fresh single-use jti
//   for every token, base64url without padding, at most 8 KiB.

export const TOKEN_ALGORITHM = "EdDSA";
export const TOKEN_TYPE = "alixai-context+jwt";
export const MAX_LIFETIME_SECONDS = 900;
export const DEFAULT_LIFETIME_SECONDS = 300;
export const MAX_TOKEN_BYTES = 8 * 1024;
export const MAX_ROLES = 64;
export const MAX_CAPABILITY_SCOPES = 256;

// The TEST-ONLY publisher key published in tools/devkeys. Anyone can sign with
// it, so no real deployment ever trusts it, and this module refuses to use it
// unless the caller says outright that this is a test setup.
export const TEST_ONLY_KEY_ID = "alixai-test-only-publisher";
const DEV_KEY_RELATIVE = join("tools", "devkeys", "test-only-publisher.key.pem");

export type TokenIssuer = {
  privateKey: KeyObject;
  keyId: string;
  issuer: string;
  audience: string;
  applicationId: string;
  clock: Clock;
};

export type ContextClaims = {
  principalId: string;
  tenantId: string;
  workspaceId: string;
  roles: string[];
  capabilityScopes: string[];
  sessionId: string;
  lifetimeSeconds?: number;
};

export type MintedToken = {
  token: string;
  jti: string;
  issuedAt: number;
  expiresAt: number;
};

// Looks for tools/devkeys/test-only-publisher.key.pem in this directory and its
// ancestors, so the app finds the one published key wherever the repository is
// checked out. An explicit path always wins.
export const findDevKeyFile = (startDirectory: string = import.meta.dirname): string => {
  let current = resolve(startDirectory);
  for (;;) {
    const candidate = join(current, DEV_KEY_RELATIVE);
    if (existsSync(candidate)) {
      return candidate;
    }
    const parent = dirname(current);
    if (parent === current) {
      throw new Error(`the TEST-ONLY dev key ${DEV_KEY_RELATIVE} was not found above ${startDirectory}`);
    }
    current = parent;
  }
};

// The file carries a warning before the PEM block; OpenSSL skips text before
// the BEGIN line.
export const loadPrivateKey = (pemPath: string): KeyObject => createPrivateKey(readFileSync(pemPath));

// The raw 32-byte public key, standard base64, as tools/devkeys/*.pub carries it.
export const publicKeyBase64 = (privateKey: KeyObject): string =>
  Buffer.from(createPublicKey(privateKey).export({ format: "jwk" }).x ?? "", "base64url").toString("base64");

export class TokenRefused extends Error {}

const refused = (message: string): TokenRefused => new TokenRefused(message);

const base64url = (data: Buffer | string): string => Buffer.from(data).toString("base64url");

const newTokenId = (): string => base64url(randomBytes(16));

const checkIdentifier = (name: string, value: string): void => {
  if (!isIdentifier(value)) {
    throw refused(`${name} must be a valid scope identifier (1 to 128 characters, no whitespace, no wildcard)`);
  }
};

export const mintContextToken = (issuer: TokenIssuer, claims: ContextClaims): MintedToken => {
  checkIdentifier("sub", claims.principalId);
  checkIdentifier("tenant_id", claims.tenantId);
  checkIdentifier("workspace_id", claims.workspaceId);
  // session_id is a required scope level: a token without one authenticates
  // nothing.
  if (typeof claims.sessionId !== "string" || claims.sessionId.trim() === "") {
    throw refused("session_id is required");
  }
  checkIdentifier("session_id", claims.sessionId);
  const roles = [...new Set(claims.roles)];
  if (roles.length === 0 || roles.length > MAX_ROLES) {
    throw refused(`roles must hold between 1 and ${MAX_ROLES} entries`);
  }
  roles.forEach((role) => checkIdentifier("role", role));
  const scopes = [...new Set(claims.capabilityScopes)];
  if (scopes.length === 0 || scopes.length > MAX_CAPABILITY_SCOPES) {
    throw refused(`capability_scopes must hold between 1 and ${MAX_CAPABILITY_SCOPES} entries`);
  }
  scopes.forEach((scope) => checkIdentifier("capability scope", scope));

  const lifetime = claims.lifetimeSeconds ?? DEFAULT_LIFETIME_SECONDS;
  if (!Number.isInteger(lifetime) || lifetime < 1 || lifetime > MAX_LIFETIME_SECONDS) {
    throw refused(`the lifetime must be a whole number of seconds from 1 to ${MAX_LIFETIME_SECONDS}`);
  }
  if (!/^[A-Za-z0-9._-]{1,64}$/u.test(issuer.keyId)) {
    throw refused("kid must be 1 to 64 characters of A-Z a-z 0-9 . _ -");
  }

  const issuedAt = Math.floor(issuer.clock.now().getTime() / 1000);
  const jti = newTokenId();
  // Member order is fixed so the same inputs always produce the same bytes
  // (apart from the jti). Header: exactly alg, typ, kid.
  const header = { alg: TOKEN_ALGORITHM, typ: TOKEN_TYPE, kid: issuer.keyId };
  const payload = {
    iss: issuer.issuer,
    aud: issuer.audience,
    sub: claims.principalId,
    application_id: issuer.applicationId,
    tenant_id: claims.tenantId,
    workspace_id: claims.workspaceId,
    roles,
    capability_scopes: scopes,
    session_id: claims.sessionId,
    iat: issuedAt,
    nbf: issuedAt,
    exp: issuedAt + lifetime,
    jti,
  };
  const signingInput = `${base64url(JSON.stringify(header))}.${base64url(JSON.stringify(payload))}`;
  const signature = sign(null, Buffer.from(signingInput, "ascii"), issuer.privateKey);
  const token = `${signingInput}.${base64url(signature)}`;
  if (Buffer.byteLength(token) > MAX_TOKEN_BYTES) {
    throw refused("the token would exceed 8 KiB");
  }
  return { token, jti, issuedAt, expiresAt: issuedAt + lifetime };
};

// A token issuer for the fixture: the TEST-ONLY dev key, allowed only when the
// application was started in test support mode.
export const createTestIssuer = (options: {
  allowTestOnlyKey: boolean;
  keyFile?: string;
  keyId?: string;
  issuer: string;
  audience: string;
  applicationId: string;
  clock: Clock;
}): TokenIssuer => {
  const keyId = options.keyId ?? TEST_ONLY_KEY_ID;
  if (keyId === TEST_ONLY_KEY_ID && !options.allowTestOnlyKey) {
    throw refused("the TEST-ONLY key may only be used when test support is enabled");
  }
  return {
    privateKey: loadPrivateKey(options.keyFile ?? findDevKeyFile()),
    keyId,
    issuer: options.issuer,
    audience: options.audience,
    applicationId: options.applicationId,
    clock: options.clock,
  };
};
