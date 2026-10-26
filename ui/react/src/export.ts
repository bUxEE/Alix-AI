// Host-driven answer and block exports. The kit never calls AlixAI: it asks the host for a file (or a Blob) and, when the
// host returns a file id, asks the host to download it.

export type ExportFormat = "csv" | "xlsx" | "html" | "md";

export interface ExportParams {
  readonly sessionId: string;
  readonly turnId: string;
  readonly blockIds?: readonly string[];
  readonly format: ExportFormat;
}

export type ExportResult = { readonly fileId: string; readonly name: string } | Blob;

export type AlixExporter = (params: ExportParams) => Promise<ExportResult>;

export type AlixDownload = (fileId: string) => void | Promise<void>;

const extensionOf = (format: ExportFormat): string => (format === "md" ? "md" : format);

/** Saves a Blob the host returned, with a stable name derived from the format. */
export const saveExportBlob = (blob: Blob, format: ExportFormat, base = "alix-export"): void => {
  if (typeof document === "undefined" || typeof URL === "undefined") return;
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = `${base}.${extensionOf(format)}`;
  anchor.rel = "noopener";
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
};

/** Runs the host exporter and either saves a Blob or hands a file id to `download`. */
export const runExport = async (
  exporter: AlixExporter,
  params: ExportParams,
  download: AlixDownload | undefined,
): Promise<void> => {
  const result = await exporter(params);
  if (result instanceof Blob) {
    saveExportBlob(result, params.format);
    return;
  }
  if (download === undefined) throw new Error("download is required when the exporter returns a file id");
  await download(result.fileId);
};
