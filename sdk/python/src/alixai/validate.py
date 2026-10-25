"""Local validation that runs before anything is sent.

A malformed argument must never cost a single-use context token or reach the wire, so every check here happens
before ``get_context_token`` is called.
"""

from __future__ import annotations

import re
from typing import Any
from urllib.parse import urlsplit

from alixai.errors import sdk_error

_PATH_ID = re.compile(r"[A-Za-z0-9_.-]{1,128}")
_CONFIRMATION_TOKEN = re.compile(r"cfm_[A-Za-z0-9_-]{43}")
# A file id as POST /v1/files returns it: the pattern of the OpenAPI document.
_FILE_ID = re.compile(r"fil_[0-9A-HJKMNP-TV-Z]{26}")
# A bearer credential is an RFC 7235 token68: letters, digits and - . _ ~ + / =.
_TOKEN68 = re.compile(r"[A-Za-z0-9._~+/=-]+")
_LOOPBACK_V4 = re.compile(r"127(\.\d{1,3}){3}")

MAX_CONTEXT_TOKEN_CHARS = 16 * 1024


def path_id(kind: str, value: Any) -> str:
    """A session, preparation or application id as it appears in a path: the pattern of the OpenAPI document.

    "." and ".." match the pattern but URL normalisation would rewrite the path, so they are refused too.
    """
    if not isinstance(value, str) or _PATH_ID.fullmatch(value) is None or value in (".", ".."):
        raise sdk_error("validation_failed", f"{kind} is not a valid identifier", details={"argument": kind})
    return value


def is_confirmation_token(value: Any) -> bool:
    return isinstance(value, str) and _CONFIRMATION_TOKEN.fullmatch(value) is not None


def is_file_id(value: Any) -> bool:
    return isinstance(value, str) and _FILE_ID.fullmatch(value) is not None


def confirmation_token(value: Any) -> str:
    """The confirmation secret as a request argument. The value is never echoed in the error."""
    if not is_confirmation_token(value):
        raise sdk_error("validation_failed", "the confirmation token is malformed", details={"argument": "token"})
    return value


def context_token(value: Any) -> str:
    """The context token as the host's provider returned it: bare, no whitespace, no ``Bearer`` prefix."""
    if not isinstance(value, str) or not value or len(value) > MAX_CONTEXT_TOKEN_CHARS or _TOKEN68.fullmatch(value) is None:
        raise sdk_error(
            "unauthenticated",
            'get_context_token must return the bare context token (no whitespace, no "Bearer" prefix)',
            details={"reason": "bad_token_shape"},
        )
    return value


def is_loopback_host(hostname: str) -> bool:
    return hostname == "localhost" or hostname.endswith(".localhost") or _LOOPBACK_V4.fullmatch(hostname) is not None or hostname == "::1"


def normalize_base_url(value: Any, allow_insecure_http: bool = False) -> str:
    """The base URL without a trailing slash, path prefix kept (``https://host/alixai``).

    The context token travels as a bearer credential, so plain http is accepted only for a loopback host unless the
    caller opts in; credentials in the URL, a query and a fragment are refused.
    """
    argument = {"argument": "base_url"}
    if not isinstance(value, str) or not value.strip():
        raise sdk_error("validation_failed", "base_url is required", details=argument)
    try:
        url = urlsplit(value.strip())
        hostname, _ = url.hostname, url.port
    except ValueError as exc:
        raise sdk_error("validation_failed", "base_url is not an absolute URL", details=argument) from exc
    if url.scheme not in ("http", "https") or not hostname:
        raise sdk_error("validation_failed", "base_url must be an absolute http or https URL", details=argument)
    if url.username or url.password or url.query or url.fragment:
        raise sdk_error("validation_failed", "base_url must not carry credentials, a query or a fragment", details=argument)
    if url.scheme == "http" and not allow_insecure_http and not is_loopback_host(hostname):
        raise sdk_error(
            "validation_failed",
            "base_url must be https: the context token is a bearer credential (http is accepted only for loopback; "
            "set allow_insecure_http to override)",
            details=argument,
        )
    return f"{url.scheme}://{url.netloc}{url.path.rstrip('/')}"
