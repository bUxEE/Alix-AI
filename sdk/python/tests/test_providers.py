"""The providers and OAuth areas of the Python SDK (task X-14): the operation table against the OpenAPI document, the clients
against the fake server, and the rules that make this area what it is: operator-only, off by default, write-only credentials,
fail-closed."""

from __future__ import annotations

import json
import re
from typing import Any, Callable
from urllib.parse import parse_qs, urlsplit

import httpx
import pytest
from pydantic import SecretStr

import alixai
from alixai import AlixAIAsyncClient, AlixAIClient, AlixAIError, Secret, is_manual_fallback
from alixai.providers import ContentEgress
from alixai.testing import FakeAlixAIServer, ProviderSeed, ProvidersConfig, default_providers_config
from tests.helpers.openapi import Spec

KEY = "sk-test-0123456789abcdef-NEVER-ECHO"
REDIRECT = "http://127.0.0.1:8123/callback"
BOOKED = {"consumer_subscription_is_not_api_access": True, "separate_api_billing_understood": True}


def failure(call: Callable[[], Any]) -> AlixAIError:
    with pytest.raises(AlixAIError) as raised:
        call()
    return raised.value


def state_of(started: Any) -> str:
    return parse_qs(urlsplit(started.authorization_url).query)["state"][0]


@pytest.fixture
def fake() -> FakeAlixAIServer:
    server = FakeAlixAIServer()
    config = default_providers_config()
    config.tenant_admins = ["admin-2"]
    server.configure_providers(config)
    return server


def operator(fake: FakeAlixAIServer, principal: str = "user-1", **options: Any) -> AlixAIClient:
    return fake.client("ses-1", claims={"principal_id": principal}, **options)


def ack_and_connect(client: AlixAIClient, provider: str) -> None:
    client.providers.acknowledge_billing_notice(provider, notice_version="2026-10-02.1", **BOOKED)
    client.providers.submit_credential(provider, Secret("ok-" + KEY))


# ---- the operation table ------------------------------------------------------------------------------------------


def test_the_operation_table_matches_the_document_for_the_providers_and_oauth_areas(spec: Spec) -> None:
    documented = [o for o in spec.operations if o.area in ("providers", "oauth")]
    assert len(documented) == 16
    assert {o.operation_id for o in documented} <= set(alixai.OPERATIONS)
    for o in documented:
        op = alixai.OPERATIONS[o.operation_id]
        assert (op.method, op.path, op.authenticated) == (o.method, o.path, True), o.operation_id


def test_the_methods_are_named_after_the_operations_on_both_clients(fake: FakeAlixAIServer) -> None:
    names = {
        "list", "submit_credential", "disconnect", "acknowledge_billing_notice", "get_disclosure", "get_mode", "set_mode", "get_egress_policy",
        "set_egress_policy", "get_budgets", "set_budget", "export_usage", "get_kill_switch", "set_kill_switch", "start_oauth", "complete_oauth",
    }
    for api in (operator(fake).providers, fake.async_client().providers):
        assert names <= {n for n in dir(api) if not n.startswith("_")}
        assert {n for n in dir(api) if not n.startswith("_")} - names == set()


# ---- every operation, every answer validated against the document ---------------------------------------------------


