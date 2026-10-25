"""The five capabilities, end to end through the HTTP API: reads, resolvers and the protocol of every
consequential action (validate, prepare, commit, replay, status, stale, expired)."""

from __future__ import annotations

from conftest import World, error_code, value

CREATE = "work-orders-create"
ASSIGN = "work-orders-assign"
CLOSE = "work-orders-close"
DECOMMISSION = "assets-decommission"


# ---- workorders.list (level 0) -------------------------------------------------------------------------


def test_list_orders_by_priority_and_filters(world: World) -> None:
    everything = world.read(world.sup, status="any", max_results=10)
    assert everything["status"] == "ok"
    ids = [r["entity_id"] for r in everything["identifiers"]]
    # most urgent first, then by reference; the closed order is listed too under "any"
    assert ids[:2] == [world.ids["wo_theirs"], world.ids["wo_mine"]]
    assert value(everything, "work_order_count") == 4
    assert value(everything, "work_order_1_priority") == "urgent"
    assert value(everything, "more_available") is False

    open_only = world.read(world.sup, status="open")
    assert [r["entity_id"] for r in open_only["identifiers"]] == [world.ids["wo_open"]]
    assert value(open_only, "search_status") == "open"

    one = world.read(world.sup, status="any", max_results=1)
    assert value(one, "work_order_count") == 1 and value(one, "more_available") is True
    assert world.read(world.sup, status="closed", priority="urgent")["identifiers"] == []


def test_list_checks_its_filters(world: World) -> None:
    assert error_code(world.read(world.sup, max_results=11)) == "validation_failed"
    assert error_code(world.read(world.sup, status="finished")) == "validation_failed"
    assert error_code(world.read(world.sup, asset=world.ids["pump_s2"])) == "scope_violation"
    assert error_code(world.read(world.sup, technician="TEC-999")) == "scope_violation"


def test_a_technician_sees_their_own_work_and_the_work_they_raised(world: World) -> None:
    mine = world.read(world.tech, status="any")
    assert [r["entity_id"] for r in mine["identifiers"]] == [world.ids["wo_mine"]]
    # raising a work order makes it theirs to see, assigned or not
    done = world.run(world.tech, CREATE, "workorders.create", {"asset": world.ids["gen"], "title": "Fuel gauge reads empty"})
    created = done["identifiers"][0]["entity_id"]
    assert created in [r["entity_id"] for r in world.read(world.tech, status="any")["identifiers"]]
    assert created not in [r["entity_id"] for r in world.read(world.tech2, status="any")["identifiers"]]
    # asking for a colleague's work is refused like a record that is not there
    assert error_code(world.read(world.tech, technician=world.ids["tec2"])) == "scope_violation"
    assert world.read(world.tech, technician=world.ids["tec1"])["status"] == "ok"


def test_an_auditor_reads_the_whole_site_and_changes_nothing(world: World) -> None:
    assert world.read(world.aud, status="any", max_results=10)["identifiers"].__len__() == 4
    for slug, fields in ((CREATE, {"asset": world.ids["ahu"], "title": "Anything at all"}), (DECOMMISSION, {"asset": world.ids["gen"], "reason": "Not allowed to ask"})):
        assert error_code(world.prepare(world.aud, slug, fields)) == "permission_denied"


# ---- entity resolution -----------------------------------------------------------------------------------


def test_resolvers_find_candidates_the_caller_may_see(world: World) -> None:
    found = world.resolve(world.sup, "assets", mention="generator")
    assert [r["entity_id"] for r in found["identifiers"]] == [world.ids["gen"]]
    assert value(found, "candidate_count") == 1
    assert [f["value"]["value"] for f in found["values"] if f["field"] == "label"] == ["Backup generator (GEN-90)"]

    by_tag = world.resolve(world.sup, "assets", mention="ahu 200")
    assert [r["entity_id"] for r in by_tag["identifiers"]] == [world.ids["ahu"]]
    exact = world.resolve(world.sup, "assets", mention="whatever", candidate_id=world.ids["gen"].lower())
    assert [r["entity_id"] for r in exact["identifiers"]] == [world.ids["gen"]]

    assert world.resolve(world.sup, "assets", mention="")["identifiers"] == []  # never lists everything
    assert world.resolve(world.sup, "assets", mention="pump")["identifiers"] == []  # another site's asset
    orders = world.resolve(world.sup, "work-orders", mention="wo-1002")
    assert len(orders["identifiers"]) == 1
    assert [r["entity_id"] for r in world.resolve(world.sup, "technicians", mention="marco")["identifiers"]] == [world.ids["tec1"]]
    # a technician resolves only themselves
    assert world.resolve(world.tech, "technicians", mention="giulia")["identifiers"] == []
    assert world.resolve(world.tech, "technicians", mention="marco")["identifiers"] != []


