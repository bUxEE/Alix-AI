import dgram from "node:dgram";
import dns from "node:dns";
import net from "node:net";

// The network guard of the gate (master plan rule 3 and ADR 0035: no cloud, no telemetry). Every outbound TCP or TLS
// connection of the process (fetch, http, https, WebSocket) goes through net.Socket.prototype.connect, every name lookup
// through dns.lookup (callback and promise forms) and every datagram through dgram. Anything that is not loopback is
// recorded and refused, so a module that tries to phone home fails the test that was running even when the code that
// tried swallowed the refusal.

export interface Attempt {
  readonly kind: "connect" | "lookup" | "datagram";
  readonly target: string;
  readonly allowed: boolean;
}

const KEY = Symbol.for("alixai.copilotkit-gate.network-guard");
interface State {
  installed: boolean;
  attempts: Attempt[];
  total: number;
}
const holder = globalThis as unknown as Record<symbol, State | undefined>;
const state = (): State => (holder[KEY] ??= { installed: false, attempts: [], total: 0 });

export const isLoopbackHost = (host: string | undefined): boolean => {
  if (host === undefined || host === "") return true;
  const bare = host.replace(/^\[|\]$/g, "").toLowerCase();
  return bare === "localhost" || bare === "::1" || bare === "0:0:0:0:0:0:0:1" || bare === "::ffff:127.0.0.1" || /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(bare);
};

export class NetworkGuardError extends Error {
  constructor(target: string) {
    super(`network guard: a connection to ${target} was refused (only loopback is allowed)`);
    this.name = "NetworkGuardError";
  }
}

const record = (kind: Attempt["kind"], target: string): boolean => {
  const allowed = isLoopbackHost(target);
  const s = state();
  s.attempts.push({ kind, target: target === "" ? "localhost" : target, allowed });
  s.total += 1;
  return allowed;
};

// net hands the socket its connect arguments already normalised: one array [options, callback].
const targetOf = (args: unknown[]): { host: string | undefined; unix: boolean } => {
  const first = Array.isArray(args[0]) ? (args[0] as unknown[])[0] : args[0];
  if (typeof first === "object" && first !== null) {
    const options = first as { host?: string; hostname?: string; path?: string; port?: number };
    if (options.path !== undefined && options.port === undefined) return { host: undefined, unix: true };
    return { host: options.host ?? options.hostname, unix: false };
  }
  if (typeof first === "string" && Number.isNaN(Number(first))) return { host: undefined, unix: true };
  return { host: typeof args[1] === "string" ? args[1] : undefined, unix: false };
};

export const installNetworkGuard = (): void => {
  const s = state();
  if (s.installed) return;
  s.installed = true;

  const connect = net.Socket.prototype.connect as (this: net.Socket, ...args: unknown[]) => net.Socket;
  net.Socket.prototype.connect = function guardedConnect(this: net.Socket, ...args: unknown[]): net.Socket {
    const { host, unix } = targetOf(args);
    if (unix) return connect.apply(this, args);
    if (!record("connect", host ?? "")) {
      const error = new NetworkGuardError(host ?? "unknown");
      queueMicrotask(() => this.destroy(error));
      return this;
    }
    return connect.apply(this, args);
  } as typeof net.Socket.prototype.connect;

  const lookup = dns.lookup as (...args: unknown[]) => unknown;
  (dns as { lookup: unknown }).lookup = (...args: unknown[]): unknown => {
    const host = typeof args[0] === "string" ? args[0] : "";
    if (record("lookup", host)) return lookup.apply(dns, args);
    const callback = args[args.length - 1];
    if (typeof callback === "function") queueMicrotask(() => (callback as (error: Error) => void)(new NetworkGuardError(host)));
    return undefined;
  };
  const lookupAsync = dns.promises.lookup as (...args: unknown[]) => Promise<unknown>;
  (dns.promises as { lookup: unknown }).lookup = (...args: unknown[]): Promise<unknown> =>
    record("lookup", typeof args[0] === "string" ? args[0] : "") ? lookupAsync.apply(dns.promises, args) : Promise.reject(new NetworkGuardError(String(args[0])));

  // c-ares resolvers and any direct datagram (DNS over UDP, QUIC) are not covered by the two patches above.
  const send = dgram.Socket.prototype.send as (this: dgram.Socket, ...args: unknown[]) => void;
  dgram.Socket.prototype.send = function guardedSend(this: dgram.Socket, ...args: unknown[]): void {
    const address = args.find((value): value is string => typeof value === "string" && /[.:]/.test(value));
    if (record("datagram", address ?? "")) return send.apply(this, args);
    throw new NetworkGuardError(address ?? "unknown");
  } as typeof dgram.Socket.prototype.send;
};

/** Every attempt so far, allowed or not (a test that wants proof that traffic really happened reads this). */
export const attempts = (): readonly Attempt[] => [...state().attempts];

/** The refused attempts since the last call; they are forgotten once returned. */
export const takeViolations = (): Attempt[] => {
  const s = state();
  const violations = s.attempts.filter((attempt) => !attempt.allowed);
  s.attempts = s.attempts.filter((attempt) => attempt.allowed);
  return violations;
};

/** How many connections, lookups and datagrams the guard has seen in this process, allowed or not. */
export const totalSeen = (): number => state().total;
