"""The five capabilities and the entity resolvers.

=====================  =====  =============================================================
capability             level  what it does
=====================  =====  =============================================================
workorders.list          0    read: the work orders the caller may see
workorders.create        1    reversible: raise a work order against an asset
workorders.assign        2    consequential: assign a technician, who is notified
workorders.close         2    consequential: close with parts used (stock is consumed)
assets.decommission      3    privileged: retire an asset; completed only in the app's own UI
=====================  =====  =============================================================
"""

from __future__ import annotations

import hashlib
import re
from datetime import timedelta
from decimal import Decimal
from typing import Any

from .access import (
    CAPABILITY_PERMISSION,
    ROLE_PERMISSIONS,
    Caller,
    can_see_technician,
    can_see_work_order,
    resolve_caller,
    visible_asset,
    visible_technician,
    visible_work_order,
)
from .clock import to_timestamp
from .errors import refuse
from .fields import (
    RawFields,
    optional_decimal,
    optional_enum,
    optional_identifier,
    optional_integer,
    optional_string,
    optional_string_list,
    parse_fields,
    required_identifier,
    required_string,
)
from .protocol import ActionDefinition, Applied, Description, Env, Loaded, freshness_of
from .results import (
    DataVersion,
    boolean,
    datetime_fact,
    decimal,
    identifier,
    integer,
    money,
    ok_result,
    plain,
    record,
    record_version,
    text,
)
from .store import ROLE_SUPERVISOR, Asset, Notification, StepUp, Technician, WorkOrder

PRIORITIES = ("low", "normal", "high", "urgent")
PRIORITY_RANK = {name: rank for rank, name in enumerate(PRIORITIES)}
STATUSES = ("open", "assigned", "closed")
MAX_OPEN_PER_TECHNICIAN = 5  # a technician with this many open work orders takes no more
MAX_RESULTS = 10
STEP_UP_SECONDS = 900

_PART_LINE = re.compile(r"^([A-Z0-9][A-Z0-9-]{1,31}):([1-9][0-9]{0,2})$")


# ---- small helpers -----------------------------------------------------------------------------------


def _orders_version(caller: Caller, asset_id: str) -> DataVersion:
    """The work orders of an asset as one snapshot: it moves when any of them is added or changed."""
    lines = sorted(f"{o.id}:{o.rev}" for o in caller.tenant.work_orders.values() if o.asset_id == asset_id)
    digest = hashlib.sha256("\n".join(lines).encode()).hexdigest()[:16] if lines else "none"
    return {"entity_type": "asset_work_orders", "entity_id": asset_id, "version": digest}


def _open_orders(caller: Caller, asset_id: str) -> list[WorkOrder]:
    return [o for o in caller.tenant.work_orders.values() if o.asset_id == asset_id and o.status != "closed"]


def _open_load(caller: Caller, technician_id: str) -> int:
    return sum(1 for o in caller.tenant.work_orders.values() if o.technician_id == technician_id and o.status != "closed")


def _asset_name(asset: Asset) -> str:
    return f"{asset.name} ({asset.tag})"


def _effect(operation: str, entity_type: str, description: str, changes: list[dict[str, Any]], *, entity_id: str | None = None, external: bool = False) -> dict[str, Any]:
    effect: dict[str, Any] = {
        "operation": operation,
        "entity_type": entity_type,
        "description": description[:500],
        "changes": changes,
        "external": external,
    }
    if entity_id is not None:
        effect["entity_id"] = entity_id
    return effect


def _effect_of(effects: list[dict[str, Any]], entity_type: str) -> dict[str, Any]:
    for effect in effects:
        if effect["entity_type"] == entity_type:
            return effect
    raise refuse("validation_failed", "the prepared action does not describe the change it commits", field="prepared")


def _technician_name(caller: Caller, technician_id: str | None) -> str:
    technician = caller.tenant.technicians.get(technician_id) if technician_id else None
    return technician.name if technician else "unassigned"


# ---- workorders.list (level 0) --------------------------------------------------------------------------

LIST_FIELDS = ("status", "priority", "asset", "technician", "max_results")


