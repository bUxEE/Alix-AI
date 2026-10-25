"""The package's own test suite (tests/facility-safety.yaml), run against the application the way the
integration audit runs it: a disposable world is created from the suite's tenants, principals and
records through the provisioning operations, and every scope probe and every safety probe the
application can answer on its own is checked against what the suite declares.

(The unconfirmed and cancelled kinds are decided by AlixAI's coordinator before anything reaches the
application, so only the audit can run them; they are not simulated here.)
"""

from __future__ import annotations

import base64
import json
from dataclasses import dataclass
from datetime import timedelta
from pathlib import Path
from typing import Any

import pytest
import yaml
from conftest import APPLICATION, START
from fastapi.testclient import TestClient

from facility_maintenance.app import create_app
from facility_maintenance.clock import FixedClock, to_timestamp
from facility_maintenance.config import Settings

PACKAGE = Path(__file__).resolve().parent.parent / "package"
SUITE = yaml.safe_load((PACKAGE / "tests" / "facility-safety.yaml").read_text(encoding="utf-8"))
SEED = {r["ref"]: r for r in json.loads((PACKAGE / "tests" / "seed" / "facility.json").read_text(encoding="utf-8"))["records"]}
CAPABILITIES = {p.stem.replace("-", "."): yaml.safe_load(p.read_text(encoding="utf-8")) for p in (PACKAGE / "capabilities").glob("*.yaml")}
SLUGS = {"workorders.create": "work-orders-create", "workorders.assign": "work-orders-assign", "workorders.close": "work-orders-close", "assets.decommission": "assets-decommission"}
SUFFIX = "a1b2c3"


@dataclass
class Principal:
    ref: str
    tenant_id: str
    workspace_id: str
    principal_id: str
    role: str

    @property
    def headers(self) -> dict[str, str]:
        body = {"application_id": APPLICATION, "tenant_id": self.tenant_id, "workspace_id": self.workspace_id, "principal_id": self.principal_id, "roles": [self.role], "session_id": "ses_01J0000000000000000000000A"}
        return {"X-AlixAI-Identity": base64.urlsafe_b64encode(json.dumps(body).encode()).rstrip(b"=").decode()}


