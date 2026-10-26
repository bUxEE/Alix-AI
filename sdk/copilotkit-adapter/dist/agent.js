import { EventType, HttpAgent, enforceOutgoingInput } from "@ag-ui/client";
import { EventSchemas } from "@ag-ui/core/schemas";
import { AlixAIError, CONFIRMATION_TOKEN_HEADER, isErrorCode } from "@alixai/sdk";
import { Observable } from "rxjs";
import { createMemorySecretStore } from "./secrets.js";
import { readSseFrames } from "./sse.js";
/** The prefix of the id of the confirmation tool call: `alixai_confirm_<preparation id>`. */
export const CONFIRM_TOOL_CALL_PREFIX = "alixai_confirm_";
/** The name of the confirmation tool call. */
export const CONFIRM_TOOL_NAME = "alixai_confirm_action";
const TOKEN68 = /^[A-Za-z0-9._~+/=-]+$/;
const SECRET_HEADER_VALUE = /^[\x21-\x7e]{1,512}$/;
const defaultSleep = (ms, signal) => new Promise((resolve) => {
    if (signal.aborted) {
        resolve();
        return;
    }
    const done = () => {
        clearTimeout(timer);
        signal.removeEventListener("abort", done);
        resolve();
    };
    const timer = setTimeout(done, ms);
    signal.addEventListener("abort", done, { once: true });
});
const isRecord = (value) => typeof value === "object" && value !== null && !Array.isArray(value);
const statusCode = (status) => {
    if (status === 400 || status === 415 || status === 422)
        return "validation_failed";
    if (status === 401)
        return "unauthenticated";
    if (status === 403)
        return "scope_violation";
    if (status === 409)
        return "conflict";
    if (status === 410)
        return "expired";
    if (status === 428)
        return "confirmation_required";
    if (status === 429)
        return "rate_limited";
    return status >= 500 ? "unavailable" : "internal";
};
const retryAfterMs = (value) => {
    if (value === null)
        return undefined;
    const seconds = Number(value);
    return Number.isFinite(seconds) && seconds >= 0 ? Math.min(seconds * 1000, 30_000) : undefined;
};
/** The typed error of a non-2xx answer of the gateway. Its message is the gateway's own, never the request's. */
const gatewayError = async (response) => {
    let parsed;
    try {
        const text = await response.text();
        parsed = text.length > 64 * 1024 ? undefined : JSON.parse(text);
    }
    catch {
        parsed = undefined;
    }
    const body = isRecord(parsed) && isRecord(parsed["error"]) ? parsed["error"] : parsed;
    const record = isRecord(body) ? body : {};
    const code = isErrorCode(record["code"]) ? record["code"] : statusCode(response.status);
    const message = typeof record["message"] === "string" ? record["message"].slice(0, 500) : `the gateway answered ${response.status}`;
    const details = isRecord(record["details"]) ? record["details"] : undefined;
    const requestId = response.headers.get("X-Request-ID") ?? undefined;
    const wait = retryAfterMs(response.headers.get("Retry-After"));
    return new AlixAIError({
        code,
        message,
        status: response.status,
        ...(typeof record["retryable"] === "boolean" && { retryable: record["retryable"] }),
        ...(details !== undefined && { details }),
        ...(requestId !== undefined && { requestId }),
        ...(wait !== undefined && { retryAfterMs: wait }),
    });
};
const agentError = (code, message, extra = {}) => new AlixAIError({ code, message, ...extra });
/** True only for exactly `{"approved": true}`: the one payload AlixAI reads as an approval. */
export const isApprovalPayload = (payload) => {
    if (!isRecord(payload))
        return false;
    const keys = Object.keys(payload);
    return keys.length === 1 && keys[0] === "approved" && payload["approved"] === true;
};
/** Reads which interrupts an AG-UI input answers: a `resume` array, or a tool message for `alixai_confirm_<id>`. */
export const answerOf = (input) => {
    const resume = input.resume ?? [];
    if (resume.length > 0) {
        let approves;
        for (const entry of resume) {
            if (approves === undefined && entry.status === "resolved" && isApprovalPayload(entry.payload))
                approves = entry.interruptId;
        }
        return { preparationIds: resume.map((entry) => entry.interruptId), approves };
    }
    const last = input.messages[input.messages.length - 1];
    if (last !== undefined && last.role === "tool" && last.toolCallId.startsWith(CONFIRM_TOOL_CALL_PREFIX)) {
        const preparationId = last.toolCallId.slice(CONFIRM_TOOL_CALL_PREFIX.length);
        let payload;
        try {
            payload = typeof last.content === "string" ? JSON.parse(last.content) : undefined;
        }
        catch {
            payload = undefined;
        }
        return { preparationIds: [preparationId], approves: isApprovalPayload(payload) ? preparationId : undefined };
    }
    return { preparationIds: [], approves: undefined };
};
/**
 * What the browser may steer in forwardedProps: the choice that answers a clarification and the language. Everything
 * else is dropped (the gateway ignores it anyway, and a browser-supplied object has no business reaching it).
 */
