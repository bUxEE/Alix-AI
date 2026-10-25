"""Synthetic demonstration data for the production tenant. Every name is made up."""

from __future__ import annotations

from datetime import timedelta
from decimal import Decimal

from .clock import Clock
from .store import ROLE_AUDITOR, ROLE_SUPERVISOR, ROLE_TECHNICIAN, Store, TenantData


def seed_demo(store: Store, tenant: TenantData, clock: Clock, passcode: str | None) -> None:
    """Two sites with a supervisor, two technicians, an auditor, assets and some history.

    Without a passcode the supervisors have none, which leaves the step-up UI shut.
    """
    for site_id, name in (("site-north", "North Campus"), ("site-south", "South Depot")):
        store.add_site(tenant, site_id, name)
    store.add_principal(tenant, "site-north", "sup-elena", ROLE_SUPERVISOR, name="Elena Conti", passcode=passcode)
    store.add_principal(tenant, "site-north", "tech-marco", ROLE_TECHNICIAN, name="Marco Bianchi")
    store.add_principal(tenant, "site-north", "tech-giulia", ROLE_TECHNICIAN, name="Giulia Ferri")
    store.add_principal(tenant, "site-north", "aud-paolo", ROLE_AUDITOR, name="Paolo Neri")
    store.add_principal(tenant, "site-south", "sup-luca", ROLE_SUPERVISOR, name="Luca Romano", passcode=passcode)

    marco = store.add_technician(tenant, "site-north", name="Marco Bianchi", trade="HVAC", principal_id="tech-marco")
    giulia = store.add_technician(tenant, "site-north", name="Giulia Ferri", trade="electrical", principal_id="tech-giulia")
    store.add_technician(tenant, "site-south", name="Sara Gallo", trade="plumbing")
    ahu = store.add_asset(tenant, "site-north", name="Air handling unit", tag="AHU-200", category="hvac")
    chiller = store.add_asset(tenant, "site-north", name="Chiller", tag="CHL-500", category="hvac")
    store.add_asset(tenant, "site-north", name="Pump", tag="PMP-30", category="pumps")
    store.add_asset(tenant, "site-south", name="Backup generator", tag="GEN-90", category="power")

    now = clock.now()
    store.add_work_order(
        tenant, workspace="site-north", asset_id=ahu.id, title="Supply fan belt squeals at start-up",
        description="Squeal for the first minute after start-up.", priority="high", status="assigned",
        technician_id=marco.id, created_by="sup-elena", created_at=now - timedelta(days=2),
    )
    store.add_work_order(
        tenant, workspace="site-north", asset_id=chiller.id, title="Chilled water temperature drifts upward",
        description="Supply temperature rises about 2 C over the afternoon.", priority="urgent", status="open",
        created_by="tech-giulia", created_at=now - timedelta(days=1),
    )
    closed = store.add_work_order(
        tenant, workspace="site-north", asset_id=ahu.id, title="Replace clogged return filter",
        description="Differential pressure alarm on the return filter.", priority="normal", status="closed",
        technician_id=giulia.id, created_by="sup-elena", created_at=now - timedelta(days=40),
    )
    closed.closed_at = now - timedelta(days=38)
    closed.resolution = "Replaced the return filter; differential pressure back to 120 Pa."
    closed.labor_hours = Decimal("0.75")
    closed.parts = [("FLT-204", 2)]
    closed.parts_cost = Decimal("37.00")
