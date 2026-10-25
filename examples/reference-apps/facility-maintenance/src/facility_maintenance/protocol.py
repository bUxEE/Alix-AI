"""The validate / prepare / commit / status protocol every consequential capability shares.

A capability supplies what is particular to it; this module supplies what must never differ
between them:

* the host reauthorises every call from the identity it receives;
* validate and prepare run the same checks and report the same snapshot version on unchanged data;
* commit applies exactly once per idempotency key, answers a repeat with the original reply,
  refuses a preparation whose data moved (``stale``) or whose time ran out (``expired``), and
  re-checks every rule before it writes;
* status says what was recorded for a key, and "nothing" in the one shape that proves nothing was
  applied.

Nothing is written until every check has passed, and the writes themselves cannot fail, so a
refusal never leaves a half-applied action behind.
"""

from __future__ import annotations

import hashlib
import json
import re
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import timedelta
from typing import Any

from .access import Caller, resolve_caller
from .clock import Clock, parse_timestamp, to_timestamp
from .errors import Refusal, refuse
from .fields import RawFields, parse_fields
from .results import DataVersion, Fact, ok_result, rejected_result, snapshot_version
from .store import IdempotencyRecord, Store

IDEMPOTENCY_WINDOW_SECONDS = 86400  # at least every prepared action's time to live
NO_RECORD = "the application has no record of this idempotency key"
_KEY = re.compile(r"^[A-Za-z0-9_-]{16,128}$")


@dataclass
class Env:
    store: Store
    clock: Clock


@dataclass
class Loaded:
    """The records an action relied on, each with its own revision, plus whatever it parsed."""

    versions: list[DataVersion]
    data: dict[str, Any] = field(default_factory=dict)


@dataclass
class Description:
    values: list[Fact]
    summary: str
    effects: list[dict[str, Any]]
    next_actions_when_prepared: list[dict[str, str]]


@dataclass
class Applied:
    identifiers: list[dict[str, str]]
    values: list[Fact]
    versions_after: list[DataVersion]
    follow_up: dict[str, str] | None = None
    next_actions: list[dict[str, str]] = field(default_factory=list)


@dataclass
class ActionDefinition:
    capability_id: str
    permission: str
    field_names: tuple[str, ...]
    declared_risk_level: int
    valid_for_seconds: int
    # Parse the candidate fields and read the records they name, inside the caller's scope.
    load: Callable[[Env, Caller, RawFields], Loaded]
    # The business rules. They run on validate, prepare and again on commit.
    check: Callable[[Env, Caller, Loaded], None]
    describe: Callable[[Env, Caller, Loaded], Description]
    # The candidate fields the exact effects of a prepared action stand for.
    fields_from_effects: Callable[[list[dict[str, Any]]], RawFields]
    # Write. Runs after every check passed.
    apply: Callable[[Env, Caller, Loaded], Applied]
    # Prepare only: stage something the application owns (the step-up request) and report facts.
    stage: Callable[[Env, Caller, Loaded], list[Fact]] | None = None
    # A capability the AI path may never complete: commit is refused once the preparation proves
    # current, and the effect happens only in the application's own authenticated interface.
    requires_step_up: bool = False


# ---- helpers -------------------------------------------------------------------------------------


def freshness_of(env: Env, versions: list[DataVersion]) -> dict[str, str]:
    return {"as_of": to_timestamp(env.clock.now()), "version": snapshot_version(versions)}


def _checked(definition: ActionDefinition, env: Env, caller: Caller, loaded: Loaded) -> None:
    """Once the records were read, every refusal carries the snapshot version the host saw."""
    try:
        definition.check(env, caller, loaded)
    except Refusal as refusal:
        if refusal.freshness is None:
            refusal.freshness = freshness_of(env, loaded.versions)
        raise