def exercise_providers_conformance(fake: FakeAlixAIServer, recorded: Callable[..., AlixAIClient], plain: Callable[..., AlixAIClient]) -> None:
    """Drives every providers and OAuth operation through the clients the contract test builds, so the recording transport checks
    every exchange against the OpenAPI document. ``plain`` is a client without the recorder: the budget create sends an empty
    ``budget_id`` (the server's documented create), which the document's request schema, the whole Budget, cannot describe."""
    unconfigured = FakeAlixAIServer()
    assert failure(unconfigured.client("ses-1", claims={"principal_id": "user-1"}).providers.list).code == "unavailable"

    config = default_providers_config()
    config.tenant_admins = ["admin-2"]
    fake.configure_providers(config)
    p = recorded(principal_id="user-1").providers

    p.list()
    assert failure(lambda: p.acknowledge_billing_notice("openai", notice_version="stale", **BOOKED)).code == "conflict"
    p.acknowledge_billing_notice("openai", notice_version="2026-10-02.1", **BOOKED)
    p.submit_credential("acme", Secret(KEY), label="main", scopes=["model.request"])
    assert failure(lambda: p.submit_credential("acme", "bad-key-value")).code == "validation_failed"
    p.disconnect("acme")

    assert is_manual_fallback(failure(lambda: p.start_oauth("acme", capabilities=["chat"], redirect_uri=REDIRECT)))
    started = p.start_oauth("openai", capabilities=["chat"], redirect_uri=REDIRECT)
    query = f"code=c0de&state={state_of(started)}"
    p.complete_oauth(redirect_uri=REDIRECT, query="?" + query)
    assert failure(lambda: p.complete_oauth(redirect_uri=REDIRECT, query=query)).code == "conflict"

    p.get_mode()
    p.set_mode("deployment", "external")
    p.get_disclosure("openai", source=["src_a"])
    p.get_egress_policy("deployment")
    p.set_egress_policy(
        level="deployment", expected_revision=0, enabled=True, source_mode="local_only", allowed_providers=["openai"],
        allowed_models=[{"provider_id": "openai", "model_id": "gpt-4.1-mini"}], allowed_classes=["public"], allowed_regions=["us"],
        files={"enabled": True, "max_class": "internal"}, audio=ContentEgress(enabled=False, max_class="public"),
    )
    p.get_egress_policy("deployment")

    created = plain(principal_id="user-1").providers.set_budget(
        {
            "subject": {"kind": "tenant", "application_id": "fake-app", "tenant_id": "tenant-1"}, "channel": "interactive", "currency": "EUR",
            "limits": {"daily_micro_units": 5_000_000, "monthly_micro_units": 100_000_000}, "warn_at_percent": 80,
        }
    )
    changed = created.model_dump(mode="json", exclude_none=True)
    changed["limits"] = {"daily_micro_units": 9_000_000, "monthly_micro_units": 100_000_000}
    p.set_budget(changed)
    assert failure(lambda: p.set_budget(changed)).code == "conflict"
    p.get_budgets()
    p.export_usage(provider="openai", scope="application")

    p.get_kill_switch()
    p.set_kill_switch(True, "incident")
    p.set_kill_switch(False)

    assert failure(recorded(principal_id="nobody").providers.list).code == "permission_denied"
    assert failure(recorded(principal_id="admin-2").providers.get_kill_switch).code == "permission_denied"
    fake.set_providers_unavailable()
    assert failure(p.list).code == "unavailable"
    fake.set_providers_unavailable(False)


# ---- the credential is write-only -----------------------------------------------------------------------------------


def test_the_secret_prints_as_a_placeholder_and_the_key_is_neither_returned_nor_recorded(fake: FakeAlixAIServer) -> None:
    secret = Secret(KEY)
    shown = "|".join([str(secret), repr(secret), f"{secret}", f"{secret!r}", repr({"key": secret}), json.dumps({"key": str(secret)})])
    assert KEY not in shown and "**********" in shown

    client = operator(fake)
    status = client.providers.submit_credential("acme", secret, label="main", expires_at="2027-01-01T00:00:00Z", scopes=["model.request"])
    assert (status.state, status.kind, status.account.organization_id) == ("connected", "api_key", "org-fake")
    assert status.model_dump(mode="json")["expires_at"] == "2027-01-01T00:00:00Z"
    assert KEY not in status.model_dump_json() and KEY not in repr(status)
    recorded = [r for r in fake.requests if r.operation == "putProviderCredential"]
    assert len(recorded) == 1 and (recorded[0].method, recorded[0].path) == ("PUT", "/v1/providers/acme/credential")
    assert recorded[0].body["key"] == "[redacted]" and KEY not in repr(fake.requests)
    assert KEY not in client.providers.list().model_dump_json()


def test_a_plain_string_key_works_a_refused_key_is_a_fixed_message_and_a_broad_key_is_quarantined(fake: FakeAlixAIServer) -> None:
    p = operator(fake).providers
    assert p.submit_credential("acme", KEY).state == "connected"
    refused = failure(lambda: p.submit_credential("acme", "bad-" + KEY))
    assert refused.code == "validation_failed" and KEY not in refused.message and KEY not in json.dumps(refused.details or {})
    broad = p.submit_credential("acme", "broad-" + KEY)
    assert broad.state == "quarantined" and "excessive_permissions_reported" in broad.warnings
    gone = p.disconnect("acme")
    assert gone.state == "not_connected" and gone.kind is None


