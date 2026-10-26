#!/usr/bin/env python
"""Generates the Pydantic models of the Python SDK from schemas/ (JSON Schema 2020-12 and OpenAPI).

    python tools/gen_models.py            # rewrite src/alixai/gen
    python tools/gen_models.py --check    # exit 1 when the committed output differs (the drift check)

What is generated
-----------------
* schemas/{common,actions,conversation,decision,providers,readiness,memory}/*.schema.json -> src/alixai/gen/<area>/<file>.py
* the components of schemas/api/openapi.yaml and its sessions, actions and apps fragments
  and the providers and oauth fragments (the operations the SDK speaks) -> src/alixai/gen/api.py

How a schema becomes a type
---------------------------
object with properties -> BaseModel (extra="forbid" when additionalProperties is false, else "ignore");
enum / const -> Literal; oneOf whose branches share a const member -> a discriminated Union; any other
oneOf / anyOf and a type list -> Union; open maps -> dict; $ref -> the named type, in its own module.
String, number and array bounds (minLength, maxLength, pattern, minimum, maximum, minItems,
maxItems) become Field constraints. A member that is not required is Optional[...] = None: absent and
null are not told apart.

What is NOT modelled: if/then/else, allOf, not, contains, uniqueItems and the cross-member rules they
carry. The schemas stay the authority: the contract tests validate real payloads against them with
jsonschema. Run only with PyYAML installed (dev dependency; the generated code needs only pydantic).
"""

from __future__ import annotations

import argparse
import json
import keyword
import re
import sys
import textwrap
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

SDK_ROOT = Path(__file__).resolve().parents[1]
REPO_ROOT = SDK_ROOT.parents[1]
SCHEMAS = REPO_ROOT / "schemas"
GEN_DIR = SDK_ROOT / "src" / "alixai" / "gen"
GEN_PACKAGE = "alixai.gen"

# The JSON Schema areas the SDK needs: a type of one of them is what the API sends and receives.
AREAS = ("common", "actions", "conversation", "decision", "providers", "readiness", "memory")
# The OpenAPI documents whose components are generated: the root and the areas the SDK implements.
API_DOCUMENTS = ("openapi.yaml", "paths/sessions.yaml", "paths/actions.yaml", "paths/apps.yaml", "paths/providers.yaml", "paths/oauth.yaml")

ANNOTATION_KEYS = {"description", "title", "$comment", "$schema", "$id", "examples", "default", "format", "deprecated"}
IGNORED_KEYS = {"if", "then", "else", "not", "allOf", "contains", "uniqueItems", "x-alixai-authority"}
BASE_MODEL_NAMES = set(dir(__import__("pydantic").BaseModel))

Node = Any


class GenerationError(Exception):
    pass


def pascal(name: str) -> str:
    parts = [p for p in re.split(r"[^A-Za-z0-9]+", name) if p]
    return "".join(p[0].upper() + p[1:] for p in parts)


def literal(value: Any) -> str:
    return json.dumps(value) if isinstance(value, str) else repr(value)


def snake_module(stem: str) -> str:
    return re.sub(r"[^A-Za-z0-9]+", "_", stem).lower()


# ---- the documents -------------------------------------------------------------------------------


@dataclass
class Doc:
    path: Path
    data: Node
    module: str  # dotted, relative to alixai.gen: "actions.shared", "api"


