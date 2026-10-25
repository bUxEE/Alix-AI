"""The readiness types (task M9-06): the models generated from schemas/readiness decode the schemas' examples (valid accepted,
invalid refused), refuse unknown members, and have no client method (readiness discovery is CLI-only and advisory)."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest
from jsonschema import Draft202012Validator
from pydantic import TypeAdapter, ValidationError
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

import alixai
from alixai.gen.readiness import ReadinessFinding, ReadinessReport, ReadinessRequirement, ReadinessStatusView
from tests.helpers.openapi import SCHEMAS_DIR

pytestmark = pytest.mark.skipif(not SCHEMAS_DIR.is_dir(), reason="schemas/ is not next to this package (an sdist install)")

READINESS = SCHEMAS_DIR / "readiness"
ROOTS: dict[str, Any] = {
    "finding": ReadinessFinding,
    "requirement": ReadinessRequirement,
    "report": ReadinessReport,
    "status-view": ReadinessStatusView,
}

# The invalid examples the models accept: each is refused only by a cross-member rule (if/then, not, uniqueItems) that the
# generator does not model. The schemas stay the authority, so these must be refused by the schema, and nothing else may slip in.
SCHEMA_ONLY = {
    ("finding", "contradicted-without-evidence"),
    ("finding", "empty-segment-path"),
    ("finding", "found-without-evidence"),
    ("finding", "model-unverified-marked-found"),
    ("finding", "parent-path"),
    ("finding", "parent-segment-path"),
    ("finding", "partial-without-evidence"),
    ("finding", "unverified-without-reason"),
    ("report", "deterministic-mode-with-model"),
    ("report", "finding-with-evidence-missing"),
    ("report", "model-leg-ran-without-digest"),
    ("report", "model-mode-without-model"),
    ("requirement", "satisfied-by-repeated"),
}


def examples(kind: str) -> list[Any]:
    return [pytest.param(schema, path, id=f"{schema}/{path.stem}") for schema in ROOTS for path in sorted((READINESS / "examples" / schema / kind).glob("*.json"))]


def schema_validator(schema: str) -> Draft202012Validator:
    resources = []
    for path in sorted(SCHEMAS_DIR.glob("*/*.schema.json")):
        doc = json.loads(path.read_text(encoding="utf-8"))
        resources.append((doc["$id"], Resource.from_contents(doc, default_specification=DRAFT202012)))
    root = json.loads((READINESS / f"{schema}.schema.json").read_text(encoding="utf-8"))
    return Draft202012Validator(root, registry=Registry().with_resources(resources))


def load(path: Path) -> Any:
    return json.loads(path.read_text(encoding="utf-8"))


def test_the_example_sets_are_the_whole_corpus() -> None:
    assert len(examples("valid")) + len(examples("invalid")) == len(list((READINESS / "examples").rglob("*.json")))


@pytest.mark.parametrize(("schema", "path"), examples("valid"))
def test_every_valid_readiness_example_is_accepted_and_round_trips(schema: str, path: Path) -> None:
    adapter: TypeAdapter[Any] = TypeAdapter(ROOTS[schema])
    data = load(path)
    assert json.loads(adapter.dump_json(adapter.validate_python(data), exclude_unset=True)) == data


@pytest.mark.parametrize(("schema", "path"), examples("invalid"))
def test_every_invalid_readiness_example_is_refused(schema: str, path: Path) -> None:
    data = load(path)
    # The schema, the authority, refuses every invalid example.
    assert not schema_validator(schema).is_valid(data)
    if (schema, path.stem) in SCHEMA_ONLY:
        return
    with pytest.raises(ValidationError):
        TypeAdapter(ROOTS[schema]).validate_python(data)


def test_the_models_refuse_unknown_members_at_every_level() -> None:
    finding = load(READINESS / "examples" / "finding" / "valid" / "deterministic-found.json")
    for mutate in (
        lambda d: d.update(certified=True),
        lambda d: d["evidence"][0].update(extra=1),
        lambda d: d["remediation"].update(fr="x"),
    ):
        data = json.loads(json.dumps(finding))
        mutate(data)
        with pytest.raises(ValidationError):
            ReadinessFinding.model_validate(data)
    report = load(next((READINESS / "examples" / "report" / "valid").glob("*model*.json")))
    for mutate in (lambda d: d["scan"].update(score=1), lambda d: d["summary"].update(passed=1), lambda d: d["checklist"][0].update(approved=True)):
        data = json.loads(json.dumps(report))
        mutate(data)
        with pytest.raises(ValidationError):
            ReadinessReport.model_validate(data)


def test_a_host_reads_a_stored_report_from_json() -> None:
    path = READINESS / "examples" / "report" / "valid" / "deterministic-only-no-model.json"
    report = ReadinessReport.model_validate_json(path.read_bytes())
    assert report.advisory is True and report.mode == "deterministic_only" and report.model is None
    assert sum(report.summary.model_dump(exclude_none=True).values()) == len(report.checklist)


def test_the_types_are_exported_and_readiness_has_no_client_method() -> None:
    for name in ("ReadinessReport", "ReadinessFinding", "ReadinessRequirement", "ReadinessStatusView"):
        assert name in alixai.__all__ and getattr(alixai, name) is not None
    # getReadiness is the /readyz health probe, not readiness discovery.
    assert not [n for n, op in alixai.OPERATIONS.items() if "/readiness" in op.path.lower()]
    for client in (alixai.AlixAIClient, alixai.AlixAIAsyncClient):
        assert not [n for n in dir(client) if n.lower().startswith(("scan_readiness", "readiness"))]
