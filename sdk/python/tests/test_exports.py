"""Exports client (1.2.0): create a generated FileRef from verified turn blocks."""

from __future__ import annotations

import httpx
import pytest

from alixai import AlixAIClient, AlixAIError
from alixai.testing import FakeAlixAIServer


def test_exports_create_against_the_fake() -> None:
    fake = FakeAlixAIServer()
    client = AlixAIClient(
        fake.base_url,
        fake.token_provider("ses-1", capability_scopes=["exports.create"]),
        http_client=httpx.Client(transport=httpx.MockTransport(fake.handler)),
    )
    ref = client.exports.create("ses-1", {"turn_id": "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", "format": "html"})
    assert ref.origin == "generated"
    assert ref.mime_type == "text/html"
    with pytest.raises(AlixAIError) as missing:
        client.exports.create("ses-1", {"turn_id": "trn_01ARZ3NDEKTSV4RRFFQ69G5FA0", "format": "csv"})
    assert missing.value.code == "validation_failed"
    denied = AlixAIClient(
        fake.base_url,
        fake.token_provider("ses-1", capability_scopes=[]),
        http_client=httpx.Client(transport=httpx.MockTransport(fake.handler)),
    )
    with pytest.raises(AlixAIError) as err:
        denied.exports.create("ses-1", {"turn_id": "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", "format": "md"})
    assert err.value.code == "permission_denied"