# ---- workorders.create (level 1) ---------------------------------------------------------------------------


def test_create_prepare_commit_and_status(world: World) -> None:
    fields = {"asset": world.ids["gen"], "title": "Fuel gauge reads empty", "description": "Gauge stuck at E.", "priority": "high"}
    validated = world.call(world.tech, CREATE, "validate", fields).json()
    prepared = world.prepare(world.tech, CREATE, fields)
    assert prepared["status"] == "ok" and prepared["preparation"]["declared_risk_level"] == 1
    assert validated["freshness"]["version"] == prepared["freshness"]["version"]  # the contract case of M1-12
    assert prepared["preparation"]["valid_for_seconds"] == 120
    assert [e["operation"] for e in prepared["preparation"]["effects"]] == ["create"]
    assert not any(e["external"] for e in prepared["preparation"]["effects"])

    action = world.prepared_action("workorders.create", prepared)
    receipt = world.commit(world.tech, CREATE, action)
    assert receipt.status_code == 200 and receipt.headers["Idempotency-Key"] == action["idempotency_key"]
    body = receipt.json()
    new_id = value(body, "work_order_id")
    assert body["identifiers"] == [{"entity_type": "work_order", "entity_id": new_id}]
    assert value(body, "priority") == "high"

    # a repeat of the commit returns the original receipt and creates nothing more
    again = world.commit(world.tech, CREATE, action).json()
    assert again == body
    assert world.status(world.tech, CREATE, action["idempotency_key"]).json() == body
    listed = world.read(world.sup, status="open", max_results=10)
    assert [r["entity_id"] for r in listed["identifiers"]].count(new_id) == 1


def test_create_refuses_what_its_rules_forbid(world: World) -> None:
    ahu, gen = world.ids["ahu"], world.ids["gen"]
    assert error_code(world.prepare(world.tech, CREATE, {"asset": ahu})) == "missing_fields"
    assert error_code(world.prepare(world.tech, CREATE, {"asset": ahu, "title": "Hi"})) == "validation_failed"
    assert error_code(world.prepare(world.tech, CREATE, {"asset": ahu, "title": "A fine title", "priority": "now"})) == "validation_failed"
    assert error_code(world.prepare(world.tech, CREATE, {"asset": ahu, "title": "A fine title", "colour": "red"})) == "validation_failed"
    assert error_code(world.prepare(world.tech, CREATE, {"asset": world.ids["pump_s2"], "title": "Another site"})) == "scope_violation"
    duplicate = world.prepare(world.tech, CREATE, {"asset": ahu, "title": "return damper STICKS"})
    assert error_code(duplicate) == "conflict" and duplicate["errors"][0]["field"] == "title"
    assert "freshness" in duplicate  # a refusal after the records were read says what version it saw
    assert world.prepare(world.tech, CREATE, {"asset": gen, "title": "Return damper sticks"})["status"] == "ok"


def test_the_same_key_for_another_action_is_a_conflict(world: World) -> None:
    prepared = world.prepare(world.tech, CREATE, {"asset": world.ids["gen"], "title": "Fuel gauge reads empty"})
    action = world.prepared_action("workorders.create", prepared)
    assert world.commit(world.tech, CREATE, action).status_code == 200
    other = world.prepare(world.tech, CREATE, {"asset": world.ids["gen"], "title": "Battery charger fault"})
    reuse = world.prepared_action("workorders.create", other, key=action["idempotency_key"])
    reply = world.commit(world.tech, CREATE, reuse)
    assert reply.status_code == 409 and error_code(reply.json()) == "conflict"


# ---- workorders.assign (level 2) ---------------------------------------------------------------------------


