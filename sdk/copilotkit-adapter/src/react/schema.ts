// CopilotKit asks activity renderers and tool renderers for a Standard Schema of their payload. The payloads here come
// from a stream, so each schema is a guard from ../activities.ts: a payload that is not what AlixAI sends is refused
// (the renderer shows nothing) instead of being rendered on trust.

export interface GuardSchema<T> {
  readonly "~standard": {
    readonly version: 1;
    readonly vendor: "alixai";
    readonly validate: (value: unknown) => { readonly value: T } | { readonly issues: ReadonlyArray<{ readonly message: string }> };
  };
}

export const guardSchema = <T>(guard: (value: unknown) => T | undefined, what: string): GuardSchema<T> => ({
  "~standard": {
    version: 1,
    vendor: "alixai",
    validate: (value) => {
      const parsed = guard(value);
      return parsed === undefined ? { issues: [{ message: `not an AlixAI ${what}` }] } : { value: parsed };
    },
  },
});

/** A schema that accepts any value, for a tool call whose arguments are only read by the card. */
export const anySchema: GuardSchema<unknown> = guardSchema((value) => value, "value");
