// The names and shapes AlixAI puts into AG-UI activities, custom events and interrupts (docs/integration/agui.md
// sections 2, 6 and 7). Everything that arrives here is data from a stream: the parsers check the shape and return
// undefined for anything else, so a renderer shows nothing rather than something wrong. None of it is markup; renderers
// show it as text.
export const ALIXAI_ACTIVITY = Object.freeze({
    attachments: "alixai.attachments",
    progress: "alixai.progress",
    citations: "alixai.citations",
    receipt: "alixai.receipt",
    blockPrefix: "alixai.block.",
});
export const ALIXAI_CUSTOM_EVENT = Object.freeze({
    directive: "alixai.directive",
    suggestions: "alixai.suggestions",
});
export const ALIXAI_BLOCK_TYPES = [
    "table",
    "chart",
    "diagram",
    "citation_list",
    "action_card",
    "clarification_choices",
    "confirmation_panel",
];
/** The activity type of a validated block: `alixai.block.<type>`. */
export const blockActivityType = (type) => `${ALIXAI_ACTIVITY.blockPrefix}${type}`;
/** The id prefix of the UI tool call of a turn (`alixai_ui_<turn id>`); its result goes back as a tool message. */
export const UI_TOOL_CALL_PREFIX = "alixai_ui_";
const isRecord = (value) => typeof value === "object" && value !== null && !Array.isArray(value);
const isString = (value) => typeof value === "string";
/** Reads the metadata of a `confirmation_required` interrupt. Anything that is not exactly that is undefined. */
export const parseConfirmation = (interrupt) => {
    if (interrupt === undefined || interrupt === null || interrupt.reason !== "confirmation_required")
        return undefined;
    const meta = interrupt.metadata;
    if (!isRecord(meta))
        return undefined;
    const { preparationId, capabilityId, summary, effects, riskLevel, expiresAt, confirmation } = meta;
    if (!isString(preparationId) || !isString(capabilityId) || !isString(summary) || !isString(expiresAt))
        return undefined;
    if (!Array.isArray(effects) || !effects.every(isRecord))
        return undefined;
    if (typeof riskLevel !== "number" || !Number.isFinite(riskLevel))
        return undefined;
    const mode = isRecord(confirmation) && isString(confirmation["mode"]) ? confirmation["mode"] : "explicit";
    return { preparationId, capabilityId, summary, effects: effects, riskLevel, expiresAt, mode };
};
/** The one payload an approval carries: exactly `{"approved": true}`; a decline is `{"approved": false}`. */
export const approvalPayload = (approved) => ({ approved });
export const parseAttachments = (content) => {
    if (!isRecord(content) || !Array.isArray(content["files"]))
        return undefined;
    const files = [];
    for (const file of content["files"]) {
        if (!isRecord(file) || !isString(file["fileId"]) || !isString(file["name"]) || !isString(file["mimeType"]))
            return undefined;
        if (typeof file["sizeBytes"] !== "number" || typeof file["previewable"] !== "boolean")
            return undefined;
        files.push({
            fileId: file["fileId"],
            name: file["name"],
            mimeType: file["mimeType"],
            sizeBytes: file["sizeBytes"],
            previewable: file["previewable"],
            ...(isString(file["expiresAt"]) && { expiresAt: file["expiresAt"] }),
        });
    }
    return files;
};
export const parseCitations = (content) => {
    if (!isRecord(content) || !Array.isArray(content["citations"]))
        return undefined;
    const ok = content["citations"].every((citation) => isRecord(citation) && isString(citation["source_id"]) && isString(citation["display_name"]) && isRecord(citation["open_ref"]));
    return ok ? content["citations"] : undefined;
};
export const parseProgress = (content) => {
    if (!isRecord(content) || !isString(content["stage"]))
        return undefined;
    return {
        stage: content["stage"],
        ...(isString(content["state"]) && { state: content["state"] }),
        ...(isString(content["capabilityId"]) && { capabilityId: content["capabilityId"] }),
        ...(typeof content["percent"] === "number" && { percent: content["percent"] }),
    };
};
/** Reads the content of an `alixai.block.<type>` activity; the block must be of the type its activity names. */
export const parseBlock = (type, content) => {
    if (!isRecord(content) || content["type"] !== type)
        return undefined;
    switch (type) {
        case "table":
            return Array.isArray(content["columns"]) && Array.isArray(content["rows"]) ? content : undefined;
        case "chart":
            return isString(content["mark"]) && Array.isArray(content["data"]) && isRecord(content["encoding"]) ? content : undefined;
        case "clarification_choices":
            return Array.isArray(content["choices"]) && isString(content["field"]) ? content : undefined;
        default:
            return content;
    }
};
/** Reads the value of the `alixai.suggestions` custom event. Picking one sends `message` as an ordinary user message. */
export const parseSuggestions = (value) => {
    if (!isRecord(value) || !Array.isArray(value["suggestions"]))
        return [];
    const out = [];
    for (const item of value["suggestions"]) {
        if (!isRecord(item) || !isString(item["title"]) || !isString(item["message"]))
            continue;
        out.push({ title: item["title"], message: item["message"], ...(isString(item["capabilityId"]) && { capabilityId: item["capabilityId"] }) });
    }
    return out;
};
const PENDING_KINDS = ["confirmation", "clarification", "goal"];
/**
 * Reads the pending goal from the shared state of the agent (`agent.state`, or the snapshot of a run): `state.alixai.pending`.
 * It is an allow-list, like the projection that produced it: a member that is not named here is dropped, and a state that is
 * not exactly this shape (an unknown kind, a field of the wrong type) is undefined, so a renderer shows nothing rather than
 * something wrong. The page's own state members are never read.
 */