class AuditWorld:
    """The disposable world of the suite, generated the way the audit generates it."""

    def __init__(self) -> None:
        self.clock = FixedClock(START)
        self.client = TestClient(create_app(Settings(test_support=True), clock=self.clock))
        name = lambda prefix, ref: f"{prefix}_{ref.replace('-', '_')}_{SUFFIX}"  # noqa: E731
        fixtures = SUITE["fixtures"]
        self.tenants = {t["ref"]: name("tnt", t["ref"]) for t in fixtures["tenants"]}
        self.workspaces = {w: name("ws", w) for t in fixtures["tenants"] for w in t["workspaces"]}
        self.principals = {
            p["ref"]: Principal(p["ref"], self.tenants[p["tenant"]], self.workspaces[p["workspace"]], name("usr", p["ref"]), p["roles"][0]) for p in fixtures["principals"]
        }
        self.ids: dict[str, str] = {}
        for tenant in fixtures["tenants"]:
            created = self.client.post(
                "/v1/test/tenants",
                json={
                    "tenant_id": self.tenants[tenant["ref"]],
                    "workspaces": [{"workspace_id": self.workspaces[w]} for w in tenant["workspaces"]],
                    "principals": [
                        {"principal_id": p.principal_id, "workspace_id": p.workspace_id, "roles": [p.role]}
                        for p in self.principals.values()
                        if p.tenant_id == self.tenants[tenant["ref"]]
                    ],
                },
            )
            assert created.status_code == 201, created.text
            records = []
            for rec in fixtures["records"]:
                if rec["tenant"] != tenant["ref"]:
                    continue
                members = {k: v for k, v in SEED[rec["ref"]].items() if k not in ("workspace", "principal", "entity_type", "ref")}
                body = {"ref": rec["ref"], "entity_type": rec["entity_type"], "workspace_id": self.workspaces[rec["workspace"]], **members}
                if "principal" in SEED[rec["ref"]]:
                    body["principal_id"] = self.principals[SEED[rec["ref"]]["principal"]].principal_id
                records.append(body)
            seeded = self.client.post(f"/v1/test/tenants/{self.tenants[tenant['ref']]}/seed", json={"records": records})
            assert seeded.status_code == 200, seeded.text
            self.ids.update({r["ref"]: r["entity_id"] for r in seeded.json()["records"]})

    # ---- requests built from what the package declares, as the audit builds them -----------------------------------

    def record_type(self, ref: str) -> str:
        return next(r["entity_type"] for r in SUITE["fixtures"]["records"] if r["ref"] == ref)

    def first_of(self, entity_type: str, principal: Principal) -> str:
        for rec in SUITE["fixtures"]["records"]:
            if rec["entity_type"] == entity_type and self.workspaces[rec["workspace"]] == principal.workspace_id:
                return self.ids[rec["ref"]]
        raise AssertionError(f"no {entity_type} in the workspace of {principal.ref}")

    def fields_for(self, capability_id: str, principal: Principal, target: str | None = None) -> dict[str, Any]:
        contract = CAPABILITIES[capability_id]
        fields: dict[str, Any] = {}
        examples = sorted(contract["examples"], key=lambda e: e["id"])
        for f in contract["fields"]:
            if f["type"] == "entity_ref":
                if target and self.record_type(target) == f["entity_type"]:
                    fields[f["name"]] = self.ids[target]
                elif f["required"]:
                    fields[f["name"]] = self.first_of(f["entity_type"], principal)
                continue
            if not f["required"]:
                continue
            if "default" in f:
                fields[f["name"]] = f["default"]["value"]
            elif f["type"] == "enum":
                fields[f["name"]] = f["values"][0]
            else:
                fields[f["name"]] = next(e["expected_fields"][f["name"]] for e in examples if f["name"] in e["expected_fields"])
        return fields

    def request(self, op: str, capability_id: str, principal: Principal, fields: dict[str, Any]):
        if op == "read":
            return self.client.get("/v1/work-orders", params=fields, headers=principal.headers)
        return self.client.post(f"/v1/{SLUGS[capability_id]}/{op}", json=fields, headers=principal.headers)

    def prepared_action(self, capability_id: str, prepared: dict[str, Any], key: str, *, expires_in: int | None = None) -> dict[str, Any]:
        prep = prepared["preparation"]
        lifetime = timedelta(days=365) if expires_in is None else timedelta(seconds=expires_in)
        return {
            "preparation_id": "prep_01J0000000000000000000000A", "capability_id": capability_id, "state": "prepared", "summary": prep["summary"],
            "effects": prep["effects"], "risk_level": prep["declared_risk_level"], "data_versions": json.loads(json.dumps(prep["data_versions"])),
            "prepared_at": to_timestamp(self.clock.now()), "expires_at": to_timestamp(self.clock.now() + lifetime),
            "invalidation_conditions": ["data_version_changed"], "confirmation": {"mode": "none", "bindings": []}, "idempotency_key": key,
        }


@pytest.fixture(scope="module")
def audit_world() -> AuditWorld:
    return AuditWorld()


SCOPE_PROBES = [p for p in SUITE["scope_probes"] if p["operation"] != "retrieve"]


def test_the_suite_declares_what_it_must() -> None:
    assert {p["expect"] for p in SUITE["scope_probes"]} == {"allow", "deny"}
    covered = {p["capability_id"] for p in SUITE["scope_probes"] if "capability_id" in p}
    assert covered == set(CAPABILITIES)
    assert {p["target_source"] for p in SUITE["scope_probes"] if p["operation"] == "retrieve"} == {"equipment-manuals", "maintenance-history"}
    for capability_id, contract in CAPABILITIES.items():
        kinds = {p["kind"] for p in SUITE["safety_probes"] if p["capability_id"] == capability_id}
        assert kinds == (set() if contract["risk_level"] == 0 else {"confirmed_commit", "unconfirmed_commit", "replayed_commit", "expired_preparation", "stale_record", "cancelled_preparation", "uncertain_commit_recovery"})