class Universe:
    """Every document, found by path and by `$id`, and the resolution of references between them."""

    def __init__(self) -> None:
        self.docs: dict[Path, Doc] = {}
        self.by_id: dict[str, Path] = {}
        for area in AREAS:
            for path in sorted((SCHEMAS / area).glob("*.schema.json")):
                data = json.loads(path.read_text(encoding="utf-8"))
                stem = path.name[: -len(".schema.json")]
                self.docs[path.resolve()] = Doc(path.resolve(), data, f"{area}.{snake_module(stem)}")
                self.by_id[data["$id"]] = path.resolve()
        for name in API_DOCUMENTS:
            path = (SCHEMAS / "api" / name).resolve()
            self.docs[path] = Doc(path, yaml.safe_load(path.read_text(encoding="utf-8")), "api")
        # Documents the generated ones reference without generating them (none today): load on demand.

    def doc(self, path: Path) -> Doc:
        if path not in self.docs:
            raise GenerationError(f"{path} is referenced but is not one of the generated documents")
        return self.docs[path]

    def resolve(self, ref: str, base: Path) -> tuple[Path, str]:
        target, _, fragment = ref.partition("#")
        if target.startswith("urn:"):
            if target not in self.by_id:
                raise GenerationError(f"unknown schema id {target} (from {base.name})")
            return self.by_id[target], fragment
        path = base if target == "" else (base.parent / target).resolve()
        return path, fragment

    def node(self, path: Path, pointer: str) -> Node:
        node: Node = self.doc(path).data
        for segment in [s for s in pointer.split("/") if s]:
            node = node[segment.replace("~1", "/").replace("~0", "~")]
        return node

    def follow(self, path: Path, pointer: str) -> tuple[Path, str]:
        """Follows a node that is nothing but a $ref (an alias such as the OpenAPI `Error`) to its target."""
        while True:
            node = self.node(path, pointer)
            if isinstance(node, dict) and set(node) == {"$ref"}:
                path, pointer = self.resolve(node["$ref"], path)
            else:
                return path, pointer


def is_named(pointer: str) -> bool:
    return pointer == "" or re.fullmatch(r"/(\$defs|components/schemas)/[^/]+", pointer) is not None


# ---- one generated module --------------------------------------------------------------------------


@dataclass
class Definition:
    name: str
    code: str
    deps: set[str] = field(default_factory=set)


