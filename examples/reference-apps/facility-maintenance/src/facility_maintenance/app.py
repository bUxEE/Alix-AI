"""The HTTP surface: a FastAPI application whose OpenAPI document is the signed contract.

The OpenAPI 3.0 document that describes the approved operations is a hand-reviewed file
(``openapi.yaml``, shipped byte for byte in the integration package), not something generated from
the routes: the adapter pins it, so it must change only on purpose. ``OPERATIONS`` below is the
table of routes; a test keeps the two in step.

Every reply to an operation is a DeterministicResult with the HTTP status a REST host would pick
(200, 401, 403, 404, 409, 422, 429, 503); a refusal is a result with status ``rejected``, never a
bare framework error. Request bodies are read as raw JSON and checked by the domain, so a malformed
body is also a typed refusal and never FastAPI's own 422 shape.
"""

from __future__ import annotations

import hmac
import json
import logging
import re
from collections.abc import Callable
from email.utils import format_datetime
from importlib import resources
from typing import Any

import yaml
from fastapi import FastAPI, Request, Response

from . import __version__, history, stepup, testsupport
from .access import check_application, decode_identity, parse_identity
from .capabilities import ACTION_SLUGS, ACTIONS, list_work_orders, resolve_entities
from .clock import Clock, FixedClock, SystemClock, parse_timestamp
from .config import Settings
from .demo import seed_demo
from .errors import Refusal
from .inspect_ import inspect, parse_aspects
from .protocol import NO_RECORD, Env, commit_action, prepare_action, status_action, validate_action
from .results import rejected_result, unavailable_result
from .store import Store

log = logging.getLogger("facility_maintenance")

MAX_BODY_BYTES = 1 << 20
IDEMPOTENCY_HEADER = "Idempotency-Key"
_KEY = re.compile(r"^[A-Za-z0-9_-]{16,128}$")

# operationId -> (method, path). The operations the package approves; the test-support ones exist
# only while the application runs for an audit.
SUFFIX = {
    "workorders.create": "CreateWorkOrder",
    "workorders.assign": "AssignTechnician",
    "workorders.close": "CloseWorkOrder",
    "assets.decommission": "DecommissionAsset",
}
OPERATIONS: dict[str, tuple[str, str]] = {
    "listWorkOrders": ("GET", "/v1/work-orders"),
    "resolveAsset": ("POST", "/v1/resolve/assets"),
    "resolveWorkOrder": ("POST", "/v1/resolve/work-orders"),
    "resolveTechnician": ("POST", "/v1/resolve/technicians"),
    **{
        f"{verb}{SUFFIX[capability]}": (method, path)
        for capability, slug in ACTION_SLUGS.items()
        for verb, method, path in (
            ("validate", "POST", f"/v1/{slug}/validate"),
            ("prepare", "POST", f"/v1/{slug}/prepare"),
            ("commit", "POST", f"/v1/{slug}/commit"),
            ("status", "GET", f"/v1/{slug}/status/{{idempotency_key}}"),
        )
    },
    "describeApplication": ("GET", "/v1/describe"),
    "getHealth": ("GET", "/v1/health"),
    "inspectApplication": ("GET", "/v1/inspect"),
}
TEST_SUPPORT_OPERATIONS: dict[str, tuple[str, str]] = {
    "createTenant": ("POST", "/v1/test/tenants"),
    "seedTenant": ("POST", "/v1/test/tenants/{tenant_id}/seed"),
    "cleanupTenant": ("DELETE", "/v1/test/tenants/{tenant_id}"),
    # Does what a supervisor does in the step-up UI, for a disposable tenant: the audit's way to complete a step-up.
    "approveStepUp": ("POST", "/v1/test/tenants/{tenant_id}/step-ups"),
}
HTTP_STATUS = {
    "unauthenticated": 401,
    "permission_denied": 403,
    "scope_missing": 403,
    "scope_violation": 403,
    "step_up_required": 403,
    "conflict": 409,
    "stale": 409,
    "expired": 409,
    "replayed": 409,
    "rate_limited": 429,
}


def load_spec() -> dict[str, Any]:
    text = resources.files("facility_maintenance").joinpath("openapi.yaml").read_text(encoding="utf-8")
    return yaml.safe_load(text)


def status_for(result: dict[str, Any]) -> int:
    """The HTTP status a REST host gives a DeterministicResult. It carries no meaning of its own."""
    status = result.get("status")  # a management document (describe, health) is not a result
    if status == "unavailable":
        return 503
    if status != "rejected":
        return 200
    issue = result["errors"][0]
    if issue["code"] == "validation_failed" and issue.get("field") == "idempotency_key" and issue["message"] == NO_RECORD:
        return 404
    return HTTP_STATUS.get(issue["code"], 422)


