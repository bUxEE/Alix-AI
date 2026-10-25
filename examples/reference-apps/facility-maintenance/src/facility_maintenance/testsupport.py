"""Disposable tenants for the integration audit's runtime probes.

Available only when the application runs with test support on. The audit creates its own tenants,
seeds them with synthetic records and removes them; the application refuses to touch any tenant it
did not create as disposable (``permission_denied``), above all the production tenant.

The status of a reply is the HTTP status; the body carries ``errors`` for a refusal. Every call can
be repeated: cleaning up a tenant that is gone answers 404 with ``existed: false`` and nothing
remaining, and creating one that exists answers 409.

A fourth call, ``approve_step_up``, does what a supervisor does in the step-up UI: it signs in to it as
the supervisor who prepared the request, with the passcode test support generated for them when it
created the tenant, and approves it. It drives the very pages the supervisor uses, so the passcode, the
anti-forgery token, the lock, the expiry and the changed-asset checks all apply, and it exists only
while the application runs for an audit and only for a disposable tenant. The passcode lives in memory
with the tenant and is never in a reply.
"""

from __future__ import annotations

import base64
import copy
import re
import secrets
from datetime import timedelta
from decimal import Decimal
from typing import Any

from .access import ROLE_PERMISSIONS, is_identifier
from .capabilities import PRIORITIES, _parse_parts
from .clock import parse_timestamp
from .errors import Refusal, refuse
from .protocol import Env
from . import stepup
from .store import ROLE_SUPERVISOR, ROLE_TECHNICIAN, STANDARD_PARTS, TenantData, WorkOrder

Reply = tuple[int, dict[str, Any]]

SEEDABLE = ("technician", "asset", "work_order")


def _refusal(refusal: Refusal, status: int) -> Reply:
    issue: dict[str, str] = {"code": refusal.code, "message": refusal.message}
    if refusal.field:
        issue["field"] = refusal.field
    return status, {"status": "rejected", "errors": [issue]}


