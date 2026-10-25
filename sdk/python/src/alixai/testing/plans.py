"""What a host-application test says about one turn of the fake AlixAI server.

A plan describes the outcome in terms of the API (a reply, a question, a prepared action, a failure); the fake turns it
into the events, headers and state a real service would produce, so a test cannot forge an invalid stream by hand.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Mapping, Optional, Sequence, Union


@dataclass(frozen=True)
class Commit:
    """What committing a prepared action does, when the user confirms it."""

    #: The receipt status: committed, queued, rejected, partial or uncertain.
    status: str = "committed"
    #: The typed reason for a rejected, partial or uncertain commit: ``{"code": ..., "message": ...}``.
    error: Optional[Mapping[str, str]] = None
    facts: Sequence[Mapping[str, Any]] = ()
    records: Sequence[Mapping[str, Any]] = ()


@dataclass(frozen=True)
class Reply:
    text: str


@dataclass(frozen=True)
class Clarify:
    field: str
    question: str
    choices: Sequence[Mapping[str, Any]]


@dataclass(frozen=True)
class Prepare:
    capability_id: str
    summary: str
    effects: Optional[Sequence[Mapping[str, Any]]] = None
    risk_level: int = 2
    valid_for_s: Optional[float] = None
    commit: Commit = field(default_factory=Commit)
    text: Optional[str] = None


@dataclass(frozen=True)
class Fail:
    code: str
    message: str
    text: Optional[str] = None


@dataclass(frozen=True)
class HttpFailure:
    status: int
    error: Mapping[str, Any]


TurnPlan = Union[Reply, Clarify, Prepare, Fail, HttpFailure]


def reply(text: str) -> Reply:
    """The assistant answers with text."""
    return Reply(text)


def ask_choice(field: str, question: str, choices: Sequence[Mapping[str, Any]]) -> Clarify:
    """The assistant asks the user to pick one of several choices (the pending state becomes a clarification).

    Each choice is ``{"choice_id": ..., "label": ...}`` with an optional canonical ``value`` (default: an identifier
    value holding the choice id). The directive schema requires at least two choices to disambiguate between.
    """
    if len(choices) < 2:
        raise ValueError("ask_choice needs at least two choices")
    return Clarify(field, question, choices)


def prepare_action(
    capability_id: str,
    summary: str,
    *,
    effects: Optional[Sequence[Mapping[str, Any]]] = None,
    risk_level: int = 2,
    valid_for_s: Optional[float] = None,
    commit: Optional[Commit] = None,
    text: Optional[str] = None,
) -> Prepare:
    """The assistant prepares an action.

    At risk level 2 (the default) the turn ends awaiting confirmation and the confirmation secret comes in the response
    header (stream) or the ``confirmation`` member (JSON); at level 3 the directive is ``request_step_up`` and there is
    no secret; at level 0 or 1 the action is committed in the same turn and a receipt event is sent.
    """
    return Prepare(capability_id, summary, effects, risk_level, valid_for_s, commit or Commit(), text)


def fail_turn(code: str, message: str, text: Optional[str] = None) -> Fail:
    """The turn ends in a typed failure: an ``error`` event, then ``done`` with finish_reason ``failed`` (still HTTP 200)."""
    return Fail(code, message, text)


def reject_turn(status: int, error: Mapping[str, Any]) -> HttpFailure:
    """The turn could not be run at all: an error response with this status, never a stream."""
    return HttpFailure(status, error)
