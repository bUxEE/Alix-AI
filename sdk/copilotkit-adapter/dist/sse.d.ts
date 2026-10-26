export interface SseFrame {
    /** The `id:` field of this frame, when it had one. */
    readonly id: string | undefined;
    /** The `data:` lines joined with a newline. */
    readonly data: string;
}
/** The longest frame accepted: the gateway caps an event at 256 KiB, so this is generous and still bounded. */
export declare const MAX_FRAME_CHARS: number;
/**
 * Reads frames from a byte stream until it ends. Comments, `event:` and `retry:` fields are ignored. A frame without
 * data is skipped, and so is a last frame the stream ended inside. A frame longer than MAX_FRAME_CHARS is an error (the
 * stream is hostile or broken).
 */
export declare function readSseFrames(body: ReadableStream<Uint8Array>, signal?: AbortSignal): AsyncGenerator<SseFrame>;
//# sourceMappingURL=sse.d.ts.map