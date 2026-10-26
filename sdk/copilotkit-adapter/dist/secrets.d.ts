/** What a secret is kept under: the thread (the AlixAI session) and the preparation the interrupt names. */
export interface ConfirmationSecretKey {
    readonly threadId: string;
    readonly preparationId: string;
}
/**
 * Where AlixAIAgent keeps confirmation secrets. The default is in memory; a host that runs several processes behind
 * one CopilotRuntime route supplies a store they share (Redis, a database), always server-side.
 */
export interface ConfirmationSecretStore {
    put: (key: ConfirmationSecretKey, secret: string) => void | Promise<void>;
    get: (key: ConfirmationSecretKey) => string | undefined | Promise<string | undefined>;
    delete: (key: ConfirmationSecretKey) => void | Promise<void>;
}
export interface MemorySecretStoreOptions {
    /** How long a secret is kept, in milliseconds. A preparation expires within minutes anyway. Default 30 minutes. */
    ttlMs?: number;
    /** The most secrets held at once; the oldest is dropped first. Default 1000. */
    maxEntries?: number;
    /** The clock, for tests. */
    now?: () => number;
}
/**
 * An in-memory ConfirmationSecretStore with a time limit and a size limit. The value is never part of anything it
 * prints: the object has no enumerable members and `toString` and JSON say nothing.
 */
export declare const createMemorySecretStore: (options?: MemorySecretStoreOptions) => ConfirmationSecretStore;
//# sourceMappingURL=secrets.d.ts.map