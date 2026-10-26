import { HttpAgent } from "@ag-ui/client";
import type { BaseEvent, HttpAgentConfig, RunAgentInput } from "@ag-ui/client";
import { Observable } from "rxjs";
import type { ConfirmationSecretStore } from "./secrets.js";
/**
 * The calls the agent makes; the host's token callback is asked once per call. `agui.connect` is the connect run that loads a
 * thread's history (a run with no message and no resume): the same route as `agui.run`, named apart so the host can tell a
 * history read from a turn.
 */
export type AlixAIGatewayOperation = "agui.run" | "agui.connect" | "agui.events" | "agui.cancel";
export interface AlixAIContextTokenRequest {
    readonly operation: AlixAIGatewayOperation;
    /** The AlixAI session the token must be minted for: the AG-UI thread id of the run (the gateway demands they match). */
    readonly sessionId: string;
    readonly runId: string;
    /** True for a run that answers an interrupt (an approval or a decline). */
    readonly resume: boolean;
    /**
     * The headers the browser sent to CopilotRuntime, lower-cased, as the runtime forwarded them to this agent (by default
     * `authorization` and custom `x-*` headers). They exist so the host can tell WHO is asking and mint the token for that
     * user; they are never sent to AlixAI.
     */
    readonly forwardedHeaders: Readonly<Record<string, string>>;
}
/** Returns a fresh, single-use context token for one gateway call: the bare token, no "Bearer " prefix. */
export type AlixAIContextTokenCallback = (request: AlixAIContextTokenRequest) => string | Promise<string>;
export type AlixAIFetch = (url: string, init: RequestInit) => Promise<Response>;
export interface AlixAIAgentConfig extends Omit<HttpAgentConfig, "url" | "headers" | "fetch"> {
    /** The AlixAI gateway, for example `http://127.0.0.1:8787`. */
    baseUrl: string;
    getContextToken: AlixAIContextTokenCallback;
    /** Where confirmation secrets are kept. Default: in memory, 30 minutes. Server-side only, shared by every clone. */
    secrets?: ConfirmationSecretStore;
    fetch?: AlixAIFetch;
    /** How many times a request that never got an answer is sent again under the same runId. Default 2. */
    maxRetries?: number;
    /** How many times a cut or still-running stream is picked up again from its cursor. Default 3. */
    maxReconnects?: number;
    /** The first delay between attempts in milliseconds, doubled each time up to 5 seconds. Default 200. */
    retryDelayMs?: number;
    /** For tests: replaces the delay between attempts. */
    sleep?: (ms: number, signal: AbortSignal) => Promise<void>;
}
/** The prefix of the id of the confirmation tool call: `alixai_confirm_<preparation id>`. */
export declare const CONFIRM_TOOL_CALL_PREFIX = "alixai_confirm_";
/** The name of the confirmation tool call. */
export declare const CONFIRM_TOOL_NAME = "alixai_confirm_action";
/** What a run answers: which interrupts, and which of them it approves. */
export interface RunAnswer {
    /** Every interrupt (preparation) the run answers. */
    readonly preparationIds: readonly string[];
    /** The preparation the run approves, when it does: the secret travels for this one only. */
    readonly approves: string | undefined;
}
/** True only for exactly `{"approved": true}`: the one payload AlixAI reads as an approval. */
export declare const isApprovalPayload: (payload: unknown) => boolean;
/** Reads which interrupts an AG-UI input answers: a `resume` array, or a tool message for `alixai_confirm_<id>`. */
export declare const answerOf: (input: Pick<RunAgentInput, "resume" | "messages">) => RunAnswer;
/**
 * The body of a run: the AG-UI input with the browser's forwardedProps reduced to what AlixAI reads. Messages, tools,
 * context and state are passed through as they are; the gateway fences them as untrusted data.
 */
export declare const requestBodyFor: (input: RunAgentInput) => RunAgentInput;
export declare class AlixAIAgent extends HttpAgent {
    private settings;
    private active;
    constructor(config: AlixAIAgentConfig);
    /** The runtime clones the agent for every request: the clone shares the settings, and so the secret store, by reference. */
    clone(): AlixAIAgent;
    run(input: RunAgentInput): Observable<BaseEvent>;
    /** CopilotKit's reconnect to a thread: a connect run (no messages, no resume) replays the thread's history and state. */
    protected connect(input: RunAgentInput): Observable<BaseEvent>;
    /**
     * The connect run of one thread, for the runtime's connect route (see AlixAIRunner): history and state come from the
     * gateway, under a context token the host's callback mints for the caller whose headers are given, so the host's own
     * authentication and the gateway's scope checks apply to a page that joins a thread exactly as they do to a run. A thread
     * that does not resolve for the caller ends the stream with an error and no event of any thread. Every call is a new
     * run id: a connect run is never recorded and never replayed.
     */
    connectThread(request: {
        threadId: string;
        headers?: Readonly<Record<string, string>>;
        runId?: string;
    }): Observable<BaseEvent>;
    /** Stops the run locally and asks the gateway to stop it (best effort: the answer for a run already over is ignored). */
    abortRun(): void;
    /** Asks the gateway to cancel the run in flight. Resolves with the gateway's status (`cancelling` or `finished`). */
    cancelRun(): Promise<string | undefined>;
    private cancelRemote;
    /** One gateway request: mints a token, then sends. A failure to send is a TransportFailure; a failed token is not. */
    private send;
    private pump;
    private consume;
    /** The run that prepared an action ends in its interrupt: the secret of its response header is kept for that preparation. */
    private bindSecret;
}
//# sourceMappingURL=agent.d.ts.map