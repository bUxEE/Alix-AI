"""The HTTP surface: the OpenAPI contract against the routes and the replies, the management
operations, the disposable tenants of the audit, the optional bearer credential and typed failures."""

from __future__ import annotations

import re
from datetime import timedelta
from typing import Any

import jsonschema
import pytest
from conftest import World, build_world, error_code, identity
from fastapi.routing import APIRoute
from fastapi.testclient import TestClient

from facility_maintenance.app import OPERATIONS, TEST_SUPPORT_OPERATIONS, create_app, load_spec
from facility_maintenance.clock import to_timestamp
from facility_maintenance.config import Settings

ALL_OPERATIONS = {**OPERATIONS, **TEST_SUPPORT_OPERATIONS}
SPEC = load_spec()


def reply_schema(operation_id: str, status: int) -> dict[str, Any]:
    """The schema the document gives the reply of an operation at a status (exact code, then default)."""
    method, path = ALL_OPERATIONS[operation_id]
    responses = SPEC["paths"][path][method.lower()]["responses"]
    response = responses.get(str(status), responses.get("default"))
    if "$ref" in response:
        response = SPEC["components"]["responses"][response["$ref"].rsplit("/", 1)[1]]
    return response["content"]["application/json"]["schema"]


def conforms(operation_id: str, response) -> None:
    schema = {**reply_schema(operation_id, response.status_code), "components": SPEC["components"]}
    jsonschema.validate(response.json(), schema)


# ---- the contract and the routes ---------------------------------------------------------------------------


def test_the_document_describes_exactly_the_operations_the_routes_serve(world: World) -> None:
    declared = {op["operationId"]: (method.upper(), path) for path, item in SPEC["paths"].items() for method, op in item.items()}
    assert declared == ALL_OPERATIONS
    served = {r.name: (next(iter(r.methods)), r.path) for r in world.app.routes if isinstance(r, APIRoute) and r.name in ALL_OPERATIONS}
    assert served == ALL_OPERATIONS
    # nothing under /v1 is served that the document does not approve
    assert {r.path for r in world.app.routes if isinstance(r, APIRoute) and r.path.startswith("/v1/")} == {p for _, p in ALL_OPERATIONS.values()}
    assert world.client.get("/openapi.json").json() == SPEC
    assert SPEC["openapi"].startswith("3.0")


def test_the_document_is_a_closed_contract() -> None:
    for name, schema in SPEC["components"]["schemas"].items():
        jsonschema.Draft4Validator.check_schema(schema)
        assert schema["type"] == "object", name
    for path, item in SPEC["paths"].items():
        for method, op in item.items():
            assert "default" in op["responses"], (path, method)
            assert any(code.startswith("2") for code in op["responses"]), (path, method)
            for param in op.get("parameters", []):
                assert param.get("in") in {"path", "query", "header"} or "$ref" in param, (path, method, param)
            declared = set(re.findall(r"\{(\w+)\}", path))
            names = {p["name"] for p in op.get("parameters", []) if p.get("in") == "path"}
            assert declared == names, (path, method)
    # the strict result schema refuses members the contract does not know
    assert SPEC["components"]["schemas"]["Result"]["additionalProperties"] is False


