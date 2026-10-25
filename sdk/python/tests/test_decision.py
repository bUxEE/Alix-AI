"""The decision-status client against the fake server: read-only, advisory, fail-closed (task D-10)."""

from __future__ import annotations

import ast
import inspect
import json
from typing import Any

import httpx
import pytest

import alixai.client as client_module
from alixai import AlixAIAsyncClient, AlixAIClient, AlixAIError, DecisionStatus
from alixai.gen.decision.fallback_reason import FallbackReason
from alixai.testing import DECISION_STATUS_CAPABILITY, FakeAlixAIServer, decision_status_document
from tests.helpers.openapi import SCHEMAS_DIR

SCOPES = {"capability_scopes": [DECISION_STATUS_CAPABILITY]}


def failure(call: Any) -> AlixAIError:
    with pytest.raises(AlixAIError) as raised:
        call()
    return raised.value


@pytest.fixture
def fake() -> FakeAlixAIServer:
    return FakeAlixAIServer()


def test_the_status_is_read_with_one_fresh_token_by_one_get_and_nothing_else(fake: FakeAlixAIServer) -> None:
    fake.set_decision_status(decision_status_document("advisory", accepted=7, fallbacks={"timeout": 1, "low_confidence": 2}))
    asked: list[Any] = []

    def provider(request: Any) -> str:
        asked.append(request)
        return fake.issue_token("ses-1", **SCOPES)

    client = AlixAIClient(fake.base_url, provider, http_client=httpx.Client(transport=fake.transport))
    status = client.get_decision_status()
    assert isinstance(status, DecisionStatus)
    assert (status.mode, status.accepted_count, status.rejected_count) == ("advisory", 7, 3)
    assert [(e.reason, e.count) for e in status.fallback_counts] == [("low_confidence", 2), ("timeout", 1)]
    assert status.model_id == "fake-scorer" and status.model_revision == "r1"
    assert [(r.operation, r.method, r.path, r.authenticated) for r in fake.requests] == [("getDecisionStatus", "GET", "/v1/decision/status", True)]
    assert [r.operation for r in asked] == ["getDecisionStatus"] and fake.tokens_redeemed == 1


def test_an_unconfigured_fake_reports_the_layer_off_with_no_model(fake: FakeAlixAIServer) -> None:
    status = fake.client("ses-1", claims=SCOPES).get_decision_status()
    assert status.mode == "off" and status.model_id is None and status.model_revision is None
    assert status.accepted_count == status.rejected_count == 0 and status.fallback_counts == []


def test_it_fails_closed_without_the_scope_without_a_service_and_for_a_quarantined_application(fake: FakeAlixAIServer) -> None:
    assert failure(fake.client("ses-1").get_decision_status).code == "permission_denied"  # the default claims lack the scope
    client = fake.client("ses-1", claims=SCOPES)
    fake.set_decision_unavailable()
    unavailable = failure(client.get_decision_status)
    assert unavailable.code == "unavailable" and unavailable.retryable
    fake.set_decision_unavailable(False)
    fake.set_application_status("quarantined")
    assert failure(client.get_decision_status).code == "quarantined"
    fake.set_application_status("active")
    fake.fail_next(operation="getDecisionStatus", status=503, error={"code": "unavailable", "message": "down", "retryable": True})
    assert failure(client.get_decision_status).code == "unavailable"


@pytest.mark.parametrize(
    "mutate",
    [
        lambda d: {**d, "rejected_count": 5},
        lambda d: {**d, "accepted_count": -1},
        lambda d: {**d, "mode": "authorised"},
        lambda d: {**d, "score": 0.97},
        lambda d: {**d, "fallback_counts": [*d["fallback_counts"], {"reason": "timeout", "count": 0}]},
        lambda d: {k: v for k, v in d.items() if k != "provider_id"},
    ],
    ids=["sum-mismatch", "negative-count", "unknown-mode", "extra-member", "duplicate-reason", "missing-provider"],
)
def test_a_status_that_is_not_the_document_is_refused_never_read_as_a_result(fake: FakeAlixAIServer, mutate: Any) -> None:
    fake.set_decision_status(mutate(decision_status_document("shadow", accepted=1, fallbacks={"timeout": 2})))
    error = failure(fake.client("ses-1", claims=SCOPES).get_decision_status)
    assert error.code == "internal" and error.details is not None and error.details["reason"] == "unexpected_shape"


@pytest.mark.asyncio
async def test_the_async_client_reads_the_same_status(fake: FakeAlixAIServer) -> None:
    fake.set_decision_status(decision_status_document("shadow", accepted=2))
    async with fake.async_client("ses-1", claims=SCOPES) as client:
        status = await client.get_decision_status()
        assert (status.mode, status.accepted_count) == ("shadow", 2)
    async with fake.async_client("ses-1") as client:
        with pytest.raises(AlixAIError) as raised:
            await client.get_decision_status()
        assert raised.value.code == "permission_denied"


def test_the_fixture_is_the_document_and_every_status_example_of_the_schemas_parses() -> None:
    reasons = {reason: 1 for reason in FallbackReason.__args__}  # type: ignore[attr-defined]
    assert DecisionStatus.model_validate(decision_status_document("advisory", accepted=1, fallbacks=reasons)).rejected_count == 13
    for path in sorted((SCHEMAS_DIR / "decision" / "examples" / "decision-status" / "valid").glob("*.json")):
        DecisionStatus.model_validate(json.loads(path.read_text(encoding="utf-8")))


def test_no_name_or_docstring_makes_a_score_look_like_an_authorisation() -> None:
    tree = ast.parse(inspect.getsource(client_module))
    names = {n.name for n in ast.walk(tree) if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef)) and "decision" in n.name}
    assert names == {"_decision_status", "decision_status", "get_decision_status"}
    for name in names:
        for banned in ("authoris", "authoriz", "allow", "permit", "approve", "grant"):
            assert banned not in name
    for client in (AlixAIClient, AlixAIAsyncClient):
        doc = " ".join((client.get_decision_status.__doc__ or "").lower().split())
        assert "advisory" in doc and "never an authorisation" in doc
