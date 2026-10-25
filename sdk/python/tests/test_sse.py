"""The server-sent-events decoder, to the "Event stream interpretation" rules of the HTML standard."""

from __future__ import annotations

import pytest

from alixai import AlixAIError
from alixai.sse import SseDecoder, SseMessage


def decode(*chunks: bytes) -> list[SseMessage]:
    decoder = SseDecoder()
    out: list[SseMessage] = []
    for chunk in chunks:
        out += decoder.feed(chunk)
    return out + decoder.finish()


def test_two_events_in_one_chunk_are_both_delivered() -> None:
    messages = decode(b"id: 0\nevent: token\ndata: {\"a\":1}\n\nid: 1\nevent: done\ndata: {}\n\n")
    assert [(m.id, m.event, m.data) for m in messages] == [("0", "token", '{"a":1}'), ("1", "done", "{}")]


def test_every_split_point_decodes_the_same_events() -> None:
    wire = "retry: 1000\r\n\r\n: keep-alive\r\n\r\nid: 0\r\nevent: token\r\ndata: héllo ☃\r\n\r\nid: 1\ndata: a\ndata: b\n\n".encode()
    whole = decode(wire)
    assert [(m.id, m.event, m.data) for m in whole] == [("0", "token", "héllo ☃"), ("1", "message", "a\nb")]
    for cut in range(1, len(wire)):  # includes the middle of a CRLF and the middle of a multi-byte character
        assert decode(wire[:cut], wire[cut:]) == whole


def test_a_lone_cr_ends_a_line_and_a_bom_is_skipped() -> None:
    assert [m.data for m in decode(b"\xef\xbb\xbfdata: x\r\r")] == ["x"]


def test_an_event_cut_off_before_its_blank_line_is_never_delivered() -> None:
    assert decode(b"id: 0\nevent: token\ndata: {\"half\"") == []
    assert decode(b"data: complete\n\ndata: cut") == [SseMessage("message", "complete", None)]


def test_comments_unknown_fields_and_data_free_frames_dispatch_nothing() -> None:
    assert decode(b": keep-alive\n\nevent: ping\n\nretry: 5\nfoo: bar\n\n") == []


def test_invalid_utf8_is_a_protocol_violation_not_a_replacement_character() -> None:
    with pytest.raises(AlixAIError) as raised:
        decode(b"data: \xff\xfe\n\n")
    assert raised.value.code == "internal" and raised.value.details == {"reason": "bad_utf8"}


def test_an_event_that_never_ends_is_refused() -> None:
    decoder = SseDecoder(max_event_chars=64)
    with pytest.raises(AlixAIError) as raised:
        for _ in range(10):
            decoder.feed(b"data: " + b"x" * 20 + b"\n")
    assert raised.value.details == {"reason": "event_too_large"}