def test_replies_conform_to_the_document(world: World) -> None:
    ahu, tec1 = world.ids["ahu"], world.ids["tec1"]
    sup = world.sup.headers
    conforms("listWorkOrders", world.client.get("/v1/work-orders", headers=sup))
    conforms("listWorkOrders", world.client.get("/v1/work-orders", params={"max_results": 99}, headers=sup))
    conforms("resolveAsset", world.client.post("/v1/resolve/assets", json={"mention": "ahu"}, headers=sup))
    for slug, capability, fields in (
        ("work-orders-create", "workorders.create", {"asset": ahu, "title": "A conforming title"}),
        ("work-orders-assign", "workorders.assign", {"work_order": world.ids["wo_open"], "technician": tec1}),
        ("work-orders-close", "workorders.close", {"work_order": world.ids["wo_open"], "resolution": "Freed and tested", "parts_used": ["BLT-11:1"], "labor_hours": 1.5}),
        ("assets-decommission", "assets.decommission", {"asset": world.ids["gen"], "reason": "Replaced last month by a new unit"}),
    ):
        suffix = {"workorders.create": "CreateWorkOrder", "workorders.assign": "AssignTechnician", "workorders.close": "CloseWorkOrder", "assets.decommission": "DecommissionAsset"}[capability]
        for verb in ("validate", "prepare"):
            response = world.call(world.sup, slug, verb, fields)
            conforms(f"{verb}{suffix}", response)
            jsonschema.validate(fields, {**SPEC["paths"][f"/v1/{slug}/{verb}"]["post"]["requestBody"]["content"]["application/json"]["schema"], "components": SPEC["components"]})
        prepared = world.prepare(world.sup, slug, fields)
        action = world.prepared_action(capability, prepared)
        jsonschema.validate(action, {"$ref": "#/components/schemas/PreparedAction", "components": SPEC["components"]})
        committed = world.commit(world.sup, slug, action)
        conforms(f"commit{suffix}", committed)
        conforms(f"status{suffix}", world.status(world.sup, slug, action["idempotency_key"]))
        conforms(f"status{suffix}", world.status(world.sup, slug, world.key()))  # the no-record answer (404)
    conforms("prepareAssignTechnician", world.call(world.tech, "work-orders-assign", "prepare", {"work_order": world.ids["wo_open"], "technician": tec1}))
    conforms("describeApplication", world.client.get("/v1/describe"))
    conforms("getHealth", world.client.get("/v1/health"))
    conforms("inspectApplication", world.client.get("/v1/inspect", headers=sup))


# ---- management ---------------------------------------------------------------------------------------------------


def test_describe_health_and_inspect(world: World) -> None:
    assert world.client.get("/v1/describe").json() == {"contract_versions": [1], "application_version": "1.0.0", "unmapped": []}
    assert world.client.get("/v1/health").json() == {"status": "ok", "application_version": "1.0.0"}
    inspection = world.client.get("/v1/inspect", headers=world.sup.headers).json()
    aspects = [r["aspect"] for r in inspection["reports"]]
    assert aspects == ["versions", "modules", "dependencies", "health", "scopes", "credentials", "limits", "storage", "network"]
    assert all(r["state"] == "reported" and r["facts"] for r in inspection["reports"])
    scopes = {f["field"]: f["value"]["value"] for r in inspection["reports"] if r["aspect"] == "scopes" for f in r["facts"]}
    assert scopes["tenant_count"] == 3 and scopes["disposable_tenant_count"] == 2  # the deployment's tenant and two disposable ones

    some = world.client.get("/v1/inspect", params=[("aspects", "network"), ("aspects", "limits")], headers=world.sup.headers).json()
    assert [r["aspect"] for r in some["reports"]] == ["limits", "network"]  # canonical order
    assert error_code(world.client.get("/v1/inspect", params={"aspects": "secrets"}, headers=world.sup.headers).json()) == "validation_failed"
    assert error_code(world.client.get("/v1/inspect", params=[("aspects", "health"), ("aspects", "health")], headers=world.sup.headers).json()) == "validation_failed"
    assert world.client.get("/v1/inspect").status_code == 403  # an identity of this application is required
    text = world.client.get("/v1/inspect", headers=world.sup.headers).text
    assert "token" not in text.lower() or "api_credential_required" in text  # facts only, never a secret


# ---- disposable tenants ---------------------------------------------------------------------------------------------


def test_disposable_tenants_are_created_seeded_and_removed_completely(world: World) -> None:
    client, store = world.client, world.app.state.env.store
    prepared = world.prepare(world.sup, "assets-decommission", {"asset": world.ids["gen"], "reason": "Replaced last month by a new unit"})
    world.run(world.sup, "work-orders-create", "workorders.create", {"asset": world.ids["gen"], "title": "Leaves a receipt behind"})
    assert prepared["status"] == "ok" and store.step_up_index

    again = client.post("/v1/test/tenants", json={"tenant_id": "t1", "workspaces": [{"workspace_id": "s1"}]})
    assert again.status_code == 409 and error_code(again.json()) == "conflict"
    gone = client.delete("/v1/test/tenants/t1")
    assert gone.status_code == 200 and gone.json()["existed"] is True
    assert set(gone.json()["remaining"].values()) == {0} and "t1" not in store.tenants and not store.step_up_index
    repeat = client.delete("/v1/test/tenants/t1")  # every call can be repeated
    assert repeat.status_code == 404 and repeat.json()["existed"] is False and set(repeat.json()["remaining"].values()) == {0}
    assert error_code(world.read(world.sup)) == "scope_violation"  # nothing of it is left to authorise