def list_work_orders(env: Env, header: str | None, raw_fields: Any) -> dict[str, Any]:
    caller = resolve_caller(env.store, header, "workorders.read")
    fields = parse_fields(raw_fields, LIST_FIELDS)
    status = optional_enum(fields, "status", (*STATUSES, "any"), "any")
    priority = optional_enum(fields, "priority", PRIORITIES)
    max_results = optional_integer(fields, "max_results", minimum=1, maximum=MAX_RESULTS, default=5) or 5
    asset_id = optional_identifier(fields, "asset")
    technician_id = optional_identifier(fields, "technician")
    if asset_id is not None:
        visible_asset(caller, asset_id)
    if technician_id is not None:
        visible_technician(caller, technician_id)  # a technician may name only themselves

    rows = [
        o
        for o in caller.tenant.work_orders.values()
        if can_see_work_order(caller, o)
        and (status == "any" or o.status == status)
        and (priority is None or o.priority == priority)
        and (asset_id is None or o.asset_id == asset_id)
        and (technician_id is None or o.technician_id == technician_id)
    ]
    rows.sort(key=lambda o: (-PRIORITY_RANK[o.priority], o.id))
    listed = rows[:max_results]

    values = [
        text("search_status", status),
        integer("work_order_count", len(listed)),
        boolean("more_available", len(rows) > len(listed)),
    ]
    lines: list[str] = []
    for number, order in enumerate(listed, start=1):
        who = _technician_name(caller, order.technician_id)
        values += [
            text(f"work_order_{number}_title", order.title),
            text(f"work_order_{number}_status", order.status),
            text(f"work_order_{number}_priority", order.priority),
            text(f"work_order_{number}_technician", who),
        ]
        lines.append(f"{order.id} {order.title} ({order.priority}, {order.status}, {who})")
    values.append(text("work_order_summary", "; ".join(lines) if lines else "none"))
    versions = [record_version("work_order", o.id, o.rev) for o in listed]
    next_actions: list[dict[str, str]] = []
    if listed and caller.role == ROLE_SUPERVISOR:
        next_actions.append({"action": "invoke_capability", "capability_id": "workorders.assign", "label": "Assign one of these work orders"})
    if listed:
        next_actions.append({"action": "invoke_capability", "capability_id": "workorders.close", "label": "Close one of these work orders"})
    return ok_result(
        identifiers=[record("work_order", o.id) for o in listed],
        values=values,
        freshness=freshness_of(env, versions),
        allowed_next_actions=[a for a in next_actions if _may(caller, a["capability_id"])],
    )


def _may(caller: Caller, capability_id: str) -> bool:
    return CAPABILITY_PERMISSION[capability_id] in ROLE_PERMISSIONS[caller.role]


# ---- workorders.create (level 1) -------------------------------------------------------------------------

CREATE_FIELDS = ("asset", "title", "description", "priority")


def _create_load(env: Env, caller: Caller, f: RawFields) -> Loaded:
    asset = visible_asset(caller, required_identifier(f, "asset"))
    title = required_string(f, "title", min_length=5, max_length=120)
    description = optional_string(f, "description", max_length=500, default="") or ""
    priority = optional_enum(f, "priority", PRIORITIES, "normal") or "normal"
    versions = [record_version("asset", asset.id, asset.rev), _orders_version(caller, asset.id)]
    return Loaded(versions, {"asset": asset, "title": title, "description": description, "priority": priority})


def _create_check(env: Env, caller: Caller, loaded: Loaded) -> None:
    asset: Asset = loaded.data["asset"]
    if asset.status != "active":
        raise refuse("conflict", "the asset is decommissioned, so no work can be raised against it", field="asset")
    title = loaded.data["title"].casefold()
    if any(o.title.casefold() == title for o in _open_orders(caller, asset.id)):
        raise refuse("conflict", "an open work order with this title already exists for the asset", field="title")


def _create_describe(env: Env, caller: Caller, loaded: Loaded) -> Description:
    d = loaded.data
    asset: Asset = d["asset"]
    changes = [identifier("asset", asset.id), text("title", d["title"]), text("priority", d["priority"])]
    if d["description"]:
        changes.insert(2, text("description", d["description"]))
    return Description(
        values=[text("asset_name", _asset_name(asset)), text("work_order_title", d["title"]), text("priority", d["priority"])],
        summary=f"Create a {d['priority']} priority work order '{d['title']}' for {_asset_name(asset)}.",
        effects=[_effect("create", "work_order", f"Create work order '{d['title']}' for {_asset_name(asset)}", changes)],
        next_actions_when_prepared=[{"action": "cancel"}],
    )


def _create_fields(effects: list[dict[str, Any]]) -> RawFields:
    changes = _effect_of(effects, "work_order")["changes"]
    fields: RawFields = {k: changes[k] for k in ("asset", "title", "description", "priority") if k in changes}
    return fields


