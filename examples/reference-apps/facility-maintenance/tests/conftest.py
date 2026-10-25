"""Shared fixtures: an application with a frozen clock and a small disposable world."""

from __future__ import annotations

import base64
import json
from dataclasses import dataclass, field
from datetime import timedelta
from typing import Any

import pytest
from fastapi.testclient import TestClient

from facility_maintenance.app import create_app
from facility_maintenance.clock import FixedClock, to_timestamp
from facility_maintenance.config import Settings

APPLICATION = "facility-maintenance"
START = "2026-10-01T09:00:00Z"


def identity(tenant: str, workspace: str, principal: str, role: str, *, application: str = APPLICATION) -> dict[str, str]:
    """The X-AlixAI-Identity header AlixAI sends: unpadded base64url of the minimum identity."""
    body = {
        "application_id": application,
        "tenant_id": tenant,
        "workspace_id": workspace,
        "principal_id": principal,
        "roles": [role],
        "session_id": "ses_01J0000000000000000000000A",
    }
    raw = base64.urlsafe_b64encode(json.dumps(body).encode()).rstrip(b"=").decode()
    return {"X-AlixAI-Identity": raw}


@dataclass
class Who:
    tenant: str
    workspace: str
    principal: str
    role: str

    @property
    def headers(self) -> dict[str, str]:
        return identity(self.tenant, self.workspace, self.principal, self.role)


@dataclass
class World:
    client: TestClient
    clock: FixedClock
    app: Any
    ids: dict[str, str] = field(default_factory=dict)
    keys: int = 0

    # people of tenant t1, site s1 (and one supervisor of site s2, one of tenant t2)
    sup = Who("t1", "s1", "sup1", "supervisor")
    tech = Who("t1", "s1", "tech1", "technician")
    tech2 = Who("t1", "s1", "tech2", "technician")
    aud = Who("t1", "s1", "aud1", "auditor")
    sup_s2 = Who("t1", "s2", "sup2", "supervisor")
    sup_t2 = Who("t2", "s9", "sup9", "supervisor")

    def key(self) -> str:
        self.keys += 1
        return f"key-{self.keys:016d}-abcdef"

    # ---- calls ------------------------------------------------------------------------------------

    def read(self, who: Who, **query: Any) -> dict[str, Any]:
        return self.client.get("/v1/work-orders", params=query, headers=who.headers).json()

    def resolve(self, who: Who, kind: str, **body: Any) -> dict[str, Any]:
        return self.client.post(f"/v1/resolve/{kind}", json=body, headers=who.headers).json()

    def call(self, who: Who, slug: str, verb: str, fields: dict[str, Any]):
        return self.client.post(f"/v1/{slug}/{verb}", json=fields, headers=who.headers)

    def prepare(self, who: Who, slug: str, fields: dict[str, Any]) -> dict[str, Any]:
        return self.call(who, slug, "prepare", fields).json()

    def prepared_action(self, capability: str, result: dict[str, Any], *, key: str | None = None, ttl: int | None = None) -> dict[str, Any]:
        """What the core hands back at commit: the preparation, bound to a key and an expiry."""
        prep = result["preparation"]
        now = self.clock.now()
        return {
            "preparation_id": "prep_01J0000000000000000000000A",
            "capability_id": capability,
            "state": "prepared",
            "summary": prep["summary"],
            "effects": prep["effects"],
            "risk_level": prep["declared_risk_level"],
            "data_versions": prep["data_versions"],
            "prepared_at": to_timestamp(now),
            "expires_at": to_timestamp(now + timedelta(seconds=ttl or prep["valid_for_seconds"])),
            "invalidation_conditions": ["data_version_changed"],
            "confirmation": {"mode": "none", "bindings": []},
            "idempotency_key": key or self.key(),
        }

    def commit(self, who: Who, slug: str, action: dict[str, Any]):
        return self.client.post(
            f"/v1/{slug}/commit", json=action, headers={**who.headers, "Idempotency-Key": action["idempotency_key"]}
        )

    def status(self, who: Who, slug: str, key: str):
        return self.client.get(f"/v1/{slug}/status/{key}", headers=who.headers)

    def run(self, who: Who, slug: str, capability: str, fields: dict[str, Any]) -> dict[str, Any]:
        """Prepare and commit in one go; returns the commit reply."""
        prepared = self.prepare(who, slug, fields)
        assert prepared["status"] == "ok", prepared
        response = self.commit(who, slug, self.prepared_action(capability, prepared))
        assert response.status_code == 200, response.json()
        return response.json()


