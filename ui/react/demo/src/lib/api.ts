import { useCallback, useEffect, useRef, useState } from "react";
import type { AlixAIAttachment } from "@alixai/copilotkit-adapter";
import { DEMO_PATHS } from "@alixai/demo-mock";

// The demo's own calls to the mock, as a host's frontend would make them to its backend. Same origin, no other network.

export { DEMO_PATHS };

const messageOf = async (response: Response): Promise<string> => {
  try {
    const body = (await response.json()) as { message?: unknown };
    if (typeof body.message === "string") return body.message;
  } catch {
    // not JSON
  }
  return `HTTP ${response.status}`;
};

export interface DemoHttpError extends Error {
  readonly status: number;
  readonly body: unknown;
}

const httpError = (status: number, message: string, body: unknown): DemoHttpError => Object.assign(new Error(message), { status, body });

export const isHttpError = (error: unknown): error is DemoHttpError => error instanceof Error && typeof (error as { status?: unknown }).status === "number";

export const requestJson = async <T>(path: string, init: RequestInit = {}): Promise<T> => {
  const response = await fetch(path, { ...init, headers: { Accept: "application/json", ...(init.body !== undefined && !(init.body instanceof FormData) && { "Content-Type": "application/json" }), ...init.headers } });
  if (!response.ok) {
    const clone = response.clone();
    const body = await clone.json().catch(() => undefined);
    throw httpError(response.status, await messageOf(response), body);
  }
  return (response.status === 204 ? undefined : await response.json()) as T;
};

export interface Resource<T> {
  readonly data: T | undefined;
  readonly error: string | undefined;
  readonly loading: boolean;
  readonly reload: () => void;
}

/** Loads one thing from the mock and keeps the last good answer on screen while it loads again. */
export const useResource = <T>(load: (signal: AbortSignal) => Promise<T>, key = ""): Resource<T> => {
  const [data, setData] = useState<T | undefined>(undefined);
  const [error, setError] = useState<string | undefined>(undefined);
  const [loading, setLoading] = useState(true);
  const [tick, setTick] = useState(0);
  const loader = useRef(load);
  loader.current = load;
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    loader
      .current(controller.signal)
      .then((value) => {
        if (controller.signal.aborted) return;
        setData(value);
        setError(undefined);
        setLoading(false);
      })
      .catch((reason: unknown) => {
        if (controller.signal.aborted) return;
        setError(reason instanceof Error ? reason.message : String(reason));
        setLoading(false);
      });
    return () => controller.abort();
  }, [tick, key]);
  const reload = useCallback(() => setTick((value) => value + 1), []);
  return { data, error, loading, reload };
};

/** A FileRef as the files service returns it (snake_case), with the members the demo reads. */
export interface DemoFileRef {
  readonly file_id: string;
  readonly name: string;
  readonly mime_type: string;
  readonly size_bytes: number;
  readonly previewable?: boolean;
  readonly permissions?: readonly string[];
  readonly expires_at?: string;
}

export const toAttachment = (ref: DemoFileRef): AlixAIAttachment => ({
  fileId: ref.file_id,
  name: ref.name,
  mimeType: ref.mime_type,
  sizeBytes: ref.size_bytes,
  previewable: ref.previewable === true,
  ...(ref.expires_at !== undefined && { expiresAt: ref.expires_at }),
});

/** Uploads one file and returns its FileRef; rejects with the mock's own message on a refusal. */
export const uploadFile = async (file: File): Promise<DemoFileRef> => {
  const form = new FormData();
  form.append("file", file, file.name);
  return requestJson<DemoFileRef>(DEMO_PATHS.files, { method: "POST", body: form });
};

/** The content of a file as a `blob:` URL (only for files that may be downloaded). Revoke it when done. */
export const fetchContentUrl = async (fileId: string, signal?: AbortSignal): Promise<string> => {
  const response = await fetch(`${DEMO_PATHS.files}/${encodeURIComponent(fileId)}/content`, signal === undefined ? {} : { signal });
  if (!response.ok) throw httpError(response.status, await messageOf(response), undefined);
  return URL.createObjectURL(await response.blob());
};
