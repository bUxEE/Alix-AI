import { useEffect, useState } from "react";
import type { ReactElement } from "react";
import { AlixAttachmentComposer, AlixFileList, AlixFilePreview, AlixLoadable, AlixSkeleton, useAlixLoadState } from "@alixai/react";
import type { AlixAIAttachment } from "@alixai/copilotkit-adapter";
import { DEMO_PATHS, fetchContentUrl, isHttpError, requestJson, toAttachment, uploadFile, useResource } from "../lib/api";
import type { DemoFileRef } from "../lib/api";
import { DemoLargePreview, renderPreview } from "../lib/preview";
import { useShell } from "../shell/context";
import { Icon } from "../shell/Icon";

const MAX_FILE_MB = 8;

interface Refused {
  readonly key: string;
  readonly file: AlixAIAttachment;
  readonly message: string;
}

// Files the page makes itself, so every path can be tried without picking anything from the disk.
const makePng = async (): Promise<File> => {
  const canvas = document.createElement("canvas");
  canvas.width = 480;
  canvas.height = 300;
  const context = canvas.getContext("2d");
  if (context !== null) {
    context.fillStyle = "rgb(236, 242, 241)";
    context.fillRect(0, 0, 480, 300);
    const bars = [62, 118, 86, 164, 132, 201];
    bars.forEach((height, index) => {
      context.fillStyle = index === 5 ? "rgb(32, 112, 108)" : "rgb(120, 168, 165)";
      context.fillRect(40 + index * 70, 260 - height, 44, height);
    });
    context.fillStyle = "rgb(40, 52, 54)";
    context.font = "600 18px system-ui, sans-serif";
    context.fillText("Harbor Point, kWh per floor", 40, 40);
  }
  const blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, "image/png"));
  return new File([blob ?? new Blob()], "harbor-point-floors.png", { type: "image/png" });
};

const makeCsv = (): File =>
  new File(["work_order,site,priority,due\nWO-2291,Harbor Point,P2,2026-10-14\nWO-2294,Riverside,P1,2026-10-03\nWO-2301,Westfield,P1,2026-10-06\n"], "open-work-orders.csv", {
    type: "text/csv",
  });

const makeSvg = (): File => new File(['<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10"/></svg>'], "logo.svg", { type: "image/svg+xml" });

const makeHtml = (): File => new File(["<!doctype html><html><body><h1>Northwind</h1></body></html>"], "notice.html", { type: "text/html" });

/** A PDF drawn by the browser from the file's content: only for a file that may be downloaded. */
const PdfView = ({ file }: { file: AlixAIAttachment }): ReactElement => {
  const { text } = useShell();
  const [url, setUrl] = useState<string | undefined>(undefined);
  const [blocked, setBlocked] = useState(false);
  useEffect(() => {
    const controller = new AbortController();
    let made: string | undefined;
    setUrl(undefined);
    setBlocked(false);
    fetchContentUrl(file.fileId, controller.signal)
      .then((value) => {
        made = value;
        if (!controller.signal.aborted) setUrl(value);
        else URL.revokeObjectURL(value);
      })
      .catch(() => {
        if (!controller.signal.aborted) setBlocked(true);
      });
    return () => {
      controller.abort();
      if (made !== undefined) URL.revokeObjectURL(made);
    };
  }, [file.fileId]);
  if (blocked) return <p className="demo-viewer__none">{text.files.pdfBlocked}</p>;
  if (url === undefined) return <div className="demo-viewer__skeleton" aria-hidden="true" />;
  return (
    <div className="demo-stack demo-stack--tight">
      <iframe className="demo-viewer__pdf" src={url} title={file.name} />
      <p className="demo-muted">{text.files.pdf}</p>
    </div>
  );
};

const LibrarySkeleton = (): ReactElement => (
  <ul className="alix-files" aria-hidden="true">
    {[0, 1, 2, 3].map((row) => (
      <li key={row} className="alix-files__item">
        <span className="alix-files__preview" />
        <span className="alix-files__body">
          <AlixSkeleton width="60%" />
          <AlixSkeleton width="35%" height="0.625rem" />
        </span>
      </li>
    ))}
  </ul>
);

