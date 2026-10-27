"""1.4 callers may still index private-LLM responses with ``res["key"]``."""

from __future__ import annotations

import httpx

from alixai.client import AlixAIClient
from alixai.testing import FakeAlixAIServer


def test_private_llm_response_supports_getitem():
    fake = FakeAlixAIServer()
    client = AlixAIClient(
        fake.base_url,
        fake.token_provider("ses-1"),
        http_client=httpx.Client(transport=fake.transport),
    )
    body = {
        "origin": "http://127.0.0.1:8080",
        "api_dialect": "openai-compatible",
        "model_ids": ["local-model"],
        "roles": ["conversational_controller"],
        "owner_kind": "shared",
    }
    res = client.ops.put_private_llm_endpoint("ple_01J0000000000000000000000A", body)
    assert res["endpoint_id"] == "ple_01J0000000000000000000000A"
    assert res.endpoint_id == "ple_01J0000000000000000000000A"
    pin = client.ops.put_tenant_private_llm("ten-1", {"endpoint_id": "ple_01J0000000000000000000000A"})
    assert pin["tenant_id"] == "ten-1"