def test_assign_notifies_the_technician_and_replays(world: World) -> None:
    fields = {"work_order": world.ids["wo_open"], "technician": world.ids["tec1"], "note": "Please start today"}
    prepared = world.prepare(world.sup, ASSIGN, fields)
    effects = prepared["preparation"]["effects"]
    assert prepared["preparation"]["declared_risk_level"] == 2
    assert [(e["operation"], e["external"]) for e in effects] == [("update", False), ("send", True)]
    action = world.prepared_action("workorders.assign", prepared)
    receipt = world.commit(world.sup, ASSIGN, action).json()
    assert receipt["status"] == "ok" and receipt["follow_up"]["state"] == "queued"
    reference = value(receipt, "notification_reference")
    assert world.app.state.env.store.tenants["t1"].outbox[reference].recipient_id == world.ids["tec1"]
    assert world.commit(world.sup, ASSIGN, action).json() == receipt
    assert len(world.app.state.env.store.tenants["t1"].outbox) == 1  # the replay sent nothing
    assigned = world.read(world.sup, status="assigned", technician=world.ids["tec1"])
    assert world.ids["wo_open"] in [r["entity_id"] for r in assigned["identifiers"]]


def test_assign_checks_roles_state_and_workload(world: World) -> None:
    base = {"work_order": world.ids["wo_open"], "technician": world.ids["tec1"]}
    assert error_code(world.prepare(world.tech, ASSIGN, base)) == "permission_denied"
    assert error_code(world.prepare(world.sup, ASSIGN, {**base, "work_order": world.ids["wo_s2"]})) == "scope_violation"
    assert error_code(world.prepare(world.sup, ASSIGN, {**base, "technician": world.ids["tec_s2"]})) == "scope_violation"
    assert error_code(world.prepare(world.sup, ASSIGN, {**base, "work_order": world.ids["wo_done"]})) == "conflict"
    assert error_code(world.prepare(world.sup, ASSIGN, {"work_order": world.ids["wo_mine"], "technician": world.ids["tec1"]})) == "conflict"
    assert error_code(world.prepare(world.sup, ASSIGN, {**base, "note": "x" * 201})) == "validation_failed"

    # a technician with five open work orders takes no more
    for number in range(4):
        world.run(world.sup, CREATE, "workorders.create", {"asset": world.ids["gen"], "title": f"Generator job number {number}"})
    open_ids = [r["entity_id"] for r in world.read(world.sup, status="open", max_results=10)["identifiers"]]
    for order_id in open_ids[:4]:
        world.run(world.sup, ASSIGN, "workorders.assign", {"work_order": order_id, "technician": world.ids["tec1"]})
    last = next(o for o in open_ids if o not in open_ids[:4])
    full = world.prepare(world.sup, ASSIGN, {"work_order": last, "technician": world.ids["tec1"]})
    assert error_code(full) == "conflict" and full["errors"][0]["field"] == "technician"


# ---- workorders.close (level 2) --------------------------------------------------------------------------------


def test_close_consumes_stock_and_prices_the_parts(world: World) -> None:
    fields = {"work_order": world.ids["wo_mine"], "resolution": "Rebalanced the fan", "parts_used": ["BLT-11:1", "FLT-204:2"], "labor_hours": 1.5}
    prepared = world.prepare(world.tech, CLOSE, fields)
    assert prepared["status"] == "ok"
    assert value(prepared, "parts_summary") == "BLT-11 x1, FLT-204 x2"
    cost = next(f for f in prepared["values"] if f["field"] == "parts_cost")["value"]
    assert cost == {"kind": "money", "value": "46.80", "currency": "EUR"}  # 9.80 + 2 x 18.50
    assert value(prepared, "labor_hours") == "1.50"
    part_effects = [e for e in prepared["preparation"]["effects"] if e["entity_type"] == "part"]
    assert {e["entity_id"] for e in part_effects} == {"BLT-11", "FLT-204"}
    stock = world.app.state.env.store.tenants["t1"].parts
    assert stock[("s1", "FLT-204")].stock == 12  # nothing moves at prepare

    action = world.prepared_action("workorders.close", prepared)
    receipt = world.commit(world.tech, CLOSE, action).json()
    assert receipt["status"] == "ok" and value(receipt, "parts_count") == 3
    assert value(receipt, "closed_at") == "2026-10-01T09:00:00Z"
    assert (stock[("s1", "FLT-204")].stock, stock[("s1", "BLT-11")].stock) == (10, 5)
    assert world.commit(world.tech, CLOSE, action).json() == receipt
    assert stock[("s1", "FLT-204")].stock == 10  # the replay consumed nothing
    assert world.read(world.tech, status="closed")["identifiers"][0]["entity_id"] == world.ids["wo_mine"]


