"""A small server-sent-events decoder, written to the "Event stream interpretation" rules of the HTML standard.

UTF-8 with an optional BOM, lines ended by CRLF, LF or CR, ``:`` comment lines, ``field: value`` with one optional
space, data lines joined by LF, an event dispatched by a blank line. An event cut off before its blank line is never
dispatched, so a connection that dies mid-frame cannot deliver half an event. It is fed bytes and decodes them
incrementally and strictly (invalid UTF-8 is a protocol violation, never silently replaced), so a multi-byte
character split across two chunks decodes correctly.
"""

from __future__ import annotations

import codecs
import re
from dataclasses import dataclass
from typing import Optional

from alixai.errors import AlixAIError, sdk_error

#: Upper bound for one event (or one unterminated line): a server that never ends a frame is refused.
MAX_EVENT_CHARS = 8 * 1024 * 1024

_LINE_END = re.compile(r"\r\n|\r|\n")


@dataclass(frozen=True)
class SseMessage:
    #: The ``event:`` field; "message" when the frame has none.
    event: str
    #: The ``data:`` lines joined by "\n".
    data: str
    #: The ``id:`` field of this frame, None when the frame has none.
    id: Optional[str]


def _bad_utf8() -> AlixAIError:
    return sdk_error("internal", "the event stream is not valid UTF-8", details={"reason": "bad_utf8"})


class SseDecoder:
    def __init__(self, max_event_chars: int = MAX_EVENT_CHARS) -> None:
        self._decoder = codecs.getincrementaldecoder("utf-8")(errors="strict")
        self._max = max_event_chars
        self._buffer = ""
        self._started = False
        self._event = ""
        self._data: list[str] = []
        self._data_chars = 0
        self._id: Optional[str] = None

    def feed(self, chunk: bytes) -> list[SseMessage]:
        """Consumes a chunk and returns the events it completed."""
        try:
            text = self._decoder.decode(chunk)
        except UnicodeDecodeError as exc:
            raise _bad_utf8() from exc
        return self._drain(text, final=False)

    def finish(self) -> list[SseMessage]:
        """Ends the stream: a trailing CR counts as a line end; an unfinished line or event is dropped."""
        try:
            text = self._decoder.decode(b"", final=True)
        except UnicodeDecodeError as exc:
            raise _bad_utf8() from exc
        return self._drain(text, final=True)

    # -- internals

    def _too_large(self) -> AlixAIError:
        return sdk_error("internal", "an event of the stream exceeds the size limit", details={"reason": "event_too_large"})

    def _drain(self, text: str, final: bool) -> list[SseMessage]:
        self._buffer += text
        if not self._started and self._buffer:
            self._started = True
            if self._buffer.startswith("﻿"):
                self._buffer = self._buffer[1:]
        out: list[SseMessage] = []
        position = 0
        while True:
            found = _LINE_END.search(self._buffer, position)
            if found is None:
                break
            if found.group() == "\r" and found.end() == len(self._buffer) and not final:
                break  # a CR at the very end may be the first half of CRLF: wait for the next chunk
            message = self._line(self._buffer[position : found.start()])
            position = found.end()
            if message is not None:
                out.append(message)
        self._buffer = self._buffer[position:]
        if len(self._buffer) > self._max:
            raise self._too_large()
        if final:
            self._buffer = ""
        return out

    def _reset(self) -> None:
        self._event = ""
        self._data = []
        self._data_chars = 0
        self._id = None

    def _line(self, line: str) -> Optional[SseMessage]:
        if line == "":
            if not self._data:
                self._reset()  # a frame with no data (comments, or only `event:`) dispatches nothing
                return None
            message = SseMessage(self._event or "message", "\n".join(self._data), self._id)
            self._reset()
            return message
        if line.startswith(":"):
            return None
        name, colon, value = line.partition(":")
        if colon and value.startswith(" "):
            value = value[1:]
        if name == "event":
            self._event = value
        elif name == "data":
            self._data_chars += len(value) + 1
            if self._data_chars > self._max:
                raise self._too_large()
            self._data.append(value)
        elif name == "id":
            if "\x00" not in value:
                self._id = value
        return None  # retry and unknown fields are ignored
