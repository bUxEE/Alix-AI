"""``python -m facility_maintenance``: serve the application on loopback."""

from __future__ import annotations

import json
import sys

import uvicorn

from .app import create_app
from .config import LOOPBACK_HOSTS, Settings


def main() -> None:
    settings = Settings.from_env()
    if settings.host not in LOOPBACK_HOSTS:
        sys.exit(f"refusing to listen on {settings.host!r}: this application binds to loopback only")
    app = create_app(settings)
    print(json.dumps({"event": "listening", "api": f"http://{settings.host}:{settings.port}"}), flush=True)
    uvicorn.run(app, host=settings.host, port=settings.port, log_level="warning")


if __name__ == "__main__":
    main()
