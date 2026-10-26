import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
import { useState } from "react";
import { formatCanonicalValue } from "../activities.js";
// Plain, unstyled renderers for the validated blocks and activities AlixAI sends (docs/integration/agui.md section 2).
// Every value is rendered as text. A host that wants its own look passes its own components to the activity renderers
// (see hooks.tsx) or styles the `data-alixai` hooks. Nothing here executes anything: a diagram is shown as its source and
// a chart is data, never code.
export const AlixAITable = ({ block }) => (_jsxs("table", { className: "alixai-table", "data-alixai": "table", children: [block.caption !== undefined && _jsx("caption", { children: block.caption }), _jsx("thead", { children: _jsx("tr", { children: block.columns.map((column) => (_jsx("th", { scope: "col", children: column.label }, column.key))) }) }), _jsx("tbody", { children: block.rows.map((row, rowIndex) => (_jsx("tr", { children: row.map((cell, cellIndex) => (_jsx("td", { children: cell === null ? "" : String(cell) }, cellIndex))) }, rowIndex))) })] }));
/**
 * A chart block: its description is the accessible name, the data is a table, and a bar chart also draws its bars. The
 * block is declarative data; there is no script and no URL in it to follow.
 */
export const AlixAIChart = ({ block }) => {
    const x = block.encoding.x?.field;
    const y = block.encoding.y?.field;
    const values = y === undefined ? [] : block.data.map((record) => Number(record[y] ?? 0)).filter((value) => Number.isFinite(value));
    const max = Math.max(1, ...values);
    return (_jsxs("figure", { className: "alixai-chart", "data-alixai": "chart", "data-mark": block.mark, "aria-label": block.title ?? block.description, children: [block.title !== undefined && _jsx("figcaption", { children: block.title }), _jsx("p", { children: block.description }), block.mark === "bar" && x !== undefined && y !== undefined && (_jsx("svg", { role: "img", "aria-label": block.description, viewBox: `0 0 ${block.data.length * 24} 60`, width: "100%", height: "60", children: block.data.map((record, index) => {
                    const height = (Number(record[y] ?? 0) / max) * 50;
                    return _jsx("rect", { x: index * 24 + 4, y: 55 - height, width: 16, height: height, fill: "currentColor" }, index);
                }) })), _jsxs("table", { children: [_jsx("thead", { children: _jsx("tr", { children: Object.keys(block.data[0] ?? {}).map((key) => (_jsx("th", { scope: "col", children: key }, key))) }) }), _jsx("tbody", { children: block.data.map((record, index) => (_jsx("tr", { children: Object.values(record).map((value, cell) => (_jsx("td", { children: value === null ? "" : String(value) }, cell))) }, index))) })] })] }));
};
export const AlixAIDiagram = ({ block }) => (_jsx("pre", { className: "alixai-diagram", "data-alixai": "diagram", "data-language": block.language, children: block.source }));
export const AlixAIActionCard = ({ block }) => (_jsxs("section", { className: "alixai-action-card", "data-alixai": "action-card", children: [_jsx("h3", { children: block.title }), _jsx("dl", { children: block.facts.map((fact, index) => (_jsxs("div", { children: [_jsx("dt", { children: fact.field }), _jsx("dd", { children: fact.display ?? formatCanonicalValue(fact.value) })] }, index))) })] }));
/** The form of a clarification: the question, its choices as buttons, and optional free text. A form is data, not executable UI. */
export const AlixAIChoices = ({ block, onChoose, onFreeText, sendLabel = "Send" }) => {
    const [text, setText] = useState("");
    return (_jsxs("form", { className: "alixai-choices", "data-alixai": "choices", onSubmit: (event) => {
            event.preventDefault();
            if (onFreeText !== undefined && text.trim() !== "")
                onFreeText(text.trim());
        }, children: [block.prompt !== undefined && _jsx("p", { children: block.prompt }), _jsx("div", { role: "group", "aria-label": block.field, children: block.choices.map((choice) => (_jsx("button", { type: "button", title: choice.description, onClick: () => onChoose(choice.choice_id), children: choice.label }, choice.choice_id))) }), block.allow_free_text && onFreeText !== undefined && (_jsxs("label", { children: [_jsx("span", { children: block.field }), _jsx("input", { type: "text", value: text, onChange: (event) => setText(event.target.value) }), _jsx("button", { type: "submit", children: sendLabel })] }))] }));
};
/** Citations with the opaque, scope-checked reference of each source. The reference is shown, never opened by this component. */
export const AlixAICitationList = ({ citations, onOpen, }) => (_jsx("ol", { className: "alixai-citations", "data-alixai": "citations", children: citations.map((citation, index) => (_jsxs("li", { "data-availability": citation.availability, "data-staleness": citation.staleness, children: [onOpen !== undefined && citation.availability === "available" ? (_jsx("button", { type: "button", onClick: () => onOpen(citation), children: citation.display_name })) : (_jsx("span", { children: citation.display_name })), _jsxs("span", { "data-alixai": "citation-version", children: [" ", citation.version] }), citation.locator?.section !== undefined && _jsxs("span", { "data-alixai": "citation-locator", children: [" ", citation.locator.section] }), citation.locator?.page !== undefined && _jsxs("span", { "data-alixai": "citation-locator", children: [" p. ", citation.locator.page] }), _jsx("ul", { children: citation.claims.map((claim, claimIndex) => (_jsx("li", { children: claim }, claimIndex))) })] }, `${citation.source_id}-${index}`))) }));
const formatSize = (bytes) => {
    if (bytes < 1024)
        return `${bytes} B`;
    if (bytes < 1024 * 1024)
        return `${Math.round(bytes / 102.4) / 10} KB`;
    return `${Math.round(bytes / 104857.6) / 10} MB`;
};
export const AlixAIAttachmentPreview = ({ files, renderPreview }) => (_jsx("ul", { className: "alixai-attachments", "data-alixai": "attachments", children: files.map((file) => (_jsxs("li", { "data-previewable": file.previewable, children: [file.previewable && renderPreview?.(file), _jsx("span", { children: file.name }), _jsxs("span", { "data-alixai": "attachment-meta", children: [" ", file.mimeType, ", ", formatSize(file.sizeBytes)] })] }, file.fileId))) }));
export const AlixAIProgressLine = ({ progress }) => (_jsxs("p", { className: "alixai-progress", "data-alixai": "progress", "data-state": progress.state, children: [progress.stage, progress.percent !== undefined && ` ${Math.round(progress.percent)}%`] }));
//# sourceMappingURL=blocks.js.map