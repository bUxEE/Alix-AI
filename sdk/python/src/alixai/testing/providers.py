"""The providers and OAuth areas of the fake server (External Provider Mode, milestone X), so a host can test its external-mode
flows without a running AlixAI: provider connections, the mode, the data-egress policy, the egress disclosure, budgets, usage, the
kill switch and the browser OAuth flow.

The area is OFF until a test calls :meth:`FakeAlixAIServer.configure_providers`, exactly like a deployment without external mode:
every route then answers 503 ``unavailable``, never an empty result. Every route needs an OPERATOR, decided from the configuration
(``tenant_admins`` and ``deployment_operators``, by principal id; the principal is the token's ``sub``), never from a role or
capability scope a token asserts; with nobody configured nobody passes, and the decision is made before the body is read.

The fake keeps no secret: a manual credential is looked at (its length and a prefix) and dropped, never stored, echoed or logged, and
the recorded request body has it replaced. Money is integer micro-units.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import TYPE_CHECKING, Any, Optional
from urllib.parse import parse_qs, urlencode

import httpx

from alixai.testing.ids import random_segment, typed_id

if TYPE_CHECKING:
    from alixai.testing.fake_server import FakeAlixAIServer, _Scope

Json = dict[str, Any]

PROVIDERS_OPERATIONS = frozenset(
    {
        "listProviders", "putProviderCredential", "deleteProviderConnection", "ackProviderBillingNotice", "getProviderDisclosure",
        "getProviderMode", "putProviderMode", "getProviderEgressPolicy", "putProviderEgressPolicy", "getProviderBudgets",
        "putProviderBudget", "exportProviderUsage", "getProviderKillSwitch", "putProviderKillSwitch", "startProviderOAuth",
        "completeProviderOAuth",
    }
)

_ROLES = ("conversational_controller", "language_realization", "vision", "image_generation", "embedding", "stt", "tts")
_LEVELS = ("deployment", "application", "tenant", "workspace", "source")
_MODE_RANK = {"local": 0, "hybrid": 1, "external": 2}
_PROFILE_ULID = "01J0000000000000000000000P"
_QUERY = {
    "getProviderDisclosure": {"role", "source", "estimated_toks_in", "max_output_toks"},
    "getProviderEgressPolicy": {"level", "source_id"},
    "exportProviderUsage": {"from", "to", "provider", "channel", "scope", "format"},
}
_DEPLOYMENT_ONLY = {"getProviderKillSwitch", "putProviderKillSwitch"}
_SOURCE_ID = re.compile(r"src_[0-9A-HJKMNP-TV-Z]{26}")


@dataclass
class ProviderSeed:
    """One provider profile the fake offers."""

    #: The provider id, for example "openai".
    id: str
    model: str
    model_version: str
    #: The current billing notice version of the profile.
    notice_version: str
    #: Whether the provider offers browser OAuth for API access. Without it ``start_oauth`` answers ``validation_failed`` with
    #: ``details.reason`` ``manual_fallback``.
    oauth: bool = False


@dataclass
class ProvidersConfig:
    """Switches the providers area on."""

    #: The principals that administer their own tenant.
    tenant_admins: list[str] = field(default_factory=list)
    #: The principals that also manage what concerns more than one tenant: the kill switch, the application and deployment mode and
    #: egress policy, provider-wide budgets, the whole application's usage.
    deployment_operators: list[str] = field(default_factory=list)
    providers: list[ProviderSeed] = field(default_factory=list)


def default_providers_config() -> ProvidersConfig:
    """A deployment whose only operator is the fake's default principal (``user-1``), offering an OAuth-capable provider ``openai``
    and a manual-key provider ``acme``."""
    return ProvidersConfig(
        deployment_operators=["user-1"],
        providers=[
            ProviderSeed("openai", "gpt-4.1-mini", "2025-04-14", "2026-10-02.1", oauth=True),
            ProviderSeed("acme", "acme-small", "2026-01-01", "2026-10-02.1"),
        ],
    )


@dataclass
class _Conn:
    state: str
    kind: str
    account: Json
    connected_at: str
    expires_at: Optional[str]
    warnings: list[str]


@dataclass
class _Flow:
    provider: str
    tenant: str
    principal: str
    redirect: str
    expires: float


class ProvidersFake:
    """The state and the handlers of the providers area. Created and driven by :class:`FakeAlixAIServer`."""

    def __init__(self, server: "FakeAlixAIServer") -> None:
        self._s = server
        self.config: Optional[ProvidersConfig] = None
        self.unavailable = False
        self.reset()

    def reset(self) -> None:
        self.conns: dict[str, _Conn] = {}
        self.acks: set[str] = set()
        self.modes: dict[str, str] = {}
        self.kill: Json = {"engaged": False}
        self.policies: dict[str, Json] = {}
        self.budgets: list[Json] = []
        self.spent: dict[str, tuple[int, int]] = {}
        self.flows: dict[str, _Flow] = {}
        self.usage: Json = {"records": [], "totals": []}

    def configure(self, config: ProvidersConfig) -> None:
        self.config = ProvidersConfig(list(config.tenant_admins), list(config.deployment_operators), list(config.providers or default_providers_config().providers))
        self.reset()

    # ---- helpers -----------------------------------------------------------------------------------------------------

    def _ok(self, body: Any) -> httpx.Response:
        return self._s._json(200, body)

    def _bad(self, message: str = "the request body is not valid for this route", details: Optional[Json] = None) -> httpx.Response:
        return self._s._problem("validation_failed", message, details=details)

    def _denied(self, message: str = "the caller is not an operator") -> httpx.Response:
        return self._s._problem("permission_denied", message)

    def _stamp(self, seconds: Optional[float] = None) -> str:
        at = self._s._now() if seconds is None else seconds
        return datetime.fromtimestamp(at, tz=timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    def _seed(self, provider: str) -> Optional[ProviderSeed]:
        return next((s for s in (self.config.providers if self.config else []) if s.id == provider), None)

    @staticmethod
    def _ref(seed: ProviderSeed) -> Json:
        return {"profile_id": f"pprf_{_PROFILE_ULID}", "provider_id": seed.id, "model_id": seed.model, "model_version": seed.model_version}

    @staticmethod
    def _conn_key(scope: "_Scope", provider: str) -> str:
        return f"{scope.tenant_id}/{provider}"

    @staticmethod
    def _mode_key(scope: "_Scope", level: str) -> str:
        return {"application": f"application/{scope.application_id}", "tenant": f"tenant/{scope.application_id}/{scope.tenant_id}"}.get(level, "deployment")

    @staticmethod
    def _policy_key(scope: "_Scope", level: str, source: str) -> str:
        a, t, w = scope.application_id, scope.tenant_id, scope.workspace_id
        return {
            "deployment": "deployment", "application": f"application/{a}", "tenant": f"tenant/{a}/{t}", "workspace": f"workspace/{a}/{t}/{w}",
        }.get(level, f"source/{a}/{t}/{w}/{source}")

    def _is_deployment_operator(self, scope: "_Scope") -> bool:
        return self.config is not None and scope.principal_id in self.config.deployment_operators

    # ---- documents ---------------------------------------------------------------------------------------------------

    def _budget_state(self, budget: Json) -> Json:
        daily_spent, monthly_spent = self.spent.get(budget["budget_id"], (0, 0))
        now = datetime.fromtimestamp(self._s._now(), tz=timezone.utc)
        day = now.replace(hour=0, minute=0, second=0, microsecond=0)
        month = day.replace(day=1)
        next_month = month.replace(year=month.year + (month.month // 12), month=month.month % 12 + 1)

        def window(name: str, start: datetime, end: datetime, limit: int, spent: int) -> Json:
            remaining, state = limit - spent, "ok"
            if remaining <= 0:
                remaining, state = 0, "exhausted"
            elif spent * 100 >= limit * budget["warn_at_percent"]:
                state = "warning"
            return {
                "window": name, "starts_at": self._stamp(start.timestamp()), "ends_at": self._stamp(end.timestamp()), "limit_micro_units": limit,
                "spent_micro_units": spent, "reserved_micro_units": 0, "remaining_micro_units": remaining, "state": state,
            }

        limits = budget["limits"]
        daily = window("daily", day, datetime.fromtimestamp(day.timestamp() + 86400, tz=timezone.utc), limits["daily_micro_units"], daily_spent)
        monthly = window("monthly", month, next_month, limits["monthly_micro_units"], monthly_spent)
        states = {daily["state"], monthly["state"]}
        worst = "exhausted" if "exhausted" in states else "warning" if "warning" in states else "ok"
        return {
            "budget_id": budget["budget_id"], "subject": budget["subject"], "channel": budget["channel"], "currency": budget["currency"],
            "as_of": self._stamp(), "state": worst, "windows": [daily, monthly], "local_fallback_available": False,
        }

    def _budget_status_for(self, provider: str) -> Optional[str]:
        worst: Optional[str] = None
        for budget in self.budgets:
            narrowed = budget["subject"].get("provider_id")
            if isinstance(narrowed, str) and narrowed != provider:
                continue
            state = self._budget_state(budget)["state"]
            if state == "exhausted" or (state == "warning" and worst != "exhausted") or worst is None:
                worst = state
        return worst

    def _status(self, scope: "_Scope", seed: ProviderSeed) -> Json:
        conn = self.conns.get(self._conn_key(scope, seed.id))
        return {
            "as_of": self._stamp(), "provider": self._ref(seed), "lifecycle": "qualified",
            "state": conn.state if conn else "not_connected", "kind": conn.kind if conn else None,
            "account": conn.account if conn else {"account_id": None, "organization_id": None, "project_id": None},
            "connected_at": conn.connected_at if conn else None, "expires_at": conn.expires_at if conn else None,
            "warnings": list(conn.warnings) if conn else [], "health": "healthy" if conn and conn.state == "connected" else "unknown",
            "last_error_kind": None, "budget_state": self._budget_status_for(seed.id),
            "billing_notice_acknowledged": self._conn_key(scope, seed.id) in self.acks,
        }

    def _effective_mode(self, scope: "_Scope") -> tuple[str, str]:
        mode, decided_by, first = "local", "default", True
        for level in ("deployment", "application", "tenant"):
            found = self.modes.get(self._mode_key(scope, level))
            if found is None:
                continue
            if first or _MODE_RANK[found] < _MODE_RANK[mode]:
                mode, decided_by = found, level
            first = False
        return mode, decided_by

    def _mode_view(self, scope: "_Scope") -> Json:
        levels = {level: self.modes[self._mode_key(scope, level)] for level in ("deployment", "application", "tenant") if self._mode_key(scope, level) in self.modes}
        configured, decided = self._effective_mode(scope)
        killed = self.kill.get("engaged") is True
        return {"effective": "local" if killed else configured, "configured": configured, "decided_by": "kill_switch" if killed else decided, "kill_switch": killed, "levels": levels}

    # ---- the request -------------------------------------------------------------------------------------------------

    def handle(self, operation: str, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any, raw: bytes) -> httpx.Response:
        if self.config is None or self.unavailable:
            return self._s._problem("unavailable", "the server has no providers service")
        # The operator decision is made from the configuration, before the query or the body is looked at.
        is_op = scope.principal_id in self.config.deployment_operators
        if not is_op and (operation in _DEPLOYMENT_ONLY or scope.principal_id not in self.config.tenant_admins):
            return self._denied()
        if any(name not in _QUERY.get(operation, set()) for name in {k for k, _ in request.url.params.multi_items()}):
            return self._bad("unknown query parameter")
        if raw and "application/json" not in request.headers.get("content-type", ""):
            return self._s._problem("validation_failed", "Content-Type must be application/json", status=415)
        return getattr(self, "_" + operation)(scope, params, request, body)

    def _closed(self, body: Any, known: set[str]) -> bool:
        return isinstance(body, dict) and set(body) <= known

    # ---- the operations ----------------------------------------------------------------------------------------------

    def _listProviders(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        assert self.config is not None
        return self._ok({"providers": [self._status(scope, s) for s in self.config.providers]})

    def _provider_or_refusal(self, params: dict[str, str]) -> "ProviderSeed | httpx.Response":
        seed = self._seed(params["provider"])
        return seed if seed is not None else self._bad("no such provider", {"reason": "not_found"})

    def _putProviderCredential(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        seed = self._provider_or_refusal(params)
        if isinstance(seed, httpx.Response):
            return seed
        if not self._closed(body, {"key", "label", "expires_at", "scopes"}):
            return self._bad()
        # The key is looked at and dropped. Nothing below keeps, logs or echoes it, and every failure has a fixed message.
        key = body.get("key")
        if not isinstance(key, str) or not key or len(key) > 4096 or key.startswith("bad"):
            return self._bad("the credential was not accepted")
        expires = body.get("expires_at")
        conn = _Conn(
            "connected", "api_key", {"account_id": None, "organization_id": "org-fake", "project_id": "proj-fake"}, self._stamp(),
            expires if isinstance(expires, str) else None, ["broader_than_oauth_scopes"],
        )
        if key.startswith("broad"):
            conn.state, conn.warnings = "quarantined", ["excessive_permissions_reported"]
        self.conns[self._conn_key(scope, seed.id)] = conn
        return self._ok(self._status(scope, seed))

    def _deleteProviderConnection(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        seed = self._provider_or_refusal(params)
        if isinstance(seed, httpx.Response):
            return seed
        self.conns.pop(self._conn_key(scope, seed.id), None)
        return self._ok(self._status(scope, seed))

    def _ackProviderBillingNotice(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        seed = self._provider_or_refusal(params)
        if isinstance(seed, httpx.Response):
            return seed
        if not self._closed(body, {"notice_version", "consumer_subscription_is_not_api_access", "separate_api_billing_understood"}):
            return self._bad()
        if not body.get("notice_version") or body.get("consumer_subscription_is_not_api_access") is not True or body.get("separate_api_billing_understood") is not True:
            return self._bad("the notice version and both confirmations are required")
        if body["notice_version"] != seed.notice_version:
            return self._s._problem("conflict", "the notice version is not the current one", details={"reason": "stale_notice"})
        self.acks.add(self._conn_key(scope, seed.id))
        return self._ok(
            {
                "ack_id": typed_id("ack", self._s._ms()), "profile_id": f"pprf_{_PROFILE_ULID}", "provider_id": seed.id, "notice_version": seed.notice_version,
                "notice_digest": "sha256:" + "c" * 64,
                "scope": {"deployment_id": "dep_fake", "application_id": scope.application_id, "tenant_id": scope.tenant_id},
                "confirmations": {"consumer_subscription_is_not_api_access": True, "separate_api_billing_understood": True},
                "acknowledged_by": scope.principal_id, "acknowledged_at": self._stamp(),
            }
        )

    def _getProviderDisclosure(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        seed = self._provider_or_refusal(params)
        if isinstance(seed, httpx.Response):
            return seed
        query = request.url.params
        role = query.get("role") or "conversational_controller"
        if role not in _ROLES:
            return self._bad("role is not a known role")
        for name in ("estimated_toks_in", "max_output_toks"):
            if name in query and not re.fullmatch(r"\d+", query[name]):
                return self._bad(f"{name} is not a valid count")
        conn = self.conns.get(self._conn_key(scope, seed.id))
        enabled = self._mode_view(scope)["effective"] != "local" and conn is not None and conn.state == "connected" and self._conn_key(scope, seed.id) in self.acks
        with_sources = bool(query.get_list("source"))
        categories = ["user_message", "structured_state", "deterministic_fact"] + (["rag_excerpt"] if with_sources else [])
        return self._ok(
            {
                "generated_at": self._stamp(), "provider": self._ref(seed), "role": role, "external_enabled": enabled,
                "data_classes": ["public"] if enabled else [], "data_categories": categories if enabled else [],
                "rag_excerpts_included": enabled and with_sources, "regions": ["us"],
                "terms": {"retention": "limited", "retention_days": 30, "training_use": "never", "residency": "selectable"},
                "cost": {"currency": "EUR", "estimate_upper_micro_units": 4200, "budget_state": self._budget_status_for(seed.id) or "ok"},
                "on_unavailable": {"behavior": "fail", "fallback_profile_id": None},
            }
        )

    def _getProviderMode(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        return self._ok(self._mode_view(scope))

    def _putProviderMode(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        if not self._closed(body, {"level", "mode"}):
            return self._bad()
        level, mode = body.get("level"), body.get("mode")
        if level not in ("deployment", "application", "tenant"):
            return self._bad("level is deployment, application or tenant")
        if mode not in _MODE_RANK:
            return self._bad("mode is local, hybrid or external")
        if level != "tenant" and not self._is_deployment_operator(scope):
            return self._denied("this level needs a deployment operator")
        self.modes[self._mode_key(scope, level)] = mode
        return self._ok(self._mode_view(scope))

    def _getProviderEgressPolicy(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        level, source = request.url.params.get("level", ""), request.url.params.get("source_id", "")
        if level not in _LEVELS or (level == "source") != (source != "") or (source != "" and _SOURCE_ID.fullmatch(source) is None):
            return self._bad("level is required; source_id is for the source level and only for it")
        return self._ok({"policy": self.policies.get(self._policy_key(scope, level, source))})

    def _putProviderEgressPolicy(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        known = {
            "level", "source_id", "expected_revision", "enabled", "source_mode", "source_class", "allowed_providers", "allowed_models", "allowed_classes", "allowed_regions",
            "files", "audio",
        }
        if not self._closed(body, known):
            return self._bad()
        level, source, expected = body.get("level"), body.get("source_id") or "", body.get("expected_revision")
        source_class = body.get("source_class")
        if (
            level not in _LEVELS or (level == "source") != (source != "") or (source != "" and _SOURCE_ID.fullmatch(str(source)) is None) or not isinstance(body.get("enabled"), bool)
            or isinstance(expected, bool) or not isinstance(expected, int) or expected < 0
            or body.get("source_mode") not in ("local_only", "allow_excerpt", "allow_metadata", "deny") or (level == "source") != isinstance(source_class, str)
        ):
            return self._bad("the policy is not valid")
        # The files and audio settings: {enabled, max_class}, never restricted, and not at source level (uploads and raw audio have no source).
        content: Json = {}
        for name in ("files", "audio"):
            setting = body.get(name)
            if setting is None:
                continue
            if (
                level == "source" or not self._closed(setting, {"enabled", "max_class"}) or not isinstance(setting.get("enabled"), bool)
                or setting.get("max_class") not in ("public", "internal", "confidential")
            ):
                return self._bad(f"the {name} setting is not valid")
            content[name] = {"enabled": setting["enabled"], "max_class": setting["max_class"]}
        above_public = any(s["enabled"] and s["max_class"] != "public" for s in content.values())
        if level in ("deployment", "application") and not self._is_deployment_operator(scope):
            return self._denied("this level needs a deployment operator")
        key = self._policy_key(scope, level, source)
        current = self.policies.get(key)
        revision = current["revision"] if current else 0
        if expected != revision:
            return self._s._problem("conflict", "the policy changed since it was read", details={"revision": revision})
        at = self._stamp()
        target: Json = {"deployment_id": "dep_fake"}
        if level != "deployment":
            target["application_id"] = scope.application_id
        if level in ("tenant", "workspace", "source"):
            target["tenant_id"] = scope.tenant_id
        if level in ("workspace", "source"):
            target["workspace_id"] = scope.workspace_id
        if level == "source":
            target["source_id"] = source
        classes = body.get("allowed_classes") or []
        policy: Json = {
            "policy_id": current["policy_id"] if current else typed_id("epol", self._s._ms()), "revision": revision + 1, "level": level, "target": target,
            "enabled": body["enabled"], "source_mode": body["source_mode"], "allowed_providers": body.get("allowed_providers") or [],
            "allowed_models": body.get("allowed_models") or [], "allowed_classes": classes, "allowed_regions": body.get("allowed_regions") or [],
            "updated_at": at, "updated_by": scope.principal_id,
        }
        if isinstance(source_class, str):
            policy["source_class"] = source_class
        policy.update(content)
        if "internal" in classes or "confidential" in classes or above_public:
            policy["approval"] = {"approved_by": scope.principal_id, "approved_at": at}
        self.policies[key] = policy
        return self._ok(policy)

    def _getProviderBudgets(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        return self._ok({"budgets": list(self.budgets), "states": [self._budget_state(b) for b in self.budgets]})

    def _putProviderBudget(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        known = {"budget_id", "revision", "subject", "channel", "currency", "limits", "warn_at_percent", "quotas", "rate_limits", "updated_at", "updated_by"}
        if not self._closed(body, known):
            return self._bad()
        subject, limits, warn = body.get("subject"), body.get("limits"), body.get("warn_at_percent")

        def money(value: Any) -> bool:
            return isinstance(value, int) and not isinstance(value, bool) and value >= 0

        if (
            not isinstance(subject, dict) or subject.get("kind") not in ("provider", "tenant", "workspace", "principal")
            or body.get("channel") not in ("interactive", "background") or not isinstance(body.get("currency"), str) or not body["currency"]
            or not isinstance(limits, dict) or not money(limits.get("daily_micro_units")) or not money(limits.get("monthly_micro_units"))
            or isinstance(warn, bool) or not isinstance(warn, int) or not 1 <= warn <= 99
        ):
            return self._bad("the budget is not valid")
        if subject["kind"] == "provider" and not self._is_deployment_operator(scope):
            return self._denied("a provider-wide budget needs a deployment operator")
        doc: Json = {
            "subject": subject, "channel": body["channel"], "currency": body["currency"],
            "limits": {"daily_micro_units": limits["daily_micro_units"], "monthly_micro_units": limits["monthly_micro_units"]},
            "warn_at_percent": warn, "updated_at": self._stamp(), "updated_by": scope.principal_id,
        }
        for optional in ("quotas", "rate_limits"):
            if body.get(optional) is not None:
                doc[optional] = body[optional]
        budget_id = body.get("budget_id")
        if not budget_id:
            created = {"budget_id": typed_id("bud", self._s._ms()), "revision": 1, **doc}
            self.budgets.append(created)
            return self._ok(created)
        index = next((i for i, b in enumerate(self.budgets) if b["budget_id"] == budget_id), -1)
        if index < 0:
            return self._bad("no such budget", {"reason": "unknown_budget"})
        existing = self.budgets[index]
        if existing["revision"] != body.get("revision"):
            return self._s._problem("conflict", "the budget changed since it was read", details={"revision": existing["revision"]})
        if existing["subject"] != subject or existing["channel"] != body["channel"] or existing["currency"] != body["currency"]:
            return self._bad("the subject, channel and currency of a budget cannot change")
        changed = {"budget_id": budget_id, "revision": existing["revision"] + 1, **doc}
        self.budgets[index] = changed
        return self._ok(changed)

    def _exportProviderUsage(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        query = request.url.params
        for name in ("from", "to"):
            if name in query and re.fullmatch(r"\d{4}-\d{2}-\d{2}T[\d:.]+(Z|[+-]\d{2}:\d{2})", query[name]) is None:
                return self._bad(f"{name} is not an RFC 3339 time")
        if "channel" in query and query["channel"] not in ("interactive", "background"):
            return self._bad("channel is not a known channel")
        if "format" in query and query["format"] != "json":
            return self._bad("this fake renders json")
        if "scope" in query:
            if query["scope"] not in ("tenant", "application"):
                return self._bad("scope is tenant or application")
            if query["scope"] == "application" and not self._is_deployment_operator(scope):
                return self._denied("the whole application needs a deployment operator")
        return self._ok(self.usage)

    def _getProviderKillSwitch(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        return self._ok(self.kill)

    def _putProviderKillSwitch(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        if not self._closed(body, {"engaged", "reason"}):
            return self._bad()
        reason = body.get("reason")
        if not isinstance(body.get("engaged"), bool) or (reason is not None and (not isinstance(reason, str) or re.fullmatch(r"[a-z][a-z0-9_]{0,63}", reason) is None)):
            return self._bad("engaged is required; reason is a fixed code")
        self.kill = {"engaged": body["engaged"], **({"reason": reason} if reason is not None else {}), "changed_by": scope.principal_id, "changed_at": self._stamp()}
        return self._ok(self.kill)

    def _startProviderOAuth(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        seed = self._provider_or_refusal(params)
        if isinstance(seed, httpx.Response):
            return seed
        if not self._closed(body, {"capabilities", "redirect_uri", "reauthorize", "switch_account", "label"}):
            return self._bad()
        caps, redirect = body.get("capabilities"), body.get("redirect_uri")
        if not isinstance(caps, list) or not 1 <= len(caps) <= 64 or not isinstance(redirect, str) or not 1 <= len(redirect) <= 512:
            return self._bad("capabilities and redirect_uri are required and bounded")
        if not seed.oauth:
            return self._bad("this provider has no OAuth for API access: use a manual credential", {"reason": "manual_fallback"})
        state = "st" + random_segment(15)
        expires = self._s._now() + 600
        self.flows[state] = _Flow(seed.id, scope.tenant_id, scope.principal_id, redirect, expires)
        url = "https://auth.fake.test/authorize?" + urlencode(
            {"response_type": "code", "state": state, "code_challenge": "ch" + random_segment(15), "code_challenge_method": "S256", "nonce": "no" + random_segment(6), "redirect_uri": redirect}
        )
        return self._ok(
            {
                "authorization_url": url, "redirect_uri": redirect, "expires_at": self._stamp(expires),
                "consent": {
                    "provider_name": seed.id, "account": {"account_id": None, "organization_id": None, "project_id": None},
                    "scopes": [{"name": "model.request", "description": "Send model requests", "data_access": "the permitted request content"}],
                    "data_access": ["the permitted request content"], "reauthorization": bool(body.get("reauthorize")), "switch_account": body.get("switch_account") is True,
                },
            }
        )

    def _completeProviderOAuth(self, scope: "_Scope", params: dict[str, str], request: httpx.Request, body: Any) -> httpx.Response:
        if not self._closed(body, {"redirect_uri", "query"}):
            return self._bad()
        redirect, query = body.get("redirect_uri"), body.get("query")
        if not isinstance(redirect, str) or not 1 <= len(redirect) <= 512 or not isinstance(query, str) or not 1 <= len(query) <= 4096:
            return self._bad("redirect_uri and query are required and bounded")
        parsed = parse_qs(query.lstrip("?"), keep_blank_values=True)
        state = (parsed.get("state") or [""])[0]
        if not state:
            return self._bad("the callback query has no usable state")
        # The state is single use: unknown, replayed, expired or issued to another scope is refused, and spent. The message is fixed.
        refuse = self._s._problem("conflict", "the authorization could not be completed")
        flow = self.flows.pop(state, None)
        if flow is None:
            return refuse
        if (
            self._s._now() > flow.expires or flow.tenant != scope.tenant_id or flow.principal != scope.principal_id or flow.redirect != redirect
            or "error" in parsed or not (parsed.get("code") or [""])[0]
        ):
            return refuse
        seed = self._seed(flow.provider)
        assert seed is not None
        self.conns[self._conn_key(scope, seed.id)] = _Conn(
            "connected", "oauth", {"account_id": "sub-fake", "organization_id": None, "project_id": None}, self._stamp(), self._stamp(self._s._now() + 3600), []
        )
        return self._ok(self._status(scope, seed))
