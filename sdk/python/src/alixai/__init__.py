"""The AlixAI Python SDK: sync and async clients over httpx, typed models generated from schemas/, SSE turn streaming.

    from alixai import AlixAIClient, ContextTokenRequest

    def get_context_token(request: ContextTokenRequest) -> str:
        return my_app.mint_single_use_context_token(request.session_id)   # one fresh token per call

    with AlixAIClient("http://127.0.0.1:8080", get_context_token) as client:
        session = client.create_session()
        with client.send_turn(session.session_id, {"text": "hello"}) as stream:
            for event in stream:
                ...

For host tests use ``alixai.testing.FakeAlixAIServer``: no port, no network.
"""

from alixai.client import AlixAIAsyncClient, AlixAIClient, AsyncContextTokenProvider, ContextTokenProvider, ContextTokenRequest
from alixai.errors import ERROR_CODES, AlixAIError, is_error_code, is_retryable_code
from alixai.events import done_event, turn_text
from alixai.gen.actions import ActionReceipt, PreparedAction
from alixai.gen.api import (
    ActionOutcome,
    Application,
    Capability,
    CapabilityList,
    Health,
    PendingState,
    ReadyReport,
    Registration,
    Session,
    SessionEnded,
)
from alixai.gen.common import Error
from alixai.gen.conversation import TurnEvent
from alixai.gen.decision import DecisionStatus
from alixai.gen.readiness import ReadinessFinding, ReadinessReport, ReadinessRequirement, ReadinessStatusView
from alixai.operations import OPERATIONS
from alixai.providers import AsyncProvidersApi, ProvidersApi, Secret, is_manual_fallback
from alixai.turn import CONFIRMATION_TOKEN_HEADER, MAX_TURN_ATTACHMENTS, AsyncTurnStream, ConfirmationGrant, TurnInput, TurnResult, TurnStream
from alixai.types import Liveness

__version__ = "1.0.0"

__all__ = [
    "AlixAIAsyncClient",
    "AlixAIClient",
    "AlixAIError",
    "AsyncProvidersApi",
    "ProvidersApi",
    "Secret",
    "is_manual_fallback",
    "ActionOutcome",
    "ActionReceipt",
    "Application",
    "AsyncContextTokenProvider",
    "AsyncTurnStream",
    "CONFIRMATION_TOKEN_HEADER",
    "Capability",
    "CapabilityList",
    "ConfirmationGrant",
    "ContextTokenProvider",
    "ContextTokenRequest",
    "DecisionStatus",
    "ERROR_CODES",
    "Error",
    "Health",
    "Liveness",
    "MAX_TURN_ATTACHMENTS",
    "OPERATIONS",
    "PendingState",
    "PreparedAction",
    "ReadinessFinding",
    "ReadinessReport",
    "ReadinessRequirement",
    "ReadinessStatusView",
    "ReadyReport",
    "Registration",
    "Session",
    "SessionEnded",
    "TurnEvent",
    "TurnInput",
    "TurnResult",
    "TurnStream",
    "done_event",
    "is_error_code",
    "is_retryable_code",
    "turn_text",
]
