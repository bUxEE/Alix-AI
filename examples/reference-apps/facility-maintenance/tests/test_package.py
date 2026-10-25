"""The integration package agrees with the application: its files are the ones that were signed, its
contract is the one the application serves, and every declaration it makes about roles, permissions,
risk and operations is true of the code.

(`alixai package validate`, run from the repository root, applies the schemas and the cross-file
checks of docs/integration/application-package.md; these tests keep the package and the Python
application from drifting apart, which no validator of the package alone can see.)
"""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

import pytest
import yaml
from conftest import World, value

from facility_maintenance import capabilities as caps
from facility_maintenance.access import CAPABILITY_PERMISSION, ROLE_PERMISSIONS, ROLES
from facility_maintenance.app import OPERATIONS, TEST_SUPPORT_OPERATIONS
from facility_maintenance.protocol import IDEMPOTENCY_WINDOW_SECONDS

ROOT = Path(__file__).resolve().parent.parent
PACKAGE = ROOT / "package"
SPEC_FILE = ROOT / "src" / "facility_maintenance" / "openapi.yaml"
ENDPOINT = "facility-api"
FLOOR = {"none": 0, "reversible": 1, "external": 2, "privileged": 3}
FIELD_NAMES = {
    "workorders.list": caps.LIST_FIELDS,
    "workorders.create": caps.CREATE_FIELDS,
    "workorders.assign": caps.ASSIGN_FIELDS,
    "workorders.close": caps.CLOSE_FIELDS,
    "assets.decommission": caps.DECOMMISSION_FIELDS,
}


def load(path: str) -> Any:
    text = (PACKAGE / path).read_text(encoding="utf-8")
    return json.loads(text) if path.endswith(".json") else yaml.safe_load(text)


MANIFEST = load("alixai.yaml")
CAPABILITIES = {cid: load(rel) for cid, rel in MANIFEST["capabilities"].items()}
SPEC = yaml.safe_load(SPEC_FILE.read_text(encoding="utf-8"))
ADAPTER = load("schemas/adapters/facility-api.yaml")
SUITE = load("tests/facility-safety.yaml")


# ---- integrity ---------------------------------------------------------------------------------------------------


def test_every_file_is_listed_in_the_signature_manifest_with_its_digest() -> None:
    manifest = load("signatures/signature-manifest.json")
    listed = {f["path"]: f for f in manifest["signed"]["files"]}
    on_disk = sorted(p for p in PACKAGE.rglob("*") if p.is_file() and "signatures" not in p.relative_to(PACKAGE).parts)
    assert set(listed) == {p.relative_to(PACKAGE).as_posix() for p in on_disk}
    for path in on_disk:
        data = path.read_bytes()
        assert b"\r" not in data, f"{path} has CRLF line endings, which would change its digest in a checkout"
        entry = listed[path.relative_to(PACKAGE).as_posix()]
        assert entry["sha256"] == hashlib.sha256(data).hexdigest() and entry["size"] == len(data)
    assert manifest["signed"]["application_id"] == MANIFEST["application"]["id"] == "facility-maintenance"
    assert manifest["signed"]["package_version"] == MANIFEST["package"]["version"]
    assert [s["key_id"] for s in manifest["signatures"]] == MANIFEST["integrity"]["publisher_key_ids"]


def test_the_package_ships_the_contract_the_application_serves() -> None:
    assert (PACKAGE / "schemas" / "facility-api.openapi.yaml").read_bytes() == SPEC_FILE.read_bytes()
    endpoint = MANIFEST["endpoints"][ENDPOINT]
    assert endpoint["transport"] == "openapi" and endpoint["spec"] == "schemas/facility-api.openapi.yaml"
    assert endpoint["base_url"].startswith("http://127.0.0.1:") and "credential_ref" not in endpoint
    egress = MANIFEST["network"]["egress"]["facility-loopback"]
    assert endpoint["base_url"] == f"http://{egress['host']}:{egress['ports'][0]}" and MANIFEST["network"]["default"] == "deny"


