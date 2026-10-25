"""Candidate field values: parsed strictly, never coerced.

AlixAI sends the values it validated; the host checks them again against its own rules. A field
the capability does not declare, a value of the wrong type and a value out of bounds are
``validation_failed`` and name the field. A missing required field is ``missing_fields``.
"""

from __future__ import annotations

from decimal import Decimal, InvalidOperation
from typing import Any

from .errors import refuse

RawFields = dict[str, Any]


def parse_fields(raw: Any, allowed: tuple[str, ...]) -> RawFields:
    if raw is None:
        return {}
    if not isinstance(raw, dict):
        raise refuse("validation_failed", "the request body must be a JSON object")
    for name in raw:
        if name not in allowed:
            raise refuse("validation_failed", "the request carries a field the capability does not declare", field=_name(name))
    return {name: value for name, value in raw.items() if value is not None}


def _name(name: Any) -> str | None:
    return name if isinstance(name, str) and name.isidentifier() and len(name) <= 64 else None


def required_string(fields: RawFields, name: str, *, min_length: int = 1, max_length: int = 500) -> str:
    if name not in fields:
        raise refuse("missing_fields", f"{name} is required", field=name)
    return optional_string(fields, name, min_length=min_length, max_length=max_length) or ""


def optional_string(
    fields: RawFields, name: str, *, min_length: int = 0, max_length: int = 500, default: str | None = None
) -> str | None:
    if name not in fields:
        return default
    value = fields[name]
    if not isinstance(value, str):
        raise refuse("validation_failed", f"{name} must be text", field=name)
    value = value.strip()
    if not min_length <= len(value) <= max_length or any(ord(ch) < 32 and ch not in "\n\t" for ch in value):
        raise refuse(
            "validation_failed", f"{name} must be {min_length} to {max_length} characters of plain text", field=name
        )
    return value


def required_identifier(fields: RawFields, name: str) -> str:
    if name not in fields:
        raise refuse("missing_fields", f"{name} is required", field=name)
    value = fields[name]
    if not isinstance(value, str) or not value.strip() or len(value) > 128:
        raise refuse("validation_failed", f"{name} must be a record identifier", field=name)
    return value.strip()


def optional_identifier(fields: RawFields, name: str) -> str | None:
    return required_identifier(fields, name) if name in fields else None


def optional_enum(fields: RawFields, name: str, values: tuple[str, ...], default: str | None = None) -> str | None:
    if name not in fields:
        return default
    value = fields[name]
    if not isinstance(value, str) or value not in values:
        raise refuse("validation_failed", f"{name} must be one of: {', '.join(values)}", field=name)
    return value


def optional_integer(fields: RawFields, name: str, *, minimum: int, maximum: int, default: int | None = None) -> int | None:
    if name not in fields:
        return default
    value = fields[name]
    if isinstance(value, bool) or not isinstance(value, int) or not minimum <= value <= maximum:
        raise refuse("validation_failed", f"{name} must be a whole number from {minimum} to {maximum}", field=name)
    return value


def optional_decimal(fields: RawFields, name: str, *, minimum: Decimal, maximum: Decimal) -> Decimal | None:
    if name not in fields:
        return None
    value = fields[name]
    if isinstance(value, bool) or not isinstance(value, int | float):
        raise refuse("validation_failed", f"{name} must be a number", field=name)
    try:
        number = Decimal(str(value))
    except InvalidOperation:  # pragma: no cover - str() of a float is always a decimal literal
        raise refuse("validation_failed", f"{name} must be a number", field=name) from None
    if not number.is_finite() or not minimum <= number <= maximum or number != number.quantize(Decimal("0.01")):
        raise refuse(
            "validation_failed", f"{name} must be from {minimum} to {maximum} with at most two decimals", field=name
        )
    return number


def optional_string_list(fields: RawFields, name: str, *, max_items: int) -> list[str]:
    if name not in fields:
        return []
    value = fields[name]
    if not isinstance(value, list) or len(value) > max_items or not all(isinstance(item, str) for item in value):
        raise refuse("validation_failed", f"{name} must be a list of at most {max_items} texts", field=name)
    return [item.strip() for item in value]
