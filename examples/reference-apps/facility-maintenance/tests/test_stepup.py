"""The step-up UI: the only place an asset is decommissioned. The AI path prepares it and stops."""

from __future__ import annotations

import base64
import re

import pytest
from conftest import World, error_code, value

PASSCODE = "correct horse battery staple"  # generated for the test; the application never stores it
DECOMMISSION = "assets-decommission"


def basic(user: str, passcode: str) -> dict[str, str]:
    return {"Authorization": "Basic " + base64.b64encode(f"{user}:{passcode}".encode()).decode()}


@pytest.fixture
def staged(world: World) -> tuple[World, str, str]:
    """A supervisor with a passcode and a decommission of an idle asset, prepared and waiting in the UI."""
    tenant = world.app.state.env.store.tenants["t1"]
    store = world.app.state.env.store
    store.set_passcode(tenant.principals[("s1", "sup1")], PASSCODE)
    store.set_passcode(tenant.principals[("s1", "tech1")], PASSCODE)  # a technician with a passcode is still no supervisor
    asset = world.ids["gen"]
    prepared = world.prepare(world.sup, DECOMMISSION, {"asset": asset, "reason": "Replaced last month by a new unit"})
    return world, value(prepared, "step_up_reference"), asset


def csrf_of(page: str) -> str:
    match = re.search(r'name="csrf" value="([0-9a-f]+)"', page)
    assert match, page
    return match.group(1)


def test_a_supervisor_completes_the_decommission_in_the_ui(staged: tuple[World, str, str]) -> None:
    world, reference, asset_id = staged
    client, url = world.client, f"/ui/step-up/{reference}"
    asset = world.app.state.env.store.tenants["t1"].assets[asset_id]

    assert client.get(url).status_code == 401  # no sign-in, nothing shown
    page = client.get(url, headers=basic("sup1", PASSCODE))
    assert page.status_code == 200 and "Backup generator" in page.text and reference in page.text
    assert page.headers["Cache-Control"] == "no-store" and "default-src 'none'" in page.headers["Content-Security-Policy"]
    assert asset.status == "active"  # looking changes nothing

    forged = client.post(url, headers=basic("sup1", PASSCODE), content=b"csrf=0000", )
    assert forged.status_code == 403 and asset.status == "active"
    done = client.post(url, headers={**basic("sup1", PASSCODE), "Content-Type": "application/x-www-form-urlencoded"}, content=f"csrf={csrf_of(page.text)}".encode())
    assert done.status_code == 200 and "is decommissioned" in done.text
    assert asset.status == "decommissioned" and asset.rev == 2

    # the effect is real everywhere: no new work, no second decommission, and the page says it is done
    assert world.prepare(world.sup, "work-orders-create", {"asset": asset_id, "title": "Work on a retired asset"})["errors"][0]["code"] == "conflict"
    assert world.prepare(world.sup, DECOMMISSION, {"asset": asset_id, "reason": "Decommission it again"})["errors"][0]["code"] == "conflict"
    assert "already decommissioned" in client.get(url, headers=basic("sup1", PASSCODE)).text
    again = client.post(url, headers=basic("sup1", PASSCODE), content=f"csrf={csrf_of(page.text)}".encode())
    assert again.status_code == 200 and asset.rev == 2  # approving twice changes nothing more


def test_only_the_supervisor_who_prepared_it_can_sign_in(staged: tuple[World, str, str]) -> None:
    world, reference, _ = staged
    url = f"/ui/step-up/{reference}"
    for headers in (basic("sup1", "wrong"), basic("tech1", PASSCODE), basic("sup1", ""), {"Authorization": "Basic !!!"}, {"Authorization": "Bearer x"}):
        assert world.client.get(url, headers=headers).status_code == 401, headers
    assert world.client.get(url, headers=basic("sup1", PASSCODE)).status_code == 200
    # a supervisor without a passcode cannot sign in at all, whatever they type
    world.app.state.env.store.tenants["t1"].principals[("s1", "sup1")].passcode_hash = None
    assert world.client.get(url, headers=basic("sup1", PASSCODE)).status_code == 401
    assert world.client.get("/ui/step-up/SU-00000000", headers=basic("sup1", PASSCODE)).status_code == 404


def test_three_wrong_passcodes_lock_the_request(staged: tuple[World, str, str]) -> None:
    world, reference, asset_id = staged
    url = f"/ui/step-up/{reference}"
    for _ in range(3):
        assert world.client.get(url, headers=basic("sup1", "guess")).status_code == 401
    assert world.client.get(url, headers=basic("sup1", PASSCODE)).status_code == 403  # locked, even for the right passcode
    # preparing again stages a new request with its own reference
    fresh = world.prepare(world.sup, DECOMMISSION, {"asset": asset_id, "reason": "Replaced last month by a new unit"})
    assert value(fresh, "step_up_reference") != reference
    assert world.client.get(f"/ui/step-up/{value(fresh, 'step_up_reference')}", headers=basic("sup1", PASSCODE)).status_code == 200


def test_an_expired_request_or_a_changed_asset_is_refused(staged: tuple[World, str, str]) -> None:
    world, reference, asset_id = staged
    url = f"/ui/step-up/{reference}"
    # work is raised against the asset after the request was staged: it must not be retired under it
    world.run(world.sup, "work-orders-create", "workorders.create", {"asset": asset_id, "title": "Raised after staging"})
    page = world.client.get(url, headers=basic("sup1", PASSCODE))
    refused = world.client.post(url, headers=basic("sup1", PASSCODE), content=f"csrf={csrf_of(page.text)}".encode())
    assert refused.status_code == 409 and "open work orders" in refused.text
    assert world.app.state.env.store.tenants["t1"].assets[asset_id].status == "active"

    world.clock.advance(901)
    assert world.client.get(url, headers=basic("sup1", PASSCODE)).status_code == 410