def test_the_audit_cannot_touch_the_deployments_own_tenant() -> None:
    world = build_world(Settings(test_support=True, demo_seed=True))
    production = world.app.state.settings.tenant_id
    for reply in (
        world.client.delete(f"/v1/test/tenants/{production}"),
        world.client.post(f"/v1/test/tenants/{production}/seed", json={"records": [{"ref": "a", "entity_type": "asset", "workspace_id": "site-north"}]}),
    ):
        assert reply.status_code == 403 and error_code(reply.json()) == "permission_denied"
    assert world.client.post("/v1/test/tenants", json={"tenant_id": production, "workspaces": [{"workspace_id": "x"}]}).status_code == 409
    demo = identity(production, "site-north", "sup-elena", "supervisor")
    assert world.client.get("/v1/work-orders", headers=demo).json()["status"] == "ok"  # still there and still served


def test_provisioning_refuses_bad_requests_and_seeds_atomically(world: World) -> None:
    client = world.client
    bad_create = (
        {"tenant_id": "x1", "workspaces": []},
        {"tenant_id": "x1", "workspaces": [{"workspace_id": "a"}, {"workspace_id": "a"}]},
        {"tenant_id": "x1", "workspaces": [{"workspace_id": "a"}], "principals": [{"principal_id": "p", "workspace_id": "a", "roles": ["root"]}]},
        {"tenant_id": "x1", "workspaces": [{"workspace_id": "a"}], "principals": [{"principal_id": "p", "workspace_id": "a", "roles": ["supervisor"], "service": True}]},
        {"tenant_id": "bad id", "workspaces": [{"workspace_id": "a"}]},
        ["not", "an", "object"],
    )
    for body in bad_create:
        assert client.post("/v1/test/tenants", json=body).status_code == 422, body
    assert "x1" not in world.app.state.env.store.tenants

    before = world.app.state.env.store.tenants["t1"].counts()
    records = [
        {"ref": "new_asset", "entity_type": "asset", "workspace_id": "s1", "name": "New asset"},
        {"ref": "bad_order", "entity_type": "work_order", "workspace_id": "s1", "asset": "missing_ref"},
    ]
    refused = client.post("/v1/test/tenants/t1/seed", json={"records": records})
    assert refused.status_code == 422 and error_code(refused.json()) == "validation_failed"
    assert world.app.state.env.store.tenants["t1"].counts() == before  # nothing was half-applied
    assert client.post("/v1/test/tenants/nope/seed", json={"records": records[:1]}).status_code == 404
    twice = {"ref": "a", "entity_type": "asset", "workspace_id": "s1"}
    assert client.post("/v1/test/tenants/t1/seed", json={"records": [twice, twice]}).status_code == 422
    elsewhere = {"ref": "a", "entity_type": "asset", "workspace_id": "s9"}
    assert client.post("/v1/test/tenants/t1/seed", json={"records": [elsewhere]}).status_code == 422
    conforms("seedTenant", client.post("/v1/test/tenants/t1/seed", json={"records": [{"ref": "ok", "entity_type": "asset", "workspace_id": "s1"}]}))


def test_test_support_is_absent_unless_asked_for() -> None:
    client = TestClient(create_app(Settings(test_support=False)))
    assert client.post("/v1/test/tenants", json={}).status_code == 404
    assert not any(p.startswith("/v1/test/") for p in client.get("/openapi.json").json()["paths"])
    assert client.get("/v1/health").status_code == 200


# ---- credentials and typed failures ---------------------------------------------------------------------------------


def test_a_configured_bearer_credential_is_required_everywhere() -> None:
    token = "not-a-real-token-for-tests"
    client = TestClient(create_app(Settings(test_support=True, api_token=token)))
    for method, path in (("get", "/v1/work-orders"), ("get", "/v1/health"), ("get", "/v1/describe"), ("post", "/v1/test/tenants"), ("get", "/rag/maintenance-history")):
        for headers in ({}, {"Authorization": "Bearer wrong"}, {"Authorization": token}):
            reply = getattr(client, method)(path, headers=headers)
            assert reply.status_code == 401, (path, headers)
            if path.startswith("/v1"):
                assert error_code(reply.json()) == "unauthenticated"  # a typed refusal, not a bare HTTP error
    bearer = {"Authorization": f"Bearer {token}"}
    assert client.get("/v1/health", headers=bearer).status_code == 200
    assert client.post("/v1/test/tenants", json={}, headers=bearer).status_code == 422  # past the credential, refused on its merits