@pytest.mark.parametrize("probe", SCOPE_PROBES, ids=lambda p: p["id"])
def test_scope_probe(audit_world: AuditWorld, probe: dict[str, Any]) -> None:
    principal = audit_world.principals[probe["principal"]]
    fields = audit_world.fields_for(probe["capability_id"], principal, probe.get("target_record"))
    result = audit_world.request(probe["operation"], probe["capability_id"], principal, fields).json()
    if probe["expect"] == "allow":
        assert result["status"] == "ok", (fields, result)
        return
    assert result["status"] == "rejected" and result["errors"][0]["code"] == probe["error_code"], (fields, result)
    assert result["identifiers"] == [] and result["values"] == []  # nothing about the target is disclosed


def observe(world: AuditWorld, capability_id: str, principal: Principal, kind: str) -> tuple[str, str | None]:
    """What the application does for one kind of safe-execution probe, as (outcome, error code)."""
    fields = world.fields_for(capability_id, principal)
    prepared = world.request("prepare", capability_id, principal, fields).json()
    assert prepared["status"] == "ok", (capability_id, fields, prepared)
    slug, key = SLUGS[capability_id], f"probe-{kind[:8]}-{capability_id.replace('.', '')}-0001"
    status = lambda k: world.client.get(f"/v1/{slug}/status/{k}", headers=principal.headers).json()  # noqa: E731
    commit = lambda action: world.client.post(f"/v1/{slug}/commit", json=action, headers={**principal.headers, "Idempotency-Key": action["idempotency_key"]}).json()  # noqa: E731
    if kind == "expired_preparation":
        action = world.prepared_action(capability_id, prepared, key, expires_in=-3600)
        result = commit(action)
        assert status(key)["errors"][0]["field"] == "idempotency_key"  # nothing was recorded
        return "rejected_no_effect", result["errors"][0]["code"]
    if kind == "stale_record":
        action = world.prepared_action(capability_id, prepared, key)
        for version in action["data_versions"]:
            version["version"] += "~probe-stale"
        result = commit(action)
        assert status(key)["errors"][0]["field"] == "idempotency_key"
        return "rejected_no_effect", result["errors"][0]["code"]
    action = world.prepared_action(capability_id, prepared, key)
    first = commit(action)
    if first["status"] != "ok":  # the level 3 action: refused, nothing recorded, so a retry is safe and applies nothing
        again = commit(action)
        assert again == first and status(key)["errors"][0]["field"] == "idempotency_key"
        return ("recovered_via_status" if kind == "uncertain_commit_recovery" else "rejected_no_effect"), (None if kind == "uncertain_commit_recovery" else first["errors"][0]["code"])
    lost_reply = status(key)  # the caller kept only the key
    assert lost_reply == first
    replay = commit(action)
    if kind == "uncertain_commit_recovery":
        assert replay == first and status(key) == first
        return "recovered_via_status", None
    return ("original_receipt_returned" if kind == "replayed_commit" else "committed_once"), None


SAFETY_PROBES = [p for p in SUITE["safety_probes"] if p["kind"] not in ("unconfirmed_commit", "cancelled_preparation")]


@pytest.mark.parametrize("probe", SAFETY_PROBES, ids=lambda p: p["id"])
def test_safety_probe(probe: dict[str, Any]) -> None:
    world = AuditWorld()  # an effect is applied, so every probe gets a world of its own
    outcome, code = observe(world, probe["capability_id"], world.principals[probe["principal"]], probe["kind"])
    assert outcome == probe["expect"]["outcome"]
    assert code == probe["expect"].get("error_code")


def test_a_probe_that_commits_applies_the_effect_exactly_once() -> None:
    world = AuditWorld()
    manager = world.principals["sup-a1"]
    observe(world, "workorders.close", manager, "replayed_commit")
    tenant = world.client.app.state.env.store.tenants[manager.tenant_id]
    closed = [o for o in tenant.work_orders.values() if o.status == "closed" and o.closed_at == world.clock.now()]
    assert len(closed) == 1
    assert tenant.parts[(manager.workspace_id, "FLT-204")].stock == 12  # no parts were named, none consumed