class ModuleBuilder:
    def __init__(self, universe: Universe, doc: Doc) -> None:
        self.universe = universe
        self.doc = doc
        self.definitions: dict[str, Definition] = {}
        self.imports: dict[str, set[str]] = {}
        self.uses_typing: set[str] = set()
        self.pending_deps: list[set[str]] = []

    # -- naming and references

    def type_name(self, path: Path, pointer: str) -> str:
        if pointer == "":
            title = self.universe.doc(path).data.get("title")
            if not isinstance(title, str):
                raise GenerationError(f"{path.name} is referenced as a type but has no title")
            return pascal(title)
        return pascal(pointer.rsplit("/", 1)[1])

    def reference(self, path: Path, pointer: str, sibling: dict[str, Node]) -> str:
        """The type expression for a $ref (to a named type, or the target's own expression when it is not named)."""
        path, pointer = self.universe.follow(path, pointer)
        if is_named(pointer):
            target = self.universe.doc(path)
            name = self.type_name(path, pointer)
            if target.module == self.doc.module:
                self.pending_deps[-1].add(name)
            else:
                self.imports.setdefault(f"{GEN_PACKAGE}.{target.module}", set()).add(name)
            base = name
            return self.constrain(base, sibling, self.kind_of(self.universe.node(path, pointer)))
        node = self.universe.node(path, pointer)
        merged = {**node, **{k: v for k, v in sibling.items() if k != "$ref"}}
        return self.expr(merged, "Inline", path)

    @staticmethod
    def kind_of(node: Node) -> str | None:
        t = node.get("type") if isinstance(node, dict) else None
        return t if isinstance(t, str) else None

    # -- constraints

    def constrain(self, base: str, node: Node, kind: str | None) -> str:
        kwargs: list[str] = []

        def add(key: str, arg: str, kinds: tuple[str, ...]) -> None:
            if key in node and (kind is None or kind in kinds):
                kwargs.append(f"{arg}={node[key]!r}")

        add("minLength", "min_length", ("string",))
        add("maxLength", "max_length", ("string",))
        add("pattern", "pattern", ("string",))
        add("minimum", "ge", ("integer", "number"))
        add("maximum", "le", ("integer", "number"))
        add("exclusiveMinimum", "gt", ("integer", "number"))
        add("exclusiveMaximum", "lt", ("integer", "number"))
        add("minItems", "min_length", ("array",))
        add("maxItems", "max_length", ("array",))
        if not kwargs:
            return base
        self.uses_typing.update({"Annotated"})
        self.imports.setdefault("pydantic", set()).add("Field")
        return f"Annotated[{base}, Field({', '.join(kwargs)})]"

    # -- type expressions

    def expr(self, node: Node, hint: str, path: Path | None = None) -> str:
        path = path or self.doc.path
        if node is True or node == {}:
            return self.any_()
        if not isinstance(node, dict):
            raise GenerationError(f"{hint}: unsupported schema {node!r}")
        if "$ref" in node:
            siblings = {k: v for k, v in node.items() if k not in ANNOTATION_KEYS and k != "$ref"}
            target, pointer = self.universe.resolve(node["$ref"], path)
            return self.reference(target, pointer, siblings)
        if "const" in node:
            self.uses_typing.add("Literal")
            return f"Literal[{literal(node['const'])}]"
        if "enum" in node:
            self.uses_typing.add("Literal")
            return f"Literal[{', '.join(literal(v) for v in node['enum'])}]"
        for key in ("oneOf", "anyOf"):
            if key in node:
                union = self.union(node[key], hint, path)
                if union is not None:
                    return self.constrain(union, node, None)
        kind = node.get("type")
        if isinstance(kind, list):
            members = [self.primitive(k, node) for k in kind]
            return self.union_of(members)
        if kind == "object" or "properties" in node:
            return self.object_(node, hint, path)
        if kind == "array":
            items = node.get("items")
            inner = self.expr(items, hint + "Item", path) if items is not None else self.any_()
            return self.constrain(f"list[{inner}]", node, "array")
        if isinstance(kind, str):
            return self.primitive(kind, node)
        return self.any_()

    def any_(self) -> str:
        self.uses_typing.add("Any")
        return "Any"

    def primitive(self, kind: str, node: Node) -> str:
        base = {"string": "str", "integer": "int", "number": "float", "boolean": "bool", "null": "None"}.get(kind)
        if base is None:
            raise GenerationError(f"unsupported type {kind!r}")
        return self.constrain(base, node, kind)

    def union_of(self, members: list[str]) -> str:
        self.uses_typing.add("Union")
        unique = list(dict.fromkeys(members))
        if len(unique) == 1:
            return unique[0]
        if len(unique) == 2 and "None" in unique:
            self.uses_typing.add("Optional")
            return f"Optional[{unique[0] if unique[1] == 'None' else unique[1]}]"
        return f"Union[{', '.join(unique)}]"

    def union(self, branches: list[Node], hint: str, path: Path) -> str | None:
        # Branches that only add `required` (TurnRequest: exactly one of ...) are rules, not types.
        real = [b for b in branches if not (set(b) <= ANNOTATION_KEYS | {"required"})]
        if not real:
            return None
        names: list[str] = []
        discriminator = self.discriminator(real, path)
        for index, branch in enumerate(real):
            branch_hint = hint
            if discriminator and "properties" in branch and "$ref" not in branch:
                branch_hint = hint + pascal(str(branch["properties"][discriminator]["const"]))
            else:
                branch_hint = hint + (f"Variant{index + 1}" if len(real) > 1 else "")
            names.append(self.expr(branch, branch_hint, path))
        result = self.union_of(names)
        if discriminator and len(names) > 1:
            self.uses_typing.add("Annotated")
            self.imports.setdefault("pydantic", set()).add("Field")
            return f"Annotated[{result}, Field(discriminator={discriminator!r})]"
        return result

    def discriminator(self, branches: list[Node], path: Path) -> str | None:
        """The member every branch declares as a const, if all branches are objects."""
        const_keys: list[set[str]] = []
        for branch in branches:
            target_path = path
            while "$ref" in branch:
                target_path, pointer = self.universe.resolve(branch["$ref"], target_path)
                target_path, pointer = self.universe.follow(target_path, pointer)
                branch = self.universe.node(target_path, pointer)
            properties = branch.get("properties") if isinstance(branch, dict) else None
            if not isinstance(properties, dict):
                return None
            const_keys.append({k for k, v in properties.items() if isinstance(v, dict) and "const" in v})
        shared = set.intersection(*const_keys) if const_keys else set()
        for preferred in ("type", "kind"):
            if preferred in shared:
                return preferred
        return sorted(shared)[0] if shared else None

    def object_(self, node: Node, hint: str, path: Path) -> str:
        properties = node.get("properties")
        if not properties:
            extra = node.get("additionalProperties")
            value = self.expr(extra, hint + "Value", path) if isinstance(extra, dict) else self.any_()
            key = "str"
            names = node.get("propertyNames")
            if isinstance(names, dict):
                key = self.expr(names, hint + "Key", path)
            return self.constrain(f"dict[{key}, {value}]", node, None)
        # An anonymous object becomes a class named after where it sits.
        definition = self.define_class(hint, node, path)
        self.pending_deps[-1].add(definition.name)
        return definition.name

    # -- definitions

    def add(self, name: str, code: str, deps: set[str]) -> Definition:
        if name in self.definitions:
            raise GenerationError(f"{self.doc.module}: the name {name} is defined twice")
        definition = Definition(name, code, deps - {name})
        self.definitions[name] = definition
        return definition

    def define_class(self, name: str, node: Node, path: Path) -> Definition:
        self.pending_deps.append(set())
        required = set(node.get("required", []))
        closed = node.get("additionalProperties") is False
        fields: list[str] = []
        aliased = False
        for prop, schema in node["properties"].items():
            annotation = self.expr(schema, name + pascal(prop), path)
            ident = prop
            alias = None
            if keyword.iskeyword(prop) or prop in BASE_MODEL_NAMES or prop.startswith("_"):
                ident, alias, aliased = f"{prop.lstrip('_')}_", prop, True
            if prop in required:
                default = f" = Field(alias={alias!r})" if alias else ""
            else:
                self.uses_typing.add("Optional")
                annotation = f"Optional[{annotation}]"
                default = f" = Field(default=None, alias={alias!r})" if alias else " = None"
            if alias:
                self.imports.setdefault("pydantic", set()).add("Field")
            fields.append(f"    {ident}: {annotation}{default}")
        self.imports.setdefault("pydantic", set()).update({"BaseModel", "ConfigDict"})
        extra = "forbid" if closed else "ignore"
        options = f'extra="{extra}", protected_namespaces=()' + (", populate_by_name=True" if aliased else "")
        lines = [f"class {name}(BaseModel):", *docstring(node.get("description")), f"    model_config = ConfigDict({options})", *fields]
        return self.add(name, "\n".join(lines), self.pending_deps.pop())

    def define_alias(self, name: str, node: Node, path: Path) -> None:
        self.pending_deps.append(set())
        expression = self.expr(node, name, path)
        self.add(name, f"{name} = {expression}", self.pending_deps.pop())

    def define(self, pointer: str, node: Node) -> None:
        name = self.type_name(self.doc.path, pointer)
        if isinstance(node, dict) and set(node) == {"$ref"}:
            return  # a pure alias (OpenAPI Error, TurnEvent, ...): references follow it to the target
        shape_is_class = isinstance(node, dict) and (node.get("type") == "object" or "properties" in node) and bool(node.get("properties"))
        if shape_is_class:
            self.define_class(name, node, self.doc.path)
        else:
            self.define_alias(name, node, self.doc.path)

    # -- output

    def ordered(self) -> list[Definition]:
        done: list[Definition] = []
        seen: set[str] = set()
        visiting: list[str] = []

        def visit(name: str) -> None:
            if name in seen:
                return
            if name in visiting:
                raise GenerationError(f"{self.doc.module}: a reference cycle through {' -> '.join(visiting + [name])}")
            visiting.append(name)
            for dep in sorted(self.definitions[name].deps):
                if dep in self.definitions:
                    visit(dep)
            visiting.pop()
            seen.add(name)
            done.append(self.definitions[name])

        for name in self.definitions:
            visit(name)
        return done

    def render(self, source: str) -> str:
        ordered = self.ordered()
        typing_names = sorted(self.uses_typing)
        header = [
            f"# Code generated by tools/gen_models.py from {source}. DO NOT EDIT.",
            "# Regenerate with `python tools/gen_models.py`; tests/test_gen_drift.py fails when this file is out of date.",
            "from __future__ import annotations",
            "",
        ]
        if typing_names:
            header.append(f"from typing import {', '.join(typing_names)}")
        for module in sorted(self.imports):
            header.append(f"from {module} import {', '.join(sorted(self.imports[module]))}")
        names = ", ".join(repr(d.name) for d in ordered)
        body = "\n\n\n".join(d.code for d in ordered)
        return "\n".join(header) + f"\n\n__all__ = [{names}]\n\n\n{body}\n"


