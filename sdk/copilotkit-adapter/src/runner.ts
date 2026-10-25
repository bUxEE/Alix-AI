import { EventType } from "@ag-ui/client";
import type { BaseEvent, RunAgentInput } from "@ag-ui/client";
import { AlixAIError } from "@alixai/sdk";
import { AgentRunner, createRunEventFinalizer } from "@copilotkit/runtime/v2";
import type { AgentRunnerConnectRequest, AgentRunnerIsRunningRequest, AgentRunnerRunRequest, AgentRunnerStopRequest } from "@copilotkit/runtime/v2";
import { Observable, ReplaySubject } from "rxjs";
import type { Subscription } from "rxjs";
import type { AlixAIAgent } from "./agent.js";

// The runner CopilotRuntime uses for AlixAI. CopilotRuntime's own default (InMemoryAgentRunner) writes every run it serves into
// a process-wide store and answers the browser's `connect` from it, so a request that names a thread read that thread's events
// (the confirmation card's effects included) without the agent, the host's token callback or the gateway being asked; a
// restarted host showed an empty history; and its local thread routes (`GET /threads`, `/threads/:id/messages|events|state`,
// `POST /threads/clear`) listed, served and wiped every thread of the process. Because that store is shared by EVERY
// InMemoryAgentRunner in the process, merely delegating to one would still let any other CopilotRuntime of the same process
// replay AlixAI threads. So this runner does not use it:
//
//   * `connect` is delegated to the agent: the host's callback mints a token for the caller whose headers came with the
//     request, the gateway's connect run answers from the AlixAI session, and the gateway's scope check decides. No store of
//     its own, no fallback: when the callback or the gateway refuses, the page gets one RUN_ERROR and no event of any thread.
//   * it keeps nothing of a run once the run is over. Only the runs in flight are tracked (to stop them), and only by thread:
//     the agent and the run id, no event. Nothing it holds can be read through any route.
//   * it does not advertise CopilotRuntime's local thread endpoints.
//   * a browser that goes away while a run is streaming cancels the gateway run, as the runtime's stop route does.
//
//   * a connect answer is never applied on top of a run it overlaps. The answer replays history and state (a MESSAGES_SNAPSHOT and a
//     state event replace what the page holds), and the page that sends in the instant after it loads, before its join was
//     answered, has a run going while its connect is read: that read predates the run, so applying it would wipe the run's messages.
//     A connect that overlaps a run in flight on its thread (the run was going when the connect came, or started while it was
//     pending) is therefore held until the run is over and then read again, with a fresh context token from the host's callback and
//     the gateway's scope check, so the page gets the history that includes the run. A connect with no overlapping run (a reload, a
//     second tab, a reconnect) is answered at once and replays as before, and the answer is only ever the connect's own, for its
//     own thread.
//
// The run itself is what the in-memory runner did (one run per thread at a time, the browser's input echoed in RUN_STARTED,
// open messages and tool calls closed and a terminal event added when a run is stopped or ends abruptly), so the browser's
// client sees the same stream. The runtime's `stop` and `info` routes do not pass the caller to a runner: the host
// authenticates the whole route.

const TERMINAL: ReadonlySet<string> = new Set([EventType.RUN_FINISHED, EventType.RUN_ERROR]);

/** What a refused or failed connect says to the page: the typed error's own curated message, nothing else. */
const connectFailure = (error: unknown): { message: string; code: string } =>
  error instanceof AlixAIError ? { message: error.message, code: error.code } : { message: "the thread could not be loaded", code: "internal" };

/** The events of a connect answer that are not history or state: they do not replace anything the page holds. */
const LATE_CONNECT_KEEPS: ReadonlySet<string> = new Set([EventType.RUN_STARTED, EventType.RUN_FINISHED, EventType.RUN_ERROR]);

/** How many times a connect is read again because a run overlapped it. Past that it is answered without history or state. */
const LATE_CONNECT_MAX_READS = 3;

/** A connect in flight. `overlapped` flips when a run is, or becomes, in flight on its thread. Dropped when the connect ends. */
interface PendingConnect {
  overlapped: boolean;
}

/** A run in flight on a thread. Dropped when the run ends. */
interface ActiveRun {
  readonly runId: string;
  readonly agent: AgentRunnerRunRequest["agent"];
  stopRequested: boolean;
}

export class AlixAIRunner extends AgentRunner {
  private readonly active = new Map<string, ActiveRun>();
  /** The connects in flight, by thread: a flag each, no event. */
  private readonly lateConnectPending = new Map<string, Set<PendingConnect>>();
  /** What waits for the run of a thread to be over: connects to be read again. No event. */
  private readonly lateConnectIdle = new Map<string, Set<() => void>>();

  constructor(private readonly agent: AlixAIAgent) {
    super();
  }

  /** A run, cancelled at the gateway when the browser disconnects before it ends. */
  run(request: AgentRunnerRunRequest): Observable<BaseEvent> {
    const source = this.track(request);
    const { threadId } = request;
    const { runId } = request.input;
    return new Observable<BaseEvent>((subscriber) => {
      let over = false;
      const subscription = source.subscribe({
        next: (event) => {
          if (TERMINAL.has(event.type)) over = true;
          subscriber.next(event);
        },
        error: (error: unknown) => {
          over = true;
          subscriber.error(error);
        },
        complete: () => {
          over = true;
          subscriber.complete();
        },
      });
      // Teardown also runs after a normal end; only a subscriber that leaves a run still in flight (the response was closed
      // by the browser) stops it. Stop aborts the agent's run, which asks the gateway to cancel it.
      return () => {
        subscription.unsubscribe();
        if (!over) void this.stop({ threadId, runId }).catch(() => undefined);
      };
    });
  }

