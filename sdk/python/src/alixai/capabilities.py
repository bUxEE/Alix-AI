"""ADR 0047 client_capabilities announcement for turn requests / AG-UI."""

from __future__ import annotations

from typing import Any

#: What this SDK release can parse of the 1.5 analyst / rich-block surface.
DEFAULT_CLIENT_CAPABILITIES: dict[str, Any] = {
    "contract": "1.5",
    "blocks": [
        "markdown",
        "code",
        "table",
        "chart",
        "diagram",
        "citation_list",
        "action_card",
        "clarification_choices",
        "confirmation_panel",
        "kpi",
        "timeline",
        "record_card",
        "provenance",
        "draft",
    ],
    "chart_marks": ["bar", "line", "area", "point", "pie", "scatter", "heatmap", "combo_bar_line"],
    "chart_layouts": ["grouped", "stacked", "horizontal", "donut"],
    "progress_stages": [
        "interpreting",
        "retrieving",
        "sources",
        "validating",
        "preparing",
        "committing",
        "tool_call",
        "queued",
        "generating",
        "verifying",
        "analyst_read",
        "analyst_compute",
        "analyst_search",
        "analyst_fetch",
        "analyst_answer",
    ],
    "source_labels": True,
    "block_fields": ["dataset_id", "bar_layout", "pie_style", "source"],
}
