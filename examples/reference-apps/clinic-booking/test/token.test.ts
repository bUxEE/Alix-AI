import { createPrivateKey, generateKeyPairSync } from "node:crypto";
import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

import { createMutableClock } from "../src/clock.ts";
import {
  createTestIssuer,
  DEFAULT_LIFETIME_SECONDS,
  findDevKeyFile,
  loadPrivateKey,
  mintContextToken,
  publicKeyBase64,
  TEST_ONLY_KEY_ID,
  TokenRefused,
  type ContextClaims,
  type TokenIssuer,
} from "../src/context-token.ts";
import { devPublicKeyBase64, verifyContextToken } from "./helpers/verify-token.ts";

const clock = createMutableClock("2026-10-05T07:00:00Z");

const issuer = (): TokenIssuer =>
  createTestIssuer({
    allowTestOnlyKey: true,
    issuer: "https://clinic-booking.example.test",
    audience: "alixai",
    applicationId: "clinic-booking",
    clock,
  });

const claims = (overrides: Partial<ContextClaims> = {}): ContextClaims => ({
  principalId: "usr_reception_a1",
  tenantId: "tnt_a",
  workspaceId: "site_a1",
  roles: ["receptionist"],
  capabilityScopes: ["availability.list", "appointments.book"],
  sessionId: "ses_01J0000000000000000000000A",
  ...overrides,
});

const expected = {
  publicKeyBase64: devPublicKeyBase64(),
  issuer: "https://clinic-booking.example.test",
  audience: "alixai",
  applicationId: "clinic-booking",
  keyId: TEST_ONLY_KEY_ID,
  now: clock.now(),
};

describe("the TEST-ONLY dev key", () => {
  it("is the key published in tools/devkeys: the derived public key equals the committed .pub", () => {
    const key = loadPrivateKey(findDevKeyFile());
    expect(publicKeyBase64(key)).toBe(devPublicKeyBase64());
    expect(readFileSync(findDevKeyFile(), "utf8")).toContain("TEST-ONLY KEY. NOT A SECRET");
  });

  it("is refused unless the application was started in test support mode", () => {
    expect(() =>
      createTestIssuer({
        allowTestOnlyKey: false,
        issuer: "https://clinic-booking.example.test",
        audience: "alixai",
        applicationId: "clinic-booking",
        clock,
      }),
    ).toThrow(TokenRefused);
  });
});

