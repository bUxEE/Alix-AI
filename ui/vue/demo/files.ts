import type { AlixAttachment, FileRef } from "@alixai/vue";
import { DEMO_PATHS, fetchDemoJson } from "../../demo-mock/src/index";

// The demo host's side of the files service: the list the mock holds, the mapping from the API's FileRef to the kit's
// attachment shape, and a download. The kit never fetches bytes itself; these are the host's calls.

export const toAttachment = (ref: FileRef): AlixAttachment => ({
  fileId: ref.file_id,
  name: ref.name,
  mimeType: ref.mime_type,
  sizeBytes: ref.size_bytes,
  previewable: ref.previewable,
  ...(ref.expires_at !== undefined && { expiresAt: ref.expires_at }),
});

export const listDemoFiles = async (signal?: AbortSignal): Promise<AlixAttachment[]> => {
  const body = await fetchDemoJson<{ files: FileRef[] }>(DEMO_PATHS.fileList, "", signal === undefined ? {} : { signal });
  return body.files.map(toAttachment);
};

/** The file's bytes as a `blob:` URL of its own type (`GET /v1/files/{id}/content`). The caller revokes it. */
export const fetchDemoContent = async (fileId: string, signal?: AbortSignal): Promise<string> => {
  const response = await fetch(`${DEMO_PATHS.files}/${encodeURIComponent(fileId)}/content`, signal === undefined ? {} : { signal });
  if (!response.ok) throw new Error(`the mock answered ${response.status}`);
  return URL.createObjectURL(await response.blob());
};

/** Saves the file to the reader's device, the way a host would answer the kit's `download` event. */
export const downloadDemoFile = async (file: AlixAttachment): Promise<void> => {
  const url = await fetchDemoContent(file.fileId);
  const link = document.createElement("a");
  link.href = url;
  link.download = file.name;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
};