  /** Starts the run and returns its events; the entry of the thread is dropped when the run ends, however it ends. */
  private track(request: AgentRunnerRunRequest): Observable<BaseEvent> {
    const { threadId, agent } = request;
    const input: RunAgentInput = request.input;
    if (this.active.has(threadId)) throw new Error("Thread already running");
    const entry: ActiveRun = { runId: input.runId, agent, stopRequested: false };
    this.active.set(threadId, entry);
    for (const pending of this.lateConnectPending.get(threadId) ?? []) pending.overlapped = true;

    const subject = new ReplaySubject<BaseEvent>(Infinity);
    const finalizer = createRunEventFinalizer();
    let started = false;
    const emit = (event: BaseEvent): void => {
      finalizer.observe(event);
      subject.next(event);
    };
    const finish = (interruptionMessage?: string): void => {
      if (entry.stopRequested && !started) emit({ type: EventType.RUN_STARTED, threadId, runId: input.runId, input: { ...input } } as BaseEvent);
      for (const event of finalizer.finalize({
        stopRequested: entry.stopRequested,
        ...(input.protocolVersion !== undefined && { protocolVersion: input.protocolVersion }),
        ...(interruptionMessage !== undefined && { interruptionMessage }),
      })) {
        subject.next(event as BaseEvent);
      }
      if (this.active.get(threadId) === entry) {
        this.active.delete(threadId);
        const waiting = this.lateConnectIdle.get(threadId);
        this.lateConnectIdle.delete(threadId);
        for (const resume of waiting ?? []) resume();
      }
      subject.complete();
    };

    agent
      .runAgent(input, {
        onEvent: ({ event }) => {
          if (event.type === EventType.RUN_STARTED) {
            started = true;
            // The runtime has always echoed the browser's own request into RUN_STARTED; the gateway never sends one.
            const echoed = event as BaseEvent & { input?: unknown };
            if (echoed.input === undefined) echoed.input = { ...input };
          }
          emit(event);
        },
      })
      .then(
        () => finish(),
        (error: unknown) => finish(error instanceof Error ? error.message : String(error)),
      );
    return subject.asObservable();
  }

  /**
   * The connect run of the thread, through the agent: the caller's headers reach the host's token callback. The answer is held to its
   * end, and read again after the run when one overlapped it (see the head of this file).
   */
  connect(request: AgentRunnerConnectRequest): Observable<BaseEvent> {
    const { threadId } = request;
    return new Observable<BaseEvent>((subscriber) => {
      const pending: PendingConnect = { overlapped: false };
      const pendingOfThread = this.lateConnectPending.get(threadId) ?? new Set<PendingConnect>();
      pendingOfThread.add(pending);
      this.lateConnectPending.set(threadId, pendingOfThread);
      let subscription: Subscription | undefined;
      let resume: (() => void) | undefined;
      const release = (): void => {
        pendingOfThread.delete(pending);
        if (pendingOfThread.size === 0 && this.lateConnectPending.get(threadId) === pendingOfThread) this.lateConnectPending.delete(threadId);
        if (resume !== undefined) {
          const waiting = this.lateConnectIdle.get(threadId);
          waiting?.delete(resume);
          if (waiting?.size === 0) this.lateConnectIdle.delete(threadId);
        }
        resume = undefined;
      };
      const emit = (events: readonly BaseEvent[], overlapped: boolean): void => {
        for (const event of events) if (!overlapped || LATE_CONNECT_KEEPS.has(event.type)) subscriber.next(event);
        release();
        subscriber.complete();
      };
      const read = (reads: number): void => {
        const runId = crypto.randomUUID();
        const events: BaseEvent[] = [];
        let started = false;
        pending.overlapped = this.active.has(threadId);
        subscription = this.agent.connectThread({ threadId, runId, headers: request.headers ?? {} }).subscribe({
          next: (event) => {
            if (event.type === EventType.RUN_STARTED) started = true;
            events.push(event);
          },
          error: (error: unknown) => {
            if (!started) events.push({ type: EventType.RUN_STARTED, threadId, runId } as BaseEvent);
            events.push({ type: EventType.RUN_ERROR, ...connectFailure(error) } as BaseEvent);
            emit(events.filter((event) => LATE_CONNECT_KEEPS.has(event.type)), false);
          },
          complete: () => {
            if (!pending.overlapped) return emit(events, false);
            if (reads >= LATE_CONNECT_MAX_READS) return emit(events, true);
            // A run overlapped this read: it predates the run, so it is dropped, and the connect is read again once the run is over.
            const again = (): void => {
              resume = undefined;
              read(reads + 1);
            };
            if (!this.active.has(threadId)) return again();
            resume = again;
            const waiting = this.lateConnectIdle.get(threadId) ?? new Set<() => void>();
            waiting.add(again);
            this.lateConnectIdle.set(threadId, waiting);
          },
        });
      };
      read(1);
      return () => {
        release();
        subscription?.unsubscribe();
      };
    });
  }

  isRunning(request: AgentRunnerIsRunningRequest): Promise<boolean> {
    const entry = this.active.get(request.threadId);
    return Promise.resolve(entry !== undefined && !entry.stopRequested);
  }

  stop(request: AgentRunnerStopRequest): Promise<boolean | undefined> {
    const entry = this.active.get(request.threadId);
    if (entry === undefined || entry.stopRequested) return Promise.resolve(false);
    if (request.runId !== undefined && entry.runId !== request.runId) return Promise.resolve(false);
    entry.stopRequested = true;
    try {
      entry.agent.abortRun();
      return Promise.resolve(true);
    } catch {
      entry.stopRequested = false;
      return Promise.resolve(false);
    }
  }
}
