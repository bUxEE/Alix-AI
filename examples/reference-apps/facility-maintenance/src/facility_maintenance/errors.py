"""The platform error taxonomy (schemas/common/error.schema.json) and the refusal exception.

The host answers with these codes only, so a failure is always typed. A refusal is how domain
code stops early: it is raised inside the domain and turned into a typed ``rejected`` result at
the HTTP edge. It never leaves the application as an exception or as a bare HTTP error.
"""

from __future__ import annotations

ERROR_CODES = (
    "scope_missing",
    "unauthenticated",
    "permission_denied",
    "scope_violation",
    "schema_invalid",
    "unknown_capability",
    "ambiguous",
    "missing_fields",
    "validation_failed",
    "stale",
    "expired",
    "replayed",
    "confirmation_required",
    "step_up_required",
    "conflict",
    "unavailable",
    "degraded",
    "timeout",
    "rate_limited",
    "resource_exhausted",
    "no_supported_evidence",
    "uncertain_outcome",
    "partial_failure",
    "quarantined",
    "internal",
)


class Refusal(Exception):
    """A typed refusal: nothing was applied."""

    def __init__(
        self,
        code: str,
        message: str,
        field: str | None = None,
        freshness: dict[str, str] | None = None,
    ) -> None:
        super().__init__(message)
        assert code in ERROR_CODES, code
        self.code = code
        self.message = message
        self.field = field
        self.freshness = freshness


def refuse(
    code: str,
    message: str,
    *,
    field: str | None = None,
    freshness: dict[str, str] | None = None,
) -> Refusal:
    return Refusal(code, message, field, freshness)