describe("minting follows the ADR 0013 profile", () => {
  it("produces a token an independent strict verifier accepts", () => {
    const { token } = mintContextToken(issuer(), claims());
    const { header, claims: payload } = verifyContextToken(token, expected);
    expect(header).toEqual({ alg: "EdDSA", typ: "alixai-context+jwt", kid: "alixai-test-only-publisher" });
    expect(payload).toMatchObject({
      iss: "https://clinic-booking.example.test",
      aud: "alixai",
      sub: "usr_reception_a1",
      application_id: "clinic-booking",
      tenant_id: "tnt_a",
      workspace_id: "site_a1",
      roles: ["receptionist"],
      capability_scopes: ["availability.list", "appointments.book"],
      session_id: "ses_01J0000000000000000000000A",
    });
  });

  it("has exactly the closed set of claims and no deployment_id", () => {
    const { token } = mintContextToken(issuer(), claims());
    const { claims: payload } = verifyContextToken(token, expected);
    expect(Object.keys(payload).sort()).toEqual([
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
    ]);
    expect("deployment_id" in payload).toBe(false);
  });

  it("is signed with EdDSA over header.payload and not by any other key", () => {
    const { token } = mintContextToken(issuer(), claims());
    const other = generateKeyPairSync("ed25519").publicKey.export({ format: "jwk" }).x ?? "";
    const otherRaw = Buffer.from(other, "base64url").toString("base64");
    expect(() => verifyContextToken(token, { ...expected, publicKeyBase64: otherRaw })).toThrow(/signature/u);
  });

  it("detects any tampering with the claims", () => {
    const { token } = mintContextToken(issuer(), claims());
    const [header, payload, signature] = token.split(".") as [string, string, string];
    const parsed = JSON.parse(Buffer.from(payload, "base64url").toString("utf8")) as Record<string, unknown>;
    parsed.tenant_id = "tnt_b";
    const forged = `${header}.${Buffer.from(JSON.stringify(parsed)).toString("base64url")}.${signature}`;
    expect(() => verifyContextToken(forged, expected)).toThrow(/signature/u);
  });

  it("lives 5 minutes by default and never more than 15", () => {
    const { issuedAt, expiresAt } = mintContextToken(issuer(), claims());
    expect(expiresAt - issuedAt).toBe(DEFAULT_LIFETIME_SECONDS);
    expect(DEFAULT_LIFETIME_SECONDS).toBe(300);
    expect(mintContextToken(issuer(), claims({ lifetimeSeconds: 900 })).expiresAt - issuedAt).toBe(900);
    for (const lifetimeSeconds of [901, 1000, 0, -1, 1.5, Number.NaN]) {
      expect(() => mintContextToken(issuer(), claims({ lifetimeSeconds }))).toThrow(TokenRefused);
    }
  });

  it("is expired after its lifetime and not yet valid before it", () => {
    const { token } = mintContextToken(issuer(), claims({ lifetimeSeconds: 60 }));
    expect(() => verifyContextToken(token, { ...expected, now: new Date(clock.now().getTime() + 91_000) })).toThrow(/expired/u);
    expect(() => verifyContextToken(token, { ...expected, now: new Date(clock.now().getTime() - 60_000) })).toThrow(/not yet valid/u);
  });

  it("carries a fresh single-use jti on every token", () => {
    const seen = new Set<string>();
    for (let i = 0; i < 200; i += 1) {
      const { token, jti } = mintContextToken(issuer(), claims());
      expect(jti.length).toBeGreaterThanOrEqual(16);
      expect(jti).toMatch(/^[A-Za-z0-9._-]{16,128}$/u);
      expect(verifyContextToken(token, expected).claims.jti).toBe(jti);
      seen.add(jti);
    }
    expect(seen.size).toBe(200);
  });

  it("requires a session_id", () => {
    expect(() => mintContextToken(issuer(), claims({ sessionId: "" }))).toThrow(/session_id is required/u);
    expect(() => mintContextToken(issuer(), claims({ sessionId: "   " }))).toThrow(TokenRefused);
    expect(() => mintContextToken(issuer(), claims({ sessionId: undefined as unknown as string }))).toThrow(TokenRefused);
  });

  it("refuses wildcards, whitespace and out-of-bound sets instead of minting them", () => {
    expect(() => mintContextToken(issuer(), claims({ tenantId: "*" }))).toThrow(TokenRefused);
    expect(() => mintContextToken(issuer(), claims({ workspaceId: "site a1" }))).toThrow(TokenRefused);
    expect(() => mintContextToken(issuer(), claims({ principalId: "" }))).toThrow(TokenRefused);
    expect(() => mintContextToken(issuer(), claims({ roles: [] }))).toThrow(TokenRefused);
    expect(() => mintContextToken(issuer(), claims({ capabilityScopes: [] }))).toThrow(TokenRefused);
    expect(() => mintContextToken(issuer(), claims({ capabilityScopes: ["*"] }))).toThrow(TokenRefused);
    expect(() => mintContextToken(issuer(), claims({ roles: Array.from({ length: 65 }, (_, i) => `role_${i}`) }))).toThrow(TokenRefused);
    expect(() => mintContextToken(issuer(), claims({ capabilityScopes: Array.from({ length: 257 }, (_, i) => `cap.${i}`) }))).toThrow(TokenRefused);
  });

  it("collapses duplicate roles and scopes", () => {
    const { token } = mintContextToken(issuer(), claims({ roles: ["receptionist", "receptionist"], capabilityScopes: ["a.b", "a.b"] }));
    const { claims: payload } = verifyContextToken(token, expected);
    expect(payload.roles).toEqual(["receptionist"]);
    expect(payload.capability_scopes).toEqual(["a.b"]);
  });

  it("stays far below the 8 KiB cap with the largest legal sets", () => {
    const { token } = mintContextToken(
      issuer(),
      claims({
        roles: Array.from({ length: 64 }, (_, i) => `role_${i}`),
        capabilityScopes: Array.from({ length: 256 }, (_, i) => `capability.n${i}`),
      }),
    );
    expect(Buffer.byteLength(token)).toBeLessThan(8 * 1024);
  });

  it("uses node:crypto only: the key file parses as PKCS#8 despite the warning text before the block", () => {
    const pem = readFileSync(findDevKeyFile());
    expect(createPrivateKey(pem).asymmetricKeyType).toBe("ed25519");
  });
});
