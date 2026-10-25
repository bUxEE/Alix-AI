"""Identity, roles, permissions and record-level visibility.

The identity AlixAI sends with every call (``X-AlixAI-Identity``, unpadded base64url of JSON) is
the minimum the host needs to reauthorise the request itself. The host never trusts the roles it
asserts: it looks the principal up in its own directory, inside the tenant and the site the
identity names.
"""

from __future__ import annotations

import base64
import binascii
import json
import re
from dataclasses import dataclass
from typing import Any

from .errors import refuse
from .store import ROLE_AUDITOR, ROLE_SUPERVISOR, ROLE_TECHNICIAN, Asset, Store, TenantData, Technician, WorkOrder

APPLICATION_ID = "facility-maintenance"

ROLES = (ROLE_TECHNICIAN, ROLE_SUPERVISOR, ROLE_AUDITOR)

# The capabilities of the application and the one permission each needs. The package declares the
# same permissions; a test keeps both in step.
CAPABILITY_PERMISSION = {
    "workorders.list": "workorders.read",
    "workorders.create": "workorders.create",
    "workorders.assign": "workorders.assign",
    "workorders.close": "workorders.close",
    "assets.decommission": "assets.decommission",
}

# What a role may do at all. Which records it may do it to is a second, record-level question.
ROLE_PERMISSIONS = {
    ROLE_TECHNICIAN: ("workorders.read", "workorders.create", "workorders.close"),
    ROLE_SUPERVISOR: (
        "workorders.read",
        "workorders.create",
        "workorders.assign",
        "workorders.close",
        "assets.decommission",
    ),
    ROLE_AUDITOR: ("workorders.read",),
}

_IDENTIFIER = re.compile(r"^[^\s\x00-\x1f\x7f*]{1,128}$")


def is_identifier(value: Any) -> bool:
    return isinstance(value, str) and _IDENTIFIER.match(value) is not None


@dataclass(frozen=True)
class Caller:
    tenant: TenantData
    workspace: str
    site_name: str
    principal_id: str
    principal_name: str
    role: str
    technician_id: str | None
    session_id: str


def decode_identity(header: str | None) -> dict[str, Any]:
    """Decode the identity header. Missing is ``scope_missing``; present but malformed is not."""
    if header is None or header.strip() == "":
        raise refuse("scope_missing", "the request carries no identity context")
    try:
        padded = header + "=" * (-len(header) % 4)
        raw = json.loads(base64.urlsafe_b64decode(padded.encode("ascii")))
    except (binascii.Error, ValueError, UnicodeError):
        raise refuse("scope_violation", "the identity context is malformed") from None
    if not isinstance(raw, dict):
        raise refuse("scope_violation", "the identity context is malformed")
    return raw


def parse_identity(raw: dict[str, Any]) -> dict[str, Any]:
    """Missing levels are ``scope_missing``; a present but malformed level is ``scope_violation``."""
    levels = ("application_id", "tenant_id", "workspace_id", "principal_id", "session_id")
    roles = raw.get("roles")
    missing = [lv for lv in levels if not (isinstance(raw.get(lv), str) and raw[lv].strip())]
    if not isinstance(roles, list) or not roles:
        missing.append("roles")
    if missing:
        raise refuse("scope_missing", "scope level missing: " + ", ".join(missing))
    malformed = [lv for lv in levels if not is_identifier(raw[lv])]
    if any(not is_identifier(role) for role in roles):
        malformed.append("roles")
    if malformed:
        raise refuse("scope_violation", "scope level malformed: " + ", ".join(malformed))
    return {**{lv: raw[lv] for lv in levels}, "roles": sorted(set(roles))}


def check_application(identity: dict[str, Any]) -> None:
    if identity["application_id"] != APPLICATION_ID:
        raise refuse("scope_violation", "the identity belongs to another application")


def resolve_caller(store: Store, header: str | None, permission: str | None) -> Caller:
    """Reauthorise one request.

    Every refusal here discloses nothing about other tenants or sites: an unknown tenant, an
    unknown site and a principal that belongs somewhere else are all the same ``scope_violation``.
    Asserting a role the principal does not hold is an escalation attempt, not a scope problem.
    """
    identity = parse_identity(decode_identity(header))
    check_application(identity)
    tenant = store.tenants.get(identity["tenant_id"])
    site = tenant.sites.get(identity["workspace_id"]) if tenant else None
    principal = tenant.principals.get((identity["workspace_id"], identity["principal_id"])) if tenant else None
    if tenant is None or site is None or principal is None:
        raise refuse("scope_violation", "the principal is not known in this tenant and workspace")
    if any(role != principal.role for role in identity["roles"]):
        raise refuse("permission_denied", "the identity asserts a role the principal does not hold")
    if permission is not None and permission not in ROLE_PERMISSIONS[principal.role]:
        raise refuse("permission_denied", "the role does not allow this operation")
    return Caller(
        tenant=tenant,
        workspace=site.id,
        site_name=site.name,
        principal_id=principal.id,
        principal_name=principal.name,
        role=principal.role,
        technician_id=principal.technician_id,
        session_id=identity["session_id"],
    )


# ---- record-level visibility -------------------------------------------------------------------
#
# A record the caller may not see is simply absent: lookups of it fail exactly like lookups of a
# record that does not exist, so the answer never says which.


def visible_asset(caller: Caller, asset_id: Any) -> Asset:
    asset = caller.tenant.assets.get(asset_id) if isinstance(asset_id, str) else None
    if asset is None or asset.workspace != caller.workspace:
        raise refuse("scope_violation", "the asset is not available to you")
    return asset


def can_see_technician(caller: Caller, technician: Technician) -> bool:
    if technician.workspace != caller.workspace:
        return False
    # A technician sees only themselves; supervisors and auditors see the site's technicians.
    return caller.role != ROLE_TECHNICIAN or technician.id == caller.technician_id


def visible_technician(caller: Caller, technician_id: Any) -> Technician:
    technician = caller.tenant.technicians.get(technician_id) if isinstance(technician_id, str) else None
    if technician is None or not can_see_technician(caller, technician):
        raise refuse("scope_violation", "the technician is not available to you")
    return technician


def can_see_work_order(caller: Caller, order: WorkOrder) -> bool:
    if order.workspace != caller.workspace:
        return False
    # Supervisors and auditors see the whole site; a technician sees the work assigned to them and
    # the work they raised themselves.
    if caller.role != ROLE_TECHNICIAN:
        return True
    assigned = caller.technician_id is not None and order.technician_id == caller.technician_id
    return assigned or order.created_by == caller.principal_id


def visible_work_order(caller: Caller, order_id: Any) -> WorkOrder:
    order = caller.tenant.work_orders.get(order_id) if isinstance(order_id, str) else None
    if order is None or not can_see_work_order(caller, order):
        raise refuse("scope_violation", "the work order is not available to you")
    return order
