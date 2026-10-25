import type { FileUpload } from "./api-types.js";
import { sdkError } from "./errors.js";
import type { FilesApi } from "./files.js";
import { validateUpload } from "./files.js";
import type { FileRef } from "./gen/conversation/index.js";
import type { CallScope } from "./transport.js";
import { createTurnStream } from "./turn.js";
import type { ReconnectPolicy, TurnStream } from "./turn.js";

// Multimodal message composition (master plan 15.1): a message is text plus attachments. Composing is two steps the SDK does in order:
// each attachment is uploaded as a scoped file of the session (`ephemeral_context` unless the attachment says otherwise, which makes it
// readable by the model inside that session), then one text turn is sent that names the uploaded file ids in `TurnRequest.attachments`.
// The core resolves them under the turn's scope before the model runs. Nothing about the order is the host's to get wrong.

/** What kind of attachment a media type is, for choosing an icon or a previewer. */
export type AttachmentKind = "image" | "audio" | "video" | "file";

/** The kind of a media type: `image/*`, `audio/*`, `video/*`, and `file` for everything else. */
export const attachmentKind = (mimeType: string): AttachmentKind => {
  const major = mimeType.split("/", 1)[0]?.toLowerCase();
  return major === "image" || major === "audio" || major === "video" ? major : "file";
};

/** A message to send: text, and the files that belong to it. */
export interface OutgoingMessage {
  /** The user's message. Required: a turn is text, a choice, a confirmation or a rejection, and attachments are never a turn by themselves. */
  text: string;
  /** A BCP 47 tag the core supports ("en", "it"); absent lets the core choose. */
  language?: string;
  /** Files to upload before the turn is sent, at most four (what one turn may carry). */
  attachments?: FileUpload[];
}

/** The most attachments one message, and one turn request, may carry. */
export const MAX_ATTACHMENTS = 4;

/** A `TurnStream` that also reports the references of the attachments the message uploaded. */
export interface MessageStream extends TurnStream {
  /**
   * The references of the uploaded attachments, in the order given (an empty list for a message without any). Rejects with the upload's
   * error when one fails; then nothing was sent and the files already stored have been deleted.
   */
  readonly attachments: Promise<FileRef[]>;
}

/** Adds the attachments promise to a stream. */
export const withAttachments = (stream: TurnStream, attachments: Promise<FileRef[]>): MessageStream => {
  attachments.catch(() => undefined); // a rejection nobody awaits is not an unhandled one
  return Object.assign(stream, { attachments });
};

export interface MessageStreamParams {
  message: OutgoingMessage;
  sessionId: string;
  files: FilesApi;
  /** The scope of the whole call: its deadline covers the uploads and the turn, and its abort cancels either. */
  scope: CallScope;
  /** Validates a turn input and returns the request body. */
  turnBody: (input: unknown) => string;
  /** Sends the turn request with the given body. */
  send: (payload: string) => Promise<Response>;
  mapFailure: (error: unknown) => unknown;
  reconnect?: ReconnectPolicy;
}

const invalid = (reason: string) => sdkError("validation_failed", `invalid message: ${reason}`, { details: { argument: "message" } });

export const createMessageStream = (params: MessageStreamParams): MessageStream => {
  let settle!: { resolve: (refs: FileRef[]) => void; reject: (error: unknown) => void };
  const attachments = new Promise<FileRef[]>((resolve, reject) => {
    settle = { resolve, reject };
  });

  const stream = createTurnStream({
    start: async () => {
      const { message } = params;
      let payload: string;
      let pending: readonly FileUpload[];
      try {
        if (typeof message !== "object" || message === null || Array.isArray(message)) throw invalid("it must be an object with text");
        for (const key of Object.keys(message)) if (!["text", "language", "attachments"].includes(key)) throw invalid(`unknown member "${key}"`);
        pending = message.attachments ?? [];
        if (!Array.isArray(pending) || pending.length > MAX_ATTACHMENTS) throw invalid(`attachments must be a list of at most ${MAX_ATTACHMENTS}`);
        // Everything is checked before the first upload, so a malformed message costs no context token and stores nothing.
        payload = params.turnBody(message.language === undefined ? { text: message.text } : { text: message.text, language: message.language });
        pending.forEach(validateUpload);
      } catch (error) {
        settle.reject(error);
        throw error;
      }
      const stored: FileRef[] = [];
      try {
        for (const file of pending) stored.push(await params.files.upload(file, { signal: params.scope.signal, sessionId: params.sessionId }));
        // The turn names the files that were just stored: the same validated parts plus their ids.
        if (stored.length > 0) {
          payload = params.turnBody({
            text: message.text,
            ...(message.language !== undefined && { language: message.language }),
            attachments: stored.map((ref) => ref.file_id),
          });
        }
      } catch (error) {
        // Nothing is sent: do not leave the files that did get stored behind (best effort; they expire in any case).
        await Promise.all(stored.map((ref) => params.files.delete(ref.file_id).catch(() => undefined)));
        settle.reject(error);
        throw error;
      }
      settle.resolve(stored);
      return params.send(payload);
    },
    scope: params.scope,
    mapFailure: params.mapFailure,
    ...(params.reconnect !== undefined && { reconnect: params.reconnect }),
  });
  return withAttachments(stream, attachments);
};