def _json(status: int, body: dict[str, Any], headers: dict[str, str] | None = None) -> Response:
    return Response(
        content=json.dumps(body, ensure_ascii=False, separators=(",", ":")),
        status_code=status,
        media_type="application/json",
        headers={"Cache-Control": "no-store", **(headers or {})},
    )


def _list_fields(request: Request) -> dict[str, Any] | None:
    """The read's fields arrive as query parameters, which are text: the one number is converted."""
    fields: dict[str, Any] = dict(request.query_params)
    count = fields.get("max_results")
    if isinstance(count, str) and count.lstrip("-").isdigit():
        fields["max_results"] = int(count)
    return fields or None


def create_app(settings: Settings | None = None, store: Store | None = None, clock: Clock | None = None) -> FastAPI:
    settings = settings or Settings.from_env()
    clock = clock or (FixedClock(settings.fixed_now) if settings.fixed_now else SystemClock())
    store = store or Store()
    if settings.tenant_id not in store.tenants:
        tenant = store.create_tenant(settings.tenant_id, disposable=False)
        if settings.demo_seed:
            seed_demo(store, tenant, clock, settings.demo_passcode)
    env = Env(store, clock)

    app = FastAPI(title="Facility Maintenance", version=__version__, docs_url=None, redoc_url=None, openapi_url=None)
    app.state.env, app.state.settings = env, settings
    spec = load_spec()
    if not settings.test_support:
        spec["paths"] = {p: v for p, v in spec["paths"].items() if not p.startswith("/v1/test/")}
    app.openapi = lambda: spec  # type: ignore[method-assign]

    def authenticated(request: Request) -> bool:
        if settings.api_token is None:
            return True
        supplied = request.headers.get("authorization", "").encode()
        return hmac.compare_digest(supplied, f"Bearer {settings.api_token}".encode())

    async def serve(request: Request, run: Callable[[str | None, Any], dict[str, Any]], *, key: str | None = None) -> Response:
        """One operation: authenticate, read the body, run the domain, answer with a typed result."""
        echo = {IDEMPOTENCY_HEADER: key} if key and _KEY.match(key) else {}
        if not authenticated(request):
            return _json(401, rejected_result("unauthenticated", "the request carries no valid credential"), echo)
        payload: Any = None
        raw = await request.body()
        if len(raw) > MAX_BODY_BYTES:
            return _json(422, rejected_result("validation_failed", "the request body is too large"), echo)
        if raw.strip():
            try:
                payload = json.loads(raw)
            except (ValueError, UnicodeDecodeError):
                return _json(422, rejected_result("validation_failed", "the request body is not valid JSON"), echo)
        try:
            with store.lock:
                result = run(request.headers.get("x-alixai-identity"), payload)
        except Refusal as refusal:
            result = rejected_result(refusal.code, refusal.message, field=refusal.field, freshness=refusal.freshness)
        except Exception:
            log.exception("operation failed")
            result = unavailable_result("internal", "the application could not complete the request")
        return _json(status_for(result), result, echo)

    def route(operation_id: str, handler: Callable[..., Any], *, with_key: bool = False) -> None:
        method, path = {**OPERATIONS, **TEST_SUPPORT_OPERATIONS}[operation_id]

        async def endpoint(request: Request) -> Response:
            key = (request.path_params.get("idempotency_key") or request.headers.get(IDEMPOTENCY_HEADER)) if with_key else None
            return await serve(request, lambda header, payload: handler(request, header, payload), key=key)

        app.add_api_route(path, endpoint, methods=[method], name=operation_id, include_in_schema=False)

    # ---- capability operations --------------------------------------------------------------------
    route("listWorkOrders", lambda r, header, _: list_work_orders(env, header, _list_fields(r)))
    for entity_type, operation_id in (("asset", "resolveAsset"), ("work_order", "resolveWorkOrder"), ("technician", "resolveTechnician")):
        route(operation_id, lambda r, header, payload, e=entity_type: resolve_entities(env, header, e, payload))
    for capability, definition in ACTIONS.items():
        suffix = SUFFIX[capability]
        route(f"validate{suffix}", lambda r, header, payload, d=definition: validate_action(d, env, header, payload))
        route(f"prepare{suffix}", lambda r, header, payload, d=definition: prepare_action(d, env, header, payload))
        route(
            f"commit{suffix}",
            lambda r, header, payload, d=definition: commit_action(d, env, header, r.headers.get(IDEMPOTENCY_HEADER), payload),
            with_key=True,
        )
        route(
            f"status{suffix}",
            lambda r, header, _, d=definition: status_action(d, env, header, r.path_params["idempotency_key"]),
            with_key=True,
        )

    # ---- management: describe, health, inspect -------------------------------------------------------
    route("describeApplication", lambda r, h, _: {"contract_versions": [1], "application_version": __version__, "unmapped": []})
    route("getHealth", lambda r, h, _: {"status": "ok", "application_version": __version__})

    def inspect_handler(request: Request, header: str | None, _: Any) -> dict[str, Any]:
        # A well-formed identity of this application is enough: the report holds deployment facts
        # and counts, no record, and it must stay readable to the audit before any tenant exists.
        check_application(parse_identity(decode_identity(header)))
        requested = [a for item in request.query_params.getlist("aspects") for a in item.split(",") if a]
        return inspect(env, settings, parse_aspects(requested))

    route("inspectApplication", inspect_handler)

    # ---- the audit's disposable tenants --------------------------------------------------------------
    if settings.test_support:
        raw_routes: dict[str, Callable[[Request, Any], tuple[int, dict[str, Any]]]] = {
            "createTenant": lambda r, payload: testsupport.create_tenant(env, payload),
            "seedTenant": lambda r, payload: testsupport.seed_tenant(env, r.path_params["tenant_id"], payload),
            "cleanupTenant": lambda r, payload: testsupport.cleanup_tenant(env, r.path_params["tenant_id"]),
            "approveStepUp": lambda r, payload: testsupport.approve_step_up(env, r.path_params["tenant_id"], payload),
        }

        def provisioning(call: Callable[[Request, Any], tuple[int, dict[str, Any]]]) -> Callable[[Request], Any]:
            async def provision(request: Request) -> Response:
                if not authenticated(request):
                    return _json(401, rejected_result("unauthenticated", "the request carries no valid credential"))
                raw = await request.body()
                try:
                    payload = json.loads(raw) if raw.strip() else None
                except (ValueError, UnicodeDecodeError):
                    return _json(422, rejected_result("validation_failed", "the request body is not valid JSON"))
                with store.lock:
                    status, body = call(request, payload)
                return _json(status, body)

            return provision

        for operation_id, call in raw_routes.items():
            method, path = TEST_SUPPORT_OPERATIONS[operation_id]
            app.add_api_route(path, provisioning(call), methods=[method], name=operation_id, include_in_schema=False)

    # ---- the contract, the RAG source and the step-up UI -----------------------------------------------
    @app.get("/openapi.json", include_in_schema=False)
    async def openapi_document(request: Request) -> Response:
        return _json(200, spec)

    @app.api_route(f"{history.ROOT}", methods=["GET", "HEAD"], include_in_schema=False)
    async def history_list(request: Request) -> Response:
        if not authenticated(request):
            return _json(401, {"error": "unauthenticated"})
        query = request.query_params
        if query.get("application_id") != "facility-maintenance" or not query.get("tenant_id") or not query.get("workspace_id"):
            return _json(403, {"error": "the scope is not valid"})
        tenant = store.tenants.get(query["tenant_id"])
        if tenant is None or query["workspace_id"] not in tenant.sites:
            return _json(403, {"error": "the scope is not valid"})
        try:
            limit = int(query.get("limit", history.DEFAULT_LIMIT))
        except ValueError:
            return _json(422, {"error": "limit must be a number"})
        limit = max(1, min(limit, history.MAX_LIMIT))
        with store.lock:
            found = history.items(store, query["tenant_id"], query["workspace_id"])
        return _json(200, history.page(found, limit, query.get("cursor") or None))

    @app.api_route(f"{history.ROOT}/{{key:path}}", methods=["GET", "HEAD"], include_in_schema=False)
    async def history_item(request: Request, key: str) -> Response:
        if not authenticated(request):
            return _json(401, {"error": "unauthenticated"})
        query = request.query_params
        with store.lock:
            found = history.items(store, query.get("tenant_id", ""), query.get("workspace_id", ""))
        item = next((i for i in found if i.key == key), None)
        if query.get("application_id") != "facility-maintenance" or item is None:
            return _json(404, {"error": "not found"})
        headers = {
            "ETag": f'"{item.version}"',
            "Last-Modified": format_datetime(parse_timestamp(item.modified_at), usegmt=True),
            "Content-Length": str(len(item.body)),
            "Cache-Control": "no-store",
        }
        return Response(content=item.body, media_type="text/markdown; charset=utf-8", headers=headers)

    @app.api_route("/ui/step-up/{reference}", methods=["GET", "POST"], include_in_schema=False)
    async def step_up_ui(request: Request, reference: str) -> Response:
        page = stepup.handle(env, request.method, reference, request.headers.get("authorization"), await request.body())
        return Response(content=page.html, status_code=page.status, media_type="text/html; charset=utf-8", headers=page.headers)

    return app

