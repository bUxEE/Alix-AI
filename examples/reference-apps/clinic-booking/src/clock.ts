// Time is injected so every test is deterministic and every freshness
// timestamp comes from one place.

export type Clock = {
  now: () => Date;
};

export const systemClock: Clock = { now: () => new Date() };

export type MutableClock = Clock & {
  set: (iso: string) => void;
  advanceSeconds: (seconds: number) => void;
};

export const createMutableClock = (startIso: string): MutableClock => {
  let current = new Date(startIso).getTime();
  return {
    now: () => new Date(current),
    set: (iso) => {
      current = new Date(iso).getTime();
    },
    advanceSeconds: (seconds) => {
      current += seconds * 1000;
    },
  };
};

// The real clock plus an offset the test support surface can move forward, so
// expiry can be exercised without waiting.
export type OffsetClock = Clock & { advanceSeconds: (seconds: number) => void; offsetSeconds: () => number };

export const createOffsetClock = (): OffsetClock => {
  let offsetMs = 0;
  return {
    now: () => new Date(Date.now() + offsetMs),
    advanceSeconds: (seconds) => {
      offsetMs += seconds * 1000;
    },
    offsetSeconds: () => Math.round(offsetMs / 1000),
  };
};

// RFC 3339 UTC with a literal Z and no fractional seconds, the one spelling
// the DeterministicResult timestamp grammar accepts.
export const toTimestamp = (date: Date): string => date.toISOString().replace(/\.\d{3}Z$/, "Z");
