"""The in-memory data model: one container per tenant, so nothing can reach across tenants.

Tenancy: a deployment serves ONE tenant (the operator's organisation, ``Settings.tenant_id``) with
many workspaces, which are its sites. Disposable tenants exist only for the integration audit and
only when test support is on; they are separate containers that the production tenant can never
see, and that cleanup removes completely.

Every record belongs to a workspace (a site). Every lookup goes through the tenant container and
then checks the workspace, so a record of another site or tenant is exactly as absent as one that
does not exist.
"""

from __future__ import annotations

import hashlib
import hmac
import secrets
import threading
from dataclasses import dataclass, field
from datetime import datetime
from decimal import Decimal
from typing import Any

# The synthetic spare-parts catalogue every site is stocked with: (part number, name, stock, EUR).
STANDARD_PARTS: tuple[tuple[str, str, int, str], ...] = (
    ("FLT-204", "Pleated air filter 592x592", 12, "18.50"),
    ("BLT-11", "V-belt B-section 1180 mm", 6, "9.80"),
    ("SEL-30", "Mechanical pump seal 30 mm", 4, "42.00"),
    ("CAP-45", "Run capacitor 45 uF", 8, "11.25"),
)

COUNT_KINDS = (
    "sites",
    "principals",
    "assets",
    "technicians",
    "parts",
    "work_orders",
    "step_ups",
    "notifications",
    "idempotency_keys",
)

ROLE_TECHNICIAN = "technician"
ROLE_SUPERVISOR = "supervisor"
ROLE_AUDITOR = "auditor"


@dataclass
class Site:
    id: str
    name: str


@dataclass
class Principal:
    id: str
    workspace: str
    name: str
    role: str
    technician_id: str | None = None
    passcode_salt: bytes | None = None
    passcode_hash: bytes | None = None
    # Only a disposable tenant made by test support carries this: the passcode test support generated for
    # a supervisor, so that its approval path can sign in to the step-up UI as that supervisor. It is never
    # put in a reply, a log or a report, exists only in memory and goes with the tenant.
    test_support_passcode: str | None = None


@dataclass
class Asset:
    id: str
    workspace: str
    tag: str
    name: str
    category: str
    status: str = "active"  # active | decommissioned
    rev: int = 1


@dataclass
class Technician:
    id: str
    workspace: str
    name: str
    trade: str
    principal_id: str | None = None
    active: bool = True
    rev: int = 1


@dataclass
class Part:
    part_number: str
    workspace: str
    name: str
    stock: int
    unit_cost: Decimal
    rev: int = 1


@dataclass
class WorkOrder:
    id: str
    workspace: str
    asset_id: str
    title: str
    description: str
    priority: str  # low | normal | high | urgent
    status: str = "open"  # open | assigned | closed
    technician_id: str | None = None
    created_by: str = ""
    created_at: datetime | None = None
    closed_at: datetime | None = None
    resolution: str = ""
    labor_hours: Decimal = Decimal("0")
    parts: list[tuple[str, int]] = field(default_factory=list)
    parts_cost: Decimal = Decimal("0")
    rev: int = 1


@dataclass
class StepUp:
    """A decommission waiting for the supervisor's own step-up in the application's UI."""

    reference: str
    workspace: str
    principal_id: str
    asset_id: str
    asset_rev: int
    reason: str
    expires_at: datetime
    status: str = "pending"  # pending | done | locked | expired
    failed_attempts: int = 0


@dataclass
class Notification:
    reference: str
    workspace: str
    kind: str
    recipient_id: str
    subject_id: str


@dataclass
class IdempotencyRecord:
    capability_id: str
    principal_id: str
    fingerprint: str
    response: dict[str, Any]
    created_at: datetime


@dataclass
class TenantData:
    id: str
    disposable: bool
    sites: dict[str, Site] = field(default_factory=dict)
    principals: dict[tuple[str, str], Principal] = field(default_factory=dict)
    assets: dict[str, Asset] = field(default_factory=dict)
    technicians: dict[str, Technician] = field(default_factory=dict)
    parts: dict[tuple[str, str], Part] = field(default_factory=dict)
    work_orders: dict[str, WorkOrder] = field(default_factory=dict)
    step_ups: dict[str, StepUp] = field(default_factory=dict)
    outbox: dict[str, Notification] = field(default_factory=dict)
    idempotency: dict[tuple[str, str], IdempotencyRecord] = field(default_factory=dict)

    def counts(self) -> dict[str, int]:
        return {
            "sites": len(self.sites),
            "principals": len(self.principals),
            "assets": len(self.assets),
            "technicians": len(self.technicians),
            "parts": len(self.parts),
            "work_orders": len(self.work_orders),
            "step_ups": len(self.step_ups),
            "notifications": len(self.outbox),
            "idempotency_keys": len(self.idempotency),
        }