def _canonical_json(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def _fingerprint(capability_id: str, caller: Caller, fields: RawFields) -> str:
    payload = {"capability": capability_id, "principal": caller.principal_id, "fields": fields}
    return hashlib.sha256(_canonical_json(payload).encode()).hexdigest()


def _require_key(key: Any) -> str:
    if not isinstance(key, str) or _KEY.match(key) is None:
        raise refuse("validation_failed", "the idempotency key must be 16 to 128 URL-safe characters", field="idempotency_key")
    return key


def _sweep(env: Env, caller: Caller) -> None:
    """Keys are remembered for the window the package declares; older ones are forgotten."""
    cutoff = env.clock.now() - timedelta(seconds=IDEMPOTENCY_WINDOW_SECONDS)
    keys = caller.tenant.idempotency
    for stale in [k for k, v in keys.items() if v.created_at < cutoff]:
        del keys[stale]


# ---- the prepared action the core hands back at commit --------------------------------------------


@dataclass
class PreparedInput:
    effects: list[dict[str, Any]]
    data_versions: list[DataVersion]
    expires_at: str | None
    capability_id: str | None


def _bad(message: str) -> Refusal:
    return refuse("validation_failed", message, field="prepared")


def parse_prepared(raw: Any) -> PreparedInput:
    """The core sends the whole PreparedAction. The host reads what it needs and trusts none of it.

    It re-derives the fields from the effects and runs every check again.
    """
    if not isinstance(raw, dict):
        raise _bad("the prepared action is missing")
    effects_raw, versions_raw = raw.get("effects"), raw.get("data_versions")
    if not isinstance(effects_raw, list) or not effects_raw or not isinstance(versions_raw, list):
        raise _bad("the prepared action is incomplete")
    effects: list[dict[str, Any]] = []
    for item in effects_raw:
        if not isinstance(item, dict) or not isinstance(item.get("operation"), str) or not isinstance(item.get("entity_type"), str):
            raise _bad("an effect of the prepared action is malformed")
        changes: dict[str, Any] = {}
        for change in item.get("changes") or []:
            value = change.get("value") if isinstance(change, dict) else None
            if (
                isinstance(change, dict)
                and isinstance(change.get("field"), str)
                and isinstance(value, dict)
                and isinstance(value.get("value"), str | int | float | bool)
            ):
                changes[change["field"]] = value["value"]
        effect = {"operation": item["operation"], "entity_type": item["entity_type"], "changes": changes}
        if isinstance(item.get("entity_id"), str):
            effect["entity_id"] = item["entity_id"]
        effects.append(effect)
    versions: list[DataVersion] = []
    for item in versions_raw:
        if not isinstance(item, dict) or not all(isinstance(item.get(k), str) for k in ("entity_type", "entity_id", "version")):
            raise _bad("a data version of the prepared action is malformed")
        versions.append({"entity_type": item["entity_type"], "entity_id": item["entity_id"], "version": item["version"]})
    expires = raw.get("expires_at")
    capability = raw.get("capability_id")
    return PreparedInput(
        effects=effects,
        data_versions=versions,
        expires_at=expires if isinstance(expires, str) else None,
        capability_id=capability if isinstance(capability, str) else None,
    )


# ---- the four operations --------------------------------------------------------------------------


def _evaluate(
    definition: ActionDefinition, env: Env, header: str | None, raw_fields: Any
) -> tuple[Caller, Loaded, Description]:
    caller = resolve_caller(env.store, header, definition.permission)
    fields = parse_fields(raw_fields, definition.field_names)
    loaded = definition.load(env, caller, fields)
    _checked(definition, env, caller, loaded)
    return caller, loaded, definition.describe(env, caller, loaded)


def validate_action(definition: ActionDefinition, env: Env, header: str | None, raw_fields: Any) -> dict[str, Any]:
    """Validate checks candidate values and stages nothing.

    On unchanged data its ``freshness.version`` is exactly the one Prepare stored: both are the
    snapshot of the same records computed by the same function, never a response ETag or a counter
    shared by the site.
    """
    _, loaded, description = _evaluate(definition, env, header, raw_fields)
    return ok_result(values=description.values, freshness=freshness_of(env, loaded.versions))


def prepare_action(definition: ActionDefinition, env: Env, header: str | None, raw_fields: Any) -> dict[str, Any]:
    caller, loaded, description = _evaluate(definition, env, header, raw_fields)
    values = list(description.values)
    if definition.stage is not None:
        values.extend(definition.stage(env, caller, loaded))
    return ok_result(
        values=values,
        freshness=freshness_of(env, loaded.versions),
        allowed_next_actions=description.next_actions_when_prepared,
        preparation={
            "summary": description.summary,
            "effects": description.effects,
            "data_versions": loaded.versions,
            "declared_risk_level": definition.declared_risk_level,
            "valid_for_seconds": definition.valid_for_seconds,
        },
    )


def commit_action(
    definition: ActionDefinition, env: Env, header: str | None, key_value: Any, raw_prepared: Any
) -> dict[str, Any]:
    caller = resolve_caller(env.store, header, definition.permission)
    key = _require_key(key_value)
    _sweep(env, caller)
    prepared = parse_prepared(raw_prepared)
    if prepared.capability_id is not None and prepared.capability_id != definition.capability_id:
        raise refuse("validation_failed", "the prepared action is for another capability", field="prepared")
    fields = definition.fields_from_effects(prepared.effects)
    fingerprint = _fingerprint(definition.capability_id, caller, fields)

    # A repeat answers with the original reply and changes nothing. The same key for anything else
    # is a conflict, never a second effect.
    previous = caller.tenant.idempotency.get((caller.workspace, key))
    if previous is not None:
        if (
            previous.capability_id != definition.capability_id
            or previous.principal_id != caller.principal_id
            or previous.fingerprint != fingerprint
        ):
            raise refuse("conflict", "the idempotency key was already used for a different action", field="idempotency_key")
        return previous.response

    now = env.clock.now()
    if prepared.expires_at is not None:
        try:
            expired = now >= parse_timestamp(prepared.expires_at)
        except ValueError:
            raise _bad("the prepared action carries no readable expiry") from None
        if expired:
            raise refuse("expired", "the prepared action expired before it was committed", field="prepared")

    loaded = definition.load(env, caller, fields)
    # First the data the user confirmed, then the rules: a record that moved is ``stale``,
    # whatever else changed with it.
    for current in loaded.versions:
        seen = next(
            (
                v
                for v in prepared.data_versions
                if v["entity_type"] == current["entity_type"] and v["entity_id"] == current["entity_id"]
            ),
            None,
        )
        if seen is None:
            raise refuse("validation_failed", "the prepared action does not record the data it relied on", field="prepared")
        if seen["version"] != current["version"]:
            raise refuse("stale", "a record changed after the action was prepared", freshness=freshness_of(env, loaded.versions))
    _checked(definition, env, caller, loaded)
    if definition.requires_step_up:
        # Defence in depth: the core never sends such a commit, and the application would not
        # apply it if it did. The change is completed in the application's own authenticated UI.
        raise refuse(
            "step_up_required",
            "this action must be completed in the application's own authenticated interface",
            freshness=freshness_of(env, loaded.versions),
        )
    applied = definition.apply(env, caller, loaded)
    result = ok_result(
        identifiers=applied.identifiers,
        values=applied.values,
        freshness=freshness_of(env, applied.versions_after),
        allowed_next_actions=applied.next_actions,
        follow_up=applied.follow_up,
    )
    caller.tenant.idempotency[(caller.workspace, key)] = IdempotencyRecord(
        definition.capability_id, caller.principal_id, fingerprint, result, now
    )
    return result


def status_action(definition: ActionDefinition, env: Env, header: str | None, key_value: Any) -> dict[str, Any]:
    """What became of the commit sent with this key.

    Exactly one answer proves that nothing was applied: rejected, one ``validation_failed`` on
    ``idempotency_key``, no identifiers. Every other answer is a recorded outcome or a refusal, and
    a refusal says nothing about whether the commit happened.
    """
    caller = resolve_caller(env.store, header, definition.permission)
    key = _require_key(key_value)
    stored = caller.tenant.idempotency.get((caller.workspace, key))
    if stored is None:
        return rejected_result("validation_failed", NO_RECORD, field="idempotency_key")
    if stored.principal_id != caller.principal_id:
        raise refuse("permission_denied", "the idempotency key belongs to another principal")
    if stored.capability_id != definition.capability_id:
        raise refuse("conflict", "the idempotency key was used for another capability", field="idempotency_key")
    return stored.response
