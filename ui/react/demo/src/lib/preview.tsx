import { useEffect, useState } from "react";
import type { ReactElement } from "react";
import type { AlixAIAttachment } from "@alixai/copilotkit-adapter";
import { fetchDemoPreview } from "@alixai/demo-mock";
import type { DemoPreview } from "@alixai/demo-mock";

// The host's side of a file preview. The kit never builds a file URL: it asks `renderPreview` what to draw for a previewable file,
// and the host fetches the safe preview (`GET /v1/files/{id}/preview`) under the user's own scope. An image becomes a `blob:` URL
// that is revoked when the preview goes away.

const usePreview = (fileId: string): DemoPreview | undefined => {
  const [preview, setPreview] = useState<DemoPreview | undefined>(undefined);
  useEffect(() => {
    const controller = new AbortController();
    let url: string | undefined;
    fetchDemoPreview(fileId, "", controller.signal)
      .then((value) => {
        if (value.kind === "image") url = value.url;
        if (controller.signal.aborted) {
          if (url !== undefined) URL.revokeObjectURL(url);
          return;
        }
        setPreview(value);
      })
      .catch(() => {
        if (!controller.signal.aborted) setPreview({ kind: "none", reason: "the preview could not be read" });
      });
    return () => {
      controller.abort();
      if (url !== undefined) URL.revokeObjectURL(url);
    };
  }, [fileId]);
  return preview;
};

/** The small preview the kit draws in a file row: the image itself, or the first lines of a text file. */
export const DemoThumbnail = ({ file }: { file: AlixAIAttachment }): ReactElement => {
  const preview = usePreview(file.fileId);
  if (preview === undefined) return <span className="demo-thumb demo-thumb--loading" aria-hidden="true" />;
  if (preview.kind === "image") return <img className="demo-thumb" src={preview.url} alt="" />;
  if (preview.kind === "text") return <span className="demo-thumb demo-thumb--text">{preview.text.slice(0, 80)}</span>;
  return <span className="demo-thumb demo-thumb--none" aria-hidden="true" />;
};

/** The kit's `renderPreview`, defined once at module scope so the chat's renderers stay stable. */
export const renderPreview = (file: AlixAIAttachment): ReactElement => <DemoThumbnail file={file} />;

/** A large preview for the files section: the image, the text (with its truncation), or why there is none. */
export const DemoLargePreview = ({ file }: { file: AlixAIAttachment }): ReactElement => {
  const preview = usePreview(file.fileId);
  if (preview === undefined) return <div className="demo-viewer__skeleton" aria-hidden="true" />;
  if (preview.kind === "image") return <img className="demo-viewer__image" src={preview.url} alt={file.name} />;
  if (preview.kind === "text") return <pre className="demo-viewer__text">{preview.text}</pre>;
  return <p className="demo-viewer__none">{preview.reason}</p>;
};