export const FilesSection = (): ReactElement => {
  const { text, notify } = useShell();
  const [pending, setPending] = useState<AlixAIAttachment[]>([]);
  const [refused, setRefused] = useState<Refused[]>([]);
  const [uploading, setUploading] = useState(0);
  const [selected, setSelected] = useState<string | undefined>(undefined);
  const library = useResource((signal) => requestJson<{ files: DemoFileRef[] }>(DEMO_PATHS.fileList, { signal }).then((body) => body.files.map(toAttachment)));
  const state = useAlixLoadState(library.loading, library.error !== undefined, library.data !== undefined);
  const chosen = library.data?.find((file) => file.fileId === selected);

  const upload = async (files: File[]): Promise<void> => {
    setUploading((count) => count + files.length);
    await Promise.all(
      files.map(async (file) => {
        try {
          const ref = await uploadFile(file);
          setPending((current) => [...current, toAttachment(ref)]);
          notify("onAddFiles", `${file.name} -> ${ref.file_id}`);
        } catch (error) {
          const message = isHttpError(error) || error instanceof Error ? error.message : String(error);
          const key = `${file.name}-${Date.now()}-${Math.random()}`;
          setRefused((current) => [
            ...current,
            { key, message, file: { fileId: key, name: file.name, mimeType: file.type === "" ? "application/octet-stream" : file.type, sizeBytes: file.size, previewable: false } },
          ]);
        } finally {
          setUploading((count) => count - 1);
        }
      }),
    );
    library.reload();
  };

  const remove = async (fileId: string): Promise<void> => {
    setPending((current) => current.filter((file) => file.fileId !== fileId));
    await requestJson<void>(`${DEMO_PATHS.files}/${encodeURIComponent(fileId)}`, { method: "DELETE" }).catch(() => undefined);
    notify("onRemoveFile", fileId);
    library.reload();
  };

  return (
    <div className="demo-split demo-split--even">
      <section className="demo-frame demo-frame--pad demo-stack" aria-labelledby="demo-composer-title">
        <div className="demo-stack demo-stack--tight">
          <h2 id="demo-composer-title" className="demo-frame__title">
            {text.files.composer}
          </h2>
          <p className="demo-muted">{text.files.composerLead}</p>
        </div>
        <AlixAttachmentComposer files={pending} maxFileMb={MAX_FILE_MB} renderPreview={renderPreview} onAddFiles={(files) => void upload(files)} onRemoveFile={(id) => void remove(id)} />
        <div className="demo-stack demo-stack--tight">
          <p className="demo-eyebrow">{text.files.samples}</p>
          <div className="demo-row demo-row--wrap">
            <button type="button" className="demo-chip" onClick={() => void makePng().then((file) => upload([file]))}>
              <Icon name="plus" size={14} />
              {text.files.sampleImage}
            </button>
            <button type="button" className="demo-chip" onClick={() => void upload([makeCsv()])}>
              <Icon name="plus" size={14} />
              {text.files.sampleCsv}
            </button>
            <button type="button" className="demo-chip" data-tone="danger" onClick={() => void upload([makeSvg()])}>
              {text.files.refusalSvg}
            </button>
            <button type="button" className="demo-chip" data-tone="danger" onClick={() => void upload([makeHtml()])}>
              {text.files.refusalHtml}
            </button>
          </div>
        </div>
        {uploading > 0 && (
          <p className="demo-note" role="status">
            <span className="demo-pulse" aria-hidden="true" />
            {text.files.uploading}
          </p>
        )}
        {refused.length > 0 && (
          <ul className="alix-files" aria-label={text.files.refused}>
            {refused.map((entry) => (
              <AlixFilePreview key={entry.key} file={entry.file} error={entry.message} onRemove={() => setRefused((current) => current.filter((item) => item.key !== entry.key))} />
            ))}
          </ul>
        )}
      </section>
      <section className="demo-frame demo-frame--pad demo-stack" aria-labelledby="demo-library-title">
        <div className="demo-stack demo-stack--tight">
          <h2 id="demo-library-title" className="demo-frame__title">
            {text.files.library}
          </h2>
          <p className="demo-muted">{text.files.libraryLead}</p>
        </div>
        <AlixLoadable state={state} skeleton={<LibrarySkeleton />}>
          {library.error !== undefined && !state.hasLoadedOnce ? (
            <p className="demo-note" data-tone="danger" role="alert">
              {text.files.failed}
            </p>
          ) : (
            <div className="demo-stack">
              <AlixFileList files={library.data ?? []} renderPreview={renderPreview} />
              <div className="demo-row demo-row--wrap" role="group" aria-label={text.files.viewer}>
                {(library.data ?? []).map((file) => (
                  <button key={file.fileId} type="button" className="demo-chip" aria-pressed={file.fileId === selected} onClick={() => setSelected(file.fileId)}>
                    {file.name}
                  </button>
                ))}
              </div>
            </div>
          )}
        </AlixLoadable>
        <div className="demo-viewer" aria-live="polite">
          {chosen === undefined ? (
            <p className="demo-viewer__none">{text.files.pick}</p>
          ) : chosen.mimeType === "application/pdf" ? (
            <PdfView key={chosen.fileId} file={chosen} />
          ) : (
            <DemoLargePreview key={chosen.fileId} file={chosen} />
          )}
        </div>
      </section>
    </div>
  );
};