def test_malformed_requests_are_typed_refusals_never_framework_errors(world: World) -> None:
    sup = world.sup.headers
    for content, expected in ((b"{not json", "validation_failed"), (b"[1, 2]", "validation_failed"), (b'"text"', "validation_failed")):
        reply = world.client.post("/v1/work-orders-create/prepare", content=content, headers={**sup, "Content-Type": "application/json"})
        assert reply.status_code == 422 and error_code(reply.json()) == expected, content
    huge = world.client.post("/v1/work-orders-create/prepare", content=b'{"title": "' + b"x" * (1 << 20) + b'"}', headers={**sup, "Content-Type": "application/json"})
    assert huge.status_code == 422 and error_code(huge.json()) == "validation_failed"
    bad_key = world.client.get("/v1/work-orders-create/status/short", headers=sup)
    assert bad_key.status_code == 422 and error_code(bad_key.json()) == "validation_failed"
    no_key = world.client.post("/v1/work-orders-create/commit", json={}, headers=sup)
    assert error_code(no_key.json()) == "validation_failed" and no_key.json()["errors"][0]["field"] == "idempotency_key"
    incomplete = world.client.post("/v1/work-orders-create/commit", json={"effects": []}, headers={**sup, "Idempotency-Key": world.key()})
    assert error_code(incomplete.json()) == "validation_failed" and incomplete.json()["errors"][0]["field"] == "prepared"


def test_an_unexpected_failure_is_reported_as_unavailable_and_changes_nothing(world: World, monkeypatch: pytest.MonkeyPatch) -> None:
    def boom(*args: Any, **kwargs: Any) -> None:
        raise RuntimeError("database on fire")

    monkeypatch.setattr("facility_maintenance.app.list_work_orders", boom)
    reply = world.client.get("/v1/work-orders", headers=world.sup.headers)
    assert reply.status_code == 503
    assert reply.json()["status"] == "unavailable" and reply.json()["errors"][0]["code"] == "internal"
    assert "fire" not in reply.text  # the cause is logged, never put in a reply
    conforms("listWorkOrders", reply)


# ---- timestamps the core sends ---------------------------------------------------------------------------------------


def test_a_timestamp_with_a_fraction_of_a_second_is_read_as_rfc_3339_says(world: World) -> None:
    """The core stamps a preparation with the precision of its own clock, so the expiry of a commit may carry a fraction."""
    from facility_maintenance.clock import parse_timestamp

    assert parse_timestamp("2026-10-03T10:11:12Z").microsecond == 0
    assert parse_timestamp("2026-10-03T10:11:12.5Z").microsecond == 500_000
    assert parse_timestamp("2026-10-03T10:11:12.123456789Z").microsecond == 123_456
    for bad in ("2026-10-03T10:11:12", "2026-10-03 10:11:12Z", "2026-10-03T10:11:12.Z", "2026-10-03T10:11:12+00:00", "2026-10-03T10:11:12.1234567891Z"):
        with pytest.raises(ValueError):
            parse_timestamp(bad)

    prepared = world.prepare(world.tech, "work-orders-create", {"asset": world.ids["gen"], "title": "A fraction of a second"})
    action = world.prepared_action("workorders.create", prepared)
    action["expires_at"] = action["expires_at"][:-1] + ".987654321Z"
    committed = world.commit(world.tech, "work-orders-create", action)
    assert committed.status_code == 200 and committed.json()["status"] == "ok"
    expired = world.prepared_action("workorders.create", world.prepare(world.tech, "work-orders-create", {"asset": world.ids["gen"], "title": "Expired by a fraction"}))
    expired["expires_at"] = to_timestamp(world.clock.now() - timedelta(seconds=1))[:-1] + ".999Z"
    refused = world.commit(world.tech, "work-orders-create", expired)
    assert refused.status_code == 409 and error_code(refused.json()) == "expired"