def test_what_the_user_wrote_is_escaped_in_the_page(world: World) -> None:
    store = world.app.state.env.store
    store.set_passcode(store.tenants["t1"].principals[("s1", "sup1")], PASSCODE)
    prepared = world.prepare(world.sup, DECOMMISSION, {"asset": world.ids["gen"], "reason": "<script>alert(1)</script> retired"})
    page = world.client.get(f"/ui/step-up/{value(prepared, 'step_up_reference')}", headers=basic("sup1", PASSCODE))
    assert "<script>" not in page.text and "&lt;script&gt;" in page.text


# ---- the audit's approval path (test support only) ------------------------------------------------------------------------


def approve(world: World, reference: str, *, tenant: str = "t1", workspace: str = "s1", principal: str = "sup1", values: dict | None = None):
    body = {
        "workspace_id": workspace,
        "principal_id": principal,
        "capability_id": "assets.decommission",
        "values": values if values is not None else {"step_up_reference": reference},
    }
    return world.client.post(f"/v1/test/tenants/{tenant}/step-ups", json=body)


def test_test_support_gives_a_disposable_supervisor_a_passcode_and_never_says_it(world: World) -> None:
    tenant = world.app.state.env.store.tenants["t1"]
    sup, tech = tenant.principals[("s1", "sup1")], tenant.principals[("s1", "tech1")]
    assert sup.passcode_hash is not None and sup.test_support_passcode and tech.passcode_hash is None and tech.test_support_passcode is None
    assert world.app.state.env.store.check_passcode(sup, sup.test_support_passcode)
    prepared = world.prepare(world.sup, DECOMMISSION, {"asset": world.ids["gen"], "reason": "Replaced last month by a new unit"})
    reply = approve(world, value(prepared, "step_up_reference"))
    assert reply.status_code == 200 and reply.json() == {"status": "ok", "tenant_id": "t1", "approved": True}
    assert sup.test_support_passcode not in reply.text


def test_the_approval_path_signs_in_to_the_ui_and_the_effect_is_the_uis(world: World) -> None:
    asset_id = world.ids["gen"]
    asset = world.app.state.env.store.tenants["t1"].assets[asset_id]
    prepared = world.prepare(world.sup, DECOMMISSION, {"asset": asset_id, "reason": "Replaced last month by a new unit"})
    reference = value(prepared, "step_up_reference")
    assert asset.status == "active"
    done = approve(world, reference)
    assert done.status_code == 200 and asset.status == "decommissioned" and asset.rev == 2
    assert approve(world, reference).status_code == 200 and asset.rev == 2  # approving twice changes nothing more
    # the commit through the adapter stays refused: the effect was the UI's, and the asset has moved on
    action = world.prepared_action("assets.decommission", prepared)
    action["confirmation"] = {"mode": "step_up", "bindings": []}
    assert error_code(world.commit(world.sup, DECOMMISSION, action).json()) in {"step_up_required", "stale"}
    assert world.status(world.sup, DECOMMISSION, action["idempotency_key"]).status_code == 404  # still no record under the key


def test_the_approval_path_is_held_to_the_uis_own_checks(world: World) -> None:
    asset_id = world.ids["gen"]
    prepared = world.prepare(world.sup, DECOMMISSION, {"asset": asset_id, "reason": "Replaced last month by a new unit"})
    reference = value(prepared, "step_up_reference")
    assert approve(world, reference, principal="sup2", workspace="s2").status_code == 404  # another supervisor's request
    assert approve(world, reference, values={"step_up_reference": "SU-9999"}).status_code == 404
    assert approve(world, reference, values={}).status_code == 422
    assert approve(world, reference, tenant="t2").status_code == 404  # a request belongs to its own tenant only
    # a changed asset is refused by the UI, and nothing is retired under the request
    world.run(world.sup, "work-orders-create", "workorders.create", {"asset": asset_id, "title": "Raised after staging"})
    refused = approve(world, reference)
    assert refused.status_code == 409 and refused.json()["errors"][0]["code"] == "conflict"
    assert world.app.state.env.store.tenants["t1"].assets[asset_id].status == "active"
    # an expired request
    world.clock.advance(901)
    expired = approve(world, reference)
    assert expired.status_code == 409 and expired.json()["errors"][0]["code"] == "expired"


def test_only_a_supervisor_of_a_disposable_tenant_can_be_approved_for() -> None:
    from conftest import build_world
    from facility_maintenance.config import Settings

    world = build_world(Settings(test_support=True, demo_seed=True, demo_passcode=PASSCODE))
    production = world.app.state.settings.tenant_id
    reply = world.client.post(
        f"/v1/test/tenants/{production}/step-ups",
        json={"workspace_id": "site-north", "principal_id": "sup-elena", "capability_id": "assets.decommission", "values": {"step_up_reference": "SU-0001"}},
    )
    assert reply.status_code == 403 and reply.json()["errors"][0]["code"] == "permission_denied"
    assert world.client.post("/v1/test/tenants/nope/step-ups", json={}).status_code == 404


def test_the_approval_path_is_absent_without_test_support() -> None:
    from fastapi.testclient import TestClient

    from facility_maintenance.app import create_app
    from facility_maintenance.config import Settings

    client = TestClient(create_app(Settings(test_support=False)))
    assert client.post("/v1/test/tenants/t1/step-ups", json={}).status_code == 404
    assert not any("step-ups" in path for path in client.get("/openapi.json").json()["paths"])
