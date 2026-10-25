"""The providers and OAuth areas of External Provider Mode (additive plan 6.3, 11 and 16), sync and async.

    client = AlixAIClient(base_url, get_context_token)
    mode = client.providers.get_mode()                    # local is the default everywhere
    try:
        started = client.providers.start_oauth("openai", capabilities=["chat"], redirect_uri=redirect_uri)
        # ... open started.authorization_url in the user's browser, then relay the redirect it came back to:
        status = client.providers.complete_oauth(redirect_uri=redirect_uri, query=query)
    except AlixAIError as error:
        if is_manual_fallback(error):                     # this provider has no OAuth for API access
            client.providers.submit_credential("acme", Secret(api_key))

External mode is opt-in and OFF by default. A deployment without the providers service (the local-only build, or external mode
not enabled) answers every method ``unavailable``: that is the absence of the feature, never an empty result. Every method needs
an OPERATOR, decided from the deployment's own configuration and never from a role or scope a token asserts
(``permission_denied`` for everybody else). The application, tenant, workspace and principal are the verified context token and
nothing an argument says.

The SDK never persists, logs or echoes a provider credential. A manual key travels only inside
:meth:`ProvidersApi.submit_credential`, as a plain string or a :data:`Secret` (pydantic's ``SecretStr``, which prints as
``**********``), and a connected provider is represented to the host by a :class:`ConnectionStatus`: the state and a safe
account id. Money is integer micro-units; there is no float for it anywhere.
"""

from __future__ import annotations

import json
import re
from typing import Any, Mapping, Optional, Sequence, Union

from pydantic import SecretStr

from alixai.client import AlixAIAsyncClient, AlixAIClient, Call
from alixai.errors import AlixAIError, sdk_error
from alixai.gen.api import EgressPolicyView, OAuthStarted, ProviderBudgets, ProviderKillSwitch, ProviderList, ProviderMode
from alixai.gen.providers import BillingNoticeAcknowledgement, ConnectionStatus, ContentEgress, DataEgressPolicy, EgressDisclosure, ProviderBudget

__all__ = [
    "AsyncProvidersApi",
    "BillingNoticeAcknowledgement",
    "ConnectionStatus",
    "ContentEgress",
    "DataEgressPolicy",
    "EgressDisclosure",
    "EgressPolicyView",
    "OAuthStarted",
    "ProviderBudget",
    "ProviderBudgets",
    "ProviderKillSwitch",
    "ProviderList",
    "ProviderMode",
    "ProvidersApi",
    "Secret",
    "is_manual_fallback",
]

#: A credential value in transit. Pass the key as a ``Secret`` so it prints as a placeholder in a log, a repr and a traceback.
Secret = SecretStr

_PROVIDER = re.compile(r"[a-z][a-z0-9_-]{0,31}")
_KILL_REASON = re.compile(r"[a-z][a-z0-9_]{0,63}")
_CONTROL = re.compile(r"[\x00-\x1f\x7f]")
_MODES = ("local", "hybrid", "external")
_MODE_LEVELS = ("deployment", "application", "tenant")
_EGRESS_LEVELS = ("deployment", "application", "tenant", "workspace", "source")
_EGRESS_MODES = ("local_only", "allow_excerpt", "allow_metadata", "deny")
_EGRESS_ELIGIBLE = ("public", "internal", "confidential")
_ROLES = ("conversational_controller", "language_realization", "vision", "image_generation", "embedding", "stt", "tts")
_SUBJECTS = ("provider", "tenant", "workspace", "principal")
_CHANNELS = ("interactive", "background")


def is_manual_fallback(error: object) -> bool:
    """True for the answer of ``start_oauth`` for a provider without OAuth for API access: ``validation_failed`` with
    ``details.reason`` ``manual_fallback``. Use ``submit_credential`` for that provider."""
    return isinstance(error, AlixAIError) and error.code == "validation_failed" and (error.details or {}).get("reason") == "manual_fallback"


# ---- validation, before a token is requested ---------------------------------------------------------------------


def _invalid(argument: str, reason: str) -> AlixAIError:
    return sdk_error("validation_failed", f"{argument}: {reason}", details={"argument": argument})


def _provider(value: Any) -> str:
    if not isinstance(value, str) or _PROVIDER.fullmatch(value) is None:
        raise _invalid("provider", "it must be a provider id: a lowercase letter, then lowercase letters, digits, _ or -, at most 32 characters")
    return value