def _status_with(organization: str) -> dict[str, Any]:
    return {
        "as_of": "2026-10-02T09:00:00Z", "provider": {"profile_id": "pprf_01J0000000000000000000000P", "provider_id": "acme", "model_id": "m", "model_version": "1"},
        "lifecycle": "qualified", "state": "connected", "kind": "api_key", "account": {"account_id": None, "organization_id": organization, "project_id": None},
        "connected_at": "2026-10-02T08:00:05Z", "expires_at": None, "warnings": [], "health": "healthy", "last_error_kind": None, "budget_state": None,
        "billing_notice_acknowledged": True,
    }


def test_an_answer_that_repeats_the_key_is_refused_and_an_error_that_quotes_it_is_replaced() -> None:
    def echo(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json=_status_with(json.loads(request.content)["key"]))

    def quote(request: httpx.Request) -> httpx.Response:
        key = json.loads(request.content)["key"]
        return httpx.Response(400, json={"code": "validation_failed", "message": f"the key {key} is bad", "retryable": False, "details": {"key": key}})

    for handler in (echo, quote):
        client = AlixAIClient("https://alixai.test", lambda request: "tok", http_client=httpx.Client(transport=httpx.MockTransport(handler)))
        error = failure(lambda: client.providers.submit_credential("acme", Secret(KEY)))
        assert KEY not in error.message and KEY not in json.dumps(error.details or {}) and KEY not in repr(error)
    echoed = failure(lambda: AlixAIClient("https://alixai.test", lambda request: "tok", http_client=httpx.Client(transport=httpx.MockTransport(echo))).providers.submit_credential("acme", KEY))
    assert (echoed.code, (echoed.details or {}).get("reason")) == ("internal", "echoed_secret")


def test_a_malformed_key_is_refused_locally_before_a_token_is_spent_and_without_naming_it(fake: FakeAlixAIServer) -> None:
    tokens: list[Any] = []
    client = fake.client("ses-1", get_context_token=lambda request: tokens.append(request) or fake.issue_token("ses-1"))
    for key in ("", "a\nb", " " + KEY, "x" * 4097, 42):
        error = failure(lambda key=key: client.providers.submit_credential("acme", key))  # type: ignore[arg-type]
        assert error.code == "validation_failed" and KEY not in error.message
    assert tokens == [] and fake.requests == []


def test_a_failed_put_is_an_uncertain_outcome_and_a_failed_get_is_unavailable() -> None:
    def down(request: httpx.Request) -> httpx.Response:
        raise httpx.ReadError("connection reset", request=request)

    client = AlixAIClient("https://alixai.test", lambda request: "tok", http_client=httpx.Client(transport=httpx.MockTransport(down)))
    assert failure(lambda: client.providers.submit_credential("acme", KEY)).code == "uncertain_outcome"
    assert failure(client.providers.list).code == "unavailable"


# ---- off by default, operator-only ---------------------------------------------------------------------------------


def test_the_area_answers_unavailable_without_the_service_never_an_empty_result() -> None:
    fake = FakeAlixAIServer()
    p = fake.client("ses-1").providers
    for call in (p.list, p.get_mode, p.get_kill_switch, lambda: p.start_oauth("openai", capabilities=["chat"], redirect_uri=REDIRECT)):
        error = failure(call)
        assert (error.code, error.retryable, error.status) == ("unavailable", True, 503)
    fake.configure_providers(ProvidersConfig(deployment_operators=["user-1"]))
    assert len(p.list().providers) == 2


def test_the_operator_is_decided_from_the_configuration_before_the_body_and_the_level_decides_which_one(fake: FakeAlixAIServer) -> None:
    nobody = operator(fake, "nobody").providers
    assert failure(nobody.list).code == "permission_denied"
    assert failure(lambda: nobody.submit_credential("acme", KEY)).code == "permission_denied"
    admin = operator(fake, "admin-2").providers
    assert len(admin.list().providers) == 2
    assert admin.set_mode("tenant", "hybrid").effective == "hybrid"
    for call in (
        lambda: admin.set_mode("deployment", "external"),
        lambda: admin.set_mode("application", "external"),
        admin.get_kill_switch,
        lambda: admin.set_kill_switch(True, "incident"),
        lambda: admin.export_usage(scope="application"),
        lambda: admin.set_egress_policy(level="application", expected_revision=0, enabled=True, source_mode="local_only"),
    ):
        assert failure(call).code == "permission_denied"
    assert admin.set_egress_policy(level="tenant", expected_revision=0, enabled=True, source_mode="local_only").revision == 1


