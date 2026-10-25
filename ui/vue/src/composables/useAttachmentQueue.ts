import { computed, ref, type ComputedRef, type Ref } from "vue";
import type { AlixAttachment } from "../agui/types";
import type { AttachmentKind, OutgoingAttachment } from "./useAlixChat";

// The files waiting to go with the next message: chosen, uploading through the host's files service, ready or failed. The kit
// holds no upload logic of its own: `uploader` is the host's call to `POST /v1/files` and returns the file id the message will
// carry (never bytes, never a URL).

export interface PendingAttachment extends AlixAttachment {
  readonly status: "uploading" | "ready" | "error";
}

export interface AttachmentQueueOptions {
  uploader: (file: File, signal: AbortSignal) => Promise<{ fileId: string }>;
  /** Defaults to the engine's limit of four. */
  maxFiles?: number;
}

export interface AttachmentQueue {
  files: Ref<PendingAttachment[]>;
  /** The files that finished uploading, in the shape a message carries. */
  outgoing: ComputedRef<OutgoingAttachment[]>;
  add: (files: File[]) => void;
  remove: (fileId: string) => void;
  clear: () => void;
}

export const kindOf = (mimeType: string): AttachmentKind =>
  mimeType.startsWith("image/") ? "image" : mimeType.startsWith("audio/") ? "audio" : mimeType.startsWith("video/") ? "video" : "document";

export const useAttachmentQueue = (options: AttachmentQueueOptions): AttachmentQueue => {
  const maxFiles = options.maxFiles ?? 4;
  const files = ref<PendingAttachment[]>([]);
  const aborts = new Map<string, AbortController>();
  let sequence = 0;

  const patch = (fileId: string, change: Partial<{ status: PendingAttachment["status"]; fileId: string }>): void => {
    files.value = files.value.map((file) => (file.fileId === fileId ? { ...file, ...change } : file));
  };

  const upload = async (localId: string, file: File): Promise<void> => {
    const controller = new AbortController();
    aborts.set(localId, controller);
    try {
      const result = await options.uploader(file, controller.signal);
      if (!controller.signal.aborted) patch(localId, { status: "ready", fileId: result.fileId });
    } catch {
      if (!controller.signal.aborted) patch(localId, { status: "error" });
    } finally {
      aborts.delete(localId);
    }
  };

  const add = (incoming: File[]): void => {
    for (const file of incoming) {
      if (files.value.length >= maxFiles) return;
      sequence += 1;
      const localId = `pending-${sequence}`;
      files.value = [...files.value, { fileId: localId, name: file.name, mimeType: file.type, sizeBytes: file.size, previewable: false, status: "uploading" }];
      void upload(localId, file);
    }
  };

  const remove = (fileId: string): void => {
    aborts.get(fileId)?.abort();
    aborts.delete(fileId);
    files.value = files.value.filter((file) => file.fileId !== fileId);
  };

  const clear = (): void => {
    for (const controller of aborts.values()) controller.abort();
    aborts.clear();
    files.value = [];
  };

  const outgoing = computed<OutgoingAttachment[]>(() =>
    files.value.filter((file) => file.status === "ready").map((file) => ({ fileId: file.fileId, kind: kindOf(file.mimeType), name: file.name })),
  );

  return { files, outgoing, add, remove, clear };
};
