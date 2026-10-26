import type { ReactElement } from "react";
import type { AlixAIInterrupt } from "../activities.js";
export interface AlixAIConfirmationLabels {
    title: string;
    approve: string;
    decline: string;
    risk: string;
    expires: string;
    unavailable: string;
}
export declare const DEFAULT_CONFIRMATION_LABELS: AlixAIConfirmationLabels;
export interface AlixAIConfirmationCardProps {
    /** The interrupt of the run, as `useInterrupt` renders it (`interrupt` of its render props). */
    interrupt: AlixAIInterrupt | null | undefined;
    /** `resolve` of the interrupt: called with {"approved": true} or {"approved": false}. */
    resolve: (payload?: unknown, interruptId?: string) => unknown;
    /** `cancel` of the interrupt: the interrupt is dismissed, which AlixAI treats as a decline. */
    cancel?: (interruptId?: string) => unknown;
    labels?: Partial<AlixAIConfirmationLabels>;
}
export declare const AlixAIConfirmationCard: ({ interrupt, resolve, cancel, labels }: AlixAIConfirmationCardProps) => ReactElement | null;
//# sourceMappingURL=confirmation.d.ts.map