def _integer(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _bounded(argument: str, value: Any, maximum: int) -> Optional[str]:
    if value is None:
        return None
    if not isinstance(value, str) or len(value) > maximum:
        raise _invalid(argument, f"it must be a string of at most {maximum} characters")
    return value


def _strings(argument: str, value: Any, maximum: int, *, required: bool = False) -> Optional[list[str]]:
    if value is None:
        if required:
            raise _invalid(argument, "it is required")
        return None
    if not isinstance(value, (list, tuple)) or len(value) > maximum or (required and not value) or not all(isinstance(v, str) and v for v in value):
        raise _invalid(argument, f"it must list {1 if required else 0} to {maximum} non-empty strings")
    return list(value)


def _count(argument: str, value: Any) -> Optional[int]:
    if value is None:
        return None
    if not _integer(value) or value < 0 or value > 2**40:
        raise _invalid(argument, "it must be a non-negative integer")
    return int(value)


def _timeout(value: Any) -> Optional[int]:
    if value is None:
        return None
    if isinstance(value, bool) or not isinstance(value, (int, float)) or value < 0:
        raise sdk_error("validation_failed", "timeout_ms must be a non-negative number of milliseconds", details={"argument": "timeout_ms"})
    return int(value)


def _egress_level(level: Any, source_id: Any) -> None:
    if level not in _EGRESS_LEVELS:
        raise _invalid("level", "it must be deployment, application, tenant, workspace or source")
    if (level == "source") != (source_id is not None and source_id != ""):
        raise _invalid("source_id", "it is for the source level and only for it")
    if source_id is not None and (not isinstance(source_id, str) or len(source_id) > 64):
        raise _invalid("source_id", "it must be a string of at most 64 characters")


def _content_egress(argument: str, value: Any) -> Optional[dict[str, Any]]:
    """The files or audio setting as the update body carries it: exactly ``{enabled, max_class}``, restricted never a class."""
    if value is None:
        return None
    document = value.model_dump(mode="json") if isinstance(value, ContentEgress) else value
    if (
        not isinstance(document, Mapping) or set(document) - {"enabled", "max_class"} or not isinstance(document.get("enabled"), bool)
        or document.get("max_class") not in _EGRESS_ELIGIBLE
    ):
        raise _invalid(argument, "it must be {enabled, max_class} with max_class public, internal or confidential")
    return {"enabled": document["enabled"], "max_class": document["max_class"]}


def _reveal(key: Any) -> str:
    value = key.get_secret_value() if isinstance(key, SecretStr) else key
    # The key is never named in a message.
    if not isinstance(value, str) or not value or len(value) > 4096 or _CONTROL.search(value) or value.strip() != value:
        raise _invalid("key", "it must be 1 to 4096 characters with no control character and no padding")
    return value


def _body(document: Mapping[str, Any]) -> bytes:
    return json.dumps(document, separators=(",", ":")).encode("utf-8")


# ---- the answers --------------------------------------------------------------------------------------------------


def _connection_status(body: Any) -> ConnectionStatus:
    status = ConnectionStatus.model_validate(body)
    _check_status(status)
    return status


def _check_status(status: ConnectionStatus) -> None:
    if status.state == "not_connected" and (status.kind is not None or status.connected_at is not None or status.expires_at is not None):
        raise ValueError("a provider that is not connected has no kind and no connection time")
    if status.state != "not_connected" and status.kind is None:
        raise ValueError("a connected provider has a kind")
    if status.state == "connected" and status.connected_at is None:
        raise ValueError("a connected provider has a connection time")


def _provider_list(body: Any) -> ProviderList:
    result = ProviderList.model_validate(body)
    for status in result.providers:
        _check_status(status)
    return result


def _mode(body: Any) -> ProviderMode:
    mode = ProviderMode.model_validate(body)
    if mode.kill_switch and mode.effective != "local":
        raise ValueError("under the kill switch the effective mode is local")
    return mode


def _disclosure(body: Any) -> EgressDisclosure:
    disclosure = EgressDisclosure.model_validate(body)
    if not disclosure.external_enabled and (disclosure.data_classes or disclosure.data_categories or disclosure.rag_excerpts_included):
        raise ValueError("a request that stays local lists nothing as leaving")
    return disclosure


def _budgets(body: Any) -> ProviderBudgets:
    result = ProviderBudgets.model_validate(body)
    if any(len(state.windows) != 2 for state in result.states):
        raise ValueError("a budget state has a daily and a monthly window")
    return result


def _usage(body: Any) -> dict[str, Any]:
    if not isinstance(body, dict):
        raise ValueError("the usage export is a JSON object")
    return body


def _kill_switch(body: Any) -> ProviderKillSwitch:
    switch = ProviderKillSwitch.model_validate(body)
    if switch.reason is not None and _KILL_REASON.fullmatch(switch.reason) is None:
        raise ValueError("the reason is not a fixed code")
    return switch


# ---- the calls ----------------------------------------------------------------------------------------------------


class _ProviderCalls:
    """Builds the Call of every operation. Arguments are validated here, before any token is requested."""

    @staticmethod
    def list(timeout_ms: Any) -> Call[ProviderList]:
        return Call("listProviders", _provider_list, timeout_ms=_timeout(timeout_ms), what="provider list")

    @staticmethod
    def submit_credential(provider: Any, key: Any, label: Any, expires_at: Any, scopes: Any, timeout_ms: Any) -> Call[ConnectionStatus]:
        name = _provider(provider)
        value = _reveal(key)
        document: dict[str, Any] = {"key": value}
        if (text := _bounded("label", label, 128)) is not None:
            document["label"] = text
        if (text := _bounded("expires_at", expires_at, 64)) is not None:
            document["expires_at"] = text
        if (listed := _strings("scopes", scopes, 64)) is not None:
            document["scopes"] = listed
        return Call(
            "putProviderCredential", _connection_status, {"provider": name}, body=_body(document), content_type="application/json",
            timeout_ms=_timeout(timeout_ms), what="connection status", forbid=value,
        )

    @staticmethod
    def disconnect(provider: Any, timeout_ms: Any) -> Call[ConnectionStatus]:
        return Call("deleteProviderConnection", _connection_status, {"provider": _provider(provider)}, timeout_ms=_timeout(timeout_ms), what="connection status")

    @staticmethod
    def acknowledge(provider: Any, notice_version: Any, not_api: Any, separate: Any, timeout_ms: Any) -> Call[BillingNoticeAcknowledgement]:
        name = _provider(provider)
        if not isinstance(notice_version, str) or not 1 <= len(notice_version) <= 64:
            raise _invalid("notice_version", "it must be the notice version, 1 to 64 characters")
        if not_api is not True or separate is not True:
            raise _invalid("confirmations", "both billing statements must be confirmed")
        document = {"notice_version": notice_version, "consumer_subscription_is_not_api_access": True, "separate_api_billing_understood": True}
        return Call(
            "ackProviderBillingNotice", BillingNoticeAcknowledgement.model_validate, {"provider": name}, body=_body(document),
            content_type="application/json", timeout_ms=_timeout(timeout_ms), what="billing notice acknowledgement",
        )

    @staticmethod
    def disclosure(provider: Any, role: Any, source: Any, estimated_toks_in: Any, max_output_toks: Any, timeout_ms: Any) -> Call[EgressDisclosure]:
        name = _provider(provider)
        query: list[tuple[str, str]] = []
        if role is not None:
            if role not in _ROLES:
                raise _invalid("role", "it is not a known role")
            query.append(("role", role))
        for item in _strings("source", source, 64) or []:
            if len(item) > 64 or re.search(r"[\s,]", item):
                raise _invalid("source", "a source id is malformed")
            query.append(("source", item))
        if (tokens := _count("estimated_toks_in", estimated_toks_in)) is not None:
            query.append(("estimated_toks_in", str(tokens)))
        if (tokens := _count("max_output_toks", max_output_toks)) is not None:
            query.append(("max_output_toks", str(tokens)))
        return Call("getProviderDisclosure", _disclosure, {"provider": name}, timeout_ms=_timeout(timeout_ms), what="egress disclosure", query=query)

    @staticmethod
    def get_mode(timeout_ms: Any) -> Call[ProviderMode]:
        return Call("getProviderMode", _mode, timeout_ms=_timeout(timeout_ms), what="provider mode")

    @staticmethod
    def set_mode(level: Any, mode: Any, timeout_ms: Any) -> Call[ProviderMode]:
        if level not in _MODE_LEVELS:
            raise _invalid("level", "it must be deployment, application or tenant")
        if mode not in _MODES:
            raise _invalid("mode", "it must be local, hybrid or external")
        return Call(
            "putProviderMode", _mode, body=_body({"level": level, "mode": mode}), content_type="application/json",
            timeout_ms=_timeout(timeout_ms), what="provider mode",
        )

    @staticmethod
    def get_policy(level: Any, source_id: Any, timeout_ms: Any) -> Call[EgressPolicyView]:
        _egress_level(level, source_id)
        query = [("level", level)] + ([("source_id", source_id)] if source_id else [])
        return Call("getProviderEgressPolicy", EgressPolicyView.model_validate, timeout_ms=_timeout(timeout_ms), what="egress policy", query=query)

    @staticmethod
    def set_policy(
        level: Any, expected_revision: Any, enabled: Any, source_mode: Any, source_id: Any, source_class: Any,
        allowed_providers: Any, allowed_models: Any, allowed_classes: Any, allowed_regions: Any, timeout_ms: Any,
        files: Any = None, audio: Any = None,
    ) -> Call[DataEgressPolicy]:
        _egress_level(level, source_id)
        if source_mode not in _EGRESS_MODES:
            raise _invalid("source_mode", "it is not a known mode")
        if not isinstance(enabled, bool):
            raise _invalid("enabled", "it must be a boolean")
        if not _integer(expected_revision) or expected_revision < 0:
            raise _invalid("expected_revision", "it must be a non-negative integer")
        document: dict[str, Any] = {"level": level, "expected_revision": expected_revision, "enabled": enabled, "source_mode": source_mode}
        if source_id:
            document["source_id"] = source_id
        if source_class is not None:
            document["source_class"] = source_class
        for name, value in (("allowed_providers", allowed_providers), ("allowed_classes", allowed_classes), ("allowed_regions", allowed_regions)):
            if (listed := _strings(name, value, 64)) is not None:
                document[name] = listed
        if allowed_models is not None:
            if not isinstance(allowed_models, (list, tuple)) or not all(isinstance(m, Mapping) for m in allowed_models):
                raise _invalid("allowed_models", "it must list provider and model pairs")
            document["allowed_models"] = [dict(m) for m in allowed_models]
        for name, value in (("files", files), ("audio", audio)):
            if (setting := _content_egress(name, value)) is not None:
                document[name] = setting
        return Call(
            "putProviderEgressPolicy", DataEgressPolicy.model_validate, body=_body(document), content_type="application/json",
            timeout_ms=_timeout(timeout_ms), what="egress policy",
        )

    @staticmethod
    def get_budgets(timeout_ms: Any) -> Call[ProviderBudgets]:
        return Call("getProviderBudgets", _budgets, timeout_ms=_timeout(timeout_ms), what="provider budgets")

    @staticmethod
    def set_budget(budget: Any, timeout_ms: Any) -> Call[ProviderBudget]:
        document = budget.model_dump(mode="json", exclude_none=True) if isinstance(budget, ProviderBudget) else budget
        if not isinstance(document, Mapping) or not isinstance(document.get("subject"), Mapping) or document["subject"].get("kind") not in _SUBJECTS:
            raise _invalid("budget", "subject.kind, channel and currency are required")
        if document.get("channel") not in _CHANNELS or not isinstance(document.get("currency"), str) or not document["currency"]:
            raise _invalid("budget", "subject.kind, channel and currency are required")
        limits = document.get("limits")
        if not isinstance(limits, Mapping) or not all(_integer(limits.get(k)) and limits[k] >= 0 for k in ("daily_micro_units", "monthly_micro_units")):
            raise _invalid("limits", "a limit is a non-negative integer number of micro-units")
        warn = document.get("warn_at_percent")
        if not _integer(warn) or not 1 <= warn <= 99:
            raise _invalid("warn_at_percent", "it must be 1 to 99")
        exists = bool(document.get("budget_id"))
        if exists and not (_integer(document.get("revision")) and document["revision"] >= 1):
            raise _invalid("revision", "a budget that exists is changed with the revision that was read")
        # A new budget carries empty placeholders for what the core sets, as the server's create expects.
        wire = dict(document) if exists else {**document, "budget_id": "", "revision": 0, "updated_at": "", "updated_by": ""}
        return Call(
            "putProviderBudget", ProviderBudget.model_validate, body=_body(wire), content_type="application/json",
            timeout_ms=_timeout(timeout_ms), what="budget",
        )

    @staticmethod
    def export_usage(from_: Any, to: Any, provider: Any, channel: Any, scope: Any, timeout_ms: Any) -> Call[dict[str, Any]]:
        query: list[tuple[str, str]] = []
        for name, value in (("from", from_), ("to", to)):
            if value is not None:
                if not isinstance(value, str) or re.fullmatch(r"\d{4}-\d{2}-\d{2}T[\d:.]+(Z|[+-]\d{2}:\d{2})", value) is None:
                    raise _invalid(name, "it must be an RFC 3339 time")
                query.append((name, value))
        if provider is not None:
            query.append(("provider", _provider(provider)))
        if channel is not None:
            if channel not in _CHANNELS:
                raise _invalid("channel", "it must be interactive or background")
            query.append(("channel", channel))
        if scope is not None:
            if scope not in ("tenant", "application"):
                raise _invalid("scope", "it must be tenant or application")
            query.append(("scope", scope))
        query.append(("format", "json"))
        return Call("exportProviderUsage", _usage, timeout_ms=_timeout(timeout_ms), what="usage export", query=query)

    @staticmethod
    def get_kill_switch(timeout_ms: Any) -> Call[ProviderKillSwitch]:
        return Call("getProviderKillSwitch", _kill_switch, timeout_ms=_timeout(timeout_ms), what="kill switch")

    @staticmethod
    def set_kill_switch(engaged: Any, reason: Any, timeout_ms: Any) -> Call[ProviderKillSwitch]:
        if not isinstance(engaged, bool):
            raise _invalid("engaged", "it must be a boolean")
        if reason not in (None, "") and (not isinstance(reason, str) or _KILL_REASON.fullmatch(reason) is None):
            raise _invalid("reason", "it must be a fixed code of lowercase letters, digits and _")
        document: dict[str, Any] = {"engaged": engaged}
        if reason:
            document["reason"] = reason
        return Call(
            "putProviderKillSwitch", _kill_switch, body=_body(document), content_type="application/json",
            timeout_ms=_timeout(timeout_ms), what="kill switch",
        )

    @staticmethod
    def start_oauth(
        provider: Any, capabilities: Any, redirect_uri: Any, reauthorize: Any, switch_account: Any, label: Any, timeout_ms: Any
    ) -> Call[OAuthStarted]:
        name = _provider(provider)
        listed = _strings("capabilities", capabilities, 64, required=True)
        if not isinstance(redirect_uri, str) or not 1 <= len(redirect_uri) <= 512:
            raise _invalid("redirect_uri", "it is required, at most 512 characters")
        document: dict[str, Any] = {"capabilities": listed, "redirect_uri": redirect_uri}
        if reauthorize is not None:
            document["reauthorize"] = _bounded("reauthorize", reauthorize, 128)
        if switch_account:
            document["switch_account"] = True
        if (text := _bounded("label", label, 128)) is not None:
            document["label"] = text
        return Call(
            "startProviderOAuth", OAuthStarted.model_validate, {"provider": name}, body=_body(document), content_type="application/json",
            timeout_ms=_timeout(timeout_ms), what="oauth start",
        )

    @staticmethod
    def complete_oauth(redirect_uri: Any, query: Any, timeout_ms: Any) -> Call[ConnectionStatus]:
        if not isinstance(redirect_uri, str) or not 1 <= len(redirect_uri) <= 512:
            raise _invalid("redirect_uri", "it is required, at most 512 characters")
        text = query[1:] if isinstance(query, str) and query.startswith("?") else query
        if not isinstance(text, str) or not 1 <= len(text) <= 4096:
            raise _invalid("query", "it is required, at most 4096 characters")
        return Call(
            "completeProviderOAuth", _connection_status, body=_body({"redirect_uri": redirect_uri, "query": text}), content_type="application/json",
            timeout_ms=_timeout(timeout_ms), what="connection status", forbid=text,
        )


# ---- the synchronous API ------------------------------------------------------------------------------------------


class ProvidersApi:
    """The providers and OAuth areas over a synchronous client: ``client.providers``. See the module documentation."""

    def __init__(self, client: AlixAIClient) -> None:
        self._client = client

    def list(self, *, timeout_ms: Optional[int] = None) -> ProviderList:
        """The provider profiles of the caller's application with the connection status of the caller's tenant: the state, how it is
        connected (``oauth`` or ``api_key``), a safe account identifier, times, warnings, health, budget state and whether the billing
        notice was acknowledged. Never a credential reference, a fingerprint or a secret."""
        return self._client._execute(_ProviderCalls.list(timeout_ms))

    def submit_credential(
        self,
        provider: str,
        key: Union[str, SecretStr],
        *,
        label: Optional[str] = None,
        expires_at: Optional[str] = None,
        scopes: Optional[Sequence[str]] = None,
        timeout_ms: Optional[int] = None,
    ) -> ConnectionStatus:
        """Sends a manual credential for a provider without a suitable OAuth flow. The one place a secret enters the system over HTTP,
        and write-only: the key goes straight to the credential service, is validated with a minimally scoped provider request and
        stored in the secret backend; it is never returned, logged or put in an error (the SDK refuses an answer that repeats it and
        removes it from any error that quotes it). The status shows the state and a safe account identifier, and a warning when the
        provider reports a key broader than needed (stored quarantined). Prefer OAuth where the provider has it. A transport failure
        is ``uncertain_outcome``: read :meth:`list` before sending again."""
        return self._client._execute(_ProviderCalls.submit_credential(provider, key, label, expires_at, scopes, timeout_ms))

    def disconnect(self, provider: str, *, timeout_ms: Optional[int] = None) -> ConnectionStatus:
        """Revokes the tenant's credential for a provider at once. Calls that need it fail closed and never fall back by themselves."""
        return self._client._execute(_ProviderCalls.disconnect(provider, timeout_ms))

    def acknowledge_billing_notice(
        self,
        provider: str,
        *,
        notice_version: str,
        consumer_subscription_is_not_api_access: bool,
        separate_api_billing_understood: bool,
        timeout_ms: Optional[int] = None,
    ) -> BillingNoticeAcknowledgement:
        """Records that the operator has seen the provider's billing notice (a consumer chat subscription is not API access; API usage
        is billed separately) and confirms both statements, which must be ``True``. A provider is not usable until its notice is
        acknowledged. A stale notice version is ``conflict``."""
        return self._client._execute(
            _ProviderCalls.acknowledge(provider, notice_version, consumer_subscription_is_not_api_access, separate_api_billing_understood, timeout_ms)
        )

    def get_disclosure(
        self,
        provider: str,
        *,
        role: Optional[str] = None,
        source: Optional[Sequence[str]] = None,
        estimated_toks_in: Optional[int] = None,
        max_output_toks: Optional[int] = None,
        timeout_ms: Optional[int] = None,
    ) -> EgressDisclosure:
        """What would leave the deployment for a request to a provider: which provider and model would process it, which data classes and
        categories may leave, whether RAG excerpts are included, the provider's stated retention and training terms, an upper-bound
        cost and what happens when the provider is unavailable. When any request-independent check fails (kill switch, policy,
        profile, allowlist, region) nothing is listed as leaving. It records nothing."""
        return self._client._execute(_ProviderCalls.disclosure(provider, role, source, estimated_toks_in, max_output_toks, timeout_ms))

    def get_mode(self, *, timeout_ms: Optional[int] = None) -> ProviderMode:
        """The mode configured at each level (deployment, application, tenant), the effective mode (the most restrictive level; ``local``
        under the kill switch) and what decided it. Local is the default everywhere."""
        return self._client._execute(_ProviderCalls.get_mode(timeout_ms))

    def set_mode(self, level: str, mode: str, *, timeout_ms: Optional[int] = None) -> ProviderMode:
        """Sets ``local``, ``hybrid`` or ``external`` at one level. The tenant level needs a tenant administrator; the application and
        deployment levels need a deployment operator. The change is audited."""
        return self._client._execute(_ProviderCalls.set_mode(level, mode, timeout_ms))

    def get_egress_policy(self, level: str, source_id: Optional[str] = None, *, timeout_ms: Optional[int] = None) -> EgressPolicyView:
        """The data-egress policy (what may leave) at one level; ``policy`` is None for a level with none, and external processing is
        disabled there. ``source_id`` names the source of a source-level policy, and only then."""
        return self._client._execute(_ProviderCalls.get_policy(level, source_id, timeout_ms))

    def set_egress_policy(
        self,
        *,
        level: str,
        expected_revision: int,
        enabled: bool,
        source_mode: str,
        source_id: Optional[str] = None,
        source_class: Optional[str] = None,
        allowed_providers: Optional[Sequence[str]] = None,
        allowed_models: Optional[Sequence[Mapping[str, str]]] = None,
        allowed_classes: Optional[Sequence[str]] = None,
        allowed_regions: Optional[Sequence[str]] = None,
        timeout_ms: Optional[int] = None,
        files: Optional[Union[ContentEgress, Mapping[str, Any]]] = None,
        audio: Optional[Union[ContentEgress, Mapping[str, Any]]] = None,
    ) -> DataEgressPolicy:
        """Creates or replaces the policy of one level and returns it as stored. A stale ``expected_revision`` (0 to create) is
        ``conflict``. Credentials and raw application database content never leave, whatever a policy says.

        ``files`` and ``audio`` (``{enabled, max_class}``, not at source level) say whether an excerpt of an uploaded file, and the
        raw audio of an utterance, may leave under this level and up to which class; left out (the default), they stay local."""
        return self._client._execute(
            _ProviderCalls.set_policy(
                level, expected_revision, enabled, source_mode, source_id, source_class,
                allowed_providers, allowed_models, allowed_classes, allowed_regions, timeout_ms, files, audio,
            )
        )

    def get_budgets(self, *, timeout_ms: Optional[int] = None) -> ProviderBudgets:
        """The per-provider, tenant, workspace and principal budgets (integer micro-units) of the caller's scope with the state of each."""
        return self._client._execute(_ProviderCalls.get_budgets(timeout_ms))

    def set_budget(self, budget: Union[ProviderBudget, Mapping[str, Any]], *, timeout_ms: Optional[int] = None) -> ProviderBudget:
        """Creates (no ``budget_id``) or changes (the ``budget_id`` and the ``revision`` that was read; a stale one is ``conflict``) a
        budget and returns it as stored. The subject, channel and currency never change. A provider-wide budget needs a deployment operator."""
        return self._client._execute(_ProviderCalls.set_budget(budget, timeout_ms))

    def export_usage(
        self,
        *,
        from_: Optional[str] = None,
        to: Optional[str] = None,
        provider: Optional[str] = None,
        channel: Optional[str] = None,
        scope: Optional[str] = None,
        timeout_ms: Optional[int] = None,
    ) -> dict[str, Any]:
        """The usage records (no prompt and no response) with totals per provider, channel and currency, for reconciliation with the
        provider's invoice. The caller's tenant by default; ``scope="application"`` needs a deployment operator. JSON only."""
        return self._client._execute(_ProviderCalls.export_usage(from_, to, provider, channel, scope, timeout_ms))

    def get_kill_switch(self, *, timeout_ms: Optional[int] = None) -> ProviderKillSwitch:
        """Whether the global external-disable switch is engaged. A deployment operator only."""
        return self._client._execute(_ProviderCalls.get_kill_switch(timeout_ms))

    def set_kill_switch(self, engaged: bool, reason: Optional[str] = None, *, timeout_ms: Optional[int] = None) -> ProviderKillSwitch:
        """Engages or releases the global kill switch. Engaging stops every external provider request of the deployment at once,
        whatever any policy says, and survives a restart; releasing it enables nothing by itself. ``reason`` is a fixed code, never
        free text. A deployment operator only."""
        return self._client._execute(_ProviderCalls.set_kill_switch(engaged, reason, timeout_ms))

    def start_oauth(
        self,
        provider: str,
        *,
        capabilities: Sequence[str],
        redirect_uri: str,
        reauthorize: Optional[str] = None,
        switch_account: bool = False,
        label: Optional[str] = None,
        timeout_ms: Optional[int] = None,
    ) -> OAuthStarted:
        """Prepares the consent for the requested capabilities and begins the browser authorization. Open ``authorization_url`` in
        the user's browser and show ``consent`` first. A provider without OAuth for API access answers ``validation_failed`` with
        ``details.reason`` ``manual_fallback`` (test with :func:`is_manual_fallback`): use :meth:`submit_credential` for it."""
        return self._client._execute(_ProviderCalls.start_oauth(provider, capabilities, redirect_uri, reauthorize, switch_account, label, timeout_ms))

    def complete_oauth(self, *, redirect_uri: str, query: str, timeout_ms: Optional[int] = None) -> ConnectionStatus:
        """Relays the redirect the browser followed, with a FRESH context token of the principal that started the flow (tokens are
        single use). A state that is unknown, replayed, expired or issued to another scope is refused and spent. The code and the
        state are sent and never echoed or logged."""
        return self._client._execute(_ProviderCalls.complete_oauth(redirect_uri, query, timeout_ms))


# ---- the asynchronous API -----------------------------------------------------------------------------------------


class AsyncProvidersApi:
    """The providers and OAuth areas over an asynchronous client: ``client.providers``. Same methods as :class:`ProvidersApi`, as coroutines."""

    def __init__(self, client: AlixAIAsyncClient) -> None:
        self._client = client

    async def list(self, *, timeout_ms: Optional[int] = None) -> ProviderList:
        """See :meth:`ProvidersApi.list`."""
        return await self._client._execute(_ProviderCalls.list(timeout_ms))

    async def submit_credential(
        self,
        provider: str,
        key: Union[str, SecretStr],
        *,
        label: Optional[str] = None,
        expires_at: Optional[str] = None,
        scopes: Optional[Sequence[str]] = None,
        timeout_ms: Optional[int] = None,
    ) -> ConnectionStatus:
        """See :meth:`ProvidersApi.submit_credential`: write-only, never returned, logged or echoed."""
        return await self._client._execute(_ProviderCalls.submit_credential(provider, key, label, expires_at, scopes, timeout_ms))

    async def disconnect(self, provider: str, *, timeout_ms: Optional[int] = None) -> ConnectionStatus:
        """See :meth:`ProvidersApi.disconnect`."""
        return await self._client._execute(_ProviderCalls.disconnect(provider, timeout_ms))

    async def acknowledge_billing_notice(
        self,
        provider: str,
        *,
        notice_version: str,
        consumer_subscription_is_not_api_access: bool,
        separate_api_billing_understood: bool,
        timeout_ms: Optional[int] = None,
    ) -> BillingNoticeAcknowledgement:
        """See :meth:`ProvidersApi.acknowledge_billing_notice`."""
        return await self._client._execute(
            _ProviderCalls.acknowledge(provider, notice_version, consumer_subscription_is_not_api_access, separate_api_billing_understood, timeout_ms)
        )

    async def get_disclosure(
        self,
        provider: str,
        *,
        role: Optional[str] = None,
        source: Optional[Sequence[str]] = None,
        estimated_toks_in: Optional[int] = None,
        max_output_toks: Optional[int] = None,
        timeout_ms: Optional[int] = None,
    ) -> EgressDisclosure:
        """See :meth:`ProvidersApi.get_disclosure`."""
        return await self._client._execute(_ProviderCalls.disclosure(provider, role, source, estimated_toks_in, max_output_toks, timeout_ms))

    async def get_mode(self, *, timeout_ms: Optional[int] = None) -> ProviderMode:
        """See :meth:`ProvidersApi.get_mode`."""
        return await self._client._execute(_ProviderCalls.get_mode(timeout_ms))

    async def set_mode(self, level: str, mode: str, *, timeout_ms: Optional[int] = None) -> ProviderMode:
        """See :meth:`ProvidersApi.set_mode`."""
        return await self._client._execute(_ProviderCalls.set_mode(level, mode, timeout_ms))

    async def get_egress_policy(self, level: str, source_id: Optional[str] = None, *, timeout_ms: Optional[int] = None) -> EgressPolicyView:
        """See :meth:`ProvidersApi.get_egress_policy`."""
        return await self._client._execute(_ProviderCalls.get_policy(level, source_id, timeout_ms))

    async def set_egress_policy(
        self,
        *,
        level: str,
        expected_revision: int,
        enabled: bool,
        source_mode: str,
        source_id: Optional[str] = None,
        source_class: Optional[str] = None,
        allowed_providers: Optional[Sequence[str]] = None,
        allowed_models: Optional[Sequence[Mapping[str, str]]] = None,
        allowed_classes: Optional[Sequence[str]] = None,
        allowed_regions: Optional[Sequence[str]] = None,
        timeout_ms: Optional[int] = None,
        files: Optional[Union[ContentEgress, Mapping[str, Any]]] = None,
        audio: Optional[Union[ContentEgress, Mapping[str, Any]]] = None,
    ) -> DataEgressPolicy:
        """See :meth:`ProvidersApi.set_egress_policy`."""
        return await self._client._execute(
            _ProviderCalls.set_policy(
                level, expected_revision, enabled, source_mode, source_id, source_class,
                allowed_providers, allowed_models, allowed_classes, allowed_regions, timeout_ms, files, audio,
            )
        )

    async def get_budgets(self, *, timeout_ms: Optional[int] = None) -> ProviderBudgets:
        """See :meth:`ProvidersApi.get_budgets`."""
        return await self._client._execute(_ProviderCalls.get_budgets(timeout_ms))

    async def set_budget(self, budget: Union[ProviderBudget, Mapping[str, Any]], *, timeout_ms: Optional[int] = None) -> ProviderBudget:
        """See :meth:`ProvidersApi.set_budget`."""
        return await self._client._execute(_ProviderCalls.set_budget(budget, timeout_ms))

    async def export_usage(
        self,
        *,
        from_: Optional[str] = None,
        to: Optional[str] = None,
        provider: Optional[str] = None,
        channel: Optional[str] = None,
        scope: Optional[str] = None,
        timeout_ms: Optional[int] = None,
    ) -> dict[str, Any]:
        """See :meth:`ProvidersApi.export_usage`."""
        return await self._client._execute(_ProviderCalls.export_usage(from_, to, provider, channel, scope, timeout_ms))

    async def get_kill_switch(self, *, timeout_ms: Optional[int] = None) -> ProviderKillSwitch:
        """See :meth:`ProvidersApi.get_kill_switch`."""
        return await self._client._execute(_ProviderCalls.get_kill_switch(timeout_ms))

    async def set_kill_switch(self, engaged: bool, reason: Optional[str] = None, *, timeout_ms: Optional[int] = None) -> ProviderKillSwitch:
        """See :meth:`ProvidersApi.set_kill_switch`."""
        return await self._client._execute(_ProviderCalls.set_kill_switch(engaged, reason, timeout_ms))

    async def start_oauth(
        self,
        provider: str,
        *,
        capabilities: Sequence[str],
        redirect_uri: str,
        reauthorize: Optional[str] = None,
        switch_account: bool = False,
        label: Optional[str] = None,
        timeout_ms: Optional[int] = None,
    ) -> OAuthStarted:
        """See :meth:`ProvidersApi.start_oauth`."""
        return await self._client._execute(
            _ProviderCalls.start_oauth(provider, capabilities, redirect_uri, reauthorize, switch_account, label, timeout_ms)
        )

    async def complete_oauth(self, *, redirect_uri: str, query: str, timeout_ms: Optional[int] = None) -> ConnectionStatus:
        """See :meth:`ProvidersApi.complete_oauth`."""
        return await self._client._execute(_ProviderCalls.complete_oauth(redirect_uri, query, timeout_ms))
