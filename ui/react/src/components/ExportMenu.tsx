import { useEffect, useId, useRef, useState } from "react";
import type { ReactElement } from "react";
import { runExport, type AlixDownload, type AlixExporter, type ExportFormat, type ExportParams } from "../export.js";
import { useAlixText } from "../i18n/context.js";

export interface AlixExportMenuProps {
  exporter: AlixExporter;
  download?: AlixDownload;
  params: Omit<ExportParams, "format">;
  formats: readonly ExportFormat[];
  label: string;
}

const DownloadIcon = (): ReactElement => (
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
    <path d="M12 3v12" />
    <path d="M7 10l5 5 5-5" />
    <path d="M5 21h14" />
  </svg>
);

export const AlixExportMenu = ({ exporter, download, params, formats, label }: AlixExportMenuProps): ReactElement => {
  const text = useAlixText().export;
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const root = useRef<HTMLDivElement>(null);
  const menuId = useId();

  const labels: Record<ExportFormat, string> = {
    csv: text.csv,
    xlsx: text.xlsx,
    html: text.html,
    md: text.md,
  };

  useEffect(() => {
    if (!open) return undefined;
    const onPointer = (event: Event): void => {
      if (root.current !== null && event.target instanceof Node && root.current.contains(event.target)) return;
      setOpen(false);
    };
    const onKey = (event: KeyboardEvent): void => {
      if (event.key === "Escape") {
        event.preventDefault();
        setOpen(false);
      }
    };
    document.addEventListener("pointerdown", onPointer, true);
    document.addEventListener("keydown", onKey, true);
    return () => {
      document.removeEventListener("pointerdown", onPointer, true);
      document.removeEventListener("keydown", onKey, true);
    };
  }, [open]);

  const choose = async (format: ExportFormat): Promise<void> => {
    if (busy) return;
    setBusy(true);
    setError("");
    setOpen(false);
    try {
      await runExport(exporter, { ...params, format }, download);
    } catch {
      setError(text.failed);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div ref={root} className="alix-export">
      <button
        type="button"
        className="alix-export__toggle"
        aria-label={label}
        aria-expanded={open}
        aria-haspopup="menu"
        aria-controls={menuId}
        aria-busy={busy}
        disabled={busy}
        onClick={() => {
          if (busy) return;
          setError("");
          setOpen((value) => !value);
        }}
      >
        {busy ? <span className="alix-export__spinner" aria-hidden="true" /> : <DownloadIcon />}
      </button>
      {open ? (
        <ul id={menuId} className="alix-export__menu" role="menu" aria-label={label}>
          {formats.map((format) => (
            <li key={format} role="none">
              <button type="button" className="alix-export__item" role="menuitem" onClick={() => void choose(format)}>
                {labels[format]}
              </button>
            </li>
          ))}
        </ul>
      ) : null}
      {error !== "" ? (
        <p className="alix-export__error" role="alert">
          {error}
        </p>
      ) : null}
    </div>
  );
};