def docstring(description: Any) -> list[str]:
    if not isinstance(description, str) or not description.strip():
        return []
    text = " ".join(description.split()).replace("\\", "\\\\").replace('"""', '\\"\\"\\"')
    wrapped = textwrap.wrap(text, 96, initial_indent="    ", subsequent_indent="    ")
    if len(wrapped) == 1 and len(wrapped[0]) < 100:
        return [f'    """{text}"""']
    return ['    """', *wrapped, '    """']


# ---- driving ---------------------------------------------------------------------------------------


def build() -> dict[Path, str]:
    universe = Universe()
    files: dict[Path, str] = {}
    exports: dict[str, dict[str, str]] = {}  # area -> {name: module}

    for doc in universe.docs.values():
        if doc.module == "api":
            continue
        builder = ModuleBuilder(universe, doc)
        data = doc.data
        for name, node in data.get("$defs", {}).items():
            builder.define(f"/$defs/{name}", node)
        has_root = isinstance(data.get("title"), str) and (data.get("type") == "object" or "oneOf" in data or "anyOf" in data or "enum" in data)
        if has_root:
            builder.define("", {k: v for k, v in data.items() if k not in ("$defs", "$schema", "$id", "title")})
        area, _, module = doc.module.partition(".")
        relative = Path(area) / f"{module}.py"
        files[GEN_DIR / relative] = builder.render(f"schemas/{area}/{doc.path.name}")
        for definition in builder.definitions.values():
            owners = exports.setdefault(area, {})
            if definition.name in owners:
                raise GenerationError(f"{area}: {definition.name} is defined by both {owners[definition.name]} and {module}")
            owners[definition.name] = module

    api = ModuleBuilder(universe, next(d for d in universe.docs.values() if d.module == "api"))
    sources: list[str] = []
    for name in API_DOCUMENTS:
        doc = universe.doc((SCHEMAS / "api" / name).resolve())
        api.doc = doc
        for schema_name, node in (doc.data.get("components", {}).get("schemas") or {}).items():
            api.define(f"/components/schemas/{schema_name}", node)
        sources.append(f"schemas/api/{name}")
    files[GEN_DIR / "api.py"] = api.render(", ".join(sources))

    files[GEN_DIR / "__init__.py"] = (
        "# Code generated by tools/gen_models.py. DO NOT EDIT.\n"
        '"""The wire types of the AlixAI API, generated from schemas/. Import them from here or from alixai."""\n'
    )
    for area, names in exports.items():
        lines = ["# Code generated by tools/gen_models.py. DO NOT EDIT."]
        by_module: dict[str, list[str]] = {}
        for name, module in names.items():
            by_module.setdefault(module, []).append(name)
        for module in sorted(by_module):
            lines.append(f"from {GEN_PACKAGE}.{area}.{module} import {', '.join(sorted(by_module[module]))}")
        lines.append("")
        lines.append(f"__all__ = [{', '.join(repr(n) for n in sorted(names))}]")
        files[GEN_DIR / area / "__init__.py"] = "\n".join(lines) + "\n"
    return files


def existing() -> set[Path]:
    return {p for p in GEN_DIR.rglob("*.py")} if GEN_DIR.exists() else set()


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--check", action="store_true", help="fail when src/alixai/gen differs from what the schemas generate")
    args = parser.parse_args(argv)
    files = build()
    if args.check:
        problems = [str(p.relative_to(SDK_ROOT)) for p, text in files.items() if not p.exists() or p.read_bytes().replace(b"\r\n", b"\n") != text.encode("utf-8")]
        problems += [f"{p.relative_to(SDK_ROOT)} (stale)" for p in sorted(existing() - set(files))]
        if problems:
            print("the generated models are out of date; run `python tools/gen_models.py`:\n  " + "\n  ".join(problems), file=sys.stderr)
            return 1
        print(f"generated models are up to date ({len(files)} files)")
        return 0
    for stale in existing() - set(files):
        stale.unlink()
    for path, text in files.items():
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(text.encode("utf-8"))
    print(f"wrote {len(files)} files under {GEN_DIR.relative_to(SDK_ROOT)}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