def _create_apply(env: Env, caller: Caller, loaded: Loaded) -> Applied:
    d = loaded.data
    asset: Asset = d["asset"]
    order = env.store.add_work_order(
        caller.tenant,
        workspace=caller.workspace,
        asset_id=asset.id,
        title=d["title"],
        description=d["description"],
        priority=d["priority"],
        created_by=caller.principal_id,
        created_at=env.clock.now(),
    )
    next_actions = []
    if caller.role == ROLE_SUPERVISOR:
        next_actions.append({"action": "invoke_capability", "capability_id": "workorders.assign", "label": "Assign this work order"})
    return Applied(
        identifiers=[record("work_order", order.id)],
        values=[
            identifier("work_order_id", order.id),
            text("work_order_title", order.title),
            text("asset_name", _asset_name(asset)),
            text("priority", order.priority),
        ],
        versions_after=[record_version("asset", asset.id, asset.rev), _orders_version(caller, asset.id)],
        next_actions=next_actions,
    )


# ---- workorders.assign (level 2) -------------------------------------------------------------------------

ASSIGN_FIELDS = ("work_order", "technician", "note")


def _assign_versions(caller: Caller, order: WorkOrder, technician: Technician) -> list[DataVersion]:
    return [
        record_version("work_order", order.id, order.rev),
        record_version("technician", technician.id, technician.rev),
        {"entity_type": "technician_workload", "entity_id": technician.id, "version": str(_open_load(caller, technician.id))},
    ]


def _assign_load(env: Env, caller: Caller, f: RawFields) -> Loaded:
    order = visible_work_order(caller, required_identifier(f, "work_order"))
    technician = visible_technician(caller, required_identifier(f, "technician"))
    note = optional_string(f, "note", max_length=200, default="") or ""
    return Loaded(_assign_versions(caller, order, technician), {"order": order, "technician": technician, "note": note})


def _assign_check(env: Env, caller: Caller, loaded: Loaded) -> None:
    order: WorkOrder = loaded.data["order"]
    technician: Technician = loaded.data["technician"]
    if order.status == "closed":
        raise refuse("conflict", "the work order is already closed", field="work_order")
    if order.technician_id == technician.id:
        raise refuse("conflict", "the work order is already assigned to this technician", field="technician")
    if not technician.active:
        raise refuse("conflict", "the technician is not active", field="technician")
    if _open_load(caller, technician.id) >= MAX_OPEN_PER_TECHNICIAN:
        raise refuse(
            "conflict",
            f"the technician already has {MAX_OPEN_PER_TECHNICIAN} open work orders",
            field="technician",
        )


def _assign_describe(env: Env, caller: Caller, loaded: Loaded) -> Description:
    order: WorkOrder = loaded.data["order"]
    technician: Technician = loaded.data["technician"]
    note: str = loaded.data["note"]
    changes = [identifier("technician", technician.id)] + ([text("note", note)] if note else [])
    return Description(
        values=[
            identifier("work_order_id", order.id),
            text("work_order_title", order.title),
            text("technician_name", technician.name),
        ],
        summary=f"Assign work order {order.id} ({order.title}) to {technician.name}.",
        effects=[
            _effect("update", "work_order", f"Assign {order.id} to {technician.name}", changes, entity_id=order.id),
            _effect(
                "send",
                "notification",
                f"Notify {technician.name} that {order.id} was assigned to them",
                [identifier("recipient", technician.id), identifier("work_order", order.id)],
                external=True,
            ),
        ],
        next_actions_when_prepared=[{"action": "confirm"}, {"action": "cancel"}],
    )


def _assign_fields(effects: list[dict[str, Any]]) -> RawFields:
    update = _effect_of(effects, "work_order")
    changes = update["changes"]
    fields: RawFields = {"work_order": update.get("entity_id"), "technician": changes.get("technician")}
    if "note" in changes:
        fields["note"] = changes["note"]
    return {k: v for k, v in fields.items() if v is not None}


def _assign_apply(env: Env, caller: Caller, loaded: Loaded) -> Applied:
    order: WorkOrder = loaded.data["order"]
    technician: Technician = loaded.data["technician"]
    order.technician_id = technician.id
    order.status = "assigned"
    order.rev += 1
    reference = env.store.next_id("NTF")
    caller.tenant.outbox[reference] = Notification(reference, caller.workspace, "work_order_assigned", technician.id, order.id)
    return Applied(
        identifiers=[record("work_order", order.id)],
        values=[
            identifier("work_order_id", order.id),
            text("work_order_title", order.title),
            text("technician_name", technician.name),
            identifier("notification_reference", reference),
        ],
        versions_after=_assign_versions(caller, order, technician),
        follow_up={"reference": reference, "state": "queued", "as_of": to_timestamp(env.clock.now())},
    )