def test_a_token_is_single_use(fake: FakeAlixAIServer) -> None:
    token = fake.issue_token("ses-1", principal_id="user-1")
    client = fake.client("ses-1", get_context_token=lambda request: token)
    client.providers.list()
    assert failure(client.providers.list).code == "replayed"


def test_every_argument_is_validated_before_a_token_is_minted(fake: FakeAlixAIServer) -> None:
    tokens: list[Any] = []
    client = fake.client("ses-1", get_context_token=lambda request: tokens.append(request) or fake.issue_token("ses-1"))
    p = client.providers
    bad: dict[str, Callable[[], Any]] = {
        "a path-shaped provider id": lambda: p.disconnect("../etc"),
        "an unknown mode": lambda: p.set_mode("tenant", "turbo"),
        "an unknown level": lambda: p.set_mode("global", "local"),
        "a source level with no id": lambda: p.get_egress_policy("source"),
        "an id for a tenant level": lambda: p.get_egress_policy("tenant", "src_1"),
        "an unconfirmed notice": lambda: p.acknowledge_billing_notice("acme", notice_version="1", consumer_subscription_is_not_api_access=False, separate_api_billing_understood=True),
        "a negative estimate": lambda: p.get_disclosure("acme", estimated_toks_in=-1),
        "a float estimate": lambda: p.get_disclosure("acme", max_output_toks=1.5),  # type: ignore[arg-type]
        "a free-text kill reason": lambda: p.set_kill_switch(True, "because I said so"),
        "no capabilities": lambda: p.start_oauth("openai", capabilities=[], redirect_uri=REDIRECT),
        "an empty callback query": lambda: p.complete_oauth(redirect_uri=REDIRECT, query="?"),
        "a float limit": lambda: p.set_budget({"subject": {"kind": "provider"}, "channel": "interactive", "currency": "EUR", "limits": {"daily_micro_units": 1.5, "monthly_micro_units": 1}, "warn_at_percent": 80}),
        "a budget that exists with no revision": lambda: p.set_budget({"budget_id": "bud_x", "subject": {"kind": "provider"}, "channel": "interactive", "currency": "EUR", "limits": {"daily_micro_units": 1, "monthly_micro_units": 1}, "warn_at_percent": 80}),
        "a usage scope": lambda: p.export_usage(scope="world"),
        "a timeout": lambda: p.list(timeout_ms=-1),
        "a restricted files class": lambda: p.set_egress_policy(level="tenant", expected_revision=0, enabled=True, source_mode="local_only", files={"enabled": True, "max_class": "restricted"}),
        "an audio setting with no enabled": lambda: p.set_egress_policy(level="tenant", expected_revision=0, enabled=True, source_mode="local_only", audio={"max_class": "public"}),
        "a files setting with another member": lambda: p.set_egress_policy(level="tenant", expected_revision=0, enabled=True, source_mode="local_only", files={"enabled": True, "max_class": "public", "max_bytes": 1}),
        "an audio setting that is no mapping": lambda: p.set_egress_policy(level="tenant", expected_revision=0, enabled=True, source_mode="local_only", audio=True),  # type: ignore[arg-type]
    }
    for name, call in bad.items():
        assert failure(call).code == "validation_failed", name
    assert tokens == [] and fake.requests == []


def test_the_token_provider_is_told_the_operation_and_the_provider_the_path_names(fake: FakeAlixAIServer) -> None:
    asked: list[Any] = []
    client = fake.client("ses-1", get_context_token=lambda request: asked.append(request) or fake.issue_token("ses-1"))
    client.providers.get_mode()
    client.providers.disconnect("acme")
    assert [(r.operation, r.provider) for r in asked] == [("getProviderMode", None), ("deleteProviderConnection", "acme")]


# ---- the behaviour ------------------------------------------------------------------------------------------------


