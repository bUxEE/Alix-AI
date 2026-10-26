import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
import { useState } from "react";
import { approvalPayload, formatCanonicalValue, parseConfirmation } from "../activities.js";
export const DEFAULT_CONFIRMATION_LABELS = {
    title: "Please confirm",
    approve: "Approve",
    decline: "Decline",
    risk: "Risk level",
    expires: "Valid until",
    unavailable: "This confirmation cannot be shown. You can decline it.",
};
export const AlixAIConfirmationCard = ({ interrupt, resolve, cancel, labels }) => {
    const text = { ...DEFAULT_CONFIRMATION_LABELS, ...labels };
    const [answered, setAnswered] = useState(false);
    if (interrupt === null || interrupt === undefined)
        return null;
    const confirmation = parseConfirmation(interrupt);
    const answer = (approved) => {
        if (answered)
            return;
        setAnswered(true);
        if (approved)
            void resolve(approvalPayload(true), interrupt.id);
        else
            void resolve(approvalPayload(false), interrupt.id);
    };
    const dismiss = () => {
        if (answered)
            return;
        setAnswered(true);
        if (cancel !== undefined)
            void cancel(interrupt.id);
        else
            void resolve(approvalPayload(false), interrupt.id);
    };
    if (confirmation === undefined) {
        return (_jsxs("section", { className: "alixai-confirmation", "data-alixai": "confirmation", "data-state": "unavailable", "aria-label": text.title, children: [_jsx("p", { children: text.unavailable }), _jsx("button", { type: "button", disabled: answered, onClick: dismiss, children: text.decline })] }));
    }
    return (_jsxs("section", { className: "alixai-confirmation", "data-alixai": "confirmation", "data-risk": confirmation.riskLevel, "aria-label": text.title, children: [_jsx("h3", { children: text.title }), _jsx("p", { "data-alixai": "summary", children: confirmation.summary }), _jsx("ul", { "data-alixai": "effects", children: confirmation.effects.map((effect, index) => (_jsxs("li", { children: [_jsx("span", { "data-alixai": "effect-description", children: effect.description }), _jsx("dl", { children: effect.changes.map((change, changeIndex) => (_jsxs("div", { children: [_jsx("dt", { children: change.field }), _jsx("dd", { children: formatCanonicalValue(change.value) })] }, changeIndex))) })] }, index))) }), _jsxs("p", { "data-alixai": "risk", children: [text.risk, ": ", confirmation.riskLevel] }), _jsxs("p", { "data-alixai": "expires", children: [text.expires, ": ", confirmation.expiresAt] }), _jsxs("div", { children: [_jsx("button", { type: "button", "data-alixai": "approve", disabled: answered, onClick: () => answer(true), children: text.approve }), _jsx("button", { type: "button", "data-alixai": "decline", disabled: answered, onClick: () => answer(false), children: text.decline })] })] }));
};
//# sourceMappingURL=confirmation.js.map