# ---- workorders.close (level 2) --------------------------------------------------------------------------

CLOSE_FIELDS = ("work_order", "resolution", "parts_used", "labor_hours")


def _parse_parts(raw: list[str]) -> list[tuple[str, int]]:
    parsed: list[tuple[str, int]] = []
    for line in raw:
        match = _PART_LINE.match(line)
        if match is None:
            raise refuse("validation_failed", "each part must be written PART-NUMBER:QUANTITY, for example FLT-204:2", field="parts_used")
        parsed.append((match.group(1), int(match.group(2))))
    if len({number for number, _ in parsed}) != len(parsed):
        raise refuse("validation_failed", "each part number may be listed only once", field="parts_used")
    return parsed


def _close_load(env: Env, caller: Caller, f: RawFields) -> Loaded:
    order = visible_work_order(caller, required_identifier(f, "work_order"))
    resolution = required_string(f, "resolution", min_length=3, max_length=500)
    labor = optional_decimal(f, "labor_hours", minimum=Decimal("0"), maximum=Decimal("24")) or Decimal("0")
    lines = _parse_parts(optional_string_list(f, "parts_used", max_items=20))
    parts = []
    for number, quantity in lines:
        part = caller.tenant.parts.get((caller.workspace, number))
        if part is None:
            raise refuse("validation_failed", f"part {number} is not in this site's inventory", field="parts_used")
        parts.append((part, quantity))
    versions = [record_version("work_order", order.id, order.rev)] + [record_version("part", p.part_number, p.rev) for p, _ in parts]
    return Loaded(versions, {"order": order, "resolution": resolution, "labor": labor, "parts": parts})


def _close_check(env: Env, caller: Caller, loaded: Loaded) -> None:
    if loaded.data["order"].status == "closed":
        raise refuse("conflict", "the work order is already closed", field="work_order")
    for part, quantity in loaded.data["parts"]:
        if quantity > part.stock:
            raise refuse("conflict", f"only {part.stock} of part {part.part_number} are in stock", field="parts_used")


def _parts_cost(loaded: Loaded) -> Decimal:
    return sum((p.unit_cost * q for p, q in loaded.data["parts"]), Decimal("0"))


def _close_describe(env: Env, caller: Caller, loaded: Loaded) -> Description:
    d = loaded.data
    order: WorkOrder = d["order"]
    summary = ", ".join(f"{p.part_number} x{q}" for p, q in d["parts"]) or "none"
    effects = [
        _effect(
            "update",
            "work_order",
            f"Close {order.id} with the resolution you gave",
            [text("status", "closed"), text("resolution", d["resolution"]), decimal("labor_hours", d["labor"])],
            entity_id=order.id,
        )
    ]
    for part, quantity in d["parts"]:
        effects.append(
            _effect(
                "update",
                "part",
                f"Use {quantity} of {part.part_number} ({part.name}): stock {part.stock} to {part.stock - quantity}",
                [integer("quantity_used", quantity), integer("stock_after", part.stock - quantity)],
                entity_id=part.part_number,
            )
        )
    return Description(
        values=[
            identifier("work_order_id", order.id),
            text("work_order_title", order.title),
            text("parts_summary", summary),
            money("parts_cost", _parts_cost(loaded)),
            decimal("labor_hours", d["labor"]),
        ],
        summary=f"Close work order {order.id} ({order.title}) using parts: {summary}, costing EUR {plain(_parts_cost(loaded))}.",
        effects=effects,
        next_actions_when_prepared=[{"action": "confirm"}, {"action": "cancel"}],
    )


def _close_fields(effects: list[dict[str, Any]]) -> RawFields:
    update = _effect_of(effects, "work_order")
    changes = update["changes"]
    fields: RawFields = {"work_order": update.get("entity_id"), "resolution": changes.get("resolution")}
    labor = changes.get("labor_hours")
    if isinstance(labor, str):
        try:
            fields["labor_hours"] = float(Decimal(labor))
        except ArithmeticError:
            raise refuse("validation_failed", "the labour hours of the prepared action are malformed", field="prepared") from None
    parts = [
        f"{e.get('entity_id')}:{e['changes'].get('quantity_used')}" for e in effects if e["entity_type"] == "part"
    ]
    if parts:
        fields["parts_used"] = parts
    return {k: v for k, v in fields.items() if v is not None}


