"""Reads schemas/api/openapi.yaml and its fragments the way the Go gateway's own contract test does (a root that
references one fragment per area) and turns the response schemas into validators, so the SDK's operation table and the
fake server's answers are checked against the real contract. A port of sdk/typescript/test/helpers/openapi.ts."""

from __future__ import annotations

import json
from dataclasses import dataclass
from functools import cached_property
from pathlib import Path
from typing import Any, Optional

import yaml
from jsonschema import Draft202012Validator
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

REPO_ROOT = Path(__file__).resolve().parents[4]
SCHEMAS_DIR = REPO_ROOT / "schemas"
API_DIR = SCHEMAS_DIR / "api"
OPENAPI_URI = "urn:alixai:test:openapi"
METHODS = ("get", "post", "put", "patch", "delete")


@dataclass
class SpecOperation:
    operation_id: str
    method: str
    path: str
    #: False when the operation declares ``security: []`` (and the root default does not apply).
    authenticated: bool
    #: Which fragment (area) declared it, or "root".
    area: str
    responses: dict[str, Any]
    #: The operation's requestBody (rewritten like the responses), or None when it takes no body.
    request_body: Optional[dict[str, Any]] = None


def _load_yaml(path: Path) -> dict[str, Any]:
    return yaml.safe_load(path.read_text(encoding="utf-8"))


class Spec:
    def __init__(self) -> None:
        root = _load_yaml(API_DIR / "openapi.yaml")
        areas: dict[str, dict[str, str]] = root.get("x-alixai-areas") or {}
        fragments = [(area, (API_DIR / ref["$ref"]).resolve()) for area, ref in areas.items()]

        # Every JSON Schema file of the repository, by file and by $id, to resolve file references.
        self.id_by_file: dict[Path, str] = {}
        self.schema_docs: list[dict[str, Any]] = []
        for path in sorted(SCHEMAS_DIR.glob("*/*.schema.json")):
            doc = json.loads(path.read_text(encoding="utf-8"))
            self.id_by_file[path.resolve()] = doc["$id"]
            self.schema_docs.append(doc)

        self.components: dict[str, dict[str, Any]] = {"schemas": {}, "responses": {}}
        self.operations: list[SpecOperation] = []
        root_components = root.get("components") or {}
        self.components["schemas"].update(self._rewrite(root_components.get("schemas") or {}, API_DIR))
        self.components["responses"].update(self._rewrite(root_components.get("responses") or {}, API_DIR))
        self._collect(root.get("paths") or {}, "root", root.get("security"), API_DIR)
        for area, path in fragments:
            doc = _load_yaml(path)
            self.components["schemas"].update(self._rewrite((doc.get("components") or {}).get("schemas") or {}, path.parent))
            self._collect(doc.get("paths") or {}, area, root.get("security"), path.parent)

        resources = [(doc["$id"], Resource.from_contents(doc)) for doc in self.schema_docs]
        resources.append((OPENAPI_URI, Resource.from_contents({"$id": OPENAPI_URI, "components": self.components}, default_specification=DRAFT202012)))
        self.registry: Registry = Registry().with_resources(resources)
        self._validators: dict[tuple[str, int], Optional[Draft202012Validator]] = {}

    # -- loading

    def _rewrite(self, value: Any, file_dir: Path) -> Any:
        """Rewrites every $ref of a document into a reference the validator universe resolves."""
        if isinstance(value, list):
            return [self._rewrite(item, file_dir) for item in value]
        if not isinstance(value, dict):
            return value
        out: dict[str, Any] = {}
        for key, item in value.items():
            if key == "$ref" and isinstance(item, str):
                file, hash_, fragment = item.partition("#")
                suffix = f"#{fragment}" if hash_ else ""
                if file == "" or file.endswith("openapi.yaml"):
                    out[key] = OPENAPI_URI + suffix
                else:
                    target = (file_dir / file).resolve()
                    if target not in self.id_by_file:
                        raise KeyError(f"unresolvable $ref {item} in {file_dir}")
                    out[key] = self.id_by_file[target] + suffix
            else:
                out[key] = self._rewrite(item, file_dir)
        return out

    def _collect(self, paths: dict[str, Any], area: str, security: Any, file_dir: Path) -> None:
        for path, item in paths.items():
            if not isinstance(item, dict):
                continue
            for method in METHODS:
                op = item.get(method)
                if not isinstance(op, dict):
                    continue
                effective = op["security"] if "security" in op else security
                self.operations.append(
                    SpecOperation(
                        operation_id=str(op["operationId"]),
                        method=method.upper(),
                        path=path,
                        authenticated=not (isinstance(effective, list) and len(effective) == 0),
                        area=area,
                        responses=self._rewrite(op.get("responses") or {}, file_dir),
                        request_body=self._rewrite(op["requestBody"], file_dir) if "requestBody" in op else None,
                    )
                )

    # -- validators

    def operation(self, operation_id: str) -> SpecOperation:
        return next(o for o in self.operations if o.operation_id == operation_id)

    def declared_statuses(self, operation_id: str) -> list[int]:
        return [int(status) for status in self.operation(operation_id).responses]

    def schema_validator(self, schema: Any) -> Draft202012Validator:
        return Draft202012Validator(schema, registry=self.registry)

    def request_validator(self, operation_id: str, content_type: str = "application/json") -> Optional[Draft202012Validator]:
        """A validator for the body an operation accepts in that content type, or None when it declares none."""
        body = self.operation(operation_id).request_body
        schema = ((body or {}).get("content") or {}).get(content_type, {}).get("schema")
        return self.schema_validator(schema) if isinstance(schema, dict) else None

    @cached_property
    def turn_event_validator(self) -> Draft202012Validator:
        return self.schema_validator({"$ref": "urn:alixai:schema:conversation:turn-event:v1"})

    def response_validator(self, operation_id: str, status: int) -> Optional[Draft202012Validator]:
        """A validator for the JSON body of a response, or None when the operation declares none for that status."""
        key = (operation_id, status)
        if key not in self._validators:
            response: Any = self.operation(operation_id).responses.get(str(status))
            if isinstance(response, dict) and isinstance(response.get("$ref"), str):
                response = self.components["responses"].get(response["$ref"].rsplit("/", 1)[-1])
            content = response.get("content") if isinstance(response, dict) else None
            schema = (content or {}).get("application/json", {}).get("schema")
            self._validators[key] = self.schema_validator(schema) if isinstance(schema, dict) else None
        return self._validators[key]


def explain(validator: Draft202012Validator, instance: Any) -> str:
    """The first few reasons the instance does not validate, for an assertion message."""
    errors = sorted(validator.iter_errors(instance), key=lambda e: list(e.path))[:5]
    return json.dumps([{"at": "/".join(str(p) for p in e.absolute_path), "why": e.message[:200]} for e in errors], indent=1)
