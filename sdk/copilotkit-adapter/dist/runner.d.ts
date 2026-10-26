import type { BaseEvent } from "@ag-ui/client";
import { AgentRunner } from "@copilotkit/runtime/v2";
import type { AgentRunnerConnectRequest, AgentRunnerIsRunningRequest, AgentRunnerRunRequest, AgentRunnerStopRequest } from "@copilotkit/runtime/v2";
import { Observable } from "rxjs";
import type { AlixAIAgent } from "./agent.js";
export declare class AlixAIRunner extends AgentRunner {
    private readonly agent;
    private readonly active;
    /** The connects in flight, by thread: a flag each, no event. */
    private readonly lateConnectPending;
    /** What waits for the run of a thread to be over: connects to be read again. No event. */
    private readonly lateConnectIdle;
    constructor(agent: AlixAIAgent);
    /** A run, cancelled at the gateway when the browser disconnects before it ends. */
    run(request: AgentRunnerRunRequest): Observable<BaseEvent>;
    /** Starts the run and returns its events; the entry of the thread is dropped when the run ends, however it ends. */
    private track;
    /**
     * The connect run of the thread, through the agent: the caller's headers reach the host's token callback. The answer is held to its
     * end, and read again after the run when one overlapped it (see the head of this file).
     */
    connect(request: AgentRunnerConnectRequest): Observable<BaseEvent>;
    isRunning(request: AgentRunnerIsRunningRequest): Promise<boolean>;
    stop(request: AgentRunnerStopRequest): Promise<boolean | undefined>;
}
//# sourceMappingURL=runner.d.ts.map