export const parsePendingState = (state) => {
    if (!isRecord(state) || !isRecord(state["alixai"]))
        return undefined;
    const pending = state["alixai"]["pending"];
    if (!isRecord(pending))
        return undefined;
    const { kind, capabilityId, phase, collectedFields, missingFields, question, preparationId, expiresAt } = pending;
    if (!isString(kind) || !PENDING_KINDS.includes(kind) || !isString(capabilityId) || !isString(phase))
        return undefined;
    const collected = [];
    for (const field of collectedFields === undefined || collectedFields === null ? [] : Array.isArray(collectedFields) ? collectedFields : [undefined]) {
        if (!isRecord(field) || !isString(field["name"]) || !isString(field["origin"]))
            return undefined;
        collected.push({ name: field["name"], origin: field["origin"] });
    }
    const missing = missingFields === undefined || missingFields === null ? [] : missingFields;
    if (!Array.isArray(missing) || !missing.every(isString))
        return undefined;
    let asked;
    if (question !== undefined && question !== null) {
        if (!isRecord(question) || !isString(question["field"]) || !isString(question["answerKind"]))
            return undefined;
        const choices = [];
        const offered = question["choices"];
        for (const choice of offered === undefined || offered === null ? [] : Array.isArray(offered) ? offered : [undefined]) {
            if (!isRecord(choice) || !isString(choice["id"]) || !isString(choice["label"]))
                return undefined;
            choices.push({ id: choice["id"], label: choice["label"] });
        }
        asked = { field: question["field"], answerKind: question["answerKind"], choices };
    }
    return {
        kind: kind,
        capabilityId,
        phase,
        collectedFields: collected,
        missingFields: missing,
        ...(asked !== undefined && { question: asked }),
        ...(isString(preparationId) && { preparationId }),
        ...(isString(expiresAt) && { expiresAt }),
    };
};
// ---- values ----
/** A canonical value of the application's facts as plain text (never markup). */
export const formatCanonicalValue = (value) => {
    if (!isRecord(value))
        return value === null || value === undefined ? "" : String(value);
    const inner = value["value"];
    if (value["kind"] === "money" && isString(value["currency"]))
        return `${String(inner)} ${value["currency"]}`;
    return inner === null || inner === undefined ? "" : String(inner);
};
//# sourceMappingURL=activities.js.map