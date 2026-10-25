import dns from "node:dns";
import net from "node:net";

// The network guard of the adapter suite (ADR 0035: no cloud, no telemetry). Every outbound connection of the process goes
// through net.Socket.prototype.connect (fetch, http, https, tls and WebSocket all do) and every name lookup through
// dns.lookup. Anything that is not loopback is recorded and refused, so a CopilotKit module that tries to phone home
// fails the test that triggered it even if its own code swallows the error.

export interface Attempt {
  readonly kind: "connect" | "lookup";
  readonly target: string;
  readonly allowed: boolean;
}

interface GuardState {
  installed: boolean;
  attempts: Attempt[];
}

const KEY = Symbol.for("alixai.copilotkit-adapter.network-guard");
const holder = globalThis as unknown as Record<symbol, GuardState | undefined>;

const state = (): GuardState => (holder[KEY] ??= { installed: false, attempts: [] });

export const isLoopbackHost = (host: string | undefined): boolean => {
  if (host === undefined || host === "") return true;
  const bare = host.replace(/^\[|\]$/g, "").toLowerCase();
  return bare === "localhost" || bare === "::1" || bare === "0:0:0:0:0:0:0:1" || /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(bare) || bare === "::ffff:127.0.0.1";
};

const hostOfConnectArgs = (args: unknown[]): { host: string | undefined; unix: boolean } => {
  // net.connect hands the socket its arguments already normalised, as one array: [options, callback].
  const first = Array.isArray(args[0]) ? (args[0] as unknown[])[0] : args[0];
  if (Array.isArray(args[0]) && (typeof first !== "object" || first === null)) return { host: undefined, unix: false };
  if (typeof first === "object" && first !== null) {
    const options = first as { host?: string; hostname?: string; path?: string; port?: number };
    if (options.path !== undefined && options.port === undefined) return { host: undefined, unix: true };
    return { host: options.host ?? options.hostname, unix: false };
  }
  if (typeof first === "string" && Number.isNaN(Number(first))) return { host: undefined, unix: true };
  return { host: typeof args[1] === "string" ? args[1] : undefined, unix: false };
};

export class NetworkGuardError extends Error {
  constructor(target: string) {
    super(`network guard: a connection to ${target} was refused (only loopback is allowed)`);
    this.name = "NetworkGuardError";
  }
}

export const installNetworkGuard = (): void => {
  const s = state();
  if (s.installed) return;
  s.installed = true;

  const connect = net.Socket.prototype.connect as (this: net.Socket, ...args: unknown[]) => net.Socket;
  net.Socket.prototype.connect = function patchedConnect(this: net.Socket, ...args: unknown[]): net.Socket {
    const { host, unix } = hostOfConnectArgs(args);
    if (unix) return connect.apply(this, args);
    const allowed = isLoopbackHost(host);
    s.attempts.push({ kind: "connect", target: host ?? "localhost", allowed });
    if (!allowed) {
      const error = new NetworkGuardError(host ?? "unknown");
      queueMicrotask(() => this.destroy(error));
      return this;
    }
    return connect.apply(this, args);
  } as typeof net.Socket.prototype.connect;

  const lookup = dns.lookup as (...args: unknown[]) => unknown;
  (dns as { lookup: unknown }).lookup = (...args: unknown[]): unknown => {
    const name = args[0];
    const host = typeof name === "string" ? name : undefined;
    const allowed = isLoopbackHost(host);
    s.attempts.push({ kind: "lookup", target: host ?? "", allowed });
    if (!allowed) {
      const callback = args[args.length - 1];
      const error = new NetworkGuardError(host ?? "unknown");
      if (typeof callback === "function") queueMicrotask(() => (callback as (error: Error) => void)(error));
      return undefined;
    }
    return lookup.apply(dns, args);
  };
};

/** Every attempt so far, allowed or not. */
export const attempts = (): readonly Attempt[] => [...state().attempts];

/** Returns the refused attempts since the last call and forgets them. */
export const takeViolations = (): Attempt[] => {
  const s = state();
  const violations = s.attempts.filter((attempt) => !attempt.allowed);
  s.attempts = s.attempts.filter((attempt) => attempt.allowed);
  return violations;
};

/** Forgets every attempt; used between tests. */
export const resetAttempts = (): void => {
  state().attempts = [];
};