def _close_apply(env: Env, caller: Caller, loaded: Loaded) -> Applied:
    d = loaded.data
    order: WorkOrder = d["order"]
    cost = _parts_cost(loaded)
    for part, quantity in d["parts"]:
        part.stock -= quantity
        part.rev += 1
    now = env.clock.now()
    order.status = "closed"
    order.closed_at = now
    order.resolution = d["resolution"]
    order.labor_hours = d["labor"]
    order.parts = [(p.part_number, q) for p, q in d["parts"]]
    order.parts_cost = cost
    order.rev += 1
    return Applied(
        identifiers=[record("work_order", order.id)],
        values=[
            identifier("work_order_id", order.id),
            datetime_fact("closed_at", to_timestamp(now)),
            money("parts_cost", cost),
            integer("parts_count", sum(q for _, q in d["parts"])),
        ],
        versions_after=[record_version("work_order", order.id, order.rev)] + [record_version("part", p.part_number, p.rev) for p, _ in d["parts"]],
    )


# ---- assets.decommission (level 3, step-up) ----------------------------------------------------------------

DECOMMISSION_FIELDS = ("asset", "reason")


def _decommission_load(env: Env, caller: Caller, f: RawFields) -> Loaded:
    asset = visible_asset(caller, required_identifier(f, "asset"))
    reason = required_string(f, "reason", min_length=10, max_length=300)
    return Loaded([record_version("asset", asset.id, asset.rev), _orders_version(caller, asset.id)], {"asset": asset, "reason": reason})


def _decommission_check(env: Env, caller: Caller, loaded: Loaded) -> None:
    asset: Asset = loaded.data["asset"]
    if asset.status != "active":
        raise refuse("conflict", "the asset is already decommissioned", field="asset")
    if _open_orders(caller, asset.id):
        raise refuse("conflict", "the asset still has open work orders; close them first", field="asset")


def _decommission_describe(env: Env, caller: Caller, loaded: Loaded) -> Description:
    asset: Asset = loaded.data["asset"]
    reason: str = loaded.data["reason"]
    return Description(
        values=[text("asset_name", asset.name), text("asset_tag", asset.tag)],
        summary=f"Decommission {_asset_name(asset)}: {reason}",
        effects=[
            _effect(
                "update",
                "asset",
                f"Retire {_asset_name(asset)}; it can no longer receive work orders",
                [text("status", "decommissioned"), text("reason", reason)],
                entity_id=asset.id,
            )
        ],
        next_actions_when_prepared=[{"action": "continue_in_app", "label": "Complete in Facility Maintenance"}, {"action": "cancel"}],
    )


def _decommission_fields(effects: list[dict[str, Any]]) -> RawFields:
    effect = _effect_of(effects, "asset")
    fields: RawFields = {"asset": effect.get("entity_id"), "reason": effect["changes"].get("reason")}
    return {k: v for k, v in fields.items() if v is not None}


def _decommission_stage(env: Env, caller: Caller, loaded: Loaded) -> list[dict[str, Any]]:
    """Stage the request the supervisor completes in the application's own UI.

    Idempotent: preparing the same decommission again finds the pending request and returns its
    reference, so the result is the same however often the core asks. It touches no record, so it
    moves no data version.
    """
    asset: Asset = loaded.data["asset"]
    expires = env.clock.now() + timedelta(seconds=STEP_UP_SECONDS)
    for step_up in caller.tenant.step_ups.values():
        if (
            step_up.status == "pending"
            and step_up.principal_id == caller.principal_id
            and step_up.workspace == caller.workspace
            and step_up.asset_id == asset.id
            and step_up.expires_at > env.clock.now()
        ):
            step_up.reason, step_up.asset_rev, step_up.expires_at = loaded.data["reason"], asset.rev, expires
            return [identifier("step_up_reference", step_up.reference)]
    local = env.store.next_id("SU")
    reference = "SU-" + hashlib.sha256(f"{caller.tenant.id}:{local}".encode()).hexdigest()[:8].upper()
    caller.tenant.step_ups[reference] = StepUp(
        reference, caller.workspace, caller.principal_id, asset.id, asset.rev, loaded.data["reason"], expires
    )
    env.store.step_up_index[reference] = caller.tenant.id
    return [identifier("step_up_reference", reference)]


