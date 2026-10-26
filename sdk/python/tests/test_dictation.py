"""Dictation (record-to-text) client and fake-server coverage."""

from __future__ import annotations

import struct

import pytest

from alixai import AlixAIClient, AlixAIError
from alixai.testing import FakeAlixAIServer


def _wav(seconds: float = 0.5, rate: int = 16000) -> bytes:
    n = int(rate * seconds)
    pcm = b"\x00\x10" * n
    header = struct.pack(
        "<4sI4s4sIHHIIHH4sI",
        b"RIFF",
        36 + len(pcm),
        b"WAVE",
        b"fmt ",
        16,
        1,
        1,
        rate,
        rate * 2,
        2,
        16,
        b"data",
        len(pcm),
    )
    return header + pcm


@pytest.fixture()
def fake() -> FakeAlixAIServer:
    return FakeAlixAIServer()


def test_capabilities_and_create(fake: FakeAlixAIServer) -> None:
    client = fake.client("ses-1")
    caps = client.dictations.capabilities()
    assert caps.available is True
    assert caps.sample_rate_hz == 16000
    assert caps.format == "audio/wav"
    fake.set_dictation_text("hello from dictation")
    result = client.dictations.create("ses-1", _wav(), language="en")
    assert result.text == "hello from dictation"
    assert result.segment_kind == "user_recorded"
    assert result.locality == "local"


def test_silent_and_unavailable(fake: FakeAlixAIServer) -> None:
    client = fake.client("ses-1")
    fake.set_dictation_silent(True)
    with pytest.raises(AlixAIError) as silent:
        client.dictations.create("ses-1", _wav())
    assert silent.value.code == "validation_failed"
    assert (silent.value.details or {}).get("reason") == "silent"

    fake.set_dictation_silent(False)
    fake.set_dictation_disabled(True)
    caps = client.dictations.capabilities()
    assert caps.available is False
    assert caps.locality == "none"
    with pytest.raises(AlixAIError) as denied:
        client.dictations.create("ses-1", _wav())
    assert denied.value.code == "unavailable"

    fake.set_dictation_disabled(False)
    fake.set_dictation_unavailable(True)
    with pytest.raises(AlixAIError) as down:
        client.dictations.capabilities()
    assert down.value.code == "unavailable"


def test_refuses_bad_wav_before_send(fake: FakeAlixAIServer) -> None:
    client = fake.client("ses-1")
    before = len(fake.requests)
    with pytest.raises(AlixAIError) as err:
        client.dictations.create("ses-1", b"not-wav")
    assert err.value.code == "validation_failed"
    assert len(fake.requests) == before
