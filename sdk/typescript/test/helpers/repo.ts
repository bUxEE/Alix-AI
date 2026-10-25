import { existsSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

/** The repository root: the nearest ancestor that holds schemas/api/openapi.yaml. */
export const findRepoRoot = (start: string = import.meta.dirname): string => {
  let current = resolve(start);
  for (;;) {
    if (existsSync(join(current, "schemas", "api", "openapi.yaml"))) return current;
    const parent = dirname(current);
    if (parent === current) throw new Error("the repository root (with schemas/api/openapi.yaml) was not found");
    current = parent;
  }
};

export const REPO_ROOT = findRepoRoot();
export const SDK_ROOT = resolve(import.meta.dirname, "..", "..");