def _decommission_apply(env: Env, caller: Caller, loaded: Loaded) -> Applied:
    asset: Asset = loaded.data["asset"]
    asset.status = "decommissioned"
    asset.rev += 1
    return Applied(
        identifiers=[record("asset", asset.id)],
        values=[text("asset_name", asset.name), text("asset_tag", asset.tag)],
        versions_after=[record_version("asset", asset.id, asset.rev), _orders_version(caller, asset.id)],
    )


# ---- the registry ---------------------------------------------------------------------------------------------

ACTIONS: dict[str, ActionDefinition] = {
    "workorders.create": ActionDefinition(
        "workorders.create", "workorders.create", CREATE_FIELDS, 1, 120,
        _create_load, _create_check, _create_describe, _create_fields, _create_apply,
    ),
    "workorders.assign": ActionDefinition(
        "workorders.assign", "workorders.assign", ASSIGN_FIELDS, 2, 600,
        _assign_load, _assign_check, _assign_describe, _assign_fields, _assign_apply,
    ),
    "workorders.close": ActionDefinition(
        "workorders.close", "workorders.close", CLOSE_FIELDS, 2, 600,
        _close_load, _close_check, _close_describe, _close_fields, _close_apply,
    ),
    "assets.decommission": ActionDefinition(
        "assets.decommission", "assets.decommission", DECOMMISSION_FIELDS, 3, STEP_UP_SECONDS,
        _decommission_load, _decommission_check, _decommission_describe, _decommission_fields, _decommission_apply,
        stage=_decommission_stage, requires_step_up=True,
    ),
}

# The route family of each consequential capability: /v1/<slug>/{validate,prepare,commit}.
ACTION_SLUGS = {
    "workorders.create": "work-orders-create",
    "workorders.assign": "work-orders-assign",
    "workorders.close": "work-orders-close",
    "assets.decommission": "assets-decommission",
}


# ---- entity resolution (reads that decide nothing) -------------------------------------------------------------

RESOLVE_FIELDS = ("mention", "candidate_id")
MAX_CANDIDATES = 10


def _compact(value: str) -> str:
    return re.sub(r"[^a-z0-9]", "", value.casefold())


def _matches(mention: str, haystack: str) -> bool:
    tokens = [t for t in re.split(r"\s+", mention.casefold()) if t]
    return bool(tokens) and (all(t in haystack.casefold() for t in tokens) or (_compact(mention) != "" and _compact(mention) in _compact(haystack)))


def resolve_entities(env: Env, header: str | None, entity_type: str, raw: Any) -> dict[str, Any]:
    """Resolve a mention to candidates the caller may see.

    ``candidate_id`` names a candidate by identifier when the model proposed one; it is honoured
    only when it is exactly a visible record. Otherwise the mention is searched. An empty mention
    matches nothing: the application never lists everything on request.
    """
    caller = resolve_caller(env.store, header, "workorders.read")
    fields = parse_fields(raw, RESOLVE_FIELDS)
    mention = optional_string(fields, "mention", max_length=200, default="") or ""
    candidate = optional_string(fields, "candidate_id", max_length=128, default="") or ""

    pool: list[tuple[str, str, str, int]]  # id, label, haystack, revision
    tenant = caller.tenant
    if entity_type == "asset":
        pool = [(a.id, _asset_name(a), f"{a.id} {a.tag} {a.name} {a.category}", a.rev) for a in tenant.assets.values() if a.workspace == caller.workspace]
    elif entity_type == "technician":
        pool = [(t.id, t.name, f"{t.id} {t.name} {t.trade}", t.rev) for t in tenant.technicians.values() if can_see_technician(caller, t)]
    else:
        pool = [
            (o.id, f"{o.id} {o.title}", f"{o.id} {o.title} {tenant.assets[o.asset_id].name if o.asset_id in tenant.assets else ''}", o.rev)
            for o in tenant.work_orders.values()
            if can_see_work_order(caller, o)
        ]
    pool.sort(key=lambda row: row[0])
    exact = [row for row in pool if candidate and row[0].casefold() == candidate.casefold()]
    found = exact or [row for row in pool if mention and _matches(mention, row[2])]
    found = found[:MAX_CANDIDATES]
    return ok_result(
        identifiers=[record(entity_type, row[0]) for row in found],
        values=[integer("candidate_count", len(found))] + [text("label", row[1]) for row in found],
        freshness=freshness_of(env, [record_version(entity_type, row[0], row[3]) for row in found]),
    )
