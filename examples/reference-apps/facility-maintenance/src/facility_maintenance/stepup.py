"""The application's own authenticated UI, where a decommission is completed.

``assets.decommission`` is level 3: the AI can prepare it and show what it would do, but it can never
complete it. The core refuses to commit it, and so does this application's API. What completes it is
a supervisor, signed in to this application with their own passcode, approving the staged request
here. Nothing about this page is reachable through an approved OpenAPI operation, and it takes
nothing from AlixAI: the request was staged by Prepare and is identified by its reference.

    GET  /ui/step-up/{reference}   the request, behind HTTP Basic (the principal id and passcode)
    POST /ui/step-up/{reference}   approve it (form field ``csrf``, bound to the reference and user)

Three wrong passcodes lock the request; it then has to be prepared again. A request is also refused
when it expired or when the asset changed since it was staged.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
from dataclasses import dataclass
from html import escape
from urllib.parse import parse_qs

from .access import ROLE_PERMISSIONS, Caller
from .capabilities import ACTIONS
from .errors import Refusal
from .protocol import Env
from .store import ROLE_SUPERVISOR, Principal, StepUp, TenantData

MAX_FAILED_ATTEMPTS = 3
REALM = 'Basic realm="Facility Maintenance step-up", charset="UTF-8"'

SECURITY_HEADERS = {
    "Cache-Control": "no-store",
    "Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'",
    "X-Frame-Options": "DENY",
    "Referrer-Policy": "no-referrer",
}

_STYLE = (
    "body{font:16px/1.5 system-ui,sans-serif;margin:0;background:#f4f5f7;color:#1c2330}"
    "main{max-width:34rem;margin:12vh auto;padding:2rem;background:#fff;border:1px solid #d9dde4;border-radius:12px}"
    "h1{font-size:1.25rem;margin:0 0 1rem}dl{display:grid;grid-template-columns:8rem 1fr;gap:.25rem 1rem;margin:0 0 1.5rem}"
    "dt{color:#566074}dd{margin:0}button{font:inherit;padding:.6rem 1.2rem;border:0;border-radius:8px;"
    "background:#b3261e;color:#fff;cursor:pointer}p.note{color:#566074}"
)


@dataclass
class Page:
    status: int
    html: str
    headers: dict[str, str]


def _page(status: int, title: str, body: str, headers: dict[str, str] | None = None) -> Page:
    html = (
        '<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">'
        f"<title>{escape(title)}</title><style>{_STYLE}</style><main><h1>{escape(title)}</h1>{body}</main></html>"
    )
    return Page(status, html, {**SECURITY_HEADERS, **(headers or {})})


def _csrf(env: Env, step_up: StepUp) -> str:
    return hmac.new(env.store.secret, f"{step_up.reference}:{step_up.principal_id}".encode(), hashlib.sha256).hexdigest()


def _basic(authorization: str | None) -> tuple[str, str] | None:
    if not authorization or not authorization.lower().startswith("basic "):
        return None
    try:
        user, _, secret = base64.b64decode(authorization[6:].strip(), validate=True).decode().partition(":")
    except (binascii.Error, UnicodeError):
        return None
    return user, secret


def _challenge() -> Page:
    return _page(401, "Sign in to continue", '<p class="note">Sign in with your principal id and step-up passcode.</p>', {"WWW-Authenticate": REALM})


def handle(env: Env, method: str, reference: str, authorization: str | None, body: bytes) -> Page:
    with env.store.lock:
        tenant_id = env.store.step_up_index.get(reference)
        tenant = env.store.tenants.get(tenant_id) if tenant_id else None
        step_up = tenant.step_ups.get(reference) if tenant else None
        if tenant is None or step_up is None:
            return _page(404, "Not found", '<p class="note">There is no such request.</p>')
        principal = tenant.principals.get((step_up.workspace, step_up.principal_id))
        if principal is None or principal.role != ROLE_SUPERVISOR:
            return _page(403, "Not allowed", '<p class="note">This request cannot be completed here.</p>')
        credentials = _basic(authorization)
        if credentials is None:
            return _challenge()
        if step_up.status == "locked":
            return _page(403, "Request locked", '<p class="note">Too many wrong passcodes. Ask the assistant to prepare it again.</p>')
        user, passcode = credentials
        if user != principal.id or not env.store.check_passcode(principal, passcode):
            if user == principal.id and step_up.status == "pending":
                step_up.failed_attempts += 1
                if step_up.failed_attempts >= MAX_FAILED_ATTEMPTS:
                    step_up.status = "locked"
            return _challenge()
        asset = tenant.assets.get(step_up.asset_id)
        if step_up.status == "done":
            return _page(200, "Already decommissioned", f'<p class="note">{escape(asset.name if asset else step_up.asset_id)} is already decommissioned.</p>')
        if step_up.status != "pending" or step_up.expires_at <= env.clock.now():
            step_up.status = "expired" if step_up.status == "pending" else step_up.status
            return _page(410, "Request expired", '<p class="note">This request is no longer valid. Ask the assistant to prepare it again.</p>')
        if method == "POST":
            return _approve(env, tenant, principal, step_up, body)
        return _show(env, step_up, asset.name if asset else step_up.asset_id, asset.tag if asset else "")


def _show(env: Env, step_up: StepUp, name: str, tag: str) -> Page:
    form = (
        f'<form method="post"><input type="hidden" name="csrf" value="{_csrf(env, step_up)}">'
        "<button>Decommission this asset</button></form>"
    )
    details = (
        f"<dl><dt>Asset</dt><dd>{escape(name)} ({escape(tag)})</dd><dt>Reason</dt><dd>{escape(step_up.reason)}</dd>"
        f"<dt>Reference</dt><dd>{escape(step_up.reference)}</dd></dl>"
    )
    return _page(200, "Confirm decommission", details + form + '<p class="note">This retires the asset. It cannot receive work orders afterwards.</p>')


def _approve(env: Env, tenant: TenantData, principal: Principal, step_up: StepUp, body: bytes) -> Page:
    sent = parse_qs(body.decode("utf-8", "replace")).get("csrf", [""])[0]
    if not hmac.compare_digest(sent, _csrf(env, step_up)):
        return _page(403, "Not allowed", '<p class="note">The form is not valid. Open the request again.</p>')
    definition = ACTIONS["assets.decommission"]
    if definition.permission not in ROLE_PERMISSIONS[principal.role]:
        return _page(403, "Not allowed", '<p class="note">Your role may not decommission assets.</p>')
    caller = Caller(
        tenant=tenant,
        workspace=step_up.workspace,
        site_name=tenant.sites[step_up.workspace].name,
        principal_id=principal.id,
        principal_name=principal.name,
        role=principal.role,
        technician_id=principal.technician_id,
        session_id="ui-" + step_up.reference,
    )
    try:
        loaded = definition.load(env, caller, {"asset": step_up.asset_id, "reason": step_up.reason})
        asset = loaded.data["asset"]
        if asset.rev != step_up.asset_rev:
            return _page(409, "The asset changed", '<p class="note">The asset changed after the request was prepared. Ask the assistant to prepare it again.</p>')
        definition.check(env, caller, loaded)
    except Refusal as refusal:
        return _page(409, "Cannot decommission", f'<p class="note">{escape(refusal.message)}</p>')
    definition.apply(env, caller, loaded)
    step_up.status = "done"
    return _page(200, "Decommissioned", f'<p class="note">{escape(asset.name)} ({escape(asset.tag)}) is decommissioned.</p>')
