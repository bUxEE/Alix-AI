"""The host members of a DeterministicResult (schemas/actions/deterministic-result).

``operation`` and ``capability_id`` are the core's: it knows which call it made, so a host never
states them. Everything here is a plain ``dict`` so it serialises as it is.
"""

from __future__ import annotations

import hashlib
from decimal import Decimal
from typing import Any

Result = dict[str, Any]
Fact = dict[str, Any]
DataVersion = dict[str, str]


# ---- canonical values -----------------------------------------------------------------------


def text(field: str, value: str) -> Fact:
    return {"field": field, "value": {"kind": "text", "value": value}}


def integer(field: str, value: int) -> Fact:
    return {"field": field, "value": {"kind": "integer", "value": value}}


def boolean(field: str, value: bool) -> Fact:
    return {"field": field, "value": {"kind": "boolean", "value": value}}


def identifier(field: str, value: str) -> Fact:
    return {"field": field, "value": {"kind": "identifier", "value": value}}


def decimal(field: str, value: Decimal) -> Fact:
    return {"field": field, "value": {"kind": "decimal", "value": plain(value)}}


def money(field: str, value: Decimal, currency: str = "EUR") -> Fact:
    return {"field": field, "value": {"kind": "money", "value": plain(value), "currency": currency}}


def datetime_fact(field: str, value: str) -> Fact:
    return {"field": field, "value": {"kind": "datetime", "value": value}}


def plain(value: Decimal) -> str:
    """An exact decimal as a string: two fraction digits, no exponent."""
    return format(value.quantize(Decimal("0.01")), "f")


def record(entity_type: str, entity_id: str) -> dict[str, str]:
    return {"entity_type": entity_type, "entity_id": entity_id}


# ---- results --------------------------------------------------------------------------------


def ok_result(
    *,
    freshness: dict[str, str],
    identifiers: list[dict[str, str]] | None = None,
    values: list[Fact] | None = None,
    allowed_next_actions: list[dict[str, str]] | None = None,
    follow_up: dict[str, str] | None = None,
    preparation: dict[str, Any] | None = None,
) -> Result:
    result: Result = {
        "status": "ok",
        "identifiers": identifiers or [],
        "values": values or [],
        "errors": [],
        "freshness": freshness,
        "allowed_next_actions": allowed_next_actions or [],
    }
    if follow_up is not None:
        result["follow_up"] = follow_up
    if preparation is not None:
        result["preparation"] = preparation
    return result


def rejected_result(
    code: str,
    message: str,
    *,
    field: str | None = None,
    freshness: dict[str, str] | None = None,
    allowed_next_actions: list[dict[str, str]] | None = None,
) -> Result:
    issue: dict[str, str] = {"code": code, "message": message}
    if field is not None:
        issue["field"] = field
    result: Result = {
        "status": "rejected",
        "identifiers": [],
        "values": [],
        "errors": [issue],
        "allowed_next_actions": allowed_next_actions or [],
    }
    if freshness is not None:
        result["freshness"] = freshness
    return result


def unavailable_result(code: str, message: str) -> Result:
    """A dependency is down, or the host failed in a way it cannot attribute to the request.

    Nothing was applied: the host only reports this before it starts to write.
    """
    return {
        "status": "unavailable",
        "identifiers": [],
        "values": [],
        "errors": [{"code": code, "message": message}],
        "allowed_next_actions": [{"action": "retry"}],
    }


# ---- data versions --------------------------------------------------------------------------


def record_version(entity_type: str, entity_id: str, revision: int | str) -> DataVersion:
    """The version of one record is its own revision counter.

    It changes exactly when that record changes. It is never a timestamp, a response ETag or a
    counter shared by a site or a tenant.
    """
    return {"entity_type": entity_type, "entity_id": entity_id, "version": str(revision)}


def snapshot_version(versions: list[DataVersion]) -> str:
    """The ``freshness.version`` of a result: a digest over the version of every record it used.

    Validate and Prepare compute it with this one function over the same records, so on unchanged
    data they agree, and any change to any of those records (and to nothing else) changes it.
    """
    lines = "\n".join(
        sorted(f"{v['entity_type']}:{v['entity_id']}:{v['version']}" for v in versions)
    )
    return "rv1-" + hashlib.sha256(lines.encode()).hexdigest()[:32]
