"""Time comes from an injected clock, so results are deterministic and expiry is testable."""

from __future__ import annotations

import re
from datetime import UTC, datetime, timedelta
from typing import Protocol

# RFC 3339 in UTC: whole seconds, or with a fraction of up to nine digits (the core stamps what it prepared with
# the precision of its own clock, and a host must read what it is sent).
_TIMESTAMP = re.compile(r"^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?Z$")


class Clock(Protocol):
    def now(self) -> datetime: ...


class SystemClock:
    def now(self) -> datetime:
        return datetime.now(UTC).replace(microsecond=0)


class FixedClock:
    """A clock that only moves when told to."""

    def __init__(self, start: datetime | str = "2026-10-01T09:00:00Z") -> None:
        self._now = parse_timestamp(start) if isinstance(start, str) else start

    def now(self) -> datetime:
        return self._now

    def advance(self, seconds: float) -> None:
        self._now = self._now + timedelta(seconds=seconds)


def to_timestamp(moment: datetime) -> str:
    """RFC 3339 in UTC with a literal Z, whole seconds: the one spelling the contract accepts."""
    return moment.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_timestamp(value: str) -> datetime:
    """Read an RFC 3339 UTC timestamp with a literal Z, with or without a fraction of a second."""
    match = _TIMESTAMP.match(value)
    if match is None:
        raise ValueError(f"not an RFC 3339 UTC timestamp: {value!r}")
    moment = datetime.strptime(match.group(1), "%Y-%m-%dT%H:%M:%S").replace(tzinfo=UTC)
    if match.group(2):
        moment = moment.replace(microsecond=int(match.group(2).ljust(6, "0")[:6]))
    return moment
