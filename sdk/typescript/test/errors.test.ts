import { readFileSync } from "node:fs";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

import { AlixAIError, ERROR_CODES, isAlixAIError, isErrorCode, isRetryableCode } from "../src/index.js";
import { REPO_ROOT } from "./helpers/repo.js";

// The SDK mirrors the ErrCode strings by hand (it has no runtime dependency to read a schema with).
// These tests keep the mirror in step with the two sources of truth the Go core keeps in step.

describe("ERROR_CODES", () => {
  it("is exactly the enum of schemas/common/error, in the same order", () => {
    const schema = JSON.parse(readFileSync(join(REPO_ROOT, "schemas", "common", "error.schema.json"), "utf8")) as {
      properties: { code: { enum: string[] } };
    };
    expect([...ERROR_CODES]).toEqual(schema.properties.code.enum);
  });

  it("classifies retryable exactly as pkg/contracts does (testdata/errcodes.golden)", () => {
    const golden = readFileSync(join(REPO_ROOT, "pkg", "contracts", "testdata", "errcodes.golden"), "utf8")
      .split(/\r?\n/)
      .filter((line) => line.trim() !== "")
      .map((line) => {
        const [code, flag] = line.split(" ");
        return [code, flag === "retryable=true"] as const;
      });
    expect(golden.length).toBe(ERROR_CODES.length);
    for (const [code, retryable] of golden) {
      expect(isErrorCode(code), code).toBe(true);
      expect(isRetryableCode(code as never), code).toBe(retryable);
    }
  });

  it("isErrorCode accepts only the closed set", () => {
    expect(isErrorCode("replayed")).toBe(true);
    for (const bad of ["", "Replayed", "made_up", 1, null, undefined, {}]) expect(isErrorCode(bad)).toBe(false);
  });
});

describe("AlixAIError", () => {
  it("is an Error with a stable name, a code, a default retryable classification and a cause", () => {
    const cause = new Error("socket hang up");
    const error = new AlixAIError({ code: "unavailable", message: "down", cause });
    expect(error).toBeInstanceOf(Error);
    expect(error.name).toBe("AlixAIError");
    expect(error.retryable).toBe(true);
    expect(error.cause).toBe(cause);
    expect(new AlixAIError({ code: "replayed", message: "used" }).retryable).toBe(false);
    expect(new AlixAIError({ code: "replayed", message: "used", retryable: true }).retryable).toBe(true);
  });

  it("is recognised by brand, also across two copies of the package", () => {
    expect(isAlixAIError(new AlixAIError({ code: "internal", message: "x" }))).toBe(true);
    const foreign = { alixaiError: Symbol.for("alixai.sdk.error"), code: "internal" };
    expect(isAlixAIError(foreign)).toBe(true);
    for (const other of [new Error("x"), { code: "internal" }, null, undefined, "x"]) expect(isAlixAIError(other)).toBe(false);
  });
});
