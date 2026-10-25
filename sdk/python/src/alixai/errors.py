"""The one error type of the SDK.

Every failure is an :class:`AlixAIError` carrying one of the closed error codes of
``schemas/common/error`` (also the vocabulary of the server's error document). A failure that never reached
the server (a malformed argument, a token provider that raised, an unreadable response) uses the same codes, so a
host application has one vocabulary to switch on.
"""

from __future__ import annotations

import re
import time
from email.utils import parsedate_to_datetime
from typing import Any, Mapping, Optional, get_args

from alixai.gen.actions.action_receipt import ActionReceipt
from alixai.gen.actions.prepared_action import PreparedAction
from alixai.gen.common.error import Error

#: Every error code, in the order of schemas/common/error (read from the generated model, so it cannot drift).
ERROR_CODES: tuple[str, ...] = get_args(Error.model_fields["code"].annotation)

#: The codes pkg/contracts classifies as retryable (testdata/errcodes.golden); schemas/common/error says the same.
RETRYABLE_CODES: frozenset[str] = frozenset({"unavailable", "degraded", "timeout", "rate_limited", "resource_exhausted"})

MAX_ERROR_BODY_BYTES = 64 * 1024


def is_error_code(value: object) -> bool:
    return isinstance(value, str) and value in ERROR_CODES


def is_retryable_code(code: str) -> bool:
    """Whether re-sending the same request, after a delay, can succeed."""
    return code in RETRYABLE_CODES


class AlixAIError(Exception):
    """A failure reported by the SDK.

    ``code`` is an error code; ``retryable`` says whether sending the same request again can reasonably give a
    different answer. A request that may have reached the server and whose outcome is unknown is
    ``uncertain_outcome``, never retryable: read the state (pending state, receipt) before sending anything again.
    The message never carries a credential.
    """

    def __init__(
        self,
        code: str,
        message: str,
        *,
        retryable: Optional[bool] = None,
        status: Optional[int] = None,
        details: Optional[Mapping[str, Any]] = None,
        request_id: Optional[str] = None,
        retry_after_ms: Optional[int] = None,
        receipt: Optional[ActionReceipt] = None,
        preparation: Optional[PreparedAction] = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.retryable = is_retryable_code(code) if retryable is None else retryable
        #: The HTTP status, when the failure is an HTTP response.
        self.status = status
        #: Structured, non-sensitive context from the server (an open map).
        self.details: Optional[dict[str, Any]] = dict(details) if details is not None else None
        #: The X-Request-ID the gateway minted, for support and log correlation.
        self.request_id = request_id
        #: The server's Retry-After in milliseconds.
        self.retry_after_ms = retry_after_ms
        #: The receipt that came beside the error of a confirmation that did not commit.
        self.receipt = receipt
        #: The preparation as it stood when a confirmation did not commit.
        self.preparation = preparation

    def __repr__(self) -> str:
        parts = [f"code={self.code!r}", f"message={self.message!r}"]
        if self.status is not None:
            parts.append(f"status={self.status}")
        return f"AlixAIError({', '.join(parts)})"


def sdk_error(code: str, message: str, **extra: Any) -> AlixAIError:
    """An error for a failure that did not come from the server's error document."""
    return AlixAIError(code, message, **extra)


def code_for_status(status: int) -> str:
    """The code for an HTTP failure whose body is not an AlixAI error document (a proxy page)."""
    if status in (400, 413, 415, 422):
        return "validation_failed"
    return {401: "unauthenticated", 403: "permission_denied", 409: "conflict", 410: "expired", 429: "rate_limited", 502: "unavailable", 503: "unavailable", 504: "timeout"}.get(status, "internal")


def parse_retry_after(value: Optional[str], now: Optional[float] = None) -> Optional[int]:
    """Retry-After as milliseconds: delta-seconds or an HTTP date."""
    if value is None:
        return None
    text = value.strip()
    if re.fullmatch(r"\d{1,9}", text):
        return int(text) * 1000
    try:
        at = parsedate_to_datetime(text).timestamp()
    except (TypeError, ValueError):
        return None
    return max(0, int((at - (time.time() if now is None else now)) * 1000))


def _model_or_none(model: Any, value: Any) -> Any:
    try:
        return model.model_validate(value)
    except Exception:  # the typed error matters more than a receipt that does not parse
        return None


def error_from_parsed(status: int, headers: Mapping[str, str], parsed: Any) -> AlixAIError:
    """Builds the error for a non-2xx response whose body was read and parsed (``None`` when it was not JSON).

    The body is the bare Error document or, for a confirmation whose commit reached the host and did not
    succeed, the ActionOutcome with the receipt beside an ``error`` member. Anything else (an HTML page from a
    proxy) becomes a typed fallback chosen by the status.
    """
    base: dict[str, Any] = {
        "status": status,
        "request_id": headers.get("x-request-id"),
        "retry_after_ms": parse_retry_after(headers.get("retry-after")),
    }
    if isinstance(parsed, dict) and isinstance(parsed.get("error"), dict):
        body = _error_body(parsed["error"])
        if body is not None:
            return AlixAIError(
                body.code,
                body.message,
                retryable=body.retryable,
                details=body.details,
                receipt=_model_or_none(ActionReceipt, parsed.get("receipt")) if "receipt" in parsed else None,
                preparation=_model_or_none(PreparedAction, parsed.get("preparation")) if "preparation" in parsed else None,
                **base,
            )
    body = _error_body(parsed)
    if body is not None:
        return AlixAIError(body.code, body.message, retryable=body.retryable, details=body.details, **base)
    return AlixAIError(
        code_for_status(status),
        f"the server answered {status} without an AlixAI error document",
        details={"reason": "non_conforming_error_body"},
        **base,
    )


def _error_body(value: Any) -> Optional[Error]:
    if not isinstance(value, dict):
        return None
    try:
        return Error.model_validate(value)
    except Exception:
        return None
