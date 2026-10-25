import { useId, useState } from "react";
import type { ChangeEvent, ReactElement } from "react";
import type { AlixAIAttachment } from "@alixai/copilotkit-adapter";
import { fill } from "../i18n/messages.js";
import { formatBytes, useAlixIntl } from "../i18n/context.js";

// Attachments. The kit never builds a file URL: a preview is whatever the host renders for a previewable file (for example
// an <img> whose source the host's backend serves from AlixAI's files service under the user's own scope).

export type AlixFileKind = "image" | "audio" | "video" | "document" | "other";

export const fileKindOf = (mimeType: string): AlixFileKind => {
  if (mimeType.startsWith("image/")) return "image";
  if (mimeType.startsWith("audio/")) return "audio";
  if (mimeType.startsWith("video/")) return "video";
  if (mimeType === "application/pdf" || mimeType.startsWith("text/") || mimeType.includes("document") || mimeType.includes("sheet")) return "document";
  return "other";
};

export interface AlixFilePreviewProps {
  file: AlixAIAttachment;
  /** What to draw for a previewable file. Default: the kind of the file. */
  renderPreview?: (file: AlixAIAttachment) => ReactElement | null;
  /** When given, a remove button is shown and called with the file id. */
  onRemove?: (fileId: string) => void;
  /** An error to show under the file (for example "too large"). */
  error?: string;
}

/** One file: a preview slot, its name, its type and size, and optionally a remove button. Renders a list item. */
export const AlixFilePreview = ({ file, renderPreview, onRemove, error }: AlixFilePreviewProps): ReactElement => {
  const intl = useAlixIntl();
  const text = intl.messages.files;
  const kind = fileKindOf(file.mimeType);
  const preview = file.previewable ? renderPreview?.(file) : null;
  return (
    <li className="alix-files__item" data-alixai="file" data-previewable={file.previewable} data-kind={kind} {...(error !== undefined && { "data-state": "error" })}>
      <span className="alix-files__preview" {...(preview !== null && preview !== undefined && { role: "img", "aria-label": fill(text.preview, { name: file.name }) })}>
        {preview ?? <span className="alix-files__kind">{text.kinds[kind]}</span>}
      </span>
      <span className="alix-files__body">
        <span className="alix-files__name" title={file.name}>
          {file.name}
        </span>
        <span className="alix-files__meta" data-alixai="attachment-meta">
          {file.mimeType}, {formatBytes(intl, file.sizeBytes)}
        </span>
        {error !== undefined && (
          <span className="alix-files__error" role="alert">
            {error}
          </span>
        )}
      </span>
      {onRemove !== undefined && (
        <button type="button" className="alix-files__remove" aria-label={fill(intl.messages.composer.removeNamed, { name: file.name })} onClick={() => onRemove(file.fileId)}>
          {text.remove}
        </button>
      )}
    </li>
  );
};

export interface AlixFileListProps {
  files: readonly AlixAIAttachment[];
  renderPreview?: AlixFilePreviewProps["renderPreview"];
  onRemove?: AlixFilePreviewProps["onRemove"];
  /** Accessible name of the list. Default: "Attachments". */
  label?: string;
}

export const AlixFileList = ({ files, renderPreview, onRemove, label }: AlixFileListProps): ReactElement => {
  const text = useAlixIntl().messages.files;
  return (
    <ul className="alix-files" data-alixai="attachments" aria-label={label ?? text.heading}>
      {files.map((file) => (
        <AlixFilePreview key={file.fileId} file={file} {...(renderPreview !== undefined && { renderPreview })} {...(onRemove !== undefined && { onRemove })} />
      ))}
    </ul>
  );
};

export interface AlixAttachmentComposerProps {
  /** Files already uploaded and waiting to go with the next message. */
  files: readonly AlixAIAttachment[];
  /** The person chose files. Called with the ones within `maxFileMb`; the host uploads them (the application decides what it accepts). */
  onAddFiles: (files: File[]) => void;
  /** The person removed a pending file. */
  onRemoveFile: (fileId: string) => void;
  /** The `accept` list of the file input. */
  accept?: string;
  /** The largest file in MB, from the effective settings (`attachments.max_file_mb`). A convenience check: the core enforces it. */
  maxFileMb?: number;
  disabled?: boolean;
  renderPreview?: AlixFilePreviewProps["renderPreview"];
}

/** The attach control and the list of pending files. It holds no upload logic: data in, events out. */
export const AlixAttachmentComposer = ({ files, onAddFiles, onRemoveFile, accept, maxFileMb, disabled = false, renderPreview }: AlixAttachmentComposerProps): ReactElement => {
  const text = useAlixIntl().messages.composer;
  const inputId = useId();
  const [rejected, setRejected] = useState<string[]>([]);
  const choose = (event: ChangeEvent<HTMLInputElement>): void => {
    const picked = Array.from(event.target.files ?? []);
    event.target.value = "";
    const tooBig = maxFileMb === undefined ? [] : picked.filter((file) => file.size > maxFileMb * 1024 * 1024);
    setRejected(tooBig.map((file) => fill(text.tooLarge, { name: file.name, max: maxFileMb ?? 0 })));
    const accepted = picked.filter((file) => !tooBig.includes(file));
    if (accepted.length > 0) onAddFiles(accepted);
  };
  return (
    <div className="alix-composer" data-alixai="attachment-composer" {...(disabled && { "data-disabled": "" })}>
      <input
        id={inputId}
        className="alix-composer__input"
        type="file"
        multiple
        disabled={disabled}
        {...(accept !== undefined && { accept })}
        onChange={choose}
      />
      <label htmlFor={inputId} className="alix-composer__pick">
        {text.pick}
      </label>
      {maxFileMb !== undefined && <p className="alix-composer__hint">{fill(text.hint, { max: maxFileMb })}</p>}
      {rejected.length > 0 && (
        <ul className="alix-composer__errors" role="alert">
          {rejected.map((message) => (
            <li key={message}>{message}</li>
          ))}
        </ul>
      )}
      {files.length > 0 && <AlixFileList files={files} label={text.pending} onRemove={onRemoveFile} {...(renderPreview !== undefined && { renderPreview })} />}
    </div>
  );
};