class Store:
    """All tenants, behind one lock: every operation runs as a unit."""

    def __init__(self) -> None:
        self.lock = threading.RLock()
        self.tenants: dict[str, TenantData] = {}
        self.step_up_index: dict[str, str] = {}  # step-up reference -> tenant id
        # Deterministic identifiers: a prefix and a counter. The counters belong to the deployment,
        # not to a tenant, so an identifier names one record in one tenant and no other: a record
        # of another tenant is never reachable by presenting its identifier.
        self.counters: dict[str, int] = {}
        self.secret = secrets.token_bytes(32)  # keys the UI's anti-forgery tokens; never persisted

    def next_id(self, kind: str) -> str:
        start, width = {"WO": (1001, 4), "AST": (1, 4), "TEC": (1, 3), "NTF": (1, 4), "SU": (1, 4)}[kind]
        number = self.counters.get(kind, start - 1) + 1
        self.counters[kind] = number
        return f"{kind}-{number:0{width}d}"

    # ---- tenants and sites --------------------------------------------------------------

    def create_tenant(self, tenant_id: str, *, disposable: bool) -> TenantData:
        if tenant_id in self.tenants:
            raise KeyError(tenant_id)
        tenant = TenantData(id=tenant_id, disposable=disposable)
        self.tenants[tenant_id] = tenant
        return tenant

    def add_site(self, tenant: TenantData, site_id: str, name: str | None = None) -> Site:
        site = Site(id=site_id, name=name or site_id)
        tenant.sites[site_id] = site
        for number, part_name, stock, cost in STANDARD_PARTS:
            tenant.parts[(site_id, number)] = Part(number, site_id, part_name, stock, Decimal(cost))
        return site

    def add_principal(
        self,
        tenant: TenantData,
        site_id: str,
        principal_id: str,
        role: str,
        *,
        name: str | None = None,
        technician_id: str | None = None,
        passcode: str | None = None,
    ) -> Principal:
        principal = Principal(
            id=principal_id,
            workspace=site_id,
            name=name or principal_id,
            role=role,
            technician_id=technician_id,
        )
        tenant.principals[(site_id, principal_id)] = principal
        if passcode is not None:
            self.set_passcode(principal, passcode)
        return principal

    @staticmethod
    def set_passcode(principal: Principal, passcode: str) -> None:
        """Store a step-up passcode as a salted scrypt hash; the passcode itself is never kept."""
        principal.passcode_salt = secrets.token_bytes(16)
        principal.passcode_hash = _scrypt(passcode, principal.passcode_salt)

    @staticmethod
    def check_passcode(principal: Principal, passcode: str) -> bool:
        if principal.passcode_salt is None or principal.passcode_hash is None:
            return False
        return hmac.compare_digest(_scrypt(passcode, principal.passcode_salt), principal.passcode_hash)

    # ---- records ------------------------------------------------------------------------

    def add_asset(self, tenant: TenantData, site_id: str, *, name: str, tag: str, category: str) -> Asset:
        asset = Asset(self.next_id("AST"), site_id, tag, name, category)
        tenant.assets[asset.id] = asset
        return asset

    def add_technician(
        self, tenant: TenantData, site_id: str, *, name: str, trade: str, principal_id: str | None = None
    ) -> Technician:
        technician = Technician(self.next_id("TEC"), site_id, name, trade, principal_id)
        tenant.technicians[technician.id] = technician
        if principal_id is not None and (site_id, principal_id) in tenant.principals:
            tenant.principals[(site_id, principal_id)].technician_id = technician.id
        return technician

    def add_work_order(self, tenant: TenantData, **values: Any) -> WorkOrder:
        order = WorkOrder(id=self.next_id("WO"), **values)
        tenant.work_orders[order.id] = order
        return order

    def delete_tenant(self, tenant_id: str) -> dict[str, int]:
        """Remove a tenant and everything that hangs off it; returns what is left (all zero)."""
        tenant = self.tenants.pop(tenant_id, None)
        if tenant is not None:
            for reference in tenant.step_ups:
                self.step_up_index.pop(reference, None)
        return self.remaining(tenant_id)

    def remaining(self, tenant_id: str) -> dict[str, int]:
        """What is left of a tenant, by kind of record: all zero once it is gone."""
        left = self.tenants.get(tenant_id)
        counts = left.counts() if left is not None else dict.fromkeys(COUNT_KINDS, 0)
        counts["step_up_index"] = sum(1 for t in self.step_up_index.values() if t == tenant_id)
        return counts


def _scrypt(passcode: str, salt: bytes) -> bytes:
    return hashlib.scrypt(passcode.encode(), salt=salt, n=2**12, r=8, p=1, dklen=32)
