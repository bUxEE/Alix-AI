"""Turn events: decoding one event and enforcing what the API promises about the sequence of a turn.

Events arrive from a server the SDK does not control (a proxy sits in between, a new server may be older or newer).
Decoding fails closed: a malformed event or an event of an unknown type is a typed error, never an event to skip. The
JSON Schema (schemas/conversation/turn-event) is the authority; the generated model enforces its structure.
"""

from __future__ import annotations

from typing import Any, Optional, Sequence

from pydantic import TypeAdapter, ValidationError

from alixai.errors import AlixAIError, sdk_error
from alixai.gen.conversation.turn_event import DoneEvent, TurnEvent

_EVENT = TypeAdapter(TurnEvent)

# Every TurnEvent discriminant, in schemas/conversation/turn-event order. A test checks this against the schema.
TURN_EVENT_TYPES = (
    "token",
    "directive",
    "clarification",
    "prepared_action",
    "receipt",
    "citation",
    "progress",
    "error",
    "done",
    "block",
    "memory_proposal",
    "text_replaced",
)


def malformed(reason: str, **extra: Any) -> AlixAIError:
    return sdk_error("internal", f"the server sent a malformed turn event: {reason}", details={"reason": "malformed_event", **extra})


def parse_turn_event(value: Any) -> TurnEvent:
    """Checks one decoded event and returns it typed. Raises an ``internal`` AlixAIError when it is malformed."""
    if not isinstance(value, dict):
        raise malformed("an event must be a JSON object")
    kind = value.get("type")
    if kind not in TURN_EVENT_TYPES:
        raise malformed("unknown event type", event_type=kind[:64] if isinstance(kind, str) else type(kind).__name__)
    if kind == "text_replaced" and value.get("reason") != "verification_failed":
        raise malformed("text_replaced.reason must be verification_failed", event_type=kind)
    try:
        return _EVENT.validate_python(value)
    except ValidationError as exc:
        # Locations and error kinds only: the values of an event are never copied into an error.
        first = exc.errors(include_input=False, include_context=False, include_url=False)[0]
        where = ".".join(str(p) for p in first["loc"])
        raise malformed(f"{first['type']} at {where}", event_type=kind) from exc


class TurnSequence:
    """One turn_id throughout, ``seq`` running from 0 without gaps, ``done`` last.

    An SSE frame's ``event:`` and ``id:`` fields must agree with the event they carry.
    """

    def __init__(self, expected_turn_id: Optional[str] = None) -> None:
        self._turn_id = expected_turn_id
        self._next = 0
        self._done = False

    @property
    def turn_id(self) -> Optional[str]:
        return self._turn_id

    @property
    def done(self) -> bool:
        return self._done

    def accept(self, event: TurnEvent, frame_event: Optional[str] = None, frame_id: Optional[str] = None, framed: bool = False) -> TurnEvent:
        """Accepts the next event; raises when it breaks the sequence. Returns it unchanged."""
        if self._done:
            raise malformed("an event follows the done event")
        if self._turn_id is None:
            self._turn_id = event.turn_id
        elif event.turn_id != self._turn_id:
            raise malformed("the turn_id changed within one turn", seq=event.seq)
        if event.seq != self._next:
            raise sdk_error(
                "internal",
                f"the event sequence is broken: expected seq {self._next}, got {event.seq}",
                details={"reason": "sequence_gap", "expected": self._next, "got": event.seq},
            )
        if framed:
            if frame_event != event.type:
                raise malformed("the frame's event name differs from the event type", seq=event.seq)
            if frame_id != str(event.seq):
                raise malformed("the frame's id differs from the event's seq", seq=event.seq)
        self._next += 1
        if event.type == "done":
            self._done = True
        return event

    def finish(self) -> None:
        """Raises unless ``done`` has been accepted: a stream that ends earlier is an unknown outcome."""
        if not self._done:
            raise sdk_error(
                "uncertain_outcome",
                "the turn ended before its done event: what the turn did is unknown; read the session's pending state "
                "before sending anything again",
                details={"reason": "stream_ended_early", "received": self._next},
            )


def turn_text(events: Sequence[TurnEvent]) -> str:
    """The text of a turn: its token events, joined in order.

    A ``text_replaced`` event (ADR 0046) discards every token shown so far; only tokens after the last
    replacement count.
    """
    parts: list[str] = []
    for event in events:
        if event.type == "token":
            parts.append(event.text)
        elif event.type == "text_replaced":
            parts.clear()
    return "".join(parts)


def done_event(events: Sequence[TurnEvent]) -> Optional[DoneEvent]:
    """The ``done`` event of a finished turn."""
    last = events[-1] if events else None
    return last if last is not None and last.type == "done" else None
