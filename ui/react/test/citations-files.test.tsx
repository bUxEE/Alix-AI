// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AlixAttachmentComposer, AlixCitationList, AlixFileList, AlixFilePreview, AlixProvider } from "../src/index.js";
import { attachment, citation } from "./fixtures.js";

afterEach(cleanup);

describe("the citation list", () => {
  it("makes a source clickable only when it is available, and reports the citation", () => {
    const onOpenCitation = vi.fn();
    const removed = citation({ source_id: "src_01J00000000000000000000002", display_name: "Old price list", availability: "removed" });
    render(<AlixCitationList citations={[citation(), removed]} onOpenCitation={onOpenCitation} />);
    fireEvent.click(screen.getByRole("button", { name: "Open Treatment protocol" }));
    expect(onOpenCitation).toHaveBeenCalledTimes(1);
    expect(onOpenCitation.mock.calls[0]?.[0].source_id).toBe("src_01J00000000000000000000001");
    // The removed source is still shown (a citation is never hidden), as text with its state in words.
    expect(screen.queryByRole("button", { name: /Old price list/ })).toBeNull();
    expect(screen.getByText("Old price list")).toBeTruthy();
    expect(screen.getByText("Removed")).toBeTruthy();
  });

  it("shows no open button when the host gave no handler, and says when a source changed", () => {
    render(<AlixCitationList citations={[citation({ staleness: "stale" })]} />);
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText("Source changed since")).toBeTruthy();
    expect(screen.getByText("Visits last 30 minutes.")).toBeTruthy();
    expect(screen.getByText("p. 14")).toBeTruthy();
  });

  it("is a labelled region with a list", () => {
    render(<AlixCitationList citations={[citation()]} />);
    expect(screen.getByRole("region", { name: "Sources" })).toBeTruthy();
    expect(screen.getAllByRole("listitem").length).toBeGreaterThan(0);
  });

  it("speaks Italian", () => {
    render(
      <AlixProvider locale="it">
        <AlixCitationList citations={[citation({ availability: "unavailable" })]} />
      </AlixProvider>,
    );
    expect(screen.getByRole("region", { name: "Fonti" })).toBeTruthy();
    expect(screen.getByText("Non disponibile")).toBeTruthy();
    expect(screen.getByText("pag. 14")).toBeTruthy();
  });
});

describe("the file preview", () => {
  it("draws the host's preview for a previewable file and only the kind otherwise", () => {
    const renderPreview = vi.fn(() => <span data-testid="thumb" />);
    render(
      <AlixFileList
        files={[attachment(), attachment({ fileId: "file_2", name: "contract.pdf", mimeType: "application/pdf", previewable: false })]}
        renderPreview={renderPreview}
      />,
    );
    expect(renderPreview).toHaveBeenCalledTimes(1);
    expect(screen.getAllByTestId("thumb").length).toBe(1);
    expect(screen.getByText("Document")).toBeTruthy();
    expect(screen.getByText("image/png, 178.1 KB")).toBeTruthy();
    expect(screen.getByRole("list", { name: "Attachments" })).toBeTruthy();
  });

  it("offers a named remove button only when it can be handled", () => {
    const onRemove = vi.fn();
    render(
      <ul>
        <AlixFilePreview file={attachment()} onRemove={onRemove} error="Too large" />
      </ul>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Remove scan-0317.png" }));
    expect(onRemove).toHaveBeenCalledWith("file_01J00000000000000000000001");
    expect(screen.getByRole("alert").textContent).toBe("Too large");
  });
});

describe("the attachment composer", () => {
  const file = (name: string, size: number): File => new File([new Uint8Array(size)], name, { type: "image/png" });

  it("passes on files within the limit and tells the person about the others", () => {
    const onAddFiles = vi.fn();
    render(<AlixAttachmentComposer files={[]} onAddFiles={onAddFiles} onRemoveFile={vi.fn()} maxFileMb={1} />);
    const input = screen.getByLabelText("Attach files") as HTMLInputElement;
    fireEvent.change(input, { target: { files: [file("small.png", 10), file("big.png", 2 * 1024 * 1024)] } });
    expect(onAddFiles).toHaveBeenCalledTimes(1);
    expect(onAddFiles.mock.calls[0]?.[0].map((f: File) => f.name)).toEqual(["small.png"]);
    expect(screen.getByRole("alert").textContent).toBe("big.png is larger than 1 MB.");
  });

  it("lists the pending files and reports a removal", () => {
    const onRemoveFile = vi.fn();
    render(<AlixAttachmentComposer files={[attachment()]} onAddFiles={vi.fn()} onRemoveFile={onRemoveFile} />);
    expect(screen.getByRole("list", { name: "Files ready to send" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove scan-0317.png" }));
    expect(onRemoveFile).toHaveBeenCalledWith("file_01J00000000000000000000001");
  });

  it("is a real file input behind a label, and can be disabled", () => {
    render(<AlixAttachmentComposer files={[]} onAddFiles={vi.fn()} onRemoveFile={vi.fn()} disabled />);
    expect((screen.getByLabelText("Attach files") as HTMLInputElement).disabled).toBe(true);
  });
});
