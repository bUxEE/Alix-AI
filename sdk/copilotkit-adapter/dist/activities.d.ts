import type { Citation, Effect, RichBlock } from "@alixai/sdk";
export declare const ALIXAI_ACTIVITY: Readonly<{
    readonly attachments: "alixai.attachments";
    readonly progress: "alixai.progress";
    readonly citations: "alixai.citations";
    readonly receipt: "alixai.receipt";
    readonly blockPrefix: "alixai.block.";
}>;
export declare const ALIXAI_CUSTOM_EVENT: Readonly<{
    readonly directive: "alixai.directive";
    readonly suggestions: "alixai.suggestions";
}>;
export declare const ALIXAI_BLOCK_TYPES: readonly ["table", "chart", "diagram", "citation_list", "action_card", "clarification_choices", "confirmation_panel"];
export type AlixAIBlockType = (typeof ALIXAI_BLOCK_TYPES)[number];
export type AlixAIBlock<T extends AlixAIBlockType = AlixAIBlockType> = Extract<RichBlock, {
    type: T;
}>;
/** The activity type of a validated block: `alixai.block.<type>`. */
export declare const blockActivityType: (type: AlixAIBlockType) => string;
/** The id prefix of the UI tool call of a turn (`alixai_ui_<turn id>`); its result goes back as a tool message. */
export declare const UI_TOOL_CALL_PREFIX = "alixai_ui_";
/** What the confirmation card shows: projected by the core from the prepared action, never from the model's text. */
export interface AlixAIConfirmation {
    readonly preparationId: string;
    readonly capabilityId: string;
    readonly summary: string;
    readonly effects: readonly Effect[];
    readonly riskLevel: number;
    readonly expiresAt: string;
    readonly mode: string;
}
/** The interrupt of an AG-UI run that ended in `outcome: interrupt`, as the confirmation helpers read it. */
export interface AlixAIInterrupt {
    readonly id: string;
    readonly reason?: string;
    readonly message?: string;
    readonly toolCallId?: string;
    readonly expiresAt?: string;
    readonly metadata?: unknown;
}
/** Reads the metadata of a `confirmation_required` interrupt. Anything that is not exactly that is undefined. */
export declare const parseConfirmation: (interrupt: AlixAIInterrupt | undefined | null) => AlixAIConfirmation | undefined;
/** The one payload an approval carries: exactly `{"approved": true}`; a decline is `{"approved": false}`. */
export declare const approvalPayload: (approved: boolean) => {
    approved: boolean;
};
export interface AlixAIAttachment {
    readonly fileId: string;
    readonly name: string;
    readonly mimeType: string;
    readonly sizeBytes: number;
    readonly previewable: boolean;
    readonly expiresAt?: string;
}
export declare const parseAttachments: (content: unknown) => AlixAIAttachment[] | undefined;
export declare const parseCitations: (content: unknown) => Citation[] | undefined;
export interface AlixAIProgress {
    readonly stage: string;
    readonly state?: string;
    readonly capabilityId?: string;
    readonly percent?: number;
}
export declare const parseProgress: (content: unknown) => AlixAIProgress | undefined;
/** Reads the content of an `alixai.block.<type>` activity; the block must be of the type its activity names. */
export declare const parseBlock: <T extends AlixAIBlockType>(type: T, content: unknown) => AlixAIBlock<T> | undefined;
export interface AlixAISuggestion {
    readonly title: string;
    readonly message: string;
    readonly capabilityId?: string;
}
/** Reads the value of the `alixai.suggestions` custom event. Picking one sends `message` as an ordinary user message. */
export declare const parseSuggestions: (value: unknown) => AlixAISuggestion[];
/** What the active goal waits for: the client-safe projection of the session (`{"alixai": {"pending": ...}}`, section 8 of agui.md). */
export interface AlixAIPending {
    readonly kind: "confirmation" | "clarification" | "goal";
    readonly capabilityId: string;
    readonly phase: string;
    /** Names and origins of the fields the goal has a value for; the values stay in the session. */
    readonly collectedFields: readonly {
        readonly name: string;
        readonly origin: string;
    }[];
    readonly missingFields: readonly string[];
    readonly question?: {
        readonly field: string;
        readonly answerKind: string;
        readonly choices: readonly {
            readonly id: string;
            readonly label: string;
        }[];
    };
    readonly preparationId?: string;
    readonly expiresAt?: string;
}
/**
 * Reads the pending goal from the shared state of the agent (`agent.state`, or the snapshot of a run): `state.alixai.pending`.
 * It is an allow-list, like the projection that produced it: a member that is not named here is dropped, and a state that is
 * not exactly this shape (an unknown kind, a field of the wrong type) is undefined, so a renderer shows nothing rather than
 * something wrong. The page's own state members are never read.
 */
export declare const parsePendingState: (state: unknown) => AlixAIPending | undefined;
/** A canonical value of the application's facts as plain text (never markup). */
export declare const formatCanonicalValue: (value: unknown) => string;
//# sourceMappingURL=activities.d.ts.map