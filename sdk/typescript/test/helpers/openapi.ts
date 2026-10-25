import { readFileSync, readdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

import { Ajv2020 } from "ajv/dist/2020.js";
import type { ValidateFunction } from "ajv/dist/2020.js";
import addFormatsImport from "ajv-formats";
import { parse } from "yaml";

import { REPO_ROOT } from "./repo.js";

// Reads schemas/api/openapi.yaml and its fragments the way the Go gateway's own contract test does
// (a root that references one fragment per area) and turns the response schemas into validators, so
// the SDK's operation table and the fake server's answers are checked against the real contract.

const API_DIR = join(REPO_ROOT, "schemas", "api");
const addFormats = ((addFormatsImport as unknown as { default?: unknown }).default ?? addFormatsImport) as (ajv: Ajv2020) => void;

type Node = Record<string, unknown>;

const isNode = (value: unknown): value is Node => typeof value === "object" && value !== null && !Array.isArray(value);

const load = (file: string): Node => parse(readFileSync(file, "utf8")) as Node;

export interface SpecOperation {
  operationId: string;
  method: string;
  path: string;
  /** False when the operation declares `security: []`. */
  authenticated: boolean;
  /** Which fragment (area) declared it, or "root". */
  area: string;
  responses: Node;
}

export interface Spec {
  operations: SpecOperation[];
  /** A validator for the JSON body of a response, or undefined when the operation declares none for that status. */
  responseValidator: (operationId: string, status: number) => ValidateFunction | undefined;
  declaredStatuses: (operationId: string) => number[];
  /** A validator for a named schema of the document's components (a request body such as `TurnRequest`). */
  componentValidator: (name: string) => ValidateFunction;
}

const METHODS = ["get", "post", "put", "patch", "delete"];

export const loadSpec = (): Spec => {
  const root = load(join(API_DIR, "openapi.yaml"));
  const areas = (root["x-alixai-areas"] ?? {}) as Record<string, { $ref: string }>;
  const fragments: Array<{ area: string; file: string; doc: Node }> = Object.entries(areas).map(([area, ref]) => {
    const file = resolve(API_DIR, ref.$ref);
    return { area, file, doc: load(file) };
  });

  // Map every JSON Schema file of the repository to its $id, to resolve file references.
  const idByFile = new Map<string, string>();
  for (const area of readdirSync(join(REPO_ROOT, "schemas"))) {
    const dir = join(REPO_ROOT, "schemas", area);
    if (area === "api") continue;
    for (const name of readdirSync(dir)) {
      if (!name.endsWith(".schema.json")) continue;
      const file = join(dir, name);
      idByFile.set(file, (JSON.parse(readFileSync(file, "utf8")) as { $id: string }).$id);
    }
  }

  // Rewrites every $ref of a document into a reference the validator universe resolves.
  const rewrite = (value: unknown, fileDir: string): unknown => {
    if (Array.isArray(value)) return value.map((item) => rewrite(item, fileDir));
    if (!isNode(value)) return value;
    const out: Node = {};
    for (const [key, item] of Object.entries(value)) {
      if (key === "$ref" && typeof item === "string") {
        const hash = item.indexOf("#");
        const file = hash === -1 ? item : item.slice(0, hash);
        const fragment = hash === -1 ? "" : item.slice(hash);
        if (file === "" || file.endsWith("openapi.yaml")) out[key] = `urn:alixai:test:openapi${fragment}`;
        else {
          const id = idByFile.get(resolve(fileDir, file));
          if (id === undefined) throw new Error(`unresolvable $ref ${item} in ${fileDir}`);
          out[key] = `${id}${fragment}`;
        }
      } else out[key] = rewrite(item, fileDir);
    }
    return out;
  };

  // The merged components of the root and every fragment, as one schema document.
  const components: { schemas: Node; responses: Node } = { schemas: {}, responses: {} };
  const operations: SpecOperation[] = [];
  const collectPaths = (paths: Node, area: string, security: unknown, fileDir: string): void => {
    for (const [path, item] of Object.entries(paths)) {
      if (!isNode(item)) continue;
      for (const method of METHODS) {
        const op = item[method];
        if (!isNode(op)) continue;
        const own = op["security"];
        const effective = own !== undefined ? own : security;
        operations.push({
          operationId: String(op["operationId"]),
          method: method.toUpperCase(),
          path,
          authenticated: !(Array.isArray(effective) && effective.length === 0),
          area,
          responses: rewrite(op["responses"] ?? {}, fileDir) as Node,
        });
      }
    }
  };
  const rootComponents = (root["components"] ?? {}) as Node;
  Object.assign(components.schemas, rewrite(rootComponents["schemas"] ?? {}, API_DIR));
  Object.assign(components.responses, rewrite(rootComponents["responses"] ?? {}, API_DIR));
  collectPaths((root["paths"] ?? {}) as Node, "root", root["security"], API_DIR);
  for (const { area, file, doc } of fragments) {
    const fragmentComponents = (doc["components"] ?? {}) as Node;
    Object.assign(components.schemas, rewrite(fragmentComponents["schemas"] ?? {}, dirname(file)));
    collectPaths((doc["paths"] ?? {}) as Node, area, root["security"], dirname(file));
  }

  const ajv = new Ajv2020({ strict: false, allErrors: true });
  addFormats(ajv);
  for (const file of idByFile.keys()) ajv.addSchema(JSON.parse(readFileSync(file, "utf8")) as object);
  ajv.addSchema({ $id: "urn:alixai:test:openapi", components });

  const cache = new Map<string, ValidateFunction | undefined>();
  const responseValidator: Spec["responseValidator"] = (operationId, status) => {
    const key = `${operationId}:${status}`;
    if (cache.has(key)) return cache.get(key);
    const op = operations.find((o) => o.operationId === operationId);
    let response = op?.responses[String(status)];
    if (isNode(response) && typeof response["$ref"] === "string") {
      const name = response["$ref"].split("/").pop() ?? "";
      response = components.responses[name];
    }
    const content = isNode(response) && isNode(response["content"]) ? response["content"] : undefined;
    const json = content !== undefined && isNode(content["application/json"]) ? content["application/json"] : undefined;
    const schema = json !== undefined ? json["schema"] : undefined;
    const validator = isNode(schema) ? ajv.compile(schema) : undefined;
    cache.set(key, validator);
    return validator;
  };

  const declaredStatuses: Spec["declaredStatuses"] = (operationId) =>
    Object.keys(operations.find((o) => o.operationId === operationId)?.responses ?? {}).map(Number);

  const componentValidator: Spec["componentValidator"] = (name) => {
    if (!(name in components.schemas)) throw new Error(`the document has no component schema ${name}`);
    return ajv.compile({ $ref: `urn:alixai:test:openapi#/components/schemas/${name}` });
  };

  return { operations, responseValidator, declaredStatuses, componentValidator };
};

export const describe = (validate: ValidateFunction): string => JSON.stringify(validate.errors?.slice(0, 5) ?? [], null, 1);