def test_close_checks_parts_stock_and_ownership(world: World) -> None:
    order = world.ids["wo_mine"]
    base = {"work_order": order, "resolution": "Done and tested"}
    bad_lines = ["FLT-204", "flt-204:1", "FLT-204:0", "FLT-204:1000"]
    for line in bad_lines:
        assert error_code(world.prepare(world.tech, CLOSE, {**base, "parts_used": [line]})) == "validation_failed", line
    assert error_code(world.prepare(world.tech, CLOSE, {**base, "parts_used": ["FLT-204:1", "FLT-204:2"]})) == "validation_failed"
    assert error_code(world.prepare(world.tech, CLOSE, {**base, "parts_used": ["XYZ-999:1"]})) == "validation_failed"
    assert error_code(world.prepare(world.tech, CLOSE, {**base, "parts_used": ["SEL-30:5"]})) == "conflict"  # only 4 in stock
    assert error_code(world.prepare(world.tech, CLOSE, {**base, "labor_hours": 25})) == "validation_failed"
    assert error_code(world.prepare(world.tech, CLOSE, {**base, "labor_hours": 1.005})) == "validation_failed"
    assert error_code(world.prepare(world.tech, CLOSE, {**base, "resolution": "ok"})) == "validation_failed"
    assert error_code(world.prepare(world.tech, CLOSE, {"work_order": order})) == "missing_fields"
    # a technician cannot close a colleague's work, a supervisor can close any open order of the site
    assert error_code(world.prepare(world.tech, CLOSE, {**base, "work_order": world.ids["wo_theirs"]})) == "scope_violation"
    assert world.prepare(world.sup, CLOSE, {**base, "work_order": world.ids["wo_theirs"]})["status"] == "ok"
    assert error_code(world.prepare(world.sup, CLOSE, {**base, "work_order": world.ids["wo_done"]})) == "conflict"
    # no parts and no hours is a valid closure
    plain = world.prepare(world.sup, CLOSE, base | {"work_order": world.ids["wo_open"]})
    assert value(plain, "parts_summary") == "none" and value(plain, "parts_cost") == "0.00"


def test_two_closures_cannot_both_take_the_last_parts(world: World) -> None:
    mine = world.prepare(world.tech, CLOSE, {"work_order": world.ids["wo_mine"], "resolution": "Seal replaced", "parts_used": ["SEL-30:3"]})
    theirs = world.prepare(world.sup, CLOSE, {"work_order": world.ids["wo_theirs"], "resolution": "Seal replaced", "parts_used": ["SEL-30:3"]})
    assert world.commit(world.tech, CLOSE, world.prepared_action("workorders.close", mine)).status_code == 200
    second = world.commit(world.sup, CLOSE, world.prepared_action("workorders.close", theirs))
    assert second.status_code == 409 and error_code(second.json()) == "stale"  # the part moved


# ---- assets.decommission (level 3, step-up) -----------------------------------------------------------------------


def test_decommission_is_prepared_but_never_committed_through_the_api(world: World) -> None:
    fields = {"asset": world.ids["gen"], "reason": "Replaced last month by a new unit"}
    prepared = world.prepare(world.sup, DECOMMISSION, fields)
    assert prepared["status"] == "ok" and prepared["preparation"]["declared_risk_level"] == 3
    assert {"action": "continue_in_app", "label": "Complete in Facility Maintenance"} in prepared["allowed_next_actions"]
    reference = value(prepared, "step_up_reference")
    # preparing again finds the pending request: same reference, same version, nothing else staged
    again = world.prepare(world.sup, DECOMMISSION, fields)
    assert value(again, "step_up_reference") == reference
    assert again["freshness"]["version"] == prepared["freshness"]["version"]
    assert len(world.app.state.env.store.tenants["t1"].step_ups) == 1
    validated = world.call(world.sup, DECOMMISSION, "validate", fields).json()
    assert "step_up_reference" not in [f["field"] for f in validated["values"]]
    assert validated["freshness"]["version"] == prepared["freshness"]["version"]

    action = world.prepared_action("assets.decommission", prepared)
    refused = world.commit(world.sup, DECOMMISSION, action)
    assert refused.status_code == 403 and error_code(refused.json()) == "step_up_required"
    gen = world.app.state.env.store.tenants["t1"].assets[world.ids["gen"]]
    assert gen.status == "active"
    missing = world.status(world.sup, DECOMMISSION, action["idempotency_key"])
    assert missing.status_code == 404
    assert missing.json()["errors"] == [{"code": "validation_failed", "field": "idempotency_key", "message": "the application has no record of this idempotency key"}]
    assert missing.json()["identifiers"] == []


