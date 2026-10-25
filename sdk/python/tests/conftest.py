import sys
from pathlib import Path

import pytest

SDK_ROOT = Path(__file__).resolve().parents[1]
# The generator is a dev tool, not part of the package: the drift test imports it from tools/.
sys.path.insert(0, str(SDK_ROOT / "tools"))

from tests.helpers.openapi import Spec  # noqa: E402


@pytest.fixture(scope="session")
def spec() -> Spec:
    return Spec()