def value(result: dict[str, Any], field_name: str) -> Any:
    """The canonical value of a fact of a result."""
    for fact in result["values"]:
        if fact["field"] == field_name:
            return fact["value"]["value"]
    raise KeyError(field_name)


def error_code(result: dict[str, Any]) -> str:
    assert result["status"] == "rejected", result
    return result["errors"][0]["code"]


def build_world(settings: Settings | None = None) -> World:
    clock = FixedClock(START)
    settings = settings or Settings(test_support=True)
    app = create_app(settings, clock=clock)
    client = TestClient(app)
    world = World(client, clock, app)

    created = client.post(
        "/v1/test/tenants",
        json={
            "tenant_id": "t1",
            "workspaces": [{"workspace_id": "s1", "name": "North Campus"}, {"workspace_id": "s2", "name": "South Depot"}],
            "principals": [
                {"principal_id": "sup1", "workspace_id": "s1", "roles": ["supervisor"]},
                {"principal_id": "tech1", "workspace_id": "s1", "roles": ["technician"]},
                {"principal_id": "tech2", "workspace_id": "s1", "roles": ["technician"]},
                {"principal_id": "aud1", "workspace_id": "s1", "roles": ["auditor"]},
                {"principal_id": "sup2", "workspace_id": "s2", "roles": ["supervisor"]},
            ],
        },
    )
    assert created.status_code == 201, created.text
    other = client.post(
        "/v1/test/tenants",
        json={"tenant_id": "t2", "workspaces": [{"workspace_id": "s9"}], "principals": [{"principal_id": "sup9", "workspace_id": "s9", "roles": ["supervisor"]}]},
    )
    assert other.status_code == 201

    def seed(tenant: str, records: list[dict[str, Any]]) -> None:
        reply = client.post(f"/v1/test/tenants/{tenant}/seed", json={"records": records})
        assert reply.status_code == 200, reply.text
        world.ids.update({r["ref"]: r["entity_id"] for r in reply.json()["records"]})

    seed(
        "t1",
        [
            {"ref": "tec1", "entity_type": "technician", "workspace_id": "s1", "principal_id": "tech1", "name": "Marco Bianchi", "trade": "HVAC"},
            {"ref": "tec2", "entity_type": "technician", "workspace_id": "s1", "principal_id": "tech2", "name": "Giulia Ferri", "trade": "electrical"},
            {"ref": "tec_s2", "entity_type": "technician", "workspace_id": "s2", "name": "Sara Gallo"},
            {"ref": "gen", "entity_type": "asset", "workspace_id": "s1", "name": "Backup generator", "tag": "GEN-90"},
            {"ref": "ahu", "entity_type": "asset", "workspace_id": "s1", "name": "Air handling unit", "tag": "AHU-200"},
            {"ref": "pump_s2", "entity_type": "asset", "workspace_id": "s2", "name": "Pump", "tag": "PMP-30"},
            {"ref": "wo_open", "entity_type": "work_order", "workspace_id": "s1", "asset": "ahu", "title": "Return damper sticks", "status": "open", "priority": "normal"},
            {"ref": "wo_mine", "entity_type": "work_order", "workspace_id": "s1", "asset": "ahu", "title": "Supply fan vibration", "status": "assigned", "technician": "tec1", "priority": "high"},
            {"ref": "wo_theirs", "entity_type": "work_order", "workspace_id": "s1", "asset": "ahu", "title": "Filter alarm", "status": "assigned", "technician": "tec2", "priority": "urgent"},
            {"ref": "wo_done", "entity_type": "work_order", "workspace_id": "s1", "asset": "ahu", "title": "Replace return filter", "status": "closed", "technician": "tec2", "resolution": "Replaced the return filter.", "parts": ["FLT-204:2"], "labor_hours": 0.75, "closed_at": "2026-09-10T14:30:00Z"},
            {"ref": "wo_s2", "entity_type": "work_order", "workspace_id": "s2", "asset": "pump_s2", "title": "Pump seal weeping", "status": "open"},
        ],
    )
    seed("t2", [{"ref": "ast_t2", "entity_type": "asset", "workspace_id": "s9", "name": "Chiller", "tag": "CHL-500"}])
    return world


@pytest.fixture
def world() -> World:
    return build_world()