def test_local_is_the_default_the_most_restrictive_level_decides_and_the_kill_switch_wins(fake: FakeAlixAIServer) -> None:
    p = operator(fake).providers
    mode = p.get_mode()
    assert (mode.effective, mode.decided_by, mode.kill_switch, mode.levels) == ("local", "default", False, {})
    p.set_mode("deployment", "external")
    mode = p.set_mode("tenant", "hybrid")
    assert (mode.effective, mode.decided_by, dict(mode.levels)) == ("hybrid", "tenant", {"deployment": "external", "tenant": "hybrid"})
    switch = p.set_kill_switch(True, "incident_42")
    assert (switch.engaged, switch.reason, switch.changed_by) == (True, "incident_42", "user-1")
    mode = p.get_mode()
    assert (mode.effective, mode.configured, mode.decided_by, mode.kill_switch) == ("local", "hybrid", "kill_switch", True)
    assert p.set_kill_switch(False).engaged is False and p.get_kill_switch().engaged is False


def test_the_disclosure_names_what_may_leave_and_nothing_when_the_request_stays_local(fake: FakeAlixAIServer) -> None:
    client = operator(fake)
    p = client.providers
    local = p.get_disclosure("openai")
    assert (local.external_enabled, local.data_classes, local.data_categories, local.on_unavailable.behavior) == (False, [], [], "fail")
    ack_and_connect(client, "openai")
    p.set_mode("deployment", "external")
    external = p.get_disclosure("openai", role="conversational_controller", source=["src_a", "src_b"], estimated_toks_in=1200)
    assert external.external_enabled and external.rag_excerpts_included and external.provider.model_id == "gpt-4.1-mini" and external.terms.training_use == "never"
    assert isinstance(external.model_dump(mode="json")["cost"]["estimate_upper_micro_units"], int)
    sent = [r for r in fake.requests if r.operation == "getProviderDisclosure"][-1]
    assert sent.method == "GET" and sent.path == "/v1/providers/openai/disclosure"
    p.set_kill_switch(True, "stop")
    assert p.get_disclosure("openai").external_enabled is False


def test_egress_policy_revisions_and_the_approval_where_internal_data_may_leave(fake: FakeAlixAIServer) -> None:
    p = operator(fake).providers
    assert p.get_egress_policy("deployment").policy is None
    base: dict[str, Any] = {"level": "deployment", "enabled": True, "source_mode": "local_only", "allowed_providers": ["openai"], "allowed_classes": ["public"], "allowed_regions": ["us"]}
    first = p.set_egress_policy(expected_revision=0, **base)
    assert first.revision == 1 and first.approval is None
    assert failure(lambda: p.set_egress_policy(expected_revision=0, **base)).code == "conflict"
    second = p.set_egress_policy(expected_revision=1, **{**base, "allowed_classes": ["public", "internal"]})
    assert second.revision == 2 and second.approval is not None
    stored = p.get_egress_policy("deployment").policy
    assert stored is not None and stored.revision == 2
    source = p.set_egress_policy(level="source", source_id="src_01J0000000000000000000000S", source_class="confidential", expected_revision=0, enabled=True, source_mode="allow_excerpt")
    assert source.target.source_id is not None


def test_the_files_and_audio_settings_are_sent_as_the_documents_update_body_and_left_out_when_unset(fake: FakeAlixAIServer, spec: Spec) -> None:
    p = operator(fake).providers
    validate = spec.request_validator("putProviderEgressPolicy")
    assert validate is not None
    base: dict[str, Any] = {"level": "workspace", "enabled": True, "source_mode": "local_only", "allowed_classes": ["public"]}
    plain = p.set_egress_policy(expected_revision=0, **base)
    assert plain.files is None and plain.audio is None
    files, audio = {"enabled": True, "max_class": "confidential"}, {"enabled": True, "max_class": "public"}
    stored = p.set_egress_policy(expected_revision=1, files=files, audio=ContentEgress(enabled=True, max_class="public"), **base)
    sent = [r.body for r in fake.requests if r.operation == "putProviderEgressPolicy"]
    assert sorted(sent[0]) == ["allowed_classes", "enabled", "expected_revision", "level", "source_mode"]
    assert sent[1] == {**base, "expected_revision": 1, "files": files, "audio": audio}
    for body in sent:
        assert validate.is_valid(body), body
    wire = stored.model_dump(mode="json", exclude_none=True)
    assert (wire["files"], wire["audio"], stored.approval is not None) == (files, audio, True)
    read = p.get_egress_policy("workspace").policy
    assert read is not None and read.model_dump(mode="json", exclude_none=True)["files"] == files

    # Uploaded files and raw audio have no source: a source policy has neither setting.
    source: dict[str, Any] = {"level": "source", "source_id": "src_01J0000000000000000000000S", "source_class": "public", "expected_revision": 0, "enabled": True, "source_mode": "allow_excerpt"}
    assert failure(lambda: p.set_egress_policy(audio=audio, **source)).code == "validation_failed"
    assert failure(lambda: p.set_egress_policy(files=files, **source)).code == "validation_failed"
    assert p.set_egress_policy(**source).files is None


