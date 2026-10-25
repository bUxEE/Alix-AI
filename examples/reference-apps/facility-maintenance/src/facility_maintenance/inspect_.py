"""The inspection document for the runtime and system half of the integration audit.

One report per requested aspect, in the canonical order, each either ``reported`` with at least one
fact or ``unsupported`` with none. This application reports all nine aspects. Nothing here is a
secret: credentials are described by reference and by whether they are required, never by value,
and counts say how much exists, not what it holds.
"""

from __future__ import annotations

import sys
from typing import Any

from . import __version__
from .access import ROLES
from .capabilities import MAX_OPEN_PER_TECHNICIAN, STEP_UP_SECONDS
from .config import Settings
from .errors import refuse
from .protocol import IDEMPOTENCY_WINDOW_SECONDS, Env
from .results import Fact, boolean, integer, text

ASPECTS = ("versions", "modules", "dependencies", "health", "scopes", "credentials", "limits", "storage", "network")
CONTRACT_VERSION = 1
MAX_BODY_BYTES = 1 << 20


def parse_aspects(requested: list[str]) -> list[str]:
    """Empty means all; an unknown or repeated aspect is ``validation_failed``."""
    if len(set(requested)) != len(requested) or any(a not in ASPECTS for a in requested):
        raise refuse("validation_failed", "aspects must be known aspect names, each at most once", field="aspects")
    return list(ASPECTS) if not requested else [a for a in ASPECTS if a in requested]


def _facts(aspect: str, env: Env, settings: Settings) -> list[Fact]:
    tenants = env.store.tenants
    if aspect == "versions":
        return [
            text("application_version", __version__),
            integer("contract_version", CONTRACT_VERSION),
            text("python_version", ".".join(str(n) for n in sys.version_info[:3])),
        ]
    if aspect == "modules":
        return [
            text("openapi_api", "enabled"),
            text("rag_history_api", "enabled"),
            text("step_up_ui", "enabled"),
            text("test_support", "enabled" if settings.test_support else "disabled"),
        ]
    if aspect == "dependencies":
        return [
            text("data_store", "available"),
            text("notification_outbox", "synthetic outbox; nothing leaves the process"),
            text("external_services", "none"),
        ]
    if aspect == "health":
        return [text("status", "ok")]
    if aspect == "scopes":
        return [
            text("tenant_meaning", "facility operator (a deployment serves exactly one tenant)"),
            text("workspace_meaning", "site"),
            text("roles", ", ".join(ROLES)),
            integer("tenant_count", len(tenants)),
            integer("disposable_tenant_count", sum(1 for t in tenants.values() if t.disposable)),
        ]
    if aspect == "credentials":
        return [
            text("api_credential_ref", "none declared by the package"),
            boolean("api_credential_required", settings.api_token is not None),
            text("step_up_credential", "per-supervisor passcode held as a salted hash; never exposed"),
        ]
    if aspect == "limits":
        return [
            integer("max_request_bytes", MAX_BODY_BYTES),
            integer("max_open_work_orders_per_technician", MAX_OPEN_PER_TECHNICIAN),
            integer("idempotency_window_seconds", IDEMPOTENCY_WINDOW_SECONDS),
            integer("step_up_window_seconds", STEP_UP_SECONDS),
        ]
    if aspect == "storage":
        return [text("data_store_kind", "memory"), text("data_store_location", "process memory")]
    return [text("bind_address", settings.host), integer("listen_port", settings.port), text("outbound_connections", "none")]


def inspect(env: Env, settings: Settings, aspects: list[str]) -> dict[str, Any]:
    return {"reports": [{"aspect": a, "state": "reported", "facts": _facts(a, env, settings)} for a in aspects]}

