"""The two RAG sources: the equipment-manuals folder and the maintenance-history record API."""

from __future__ import annotations

import hashlib
import re
from pathlib import Path

import jsonschema
from conftest import World

from facility_maintenance.store import STANDARD_PARTS

MANUALS = Path(__file__).resolve().parent.parent / "manuals"
HISTORY = "/rag/maintenance-history"
SCOPE = {"application_id": "facility-maintenance", "tenant_id": "t1", "workspace_id": "s1"}

# the list page the application contract of AlixAI's document-API provider accepts
PAGE_SCHEMA = {
    "type": "object",
    "required": ["items"],
    "additionalProperties": False,
    "properties": {
        "items": {
            "type": "array",
            "items": {
                "type": "object",
                "required": ["key", "version"],
                "additionalProperties": False,
                "properties": {
                    "key": {"type": "string", "minLength": 1, "maxLength": 4096},
                    "version": {"type": "string", "minLength": 1, "maxLength": 256, "pattern": r"^[^\s]+$"},
                    "size": {"type": "integer", "minimum": 0},
                    "modified_at": {"type": "string", "pattern": r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$"},
                    "deleted": {"type": "boolean"},
                },
            },
        },
        "next_cursor": {"type": ["string", "null"]},
    },
}


# ---- the manuals folder ----------------------------------------------------------------------------------------


def test_the_manuals_folder_holds_pdf_and_markdown_in_both_languages() -> None:
    names = sorted(p.name for p in MANUALS.iterdir())
    assert any(n.endswith(".pdf") for n in names) and any(n.endswith(".md") for n in names)
    assert any(n.endswith(".it.md") for n in names)
    assert {p.suffix for p in MANUALS.iterdir()} <= {".pdf", ".md"}  # nothing else would be ingested
    for path in MANUALS.glob("*.md"):
        text = path.read_text(encoding="utf-8")
        assert text.startswith("# ") and "\r" not in text and ("Synthetic" in text or "sintetico" in text)


def test_the_pdfs_are_well_formed_and_carry_a_text_layer() -> None:
    for path in MANUALS.glob("*.pdf"):
        data = path.read_bytes()
        assert data.startswith(b"%PDF-1.") and data.rstrip().endswith(b"%%EOF")
        start = int(re.search(rb"startxref\n(\d+)\n%%EOF", data).group(1))
        assert data[start : start + 4] == b"xref"
        entries = re.findall(rb"(\d{10}) 00000 n \n", data[start:])
        assert entries, path
        for number, offset in enumerate(entries, start=1):
            assert data[int(offset) :].startswith(f"{number} 0 obj".encode()), (path.name, number)
        for length, body in re.findall(rb"/Length (\d+) >>\nstream\n(.*?)\nendstream", data, re.S):
            assert int(length) == len(body)  # uncompressed: a parser needs no filter to read it
        assert b"/FlateDecode" not in data and b"/Encrypt" not in data and b"/JavaScript" not in data
        assert b" Tj" in data


def test_the_manuals_agree_with_the_application_about_its_parts() -> None:
    catalogue = {number for number, _, _, _ in STANDARD_PARTS}
    mentioned: set[str] = set()
    for path in MANUALS.iterdir():
        text = path.read_bytes().decode("latin-1" if path.suffix == ".pdf" else "utf-8")
        mentioned |= set(re.findall(r"\b(?:FLT|BLT|SEL|CAP)-\d+\b", text))
    assert mentioned and mentioned <= catalogue
    pdf = (MANUALS / "ahu-200-maintenance.pdf").read_bytes().decode("latin-1")
    assert "FLT-204" in pdf and "250 Pa" in pdf and "3 months" in pdf  # what the RAG smoke question asks for


# ---- the maintenance-history API ------------------------------------------------------------------------------------


def close_some(world: World, count: int) -> list[str]:
    ids = []
    for number in range(count):
        made = world.run(world.sup, "work-orders-create", "workorders.create", {"asset": world.ids["gen"], "title": f"Generator job number {number}"})
        order = made["identifiers"][0]["entity_id"]
        world.run(world.sup, "work-orders-close", "workorders.close", {"work_order": order, "resolution": f"Finished job {number}", "parts_used": ["CAP-45:1"]})
        ids.append(order)
    return ids


def test_the_history_is_the_closed_work_orders_of_one_site(world: World) -> None:
    listing = world.client.get(HISTORY, params=SCOPE)
    assert listing.status_code == 200
    page = listing.json()
    jsonschema.validate(page, PAGE_SCHEMA)
    done = world.ids["wo_done"]
    assert [i["key"] for i in page["items"]] == [f"work-orders/{done}.md"] and page["next_cursor"] is None
    item = page["items"][0]
    assert item["modified_at"] == "2026-09-10T14:30:00Z"

    fetched = world.client.get(f"{HISTORY}/{item['key']}", params=SCOPE)
    assert fetched.status_code == 200 and fetched.headers["content-type"].startswith("text/markdown")
    body = fetched.content
    # the version is the content address of exactly the bytes served, which the provider verifies
    assert item["version"] == "sha256:" + hashlib.sha256(body).hexdigest() and item["size"] == len(body)
    text = body.decode()
    assert text.startswith(f"# Work order {done}: Replace return filter") and "Replaced the return filter." in text
    assert "FLT-204 x2: Pleated air filter 592x592" in text and "Parts cost: EUR 37.00" in text and "Marco" not in text
    assert "Site: North Campus" in text and "Technician: Giulia Ferri" in text

    head = world.client.head(f"{HISTORY}/{item['key']}", params=SCOPE)
    assert head.status_code == 200 and head.headers["etag"] == f'"{item["version"]}"' and int(head.headers["content-length"]) == len(body)
    assert head.headers["last-modified"] == "Thu, 10 Sep 2026 14:30:00 GMT"
    assert head.content == b""


def test_the_history_is_paged_by_key_and_follows_the_work_that_is_closed(world: World) -> None:
    closed = close_some(world, 4)
    seen, cursor, pages = [], None, 0
    while True:
        params = {**SCOPE, "limit": 2, **({"cursor": cursor} if cursor else {})}
        page = world.client.get(HISTORY, params=params).json()
        jsonschema.validate(page, PAGE_SCHEMA)
        seen += [i["key"] for i in page["items"]]
        cursor, pages = page["next_cursor"], pages + 1
        if cursor is None:
            break
    assert pages == 3 and seen == sorted(seen) and len(seen) == len(set(seen)) == 5
    assert {f"work-orders/{o}.md" for o in closed} < set(seen)
    # a limit outside the range is clamped, never an error and never the whole database
    assert len(world.client.get(HISTORY, params={**SCOPE, "limit": 0}).json()["items"]) == 1
    assert world.client.get(HISTORY, params={**SCOPE, "limit": "many"}).status_code == 422


def test_the_history_never_leaves_its_scope(world: World) -> None:
    closed = close_some(world, 1)[0]
    key = f"work-orders/{closed}.md"
    other_site = {**SCOPE, "workspace_id": "s2"}
    assert world.client.get(HISTORY, params=other_site).json()["items"] == []
    assert world.client.get(f"{HISTORY}/{key}", params=other_site).status_code == 404  # exactly as if it did not exist
    # the same identifiers in another tenant name other records
    assert world.client.get(f"{HISTORY}/{key}", params={**SCOPE, "tenant_id": "t2", "workspace_id": "s9"}).status_code == 404
    for params in ({}, {**SCOPE, "application_id": "clinic-booking"}, {**SCOPE, "tenant_id": "nope"}, {**SCOPE, "workspace_id": "nope"}, {k: v for k, v in SCOPE.items() if k != "workspace_id"}):
        assert world.client.get(HISTORY, params=params).status_code == 403, params
    assert world.client.get(f"{HISTORY}/work-orders/WO-9999.md", params=SCOPE).status_code == 404
    assert world.client.get(f"{HISTORY}/../etc/passwd", params=SCOPE).status_code == 404
