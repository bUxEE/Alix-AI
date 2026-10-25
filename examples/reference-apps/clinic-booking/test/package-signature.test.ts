import { createHash, verify } from "node:crypto";
import { lstatSync, readdirSync, readFileSync } from "node:fs";
import { join, relative, sep } from "node:path";

import { describe, expect, it } from "vitest";

import { PACKAGE_DIR, manifest, readJson } from "./helpers/package.ts";
import { devPublicKeyBase64, publicKeyFromRaw } from "./helpers/verify-token.ts";

// The signature on the package, checked here without the Go tool, from the
// format in docs/integration/application-package.md: the digest of every file
// outside signatures/, and a detached Ed25519 signature over the canonical JSON
// of the integrity manifest. If a package file changes and the package is not
// signed again, `pnpm test` says so.

type SignatureManifest = {
  format: string;
  signed: {
    application_id: string;
    package_version: string;
    created_at: string;
    hash_algorithm: string;
    files: { path: string; sha256: string; size: number }[];
  };
  signatures: { key_id: string; algorithm: string; signature: string; signed_at: string; expires_at: string }[];
};

const listFiles = (dir: string): string[] =>
  readdirSync(dir).flatMap((name) => {
    const full = join(dir, name);
    const stat = lstatSync(full);
    expect(stat.isSymbolicLink(), `${full} is a symbolic link`).toBe(false);
    return stat.isDirectory() ? listFiles(full) : [full];
  });

// Object keys sorted, no insignificant whitespace; the payload holds only strings and integers.
const canonical = (value: unknown): string => {
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  if (typeof value === "object" && value !== null) {
    const record = value as Record<string, unknown>;
    return `{${Object.keys(record)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${canonical(record[key])}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
};

const signatureManifest = readJson<SignatureManifest>("signatures/signature-manifest.json");

describe("the package is signed with the TEST-ONLY key and still matches its signature", () => {
  it("lists every file outside signatures/ with its digest and size, sorted, and nothing else", () => {
    const onDisk = listFiles(PACKAGE_DIR)
      .map((file) => relative(PACKAGE_DIR, file).split(sep).join("/"))
      .filter((path) => !path.startsWith("signatures/"))
      .sort((a, b) => Buffer.compare(Buffer.from(a), Buffer.from(b)));
    expect(listFiles(join(PACKAGE_DIR, "signatures")).map((f) => relative(PACKAGE_DIR, f).split(sep).join("/"))).toEqual([
      "signatures/signature-manifest.json",
    ]);
    expect(signatureManifest.signed.files.map((f) => f.path)).toEqual(onDisk);
    for (const entry of signatureManifest.signed.files) {
      const bytes = readFileSync(join(PACKAGE_DIR, entry.path));
      expect(createHash("sha256").update(bytes).digest("hex"), `${entry.path} changed since it was signed`).toBe(entry.sha256);
      expect(bytes.length, entry.path).toBe(entry.size);
      expect(bytes.includes(0x0d), `${entry.path} has CR line endings`).toBe(false);
    }
  });

  it("carries a valid Ed25519 signature of the published TEST-ONLY key over the canonical integrity manifest", () => {
    expect(signatureManifest.format).toBe("alixai.signature-manifest/v1");
    expect(signatureManifest.signed.application_id).toBe(manifest.application.id);
    expect(signatureManifest.signed.hash_algorithm).toBe("sha256");
    const packageVersion = (readFileSync(join(PACKAGE_DIR, "alixai.yaml"), "utf8").match(/^package:\n(?:  .*\n)*?  version: (\S+)/mu) ?? [])[1];
    expect(signatureManifest.signed.package_version).toBe(packageVersion);
    expect(signatureManifest.signatures).toHaveLength(1);
    const [signature] = signatureManifest.signatures;
    expect(signature?.algorithm).toBe("ed25519");
    expect(manifest.integrity.publisher_key_ids).toContain(signature?.key_id);
    expect(signature?.key_id).toBe("alixai-test-only-publisher");
    const valid = verify(
      null,
      Buffer.from(canonical(signatureManifest.signed), "utf8"),
      publicKeyFromRaw(devPublicKeyBase64()),
      Buffer.from(signature?.signature ?? "", "base64"),
    );
    expect(valid).toBe(true);
    // a signature never lasts forever, and this one is valid well past the time of writing
    expect(Date.parse(signature?.expires_at ?? "")).toBeGreaterThan(Date.parse("2030-01-01T00:00:00Z"));
    expect(Date.parse(signature?.signed_at ?? "")).toBeLessThan(Date.parse(signature?.expires_at ?? ""));
  });

  it("would not verify if a single byte of the integrity manifest changed", () => {
    const tampered = { ...signatureManifest.signed, package_version: "1.0.1" };
    const valid = verify(
      null,
      Buffer.from(canonical(tampered), "utf8"),
      publicKeyFromRaw(devPublicKeyBase64()),
      Buffer.from(signatureManifest.signatures[0]?.signature ?? "", "base64"),
    );
    expect(valid).toBe(false);
  });
});
