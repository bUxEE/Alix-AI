import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";

import { Ajv2020 } from "ajv/dist/2020.js";
import type { ValidateFunction } from "ajv/dist/2020.js";
import addFormatsImport from "ajv-formats";

import { REPO_ROOT } from "./repo.js";

// The repository's schemas are the contract: the fake server's output is validated against them,
// not against a copy.

// ajv-formats is CommonJS: depending on the loader the function is the module or its default export.
const addFormats = ((addFormatsImport as unknown as { default?: unknown }).default ?? addFormatsImport) as (ajv: Ajv2020) => void;

const AREAS = ["common", "actions", "conversation", "memory", "providers"];

export const createAjv = (): Ajv2020 => {
  const ajv = new Ajv2020({ strict: false, allErrors: true });
  addFormats(ajv);
  for (const area of AREAS) {
    for (const name of readdirSync(join(REPO_ROOT, "schemas", area))) {
      if (name.endsWith(".schema.json")) ajv.addSchema(JSON.parse(readFileSync(join(REPO_ROOT, "schemas", area, name), "utf8")) as object);
    }
  }
  return ajv;
};

const ajv = createAjv();

const compile = (id: string): ValidateFunction => {
  const validate = ajv.getSchema(id);
  if (validate === undefined) throw new Error(`schema ${id} is not loaded`);
  return validate;
};

export const turnEventSchema = compile("urn:alixai:schema:conversation:turn-event:v1");
export const preparedActionSchema = compile("urn:alixai:schema:actions:prepared-action:v1");
export const actionReceiptSchema = compile("urn:alixai:schema:actions:action-receipt:v1");
export const errorSchema = compile("urn:alixai:schema:common:error:v1");

export const describeErrors = (validate: ValidateFunction): string => JSON.stringify(validate.errors?.slice(0, 6) ?? [], null, 1);