def _object(value: Any, what: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise refuse("validation_failed", f"{what} must be an object")
    return value


def _identifier(value: Any, what: str) -> str:
    if not is_identifier(value):
        raise refuse("validation_failed", f"{what} must be an identifier", field=what)
    return value


def _disposable(env: Env, tenant_id: str, *, must_exist: bool) -> TenantData | None:
    tenant = env.store.tenants.get(tenant_id)
    if tenant is None:
        if must_exist:
            raise refuse("scope_violation", "there is no such tenant")
        return None
    if not tenant.disposable:
        raise refuse("permission_denied", "this tenant was not created by the audit and is never touched by it")
    return tenant


def create_tenant(env: Env, body: Any) -> Reply:
    try:
        data = _object(body, "the request body")
        tenant_id = _identifier(data.get("tenant_id"), "tenant_id")
        workspaces = [_object(w, "a workspace") for w in data.get("workspaces") or []]
        principals = [_object(p, "a principal") for p in data.get("principals") or []]
        site_ids = [_identifier(w.get("workspace_id"), "workspace_id") for w in workspaces]
        if not site_ids or len(set(site_ids)) != len(site_ids):
            raise refuse("validation_failed", "give at least one workspace, each once", field="workspaces")
        parsed: list[tuple[str, str, str]] = []
        for p in principals:
            site = _identifier(p.get("workspace_id"), "workspace_id")
            roles = p.get("roles")
            known = isinstance(roles, list) and len(roles) == 1 and roles[0] in ROLE_PERMISSIONS
            if site not in site_ids or p.get("service") or not known:
                raise refuse(
                    "validation_failed",
                    "a principal needs a known workspace, one known role and to be a human",
                    field="principals",
                )
            parsed.append((site, _identifier(p.get("principal_id"), "principal_id"), roles[0]))
        if len({(s, p) for s, p, _ in parsed}) != len(parsed):
            raise refuse("validation_failed", "a principal is listed twice", field="principals")
    except Refusal as refusal:
        return _refusal(refusal, 422)
    if tenant_id in env.store.tenants:
        return _refusal(refuse("conflict", "the tenant already exists", field="tenant_id"), 409)
    tenant = env.store.create_tenant(tenant_id, disposable=True)
    for w in workspaces:
        env.store.add_site(tenant, w["workspace_id"], w.get("name") if isinstance(w.get("name"), str) else None)
    for site, principal_id, role in parsed:
        # A supervisor can complete a step-up, so a disposable supervisor has a passcode of their own: random,
        # kept only as a hash in the record and in memory for test support's own sign-in, never stated.
        passcode = secrets.token_urlsafe(24) if role == ROLE_SUPERVISOR else None
        principal = env.store.add_principal(tenant, site, principal_id, role, passcode=passcode)
        principal.test_support_passcode = passcode
    return 201, {
        "status": "ok",
        "tenant_id": tenant_id,
        "workspaces": site_ids,
        "principals": [principal_id for _, principal_id, _ in parsed],
    }


def seed_tenant(env: Env, tenant_id: str, body: Any) -> Reply:
    try:
        tenant = _disposable(env, tenant_id, must_exist=True)
        assert tenant is not None
        records = _object(body, "the request body").get("records")
        if not isinstance(records, list) or not records:
            raise refuse("validation_failed", "give at least one record", field="records")
        plan = [_plan_record(tenant, _object(r, "a record")) for r in records]
        refs = [p["ref"] for p in plan]
        if len(set(refs)) != len(refs):
            raise refuse("validation_failed", "a record reference is used twice", field="records")
        # A record that cannot be made fails the whole call: the tenant is put back as it was.
        before, counters = copy.deepcopy(tenant), dict(env.store.counters)
        try:
            return 200, _apply_seed(env, tenant, plan)
        except Refusal:
            env.store.tenants[tenant_id] = before
            env.store.counters = counters
            raise
    except Refusal as refusal:
        status = {"permission_denied": 403, "scope_violation": 404}.get(refusal.code, 422)
        return _refusal(refusal, status)


def _plan_record(tenant: TenantData, record: dict[str, Any]) -> dict[str, Any]:
    ref = _identifier(record.get("ref"), "ref")
    kind = record.get("entity_type")
    if kind not in SEEDABLE:
        raise refuse("validation_failed", f"entity_type must be one of: {', '.join(SEEDABLE)}", field="entity_type")
    site = _identifier(record.get("workspace_id"), "workspace_id")
    if site not in tenant.sites:
        raise refuse("validation_failed", "the record names a workspace the tenant does not have", field="workspace_id")
    principal_id = record.get("principal_id")
    if principal_id is not None:
        principal = tenant.principals.get((site, principal_id))
        if kind != "technician" or principal is None or principal.role != ROLE_TECHNICIAN:
            raise refuse(
                "validation_failed",
                "only a technician record links to a technician principal of its workspace",
                field="principal_id",
            )
    return {**record, "ref": ref, "entity_type": kind, "workspace_id": site}


def _apply_seed(env: Env, tenant: TenantData, plan: list[dict[str, Any]]) -> dict[str, Any]:
    """Create technicians and assets first, then the work orders that point at them."""
    ids: dict[str, str] = {}
    store = env.store
    for rec in (r for r in plan if r["entity_type"] == "technician"):
        technician = store.add_technician(
            tenant,
            rec["workspace_id"],
            name=str(rec.get("name") or f"Technician {rec['ref']}"),
            trade=str(rec.get("trade") or "general"),
            principal_id=rec.get("principal_id"),
        )
        ids[rec["ref"]] = technician.id
    for rec in (r for r in plan if r["entity_type"] == "asset"):
        asset = store.add_asset(
            tenant,
            rec["workspace_id"],
            name=str(rec.get("name") or f"Asset {rec['ref']}"),
            tag=str(rec.get("tag") or rec["ref"].upper()),
            category=str(rec.get("category") or "hvac"),
        )
        ids[rec["ref"]] = asset.id
    for rec in (r for r in plan if r["entity_type"] == "work_order"):
        ids[rec["ref"]] = _seed_work_order(env, tenant, rec, ids).id
    return {
        "status": "ok",
        "tenant_id": tenant.id,
        "records": [{"ref": r["ref"], "entity_type": r["entity_type"], "entity_id": ids[r["ref"]]} for r in plan],
    }


def _seed_work_order(env: Env, tenant: TenantData, rec: dict[str, Any], ids: dict[str, str]) -> WorkOrder:
    asset = tenant.assets.get(ids.get(str(rec.get("asset")), ""))
    if asset is None or asset.workspace != rec["workspace_id"]:
        raise refuse("validation_failed", "a work order needs an asset record of its own workspace", field="asset")
    technician_id = ids.get(str(rec["technician"])) if rec.get("technician") else None
    status = rec.get("status", "assigned" if technician_id else "open")
    priority = rec.get("priority", "normal")
    if status not in ("open", "assigned", "closed") or priority not in PRIORITIES:
        raise refuse("validation_failed", "a work order has a known status and priority", field="status")
    if (status == "open" and technician_id is not None) or (status == "assigned" and technician_id is None):
        raise refuse("validation_failed", "an open work order has no technician and an assigned one has", field="technician")
    catalogue = {number: Decimal(cost) for number, _, _, cost in STANDARD_PARTS}
    parts = _parse_parts([str(p) for p in rec.get("parts") or []])
    if any(number not in catalogue for number, _ in parts):
        raise refuse("validation_failed", "a seeded part is not in the standard catalogue", field="parts")
    now = env.clock.now()
    order = env.store.add_work_order(
        tenant,
        workspace=rec["workspace_id"],
        asset_id=asset.id,
        title=str(rec.get("title") or f"Seeded work order {rec['ref']}"),
        description=str(rec.get("description") or ""),
        priority=priority,
        status=status,
        technician_id=technician_id,
        created_by="seed",
        created_at=now - timedelta(days=30),
    )
    if status == "closed":
        try:
            order.closed_at = parse_timestamp(rec["closed_at"]) if rec.get("closed_at") else now - timedelta(days=20)
        except (ValueError, TypeError):
            raise refuse("validation_failed", "closed_at must be an RFC 3339 UTC timestamp", field="closed_at") from None
        order.resolution = str(rec.get("resolution") or "Seeded history: work completed.")
        order.labor_hours = Decimal(str(rec.get("labor_hours", "1.00"))).quantize(Decimal("0.01"))
        order.parts = parts
        order.parts_cost = sum((catalogue[number] * quantity for number, quantity in parts), Decimal("0"))
    return order


def cleanup_tenant(env: Env, tenant_id: str) -> Reply:
    try:
        tenant = _disposable(env, tenant_id, must_exist=False)
    except Refusal as refusal:
        return _refusal(refusal, 403)
    remaining = env.store.delete_tenant(tenant_id)
    body: dict[str, Any] = {"tenant_id": tenant_id, "existed": tenant is not None, "remaining": remaining}
    if tenant is None:
        return 404, body
    return 200, {"status": "ok", **body}


_CSRF = re.compile(r'name="csrf" value="([0-9a-f]+)"')
_REFUSED_BY_PAGE = {401: ("permission_denied", 403), 403: ("permission_denied", 403), 404: ("scope_violation", 404), 410: ("expired", 409)}


def approve_step_up(env: Env, tenant_id: str, body: Any) -> Reply:
    """Complete a prepared step-up as the supervisor who prepared it, through the step-up UI itself.

    The body names the workspace and the principal the action was prepared for, the capability, and
    ``values``: the facts Prepare stated, from which the application takes its own ``step_up_reference``.
    """
    try:
        tenant = _disposable(env, tenant_id, must_exist=True)
        assert tenant is not None
        data = _object(body, "the request body")
        workspace = _identifier(data.get("workspace_id"), "workspace_id")
        principal_id = _identifier(data.get("principal_id"), "principal_id")
        if data.get("capability_id") != "assets.decommission":
            raise refuse("validation_failed", "only assets.decommission has a step-up here", field="capability_id")
        reference = _object(data.get("values"), "values").get("step_up_reference")
        if not isinstance(reference, str) or not reference:
            raise refuse("validation_failed", "values must state the step_up_reference", field="values")
        step_up = tenant.step_ups.get(reference)
        principal = tenant.principals.get((workspace, principal_id))
        if step_up is None or principal is None or step_up.workspace != workspace or step_up.principal_id != principal_id:
            raise refuse("scope_violation", "there is no such request for this principal", field="values")
        if principal.role != ROLE_SUPERVISOR or principal.test_support_passcode is None:
            raise refuse("permission_denied", "only a supervisor with a step-up passcode can complete it", field="principal_id")
    except Refusal as refusal:
        status = {"permission_denied": 403, "scope_violation": 404}.get(refusal.code, 422)
        return _refusal(refusal, status)

    token = base64.b64encode(f"{principal.id}:{principal.test_support_passcode}".encode()).decode()
    signed_in = {"Authorization": "Basic " + token}
    page = stepup.handle(env, "GET", reference, signed_in["Authorization"], b"")
    if page.status == 200 and "already decommissioned" not in page.html:
        csrf = _CSRF.search(page.html)
        if csrf is None:
            return _refusal(refuse("internal", "the step-up page has no anti-forgery token"), 500)
        page = stepup.handle(env, "POST", reference, signed_in["Authorization"], f"csrf={csrf.group(1)}".encode())
    if page.status != 200:
        code, status = _REFUSED_BY_PAGE.get(page.status, ("conflict", 409))
        return _refusal(refuse(code, "the step-up could not be completed"), status)
    return 200, {"status": "ok", "tenant_id": tenant_id, "approved": True}