def canonical_subset_hash(spec: dict[str, Any], operation_ids: list[str]) -> str:
    """contract_sha256 as adapters/openapi.ContractSHA256 defines it, written down independently."""
    annotations = {"description", "summary", "title", "example", "examples", "externalDocs"}
    name_maps = {"properties", "patternProperties", "dependentSchemas", "$defs", "definitions", "content", "responses", "headers", "links", "callbacks", "encoding", "mapping", "variables", "schemas", "securitySchemes"}

    def inline(node: Any, names: bool = False, active: tuple[str, ...] = ()) -> Any:
        if isinstance(node, list):
            return [inline(c, False, active) for c in node]
        if not isinstance(node, dict):
            return node
        ref = node.get("$ref")
        if isinstance(ref, str) and not names:
            if ref in active:
                return {"$ref": ref}
            target: Any = spec
            for token in ref[2:].split("/"):
                target = target[token.replace("~1", "/").replace("~0", "~")]
            return inline(target, False, (*active, ref))
        out = {}
        for key, child in node.items():
            if not names and (key in annotations or key.startswith("x-")):
                continue
            if not names and key in {"enum", "const", "default"}:
                out[key] = child
                continue
            out[key] = inline(child, not names and key in name_maps, active)
        return out

    entries = []
    for operation_id in sorted(operation_ids):
        for path, item in sorted(spec["paths"].items()):
            for method in ("get", "put", "post", "delete", "options", "head", "patch", "trace"):
                operation = item.get(method)
                if operation and operation["operationId"] == operation_id:
                    entries.append({"operation_id": operation_id, "method": method.upper(), "path": path, "path_parameters": inline(item.get("parameters")), "operation": inline(operation)})
    raw = json.dumps(entries, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    raw = raw.replace("<", "\\u003c").replace(">", "\\u003e").replace("&", "\\u0026")
    return hashlib.sha256(raw.encode()).hexdigest()


def test_the_manifest_pins_the_approved_operation_subset() -> None:
    declared = {op["operationId"] for item in SPEC["paths"].values() for op in item.values()}
    pin = MANIFEST["endpoints"][ENDPOINT]["contract_sha256"]
    assert pin == canonical_subset_hash(SPEC, sorted(declared)), "regenerate the pin: the approved operations of the document changed"


# ---- what the package declares is true of the application ---------------------------------------------------------------


def test_roles_permissions_and_grants_match_the_code() -> None:
    assert set(MANIFEST["tenancy"]["roles"]) == set(ROLES)
    assert MANIFEST["tenancy"]["principal_kinds"] == ["human"]
    assert set(CAPABILITIES) == set(CAPABILITY_PERMISSION) == set(FIELD_NAMES)
    for capability_id, contract in CAPABILITIES.items():
        assert contract["id"] == capability_id and contract["permissions"] == [CAPABILITY_PERMISSION[capability_id]]
        assert tuple(f["name"] for f in contract["fields"]) == FIELD_NAMES[capability_id]
    policy = load("policies/default-access.yaml")
    granted = {g["role"]: set(g["capabilities"]) for g in policy["role_grants"]}
    for role in ROLES:
        assert granted[role] == {c for c, p in CAPABILITY_PERMISSION.items() if p in ROLE_PERMISSIONS[role]}, role
    assert {p["capability_id"]: p["model_visible_fields"] for p in policy["field_projection"]} == {c: list(f) for c, f in FIELD_NAMES.items()}
    assert policy["risk_overrides"] == []


def test_risk_confirmation_and_time_to_live_match_what_the_application_does(world: World) -> None:
    levels = {}
    for capability_id, contract in CAPABILITIES.items():
        levels[capability_id] = contract["risk_level"]
        assert contract["risk_level"] >= FLOOR[contract["effect_class"]]
        if contract["risk_level"] == 0:
            assert contract["confirmation"]["mode"] == "none" and "prepare" not in contract["operations"]
            continue
        definition = caps.ACTIONS[capability_id]
        assert definition.declared_risk_level == contract["risk_level"]
        assert definition.valid_for_seconds == contract["prepared_action"]["ttl_seconds"]
        assert contract["idempotency"]["window_seconds"] == IDEMPOTENCY_WINDOW_SECONDS >= contract["prepared_action"]["ttl_seconds"]
        assert (definition.requires_step_up, contract["confirmation"]["mode"] == "step_up") == (contract["risk_level"] == 3,) * 2
        assert set(contract["operations"]) == {"validate", "prepare", "commit", "status"}
    assert levels == {"workorders.list": 0, "workorders.create": 1, "workorders.assign": 2, "workorders.close": 2, "assets.decommission": 3}
    assert CAPABILITIES["assets.decommission"]["confirmation"] == {"mode": "step_up", "step_up_channel": "application_ui"}
    # the effects the application reports agree with the effect class: external side effects are level 2 or more
    prepared = world.prepare(world.sup, "work-orders-assign", {"work_order": world.ids["wo_open"], "technician": world.ids["tec1"]})
    assert any(e["external"] for e in prepared["preparation"]["effects"]) and CAPABILITIES["workorders.assign"]["effect_class"] == "external"


def test_every_operation_of_the_document_is_accounted_for_exactly_once() -> None:
    declared = {op["operationId"] for item in SPEC["paths"].values() for op in item.values()}
    mapped = [
        b["openapi_operation_id"]
        for contract in CAPABILITIES.values()
        for b in (*contract["operations"].values(), *(e["resolve"] for e in contract["entity_types"].values()))
    ]
    management = ADAPTER["management"]
    approved = {*mapped, management["describe"], management["health"], management["inspect"], *management["provisioning"]}
    assert declared == approved == set(OPERATIONS) | set(TEST_SUPPORT_OPERATIONS)  # nothing in the document is left unmapped
    assert set(management["provisioning"]) == set(TEST_SUPPORT_OPERATIONS)
    assert all(b["endpoint"] == ENDPOINT for contract in CAPABILITIES.values() for b in contract["operations"].values())
    assert ADAPTER["server"] == ENDPOINT and "auth" not in ADAPTER  # the package declares no credential reference
    provisioning = SUITE["fixtures"]["provisioning"]
    assert [provisioning[k]["openapi_operation_id"] for k in ("create_tenant", "seed", "cleanup")] == ["createTenant", "seedTenant", "cleanupTenant"]


def test_the_tenancy_model_differs_from_the_clinic_application() -> None:
    tenancy = MANIFEST["tenancy"]
    assert "exactly one tenant" in tenancy["tenant"]["description"] and tenancy["workspace"]["label"] == "site"
    assert {c["risk_level"] for c in CAPABILITIES.values()} == {0, 1, 2, 3}
    sources = {s: load(rel) for s, rel in MANIFEST["rag_sources"].items()}
    assert {s["type"] for s in sources.values()} == {"mounted_volume", "application_document_api"}
    assert sources["maintenance-history"]["root"]["endpoint"] == ENDPOINT and sources["maintenance-history"]["root"]["base_path"] == "/rag/maintenance-history"
    assert sources["equipment-manuals"]["scope"]["workspace"]["mode"] == "shared"  # a manual describes a model, not a site


# ---- the results the package describes are the results the application gives -----------------------------------------------


def schema_of(name: str) -> dict[str, Any]:
    return load(f"schemas/{name}")


def test_results_satisfy_the_result_schemas_of_the_package(world: World) -> None:
    import jsonschema

    def check(name: str, result: dict[str, Any]) -> None:
        jsonschema.validate(result, schema_of(name))

    check("workorders.list.result.schema.json", world.read(world.sup, status="any"))
    check("workorders.list.result.schema.json", world.read(world.sup, status="closed", priority="low"))
    check("resolve.result.schema.json", world.resolve(world.sup, "assets", mention="generator"))
    check("resolve.result.schema.json", world.resolve(world.sup, "technicians", mention=""))
    cases = {  # the decommission goes first: raising work against the asset would block it
        "assets.decommission": ("assets-decommission", {"asset": world.ids["gen"], "reason": "Replaced last month by a new unit"}),
        "workorders.create": ("work-orders-create", {"asset": world.ids["gen"], "title": "Fuel gauge reads empty"}),
        "workorders.assign": ("work-orders-assign", {"work_order": world.ids["wo_open"], "technician": world.ids["tec1"]}),
        "workorders.close": ("work-orders-close", {"work_order": world.ids["wo_mine"], "resolution": "Rebalanced the fan", "parts_used": ["BLT-11:1"], "labor_hours": 1.5}),
    }
    for capability_id, (slug, fields) in cases.items():
        schemas = {op: Path(b["result_schema"]).name for op, b in CAPABILITIES[capability_id]["operations"].items()}
        check(schemas["validate"], world.call(world.sup, slug, "validate", fields).json())
        check(schemas["validate"], world.call(world.sup, slug, "validate", {**fields, "bogus": 1}).json())  # a refusal too
        prepared = world.prepare(world.sup, slug, fields)
        check(schemas["prepare"], prepared)
        action = world.prepared_action(capability_id, prepared)
        committed = world.commit(world.sup, slug, action).json()
        check(schemas["commit"], committed)
        check(schemas["status"], world.status(world.sup, slug, action["idempotency_key"]).json())
        check(schemas["status"], world.status(world.sup, slug, world.key()).json())
    # a failure of the contract's own: the schema refuses a member it does not know
    with pytest.raises(jsonschema.ValidationError):
        check("workorders.list.result.schema.json", {**world.read(world.sup), "prose": "ok"})


def test_the_facts_the_conversation_contracts_rely_on_are_stated_by_the_results(world: World) -> None:
    """Every response obligation names a fact some result of the capability really carries."""
    listed = world.read(world.sup, status="any")
    prepared = {
        "workorders.create": world.prepare(world.sup, "work-orders-create", {"asset": world.ids["gen"], "title": "Fuel gauge reads empty"}),
        "workorders.assign": world.prepare(world.sup, "work-orders-assign", {"work_order": world.ids["wo_open"], "technician": world.ids["tec1"]}),
        "workorders.close": world.prepare(world.sup, "work-orders-close", {"work_order": world.ids["wo_mine"], "resolution": "Rebalanced the fan"}),
        "assets.decommission": world.prepare(world.sup, "assets-decommission", {"asset": world.ids["gen"], "reason": "Replaced last month by a new unit"}),
    }
    receipts = {
        c: world.run(world.sup, slug, c, f)
        for c, slug, f in (
            ("workorders.create", "work-orders-create", {"asset": world.ids["gen"], "title": "Fuel gauge reads empty"}),
            ("workorders.assign", "work-orders-assign", {"work_order": world.ids["wo_open"], "technician": world.ids["tec1"]}),
            ("workorders.close", "work-orders-close", {"work_order": world.ids["wo_mine"], "resolution": "Rebalanced the fan"}),
        )
    }
    for capability_id, contract in CAPABILITIES.items():
        for obligation in contract["response_obligations"]:
            fact = obligation["fact"]
            if capability_id == "workorders.list":
                assert value(listed, fact) is not None
            elif obligation["when"] in ("prepared", "always"):
                assert value(prepared[capability_id], fact) is not None, (capability_id, fact)
            elif capability_id in receipts:
                assert value(receipts[capability_id], fact) is not None, (capability_id, fact)
