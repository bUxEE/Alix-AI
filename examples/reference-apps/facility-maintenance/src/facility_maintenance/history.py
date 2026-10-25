"""The maintenance-history record API: a RAG source read through the application's own API.

This is the small, read-only HTTP contract of AlixAI's ``application_document_api`` source provider
(internal/rag/providers/httpapi). It is deliberately not an OpenAPI capability operation: a model
never calls it, and AlixAI's ingestion reads it on its own schedule.

    GET  /rag/maintenance-history?application_id=&tenant_id=&workspace_id=&limit=&cursor=
    GET  /rag/maintenance-history/{key}         the record, as Markdown bytes
    HEAD /rag/maintenance-history/{key}         its metadata

A record is one closed work order, rendered as a Markdown document. Its key is
``work-orders/<id>.md`` and its version is the content address ``sha256:<hex>`` of exactly the bytes
served, which the provider verifies. The listing is keyset-paged (``cursor`` is the last key of the
previous page), so a record added during a scan never shifts another across a page boundary.

Scope: ``tenant_id`` and ``workspace_id`` are always sent and are the only records ever returned.
The history of a site is shared by every role of that site; it describes finished work, not people.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass

from .clock import to_timestamp
from .results import plain
from .store import Store, TenantData, WorkOrder

ROOT = "/rag/maintenance-history"
DEFAULT_LIMIT = 100
MAX_LIMIT = 500


@dataclass(frozen=True)
class Item:
    key: str
    body: bytes
    version: str
    modified_at: str


def render(tenant: TenantData, order: WorkOrder) -> str:
    asset = tenant.assets.get(order.asset_id)
    technician = tenant.technicians.get(order.technician_id) if order.technician_id else None
    lines = [
        f"# Work order {order.id}: {order.title}",
        "",
        f"- Site: {tenant.sites[order.workspace].name}",
        f"- Asset: {asset.name} ({asset.tag}, {asset.id})" if asset else "- Asset: unknown",
        f"- Category: {asset.category}" if asset else "- Category: unknown",
        f"- Priority: {order.priority}",
        f"- Technician: {technician.name if technician else 'unassigned'}",
        f"- Closed: {to_timestamp(order.closed_at) if order.closed_at else 'not closed'}",
        f"- Labour hours: {plain(order.labor_hours)}",
        "",
        "## Problem",
        "",
        order.description or order.title,
        "",
        "## Resolution",
        "",
        order.resolution,
        "",
        "## Parts used",
        "",
    ]
    names = {p.part_number: p.name for (_, _), p in tenant.parts.items() if p.workspace == order.workspace}
    if order.parts:
        lines += [f"- {number} x{quantity}: {names.get(number, number)}" for number, quantity in order.parts]
        lines.append(f"- Parts cost: EUR {plain(order.parts_cost)}")
    else:
        lines.append("- none")
    return "\n".join(lines) + "\n"


def items(store: Store, tenant_id: str, workspace_id: str) -> list[Item]:
    """The history of one site: its closed work orders, in key order."""
    tenant = store.tenants.get(tenant_id)
    if tenant is None or workspace_id not in tenant.sites:
        return []
    out = []
    for order in tenant.work_orders.values():
        if order.workspace != workspace_id or order.status != "closed" or order.closed_at is None:
            continue
        body = render(tenant, order).encode()
        out.append(Item(f"work-orders/{order.id}.md", body, "sha256:" + hashlib.sha256(body).hexdigest(), to_timestamp(order.closed_at)))
    return sorted(out, key=lambda item: item.key)


def page(found: list[Item], limit: int, cursor: str | None) -> dict[str, object]:
    after = [item for item in found if cursor is None or item.key > cursor]
    shown = after[:limit]
    return {
        "items": [
            {"key": i.key, "version": i.version, "size": len(i.body), "modified_at": i.modified_at} for i in shown
        ],
        "next_cursor": shown[-1].key if len(after) > len(shown) else None,
    }

