"""Settings: read from the environment, loopback only, nothing secret in the code."""

from __future__ import annotations

import os
from collections.abc import Mapping
from dataclasses import dataclass

LOOPBACK_HOSTS = ("127.0.0.1", "::1", "localhost")


@dataclass
class Settings:
    # The one tenant of a deployment: the operator's organisation. Its sites are the workspaces.
    tenant_id: str = "facility-main"
    # The bearer credential AlixAI presents (cred:facility-maintenance-api). Unset means the API
    # does not demand one, which only a developer's machine should do.
    api_token: str | None = None
    # Disposable-tenant provisioning for the integration audit. Off unless asked for.
    test_support: bool = False
    # Load the synthetic demonstration data into the tenant at start.
    demo_seed: bool = False
    # The step-up passcode of the demonstration supervisors; unset leaves the step-up UI shut.
    demo_passcode: str | None = None
    host: str = "127.0.0.1"
    port: int = 8788
    # Freeze the clock (RFC 3339 UTC) so results are reproducible.
    fixed_now: str | None = None

    @classmethod
    def from_env(cls, env: Mapping[str, str] | None = None) -> Settings:
        env = os.environ if env is None else env
        flag = lambda name: env.get(name, "").strip().lower() in ("1", "true", "yes")  # noqa: E731
        return cls(
            tenant_id=env.get("FACILITY_TENANT_ID", cls.tenant_id),
            api_token=env.get("FACILITY_API_TOKEN") or None,
            test_support=flag("FACILITY_TEST_SUPPORT"),
            demo_seed=flag("FACILITY_DEMO_SEED"),
            demo_passcode=env.get("FACILITY_DEMO_PASSCODE") or None,
            host=env.get("FACILITY_HOST", cls.host),
            port=int(env.get("FACILITY_PORT", str(cls.port))),
            fixed_now=env.get("FACILITY_FIXED_NOW") or None,
        )
