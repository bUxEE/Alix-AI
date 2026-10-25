"""Identifier and secret generation for the fake server, from the platform's CSPRNG.

No key material and no fixed secret lives in this package.
"""

from __future__ import annotations

import base64
import secrets

_CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"


def ulid(now_ms: int) -> str:
    """A 26-character ULID (Crockford base32), time first, as the core mints them."""
    time_part = ""
    t = int(now_ms)
    for _ in range(10):
        time_part = _CROCKFORD[t % 32] + time_part
        t //= 32
    return time_part + "".join(_CROCKFORD[b & 31] for b in secrets.token_bytes(16))


def typed_id(prefix: str, now_ms: int) -> str:
    return f"{prefix}_{ulid(now_ms)}"


def b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).decode("ascii").rstrip("=")


def b64url_text(text: str) -> str:
    return b64url(text.encode("utf-8"))


def decode_b64url_text(segment: str) -> str:
    padded = segment + "=" * (-len(segment) % 4)
    return base64.urlsafe_b64decode(padded.encode("ascii")).decode("utf-8")


def confirmation_secret() -> str:
    """A confirmation secret: ``cfm_`` and 43 base64url characters (256 random bits)."""
    return f"cfm_{b64url(secrets.token_bytes(32))}"


def idempotency_key() -> str:
    """An idempotency key: 16 to 128 URL-safe characters."""
    return b64url(secrets.token_bytes(24))


def random_segment(nbytes: int) -> str:
    return b64url(secrets.token_bytes(nbytes))
