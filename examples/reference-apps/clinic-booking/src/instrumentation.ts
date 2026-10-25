// Counters and one-shot faults for the integration audit and the end-to-end
// gate. Only the test support surface exposes them (the HTTP routes and the
// provisioning tools are not mounted otherwise), so a production-like start
// has no way to inject a fault.

// unavailable: the host reports a failed dependency before it does anything
//   (status `unavailable`, nothing applied).
// error_after_commit: the operation is applied, then the reply is lost (the
//   caller sees a tool error): the case idempotent recovery through status
//   exists for.
export type FaultMode = "unavailable" | "error_after_commit";

export type Instrumentation = {
  recordCall: (tool: string) => void;
  calls: () => Record<string, number>;
  // A fault with tenants is armed for requests of those tenants only: another tenant's request neither
  // triggers it nor consumes it.
  injectFault: (tool: string, mode: FaultMode, times?: number, tenants?: readonly string[]) => void;
  takeFault: (tool: string, tenantId?: string) => FaultMode | undefined;
  clearFaults: () => void;
  pendingFaults: () => { tool: string; mode: FaultMode; remaining: number; tenants?: string[] }[];
  reset: () => void;
};

export const createInstrumentation = (): Instrumentation => {
  const calls = new Map<string, number>();
  const faults = new Map<string, { mode: FaultMode; remaining: number; tenants?: ReadonlySet<string> }[]>();
  return {
    recordCall: (tool) => {
      calls.set(tool, (calls.get(tool) ?? 0) + 1);
    },
    calls: () => Object.fromEntries([...calls.entries()].sort(([a], [b]) => a.localeCompare(b))),
    injectFault: (tool, mode, times = 1, tenants) => {
      faults.set(tool, [
        ...(faults.get(tool) ?? []),
        { mode, remaining: times, ...(tenants === undefined ? {} : { tenants: new Set(tenants) }) },
      ]);
    },
    takeFault: (tool, tenantId) => {
      const queue = faults.get(tool);
      const index = queue?.findIndex((f) => f.tenants === undefined || (tenantId !== undefined && f.tenants.has(tenantId))) ?? -1;
      const head = queue?.[index];
      if (queue === undefined || head === undefined) {
        return undefined;
      }
      head.remaining -= 1;
      if (head.remaining <= 0) {
        queue.splice(index, 1);
      }
      return head.mode;
    },
    clearFaults: () => {
      faults.clear();
    },
    pendingFaults: () =>
      [...faults.entries()].flatMap(([tool, queue]) =>
        queue.map((f) => ({
          tool,
          mode: f.mode,
          remaining: f.remaining,
          ...(f.tenants === undefined ? {} : { tenants: [...f.tenants].sort() }),
        })),
      ),
    reset: () => {
      calls.clear();
      faults.clear();
    },
  };
};