const forwardedPropsFor = (value) => {
    if (!isRecord(value) || !isRecord(value["alixai"]))
        return {};
    const alixai = {};
    const { choiceId, language } = value["alixai"];
    if (typeof choiceId === "string")
        alixai["choiceId"] = choiceId;
    if (typeof language === "string")
        alixai["language"] = language;
    return Object.keys(alixai).length === 0 ? {} : { alixai };
};
/**
 * The body of a run: the AG-UI input with the browser's forwardedProps reduced to what AlixAI reads. Messages, tools,
 * context and state are passed through as they are; the gateway fences them as untrusted data.
 */
export const requestBodyFor = (input) => enforceOutgoingInput({ ...input, forwardedProps: forwardedPropsFor(input.forwardedProps) });
const abortReason = (signal) => signal.reason instanceof Error ? signal.reason : new DOMException("The run was aborted", "AbortError");
class TransportFailure extends Error {
    constructor(cause) {
        super("transport failure", { cause });
    }
}
export class AlixAIAgent extends HttpAgent {
    settings;
    active;
    constructor(config) {
        const { baseUrl, getContextToken, secrets, fetch: fetchImpl, maxRetries, maxReconnects, retryDelayMs, sleep, ...agentConfig } = config;
        const base = normaliseBaseUrl(baseUrl);
        const http = fetchImpl ?? ((url, init) => fetch(url, init));
        super({ ...agentConfig, url: `${base}/v1/agui`, headers: {}, fetch: http });
        this.settings = {
            baseUrl: base,
            getContextToken,
            secrets: secrets ?? createMemorySecretStore(),
            fetch: http,
            maxRetries: maxRetries ?? 2,
            maxReconnects: maxReconnects ?? 3,
            retryDelayMs: retryDelayMs ?? 200,
            sleep: sleep ?? defaultSleep,
        };
    }
    /** The runtime clones the agent for every request: the clone shares the settings, and so the secret store, by reference. */
    clone() {
        const copy = super.clone();
        copy.settings = this.settings;
        copy.active = undefined;
        return copy;
    }
    run(input) {
        return new Observable((subscriber) => {
            const own = new AbortController();
            const outer = this.abortController.signal;
            const forward = () => own.abort(outer.reason);
            if (outer.aborted)
                own.abort(outer.reason);
            else
                outer.addEventListener("abort", forward, { once: true });
            this.pump(input, own.signal, subscriber).then(() => subscriber.complete(), (error) => subscriber.error(own.signal.aborted ? abortReason(own.signal) : error));
            return () => {
                outer.removeEventListener("abort", forward);
                own.abort();
            };
        });
    }
    /** CopilotKit's reconnect to a thread: a connect run (no messages, no resume) replays the thread's history and state. */
    connect(input) {
        return this.run({ ...input, messages: [], resume: undefined });
    }
    /**
     * The connect run of one thread, for the runtime's connect route (see AlixAIRunner): history and state come from the
     * gateway, under a context token the host's callback mints for the caller whose headers are given, so the host's own
     * authentication and the gateway's scope checks apply to a page that joins a thread exactly as they do to a run. A thread
     * that does not resolve for the caller ends the stream with an error and no event of any thread. Every call is a new
     * run id: a connect run is never recorded and never replayed.
     */
    connectThread(request) {
        const copy = this.clone();
        copy.threadId = request.threadId;
        copy.headers = { ...request.headers };
        return copy.connect({
            threadId: request.threadId,
            runId: request.runId ?? crypto.randomUUID(),
            state: {},
            messages: [],
            tools: [],
            context: [],
            forwardedProps: {},
        });
    }
    /** Stops the run locally and asks the gateway to stop it (best effort: the answer for a run already over is ignored). */
    abortRun() {
        const active = this.active;
        super.abortRun();
        if (active !== undefined)
            void this.cancelRemote(active).catch(() => undefined);
    }
    /** Asks the gateway to cancel the run in flight. Resolves with the gateway's status (`cancelling` or `finished`). */
    async cancelRun() {
        const active = this.active;
        return active === undefined ? undefined : this.cancelRemote(active);
    }
    async cancelRemote(run) {
        const signal = AbortSignal.timeout(10_000);
        const response = await this.send("POST", `/v1/agui/threads/${encodeURIComponent(run.threadId)}/runs/${encodeURIComponent(run.runId)}/cancel`, { Accept: "application/json" }, undefined, { operation: "agui.cancel", sessionId: run.threadId, runId: run.runId, resume: false }, signal);
        if (!response.ok)
            return undefined;
        const body = await response.json().catch(() => undefined);
        return isRecord(body) && typeof body["status"] === "string" ? body["status"] : undefined;
    }
    /** One gateway request: mints a token, then sends. A failure to send is a TransportFailure; a failed token is not. */
    async send(method, path, headers, body, token, signal) {
        const settings = this.settings;
        let raw;
        try {
            raw = await settings.getContextToken({ ...token, forwardedHeaders: forwardedHeadersOf(this.headers) });
        }
        catch (cause) {
            throw agentError("unauthenticated", "getContextToken failed, so the request was not sent", {
                details: { reason: "token_provider_failed" },
                cause,
            });
        }
        if (typeof raw !== "string" || raw.length === 0 || raw.length > 16 * 1024 || !TOKEN68.test(raw)) {
            throw agentError("unauthenticated", "getContextToken must return the bare context token (no whitespace, no \"Bearer\" prefix)", {
                details: { reason: "bad_token_shape" },
            });
        }
        if (signal.aborted)
            throw abortReason(signal);
        try {
            return await settings.fetch(`${settings.baseUrl}${path}`, {
                method,
                // Authorization is set last: nothing the browser forwarded can replace or precede it.
                headers: { ...headers, Authorization: `Bearer ${raw}` },
                ...(body !== undefined && { body }),
                signal,
                redirect: "error",
                credentials: "omit",
                cache: "no-store",
            });
        }
        catch (cause) {
            if (signal.aborted)
                throw abortReason(signal);
            throw new TransportFailure(cause);
        }
    }
    async pump(input, signal, out) {
        const settings = this.settings;
        const body = requestBodyFor(input);
        const { threadId, runId } = body;
        if (threadId.length === 0 || runId.length === 0)
            throw agentError("validation_failed", "the run needs a threadId and a runId");
        const answer = answerOf(body);
        const resume = answer.preparationIds.length > 0;
        // The gateway's rule (docs/integration/agui.md section 3): no message and no resume is a connect run.
        const operation = !resume && body.messages.length === 0 ? "agui.connect" : "agui.run";
        // An approval without the secret is refused before a context token is spent: only the run that prepared the action
        // returned it, and only this server side holds it.
        let secret;
        if (answer.approves !== undefined) {
            secret = await settings.secrets.get({ threadId, preparationId: answer.approves });
            if (secret === undefined) {
                throw agentError("confirmation_required", "no confirmation secret is held for this thread and preparation; start the action again", {
                    details: { reason: "confirmation_secret_missing" },
                });
            }
        }
        this.active = { threadId, runId };
        const pump = { cursor: undefined, since: [], terminal: false, runSecret: undefined };
        let mode = "post";
        let retries = 0;
        let reconnects = 0;
        const payload = JSON.stringify(body);
        const backoff = async (error, attempt) => {
            await settings.sleep(error?.retryAfterMs ?? Math.min(settings.retryDelayMs * 2 ** (attempt - 1), 5000), signal);
            if (signal.aborted)
                throw abortReason(signal);
        };
        // One more attempt in the current mode, when there is budget for it.
        const again = async (error) => {
            if (mode === "post" ? retries >= settings.maxRetries : reconnects >= settings.maxReconnects)
                return false;
            if (mode === "post")
                retries += 1;
            else
                reconnects += 1;
            await backoff(error, mode === "post" ? retries : reconnects);
            return true;
        };
        // The gateway has seen the approval: the secret is used up whatever the outcome of the run.
        const settle = async () => {
            for (const preparationId of answer.preparationIds)
                await settings.secrets.delete({ threadId, preparationId });
        };
        for (;;) {
            if (signal.aborted)
                throw abortReason(signal);
            let response;
            try {
                response =
                    mode === "post"
                        ? await this.send("POST", "/v1/agui", {
                            "Content-Type": "application/json",
                            Accept: "text/event-stream",
                            ...(secret !== undefined && { [CONFIRMATION_TOKEN_HEADER]: secret }),
                        }, payload, { operation, sessionId: threadId, runId, resume }, signal)
                        : await this.send("GET", `/v1/agui/threads/${encodeURIComponent(threadId)}/runs/${encodeURIComponent(runId)}/events`, { Accept: "text/event-stream", ...(pump.cursor !== undefined && { "Last-Event-ID": pump.cursor }) }, undefined, { operation: "agui.events", sessionId: threadId, runId, resume: false }, signal);
            }
            catch (error) {
                if (!(error instanceof TransportFailure))
                    throw error;
                // The request may or may not have reached the gateway. The same runId is safe to send again.
                if (await again(undefined))
                    continue;
                throw agentError("uncertain_outcome", "the AlixAI gateway could not be reached; read the run's state before sending it again", {
                    details: { phase: "transport" },
                    cause: error.cause,
                });
            }
            if (response.status === 204 && mode === "events") {
                throw agentError("internal", "the gateway says the client has every event, but the run never ended", { details: { reason: "no_terminal_event" } });
            }
            if (!response.ok) {
                const failure = await gatewayError(response);
                if (failure.status === 409 && failure.details?.["reason"] === "run_in_progress") {
                    // The same run is still being answered: follow it from its start.
                    mode = "events";
                    if (await again(failure))
                        continue;
                    throw failure;
                }
                // Used up: already decided (409) or expired (410). Not on 403: the thread or interrupt did not resolve for this
                // caller, so the preparation is still pending for its owner and a foreign request must not burn its secret.
                if (response.status === 409 || response.status === 410)
                    await settle();
                if (failure.retryable && (await again(failure)))
                    continue;
                throw failure;
            }
            if (mode === "post") {
                await settle();
                const header = response.headers.get(CONFIRMATION_TOKEN_HEADER);
                if (header !== null && SECRET_HEADER_VALUE.test(header))
                    pump.runSecret = header;
            }
            const contentType = (response.headers.get("Content-Type") ?? "").toLowerCase();
            if (!contentType.includes("text/event-stream") || response.body === null) {
                throw agentError("internal", "the gateway did not answer with an event stream", { details: { reason: "not_an_event_stream" } });
            }
            await this.consume(response.body, pump, out, { threadId, signal });
            if (signal.aborted)
                throw abortReason(signal);
            if (pump.terminal)
                return;
            // The stream ended without RUN_FINISHED or RUN_ERROR: pick it up again from the cursor.
            mode = "events";
            if (!(await again(undefined))) {
                throw agentError("uncertain_outcome", "the event stream was cut and could not be resumed", { details: { phase: "stream" } });
            }
        }
    }
    async consume(stream, pump, out, run) {
        // A reconnect starts at the cursor: the frames already delivered since then come first and are checked, not repeated.
        let skip = pump.since.length;
        let skipped = 0;
        const frames = readSseFrames(stream, run.signal);
        try {
            for (;;) {
                let next;
                try {
                    next = await frames.next();
                }
                catch {
                    // A body that broke in the middle is a cut stream, not a failed run: the caller reconnects from the cursor.
                    if (run.signal.aborted)
                        throw abortReason(run.signal);
                    return;
                }
                if (next.done === true)
                    return;
                const frame = next.value;
                if (skipped < skip) {
                    if (frame.data !== pump.since[skipped]) {
                        throw agentError("internal", "the replayed stream differs from the events already delivered", { details: { reason: "replay_mismatch" } });
                    }
                    skipped += 1;
                    continue;
                }
                if (pump.runSecret !== undefined && frame.data.includes(pump.runSecret)) {
                    // Defence in depth: the gateway never puts the secret in an event; if one did, nothing of it goes on.
                    throw agentError("internal", "an event carried the confirmation secret and was withheld", { details: { reason: "secret_leak_blocked" } });
                }
                let event;
                try {
                    event = EventSchemas.parse(JSON.parse(frame.data));
                }
                catch (cause) {
                    throw agentError("internal", "the gateway sent an event that is not valid AG-UI", { details: { reason: "invalid_event" }, cause });
                }
                if (event.type === EventType.RUN_FINISHED)
                    await this.bindSecret(event, pump, run.threadId);
                out.next(event);
                if (frame.id !== undefined) {
                    pump.cursor = frame.id;
                    pump.since = [];
                    skip = 0;
                    skipped = 0;
                }
                else {
                    pump.since.push(frame.data);
                }
                if (event.type === EventType.RUN_FINISHED || event.type === EventType.RUN_ERROR) {
                    pump.terminal = true;
                    return;
                }
            }
        }
        finally {
            await frames.return(undefined);
        }
    }
    /** The run that prepared an action ends in its interrupt: the secret of its response header is kept for that preparation. */
    async bindSecret(event, pump, threadId) {
        if (pump.runSecret === undefined)
            return;
        const outcome = event.outcome;
        if (outcome?.type !== "interrupt" || !Array.isArray(outcome.interrupts))
            return;
        for (const interrupt of outcome.interrupts) {
            if (typeof interrupt.id === "string" && interrupt.reason === "confirmation_required") {
                await this.settings.secrets.put({ threadId, preparationId: interrupt.id }, pump.runSecret);
            }
        }
    }
}
const normaliseBaseUrl = (value) => {
    let url;
    try {
        url = new URL(value);
    }
    catch {
        throw new TypeError("baseUrl must be an absolute http(s) URL");
    }
    if (url.protocol !== "http:" && url.protocol !== "https:")
        throw new TypeError("baseUrl must be an http(s) URL");
    if (url.username !== "" || url.password !== "")
        throw new TypeError("baseUrl must not carry credentials");
    return url.toString().replace(/\/+$/, "");
};
const forwardedHeadersOf = (headers) => {
    const copy = {};
    for (const [name, value] of Object.entries(headers ?? {}))
        copy[name.toLowerCase()] = value;
    return Object.freeze(copy);
};
//# sourceMappingURL=agent.js.map