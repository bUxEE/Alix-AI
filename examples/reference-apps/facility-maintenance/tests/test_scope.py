"""Tenancy, identity and permissions: a record outside the caller's site or tenant is exactly as absent
as one that does not exist, and the application never believes the roles an identity asserts."""

from __future__ import annotations

import base64
import json

import pytest
from conftest import APPLICATION, Who, World, error_code, identity

CREATE = "work-orders-create"
ASSIGN = "work-orders-assign"


def test_a_site_never_reaches_another_site(world: World) -> None:
    other_site_order = world.ids["wo_s2"]
    # the same call, for a record that exists at another site and for one that does not exist at all
    foreign = world.prepare(world.sup, ASSIGN, {"work_order": other_site_order, "technician": world.ids["tec1"]})
    nothing = world.prepare(world.sup, ASSIGN, {"work_order": "WO-9999", "technician": world.ids["tec1"]})
    assert foreign == nothing and error_code(foreign) == "scope_violation"
    assert foreign["identifiers"] == [] and foreign["values"] == []
    assert error_code(world.prepare(world.sup_s2, ASSIGN, {"work_order": world.ids["wo_open"], "technician": world.ids["tec_s2"]})) == "scope_violation"
    assert world.read(world.sup_s2, status="any")["identifiers"] == [{"entity_type": "work_order", "entity_id": other_site_order}]


def test_a_tenant_never_reaches_another_tenant(world: World) -> None:
    mine, theirs = world.ids["gen"], world.ids["ast_t2"]
    assert mine != theirs  # an identifier names one record in one tenant, so presenting another's reaches nothing
    foreign = world.prepare(world.sup, CREATE, {"asset": theirs, "title": "Work on the other tenant's asset"})
    nothing = world.prepare(world.sup, CREATE, {"asset": "AST-9999", "title": "Work on the other tenant's asset"})
    assert foreign == nothing and error_code(foreign) == "scope_violation"
    assert world.prepare(world.sup_t2, CREATE, {"asset": theirs, "title": "Work on their own asset"})["status"] == "ok"
    assert error_code(world.prepare(world.sup_t2, CREATE, {"asset": mine, "title": "Work on the first tenant's asset"})) == "scope_violation"
    # an identity names its tenant: a principal of t1 presented under t2 is unknown there
    stranger = Who("t2", "s1", "sup1", "supervisor")
    assert error_code(world.prepare(stranger, CREATE, {"asset": theirs, "title": "Anything at all"})) == "scope_violation"
    # a work order of t1 does not exist in t2
    assert error_code(world.prepare(world.sup_t2, ASSIGN, {"work_order": world.ids["wo_open"], "technician": world.ids["tec1"]})) == "scope_violation"


def test_the_application_does_not_believe_the_roles_an_identity_asserts(world: World) -> None:
    forged = Who("t1", "s1", "tech1", "supervisor")  # a technician claiming to be a supervisor
    reply = world.client.post(f"/v1/{ASSIGN}/prepare", json={"work_order": world.ids["wo_open"], "technician": world.ids["tec1"]}, headers=forged.headers)
    assert reply.status_code == 403 and error_code(reply.json()) == "permission_denied"
    # an unknown principal and a principal of another site are the same scope_violation
    assert error_code(world.read(Who("t1", "s1", "nobody", "supervisor"))) == "scope_violation"
    assert error_code(world.read(Who("t1", "s2", "sup1", "supervisor"))) == "scope_violation"


@pytest.mark.parametrize(
    ("headers", "code"),
    [
        ({}, "scope_missing"),
        ({"X-AlixAI-Identity": ""}, "scope_missing"),
        ({"X-AlixAI-Identity": "not base64 !"}, "scope_violation"),
        ({"X-AlixAI-Identity": base64.urlsafe_b64encode(b"[1,2]").decode()}, "scope_violation"),
        (identity("t1", "s1", "sup1", "supervisor", application="clinic-booking"), "scope_violation"),
    ],
)
def test_a_missing_or_doubtful_identity_never_falls_back_to_a_default(world: World, headers: dict[str, str], code: str) -> None:
    reply = world.client.get("/v1/work-orders", headers=headers)
    assert reply.status_code == 403 and error_code(reply.json()) == code


def test_a_missing_scope_level_is_named_not_defaulted(world: World) -> None:
    body = {"application_id": APPLICATION, "tenant_id": "t1", "workspace_id": "s1", "principal_id": "sup1", "roles": ["supervisor"]}  # no session
    raw = base64.urlsafe_b64encode(json.dumps(body).encode()).rstrip(b"=").decode()
    reply = world.client.get("/v1/work-orders", headers={"X-AlixAI-Identity": raw}).json()
    assert error_code(reply) == "scope_missing" and "session_id" in reply["errors"][0]["message"]


def test_status_belongs_to_the_principal_that_committed(world: World) -> None:
    prepared = world.prepare(world.sup, CREATE, {"asset": world.ids["gen"], "title": "Fuel gauge reads empty"})
    action = world.prepared_action("workorders.create", prepared)
    assert world.commit(world.sup, CREATE, action).status_code == 200
    other = world.status(world.tech, CREATE, action["idempotency_key"])
    assert other.status_code == 403 and error_code(other.json()) == "permission_denied"
    elsewhere = world.status(world.sup_s2, CREATE, action["idempotency_key"])
    assert elsewhere.status_code == 404  # keys are per site: nothing is on record there
    wrong_capability = world.status(world.sup, ASSIGN, action["idempotency_key"])
    assert wrong_capability.status_code == 409 and error_code(wrong_capability.json()) == "conflict"


def test_a_commit_of_another_principal_cannot_replay_a_receipt(world: World) -> None:
    prepared = world.prepare(world.sup, CREATE, {"asset": world.ids["gen"], "title": "Fuel gauge reads empty"})
    action = world.prepared_action("workorders.create", prepared)
    assert world.commit(world.sup, CREATE, action).status_code == 200
    hijack = world.commit(world.tech, CREATE, action)  # same key, same body, another principal
    assert hijack.status_code == 409 and error_code(hijack.json()) == "conflict"
