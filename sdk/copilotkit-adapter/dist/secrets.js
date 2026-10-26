// The confirmation secret of a prepared action is the single-use proof that one preparation was shown to one principal
// in one session (docs/integration/agui.md section 6). It reaches the host backend in the response header of the run
// that prepared the action, and it goes back in the request header of the run that approves it. It is never an event,
// never a message and never something the browser holds: the agent keeps it here, per thread and preparation, and
// deletes it on use.
const keyOf = (key) => `${key.threadId.length}:${key.threadId}${key.preparationId}`;
/**
 * An in-memory ConfirmationSecretStore with a time limit and a size limit. The value is never part of anything it
 * prints: the object has no enumerable members and `toString` and JSON say nothing.
 */
export const createMemorySecretStore = (options = {}) => {
    const ttlMs = options.ttlMs ?? 30 * 60 * 1000;
    const maxEntries = options.maxEntries ?? 1000;
    const now = options.now ?? Date.now;
    const entries = new Map();
    const sweep = () => {
        const at = now();
        for (const [key, entry] of entries)
            if (entry.expires <= at)
                entries.delete(key);
    };
    const store = {
        put: (key, secret) => {
            sweep();
            entries.delete(keyOf(key));
            entries.set(keyOf(key), { secret, expires: now() + ttlMs });
            while (entries.size > maxEntries) {
                const oldest = entries.keys().next();
                if (oldest.done)
                    break;
                entries.delete(oldest.value);
            }
        },
        get: (key) => {
            sweep();
            return entries.get(keyOf(key))?.secret;
        },
        delete: (key) => {
            entries.delete(keyOf(key));
        },
    };
    Object.defineProperty(store, "size", { value: () => entries.size, enumerable: false });
    return store;
};
//# sourceMappingURL=secrets.js.map