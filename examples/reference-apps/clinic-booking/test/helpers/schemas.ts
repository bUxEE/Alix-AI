import { existsSync, readdirSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

import { Ajv2020, type ValidateFunction } from "ajv/dist/2020.js";
import addFormatsImport from "ajv-formats";

// The repository's own schemas are the contract, so the tests validate the
// application's real outputs against them instead of against a copy.

// ajv-formats is CommonJS: depending on the loader the function is the module
// itself or its default export.
const addFormats = ((addFormatsImport as unknown as { default?: unknown }).default ?? addFormatsImport) as (
  ajv: Ajv2020,
) => void;

export const findRepoRoot = (start: string = import.meta.dirname): string => {
  let current = resolve(start);
  for (;;) {
    if (existsSync(join(current, "schemas", "actions", "deterministic-result.schema.json"))) {
      return current;
    }
    const parent = dirname(current);
    if (parent === current) {
      throw new Error("the repository root (with schemas/actions) was not found");
    }
    current = parent;
  }
};

export const REPO_ROOT = findRepoRoot();

const schemaFiles = (area: string): string[] =>
  readdirSync(join(REPO_ROOT, "schemas", area))
    .filter((name) => name.endsWith(".schema.json"))
    .map((name) => join(REPO_ROOT, "schemas", area, name));

export const createAjv = (): Ajv2020 => {
  const ajv = new Ajv2020({ strict: false, allErrors: true });
  addFormats(ajv);
  for (const area of ["common", "actions"]) {
    for (const file of schemaFiles(area)) {
      ajv.addSchema(JSON.parse(readFileSync(file, "utf8")) as object);
    }
  }
  return ajv;
};

const ajv = createAjv();

const compile = (id: string): ValidateFunction => {
  const validate = ajv.getSchema(id);
  if (validate === undefined) {
    throw new Error(`schema ${id} is not loaded`);
  }
  return validate;
};

export const deterministicResultSchema = compile("urn:alixai:schema:actions:deterministic-result:v1");
export const preparedActionSchema = compile("urn:alixai:schema:actions:prepared-action:v1");
export const factSchema = compile("urn:alixai:schema:actions:shared:v1#/$defs/fact");

export const describeErrors = (validate: ValidateFunction): string =>
  JSON.stringify(validate.errors?.slice(0, 6) ?? [], null, 1);

// The core fills operation and capability_id from the call it made; a test
// does the same before it validates a host reply.
export const asCoreResult = (hostResult: unknown, operation: string, capabilityId: string): unknown => ({
  ...(hostResult as object),
  operation,
  capability_id: capabilityId,
});
