"""Turn event decoding and text join (including text_replaced / ADR 0046)."""

from __future__ import annotations

import json

import pytest

from alixai.errors import AlixAIError
from alixai.events import TURN_EVENT_TYPES, parse_turn_event, turn_text
from tests.helpers.openapi import SCHEMAS_DIR

pytestmark = pytest.mark.skipif(not SCHEMAS_DIR.is_dir(), reason="schemas/ is not next to this package (an sdist install)")

TRN = "trn_01J0000000000000000000000A"


def test_turn_text_resets_on_text_replaced() -> None:
    events = [
        parse_turn_event({"type": "token", "turn_id": TRN, "seq": 0, "text": "shown "}),
        parse_turn_event({"type": "token", "turn_id": TRN, "seq": 1, "text": "text"}),
        parse_turn_event({"type": "text_replaced", "turn_id": TRN, "seq": 2, "reason": "verification_failed"}),
        parse_turn_event({"type": "token", "turn_id": TRN, "seq": 3, "text": "template"}),
        parse_turn_event({"type": "done", "turn_id": TRN, "seq": 4, "finish_reason": "completed"}),
    ]
    assert turn_text(events) == "template"


def test_text_replaced_requires_verification_failed() -> None:
    with pytest.raises(AlixAIError) as caught:
        parse_turn_event({"type": "text_replaced", "turn_id": TRN, "seq": 0, "reason": "other"})
    assert caught.value.code == "internal"
    assert caught.value.details.get("reason") == "malformed_event"


def test_turn_event_types_match_schema() -> None:
    schema = json.loads((SCHEMAS_DIR / "conversation" / "turn-event.schema.json").read_text(encoding="utf-8"))
    want: list[str] = []
    for item in schema["oneOf"]:
        ref = item["$ref"]
        name = ref.removeprefix("#/$defs/")
        want.append(schema["$defs"][name]["properties"]["type"]["const"])
    assert list(TURN_EVENT_TYPES) == want


def test_valid_text_replaced_example_parses() -> None:
    path = SCHEMAS_DIR / "conversation" / "examples" / "turn-event" / "valid" / "text-replaced.json"
    event = parse_turn_event(json.loads(path.read_text(encoding="utf-8")))
    assert event.type == "text_replaced"
    assert event.reason == "verification_failed"