def test_budgets_are_created_changed_and_read_in_integer_micro_units_and_the_state_follows_the_spend(fake: FakeAlixAIServer) -> None:
    p = operator(fake).providers
    created = p.set_budget({"subject": {"kind": "provider", "provider_id": "openai"}, "channel": "interactive", "currency": "EUR", "limits": {"daily_micro_units": 1000, "monthly_micro_units": 5000}, "warn_at_percent": 80})
    assert created.revision == 1 and created.model_dump(mode="json")["budget_id"].startswith("bud_")
    wire = created.model_dump(mode="json", exclude_none=True)
    assert failure(lambda: p.set_budget({**wire, "revision": 7})).code == "conflict"
    changed = p.set_budget({**wire, "limits": {"daily_micro_units": 2000, "monthly_micro_units": 5000}})
    assert changed.revision == 2
    assert failure(lambda: p.set_budget({**changed.model_dump(mode="json", exclude_none=True), "subject": {"kind": "provider", "provider_id": "acme"}})).code == "validation_failed"
    budget_id = changed.model_dump(mode="json")["budget_id"]
    fake.set_provider_budget_spend(budget_id, 1700, 0)
    assert p.get_budgets().states[0].state == "warning"
    fake.set_provider_budget_spend(budget_id, 2000, 0)
    view = p.get_budgets()
    assert view.states[0].state == "exhausted" and view.states[0].windows[0].remaining_micro_units == 0
    openai = next(s for s in p.list().providers if s.provider.provider_id == "openai")
    assert openai.budget_state == "exhausted"


def test_usage_is_exported_with_the_selection_in_the_query(fake: FakeAlixAIServer) -> None:
    p = operator(fake).providers
    fake.set_provider_usage({"records": [{"provider_id": "openai", "toks_in": 12}], "totals": []})
    assert p.export_usage(from_="2026-10-01T00:00:00Z", provider="openai", channel="background", scope="application") == {"records": [{"provider_id": "openai", "toks_in": 12}], "totals": []}


def test_the_oauth_flow_the_manual_fallback_and_a_state_that_is_single_use(fake: FakeAlixAIServer) -> None:
    p = operator(fake).providers
    fallback = failure(lambda: p.start_oauth("acme", capabilities=["chat"], redirect_uri=REDIRECT))
    assert fallback.code == "validation_failed" and is_manual_fallback(fallback) and not is_manual_fallback(ValueError("x"))
    started = p.start_oauth("openai", capabilities=["chat"], redirect_uri=REDIRECT, label="main")
    assert started.consent.provider_name == "openai" and started.consent.reauthorization is False and "verifier" not in started.authorization_url
    state = state_of(started)
    connected = p.complete_oauth(redirect_uri=REDIRECT, query=f"code=abc123&state={state}")
    assert (connected.state, connected.kind, connected.account.account_id) == ("connected", "oauth", "sub-fake")
    replay = failure(lambda: p.complete_oauth(redirect_uri=REDIRECT, query=f"code=abc123&state={state}"))
    assert replay.code == "conflict" and state not in replay.message and "abc123" not in replay.message
    recorded = [r for r in fake.requests if r.operation == "completeProviderOAuth"]
    assert all(r.body["query"] == "[redacted]" for r in recorded) and "abc123" not in repr(fake.requests)
    other = p.start_oauth("openai", capabilities=["chat"], redirect_uri=REDIRECT)
    assert failure(lambda: p.complete_oauth(redirect_uri="http://127.0.0.1:1/other", query=f"code=x&state={state_of(other)}")).code == "conflict"


def test_the_billing_notice_is_acknowledged_on_the_current_version(fake: FakeAlixAIServer) -> None:
    p = operator(fake).providers
    assert failure(lambda: p.acknowledge_billing_notice("openai", notice_version="old", **BOOKED)).code == "conflict"
    ack = p.acknowledge_billing_notice("openai", notice_version="2026-10-02.1", **BOOKED)
    assert (ack.notice_version, ack.acknowledged_by) == ("2026-10-02.1", "user-1")
    assert next(s for s in p.list().providers if s.provider.provider_id == "openai").billing_notice_acknowledged is True