def test_decommission_refuses_what_it_must(world: World) -> None:
    assert error_code(world.prepare(world.sup, DECOMMISSION, {"asset": world.ids["ahu"], "reason": "Looks worn out"})) == "conflict"  # open work orders
    assert error_code(world.prepare(world.sup, DECOMMISSION, {"asset": world.ids["gen"], "reason": "short"})) == "validation_failed"
    assert error_code(world.prepare(world.sup, DECOMMISSION, {"asset": world.ids["pump_s2"], "reason": "Another site's pump"})) == "scope_violation"
    assert error_code(world.prepare(world.tech, DECOMMISSION, {"asset": world.ids["gen"], "reason": "A technician may not"})) == "permission_denied"


# ---- stale, expired, determinism ------------------------------------------------------------------------------------


def test_stale_preparations_are_refused_and_change_nothing(world: World) -> None:
    fields = {"work_order": world.ids["wo_open"], "technician": world.ids["tec1"]}
    prepared = world.prepare(world.sup, ASSIGN, fields)
    # someone else assigns the same order between prepare and commit
    world.run(world.sup, ASSIGN, "workorders.assign", {"work_order": world.ids["wo_open"], "technician": world.ids["tec2"]})
    action = world.prepared_action("workorders.assign", prepared)
    refused = world.commit(world.sup, ASSIGN, action)
    assert refused.status_code == 409 and error_code(refused.json()) == "stale"
    assert world.status(world.sup, ASSIGN, action["idempotency_key"]).status_code == 404  # nothing recorded
    order = world.app.state.env.store.tenants["t1"].work_orders[world.ids["wo_open"]]
    assert order.technician_id == world.ids["tec2"]

    tampered = world.prepared_action("workorders.assign", world.prepare(world.sup, ASSIGN, {"work_order": world.ids["wo_open"], "technician": world.ids["tec1"]}))
    tampered["data_versions"][0]["version"] += "~probe-stale"
    assert error_code(world.commit(world.sup, ASSIGN, tampered).json()) == "stale"


def test_expired_preparations_are_refused_but_receipts_replay(world: World) -> None:
    prepared = world.prepare(world.sup, CLOSE, {"work_order": world.ids["wo_open"], "resolution": "Freed and lubricated"})
    action = world.prepared_action("workorders.close", prepared)
    world.clock.advance(601)
    refused = world.commit(world.sup, CLOSE, action)
    assert error_code(refused.json()) == "expired" and refused.status_code == 409

    fresh = world.prepared_action("workorders.close", world.prepare(world.sup, CLOSE, {"work_order": world.ids["wo_open"], "resolution": "Freed and lubricated"}))
    receipt = world.commit(world.sup, CLOSE, fresh).json()
    world.clock.advance(10_000)  # after the preparation expired the original receipt is still the answer
    assert world.commit(world.sup, CLOSE, fresh).json() == receipt


def test_results_are_deterministic_for_the_same_data(world: World) -> None:
    fields = {"work_order": world.ids["wo_mine"], "resolution": "Rebalanced the fan", "parts_used": ["BLT-11:1"]}
    first, second = world.prepare(world.tech, CLOSE, fields), world.prepare(world.tech, CLOSE, fields)
    assert first == second
    world.clock.advance(5)
    third = world.prepare(world.tech, CLOSE, fields)
    assert third["freshness"]["version"] == first["freshness"]["version"]
    assert {**third, "freshness": None} == {**first, "freshness": None}  # only freshness.as_of moves
    assert world.read(world.sup) == world.read(world.sup)
