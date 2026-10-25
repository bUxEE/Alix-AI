"""Wire types the OpenAPI document declares inline (on a response) rather than as a named component."""

from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, ConfigDict


class Liveness(BaseModel):
    """The answer of ``GET /healthz``: the process is serving HTTP (schemas/api/openapi.yaml, getLiveness)."""

    model_config = ConfigDict(extra="forbid")
    status: Literal["ok"]
