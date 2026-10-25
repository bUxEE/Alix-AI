// One run as the gateway streams it: AG-UI events grouped by the TurnEvent they were mapped from. The last event of each group
// carries the SSE `id:` (the TurnEvent's seq, the resume cursor); the others carry none (docs/integration/agui.md section 2,
// "Ids, determinism and the cursor"). Each group may wait a little before it is written, so text streams in fragments.

export interface AguiEvent {
  readonly type: string;
  readonly [member: string]: unknown;
}

export interface Frame {
  /** The SSE id: present on the last event of a group. */
  readonly id: string | undefined;
  readonly event: AguiEvent;
  /** Milliseconds to wait before writing this event (set on the first event of a group). */
  readonly delayMs: number;
}

export interface TurnBuilder {
  /** Adds events to the open group (they are sent with the next committed one). */
  add: (...events: AguiEvent[]) => void;
  /** Closes the open group: its last event gets the next seq as its id. `delayMs` is waited before the group is written. */
  commit: (delayMs?: number) => void;
  /** A pipeline stage: STEP_STARTED, whatever `inside` adds, STEP_FINISHED (the end of the stage's TurnEvent). */
  step: (name: string, delayMs: number, inside?: () => void) => void;
  /** An assistant message streamed in fragments: START, one CONTENT per fragment (each its own TurnEvent), END left open. */
  text: (messageId: string, text: string) => void;
  /** A finished snapshot of an activity, committed as one TurnEvent. */
  activity: (messageId: string, activityType: string, content: unknown, delayMs?: number) => void;
  /** The frames so far. */
  frames: () => Frame[];
  /** The assistant text of the run, for the thread's history. */
  transcript: () => string;
}

export interface TurnOptions {
  /** Multiplies every delay: 1 is realistic, 0 sends everything at once (tests). */
  pace: number;
}

// Fragments of two or three words, as a small model emits them.
const fragments = (text: string): string[] => {
  const words = text.match(/\S+\s*|\s+/g) ?? [];
  const out: string[] = [];
  for (let i = 0; i < words.length; i += 2) out.push(words.slice(i, i + 2).join(""));
  return out.filter((fragment) => fragment !== "");
};

export const createTurn = (options: TurnOptions): TurnBuilder => {
  const out: Frame[] = [];
  let open: AguiEvent[] = [];
  let seq = 0;
  const texts: string[] = [];

  const ms = (value: number): number => Math.round(value * options.pace);

  const commit = (delayMs = 0): void => {
    if (open.length === 0) return;
    const group = open;
    open = [];
    const id = String(seq);
    seq += 1;
    group.forEach((event, index) => {
      out.push({ id: index === group.length - 1 ? id : undefined, event, delayMs: index === 0 ? ms(delayMs) : 0 });
    });
  };

  const add = (...events: AguiEvent[]): void => {
    open.push(...events);
  };

  const step = (name: string, delayMs: number, inside?: () => void): void => {
    add({ type: "STEP_STARTED", stepName: name });
    inside?.();
    add({ type: "STEP_FINISHED", stepName: name });
    commit(delayMs);
  };

  const text = (messageId: string, body: string): void => {
    texts.push(body);
    add({ type: "TEXT_MESSAGE_START", messageId, role: "assistant" });
    fragments(body).forEach((fragment, index) => {
      add({ type: "TEXT_MESSAGE_CONTENT", messageId, delta: fragment });
      // A little irregular, like real decoding: a longer pause after the end of a sentence or a line.
      const pause = /[.!?:]\s*$|\n\s*$/.test(fragment) ? 90 : 28 + ((index * 7) % 23);
      commit(index === 0 ? 180 : pause);
    });
    add({ type: "TEXT_MESSAGE_END", messageId });
  };

  const activity = (messageId: string, activityType: string, content: unknown, delayMs = 120): void => {
    add({ type: "ACTIVITY_SNAPSHOT", messageId, activityType, content });
    commit(delayMs);
  };

  return {
    add,
    commit,
    step,
    text,
    activity,
    frames: () => [...out, ...open.map((event): Frame => ({ id: undefined, event, delayMs: 0 }))],
    transcript: () => texts.join("\n\n"),
  };
};
