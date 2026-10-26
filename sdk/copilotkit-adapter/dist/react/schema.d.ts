export interface GuardSchema<T> {
    readonly "~standard": {
        readonly version: 1;
        readonly vendor: "alixai";
        readonly validate: (value: unknown) => {
            readonly value: T;
        } | {
            readonly issues: ReadonlyArray<{
                readonly message: string;
            }>;
        };
    };
}
export declare const guardSchema: <T>(guard: (value: unknown) => T | undefined, what: string) => GuardSchema<T>;
/** A schema that accepts any value, for a tool call whose arguments are only read by the card. */
export declare const anySchema: GuardSchema<unknown>;
//# sourceMappingURL=schema.d.ts.map