def _stored_policy(**members: Any) -> dict[str, Any]:
    return {
        "policy_id": "epol_01J0000000000000000000000E", "revision": 1, "level": "workspace",
        "target": {"deployment_id": "dep_fake", "application_id": "fake-app", "tenant_id": "tenant-1", "workspace_id": "wsp-1"},
        "enabled": True, "source_mode": "local_only", "allowed_providers": [], "allowed_models": [], "allowed_classes": [], "allowed_regions": [],
        "approval": {"approved_by": "user-1", "approved_at": "2026-10-10T09:00:00Z"}, "updated_at": "2026-10-10T09:00:00Z", "updated_by": "user-1", **members,
    }


def _policy_client(document: dict[str, Any]) -> AlixAIClient:
    answer = httpx.MockTransport(lambda request: httpx.Response(200, json={"policy": document} if request.method == "GET" else document))
    return AlixAIClient("https://alixai.test", lambda request: "tok", http_client=httpx.Client(transport=answer))


def test_a_stored_policy_with_the_files_and_audio_settings_is_read_and_a_setting_of_another_shape_is_refused(spec: Spec) -> None:
    files, audio = {"enabled": True, "max_class": "confidential"}, {"enabled": False, "max_class": "public"}
    document = _stored_policy(files=files, audio=audio)
    validator = spec.response_validator("getProviderEgressPolicy", 200)
    assert validator is not None and validator.is_valid({"policy": document})
    policy = _policy_client(document).providers.get_egress_policy("workspace").policy
    assert policy is not None and policy.model_dump(mode="json", exclude_none=True)["files"] == files
    assert policy.model_dump(mode="json", exclude_none=True)["audio"] == audio
    assert _policy_client(document).providers.set_egress_policy(level="workspace", expected_revision=0, enabled=True, source_mode="local_only").revision == 1
    for bad in ({"files": {"enabled": True, "max_class": "restricted"}}, {"audio": {"max_class": "public"}}, {"files": {**files, "max_bytes": 1}}, {"video": audio}):
        error = failure(lambda bad=bad: _policy_client(_stored_policy(**bad)).providers.get_egress_policy("workspace"))
        assert (error.code, (error.details or {}).get("reason")) == ("internal", "unexpected_shape"), bad


def test_a_status_that_contradicts_itself_is_an_unexpected_shape() -> None:
    document = {**_status_with("org"), "state": "not_connected"}  # but with a kind
    client = AlixAIClient("https://alixai.test", lambda request: "tok", http_client=httpx.Client(transport=httpx.MockTransport(lambda request: httpx.Response(200, json={"providers": [document]}))))
    error = failure(client.providers.list)
    assert (error.code, (error.details or {}).get("reason")) == ("internal", "unexpected_shape")


# ---- the async client ---------------------------------------------------------------------------------------------


async def test_the_async_client_speaks_the_same_methods(fake: FakeAlixAIServer) -> None:
    client: AlixAIAsyncClient = fake.async_client("ses-1", claims={"principal_id": "user-1"})
    p = client.providers
    assert len((await p.list()).providers) == 2
    status = await p.submit_credential("acme", Secret(KEY))
    assert status.state == "connected" and KEY not in repr(fake.requests)
    assert (await p.set_mode("tenant", "hybrid")).effective == "hybrid"
    started = await p.start_oauth("openai", capabilities=["chat"], redirect_uri=REDIRECT)
    assert (await p.complete_oauth(redirect_uri=REDIRECT, query=f"code=c&state={state_of(started)}")).kind == "oauth"
    with pytest.raises(AlixAIError) as raised:
        await p.start_oauth("acme", capabilities=["chat"], redirect_uri=REDIRECT)
    assert is_manual_fallback(raised.value)
    with pytest.raises(AlixAIError) as invalid:
        await p.disconnect("../etc")
    assert invalid.value.code == "validation_failed"
    await client.aclose()


def test_the_default_config_offers_an_oauth_provider_and_a_manual_one() -> None:
    config = default_providers_config()
    assert [(s.id, s.oauth) for s in config.providers] == [("openai", True), ("acme", False)]
    assert isinstance(config.providers[0], ProviderSeed) and re.fullmatch(r"[a-z]+", config.providers[0].id)
    assert isinstance(Secret("x"), SecretStr)
