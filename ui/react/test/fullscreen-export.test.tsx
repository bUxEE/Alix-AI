/** @vitest-environment jsdom */
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useRef, useState } from "react";
import type { CSSProperties, ReactElement } from "react";

import { AlixExportMenu, AlixProvider, AlixTableBlock } from "../src/index.js";
import { AlixExportsProvider } from "../src/exports-context.js";
import type { AlixExporter } from "../src/export.js";
import { useFullscreen } from "../src/hooks/useFullscreen.js";

afterEach(() => cleanup());

const FullscreenProbe = ({
  controlled,
  messages = ["hello"],
  fullscreenZIndex,
}: {
  controlled?: boolean;
  messages?: readonly string[];
  fullscreenZIndex?: number;
}): ReactElement => {
  const rootRef = useRef<HTMLDivElement>(null);
  const toggleRef = useRef<HTMLButtonElement>(null);
  const [value, setValue] = useState(false);
  const { isFullscreen, toggleFullscreen } = useFullscreen({
    rootRef,
    toggleRef,
    ...(controlled === true && { fullscreen: value, onFullscreenChange: setValue }),
  });
  const style: CSSProperties | undefined =
    fullscreenZIndex === undefined
      ? undefined
      : ({ ["--alixai-z-fullscreen"]: String(fullscreenZIndex), ["--alix-fullscreen-z"]: String(fullscreenZIndex) } as CSSProperties);
  return (
    <div data-testid="host">
      <button ref={toggleRef} type="button" aria-label={isFullscreen ? "Exit full screen" : "Full screen"} onClick={toggleFullscreen}>
        toggle
      </button>
      <div
        ref={rootRef}
        role={isFullscreen ? "dialog" : "region"}
        className={isFullscreen ? "alix-panel alix-panel--fullscreen" : "alix-panel"}
        {...(style !== undefined && { style })}
        {...(isFullscreen ? { "data-fullscreen": "" } : {})}
        tabIndex={-1}
      >
        <ul aria-label="messages">
          {messages.map((message) => (
            <li key={message}>{message}</li>
          ))}
        </ul>
        <input aria-label="draft" defaultValue="" />
        <span data-testid="state">{isFullscreen ? "open" : "closed"}</span>
      </div>
    </div>
  );
};

describe("fullscreen", () => {
  it("applies fullscreenZIndex to CSS custom properties", () => {
    render(
      <AlixProvider>
        <FullscreenProbe controlled fullscreenZIndex={3100} />
      </AlixProvider>,
    );
    fireEvent.click(screen.getByLabelText("Full screen"));
    const panel = screen.getByRole("dialog");
    expect(panel.style.getPropertyValue("--alixai-z-fullscreen")).toBe("3100");
    expect(panel.style.getPropertyValue("--alix-fullscreen-z")).toBe("3100");
  });

  it("toggles without clearing the draft, keeps the message list, and exits on Escape", async () => {
    render(
      <AlixProvider>
        <FullscreenProbe messages={["one", "two"]} />
      </AlixProvider>,
    );
    const draft = screen.getByLabelText("draft") as HTMLInputElement;
    fireEvent.change(draft, { target: { value: "keep me" } });
    fireEvent.click(screen.getByLabelText("Full screen"));
    expect(screen.getByTestId("state").textContent).toBe("open");
    expect(screen.getByRole("dialog").hasAttribute("data-fullscreen")).toBe(true);
    expect((screen.getByLabelText("draft") as HTMLInputElement).value).toBe("keep me");
    expect(screen.getByLabelText("messages").textContent).toContain("one");
    expect(screen.getByLabelText("messages").textContent).toContain("two");
    // Node stays under the React host — never moved to document.body.
    expect(screen.getByRole("dialog").parentElement).toBe(screen.getByTestId("host"));
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(screen.getByTestId("state").textContent).toBe("closed"));
    expect(document.activeElement).toBe(screen.getByLabelText("Full screen"));
  });

  it("unmounts while fullscreen without throwing", () => {
    const view = render(
      <AlixProvider>
        <FullscreenProbe />
      </AlixProvider>,
    );
    fireEvent.click(screen.getByLabelText("Full screen"));
    expect(screen.getByTestId("state").textContent).toBe("open");
    expect(() => view.unmount()).not.toThrow();
  });

  it("re-renders while fullscreen when a new message arrives", () => {
    const Host = (): ReactElement => {
      const [messages, setMessages] = useState<string[]>(["first"]);
      return (
        <AlixProvider>
          <button type="button" onClick={() => setMessages((list) => [...list, `m${list.length + 1}`])}>
            add
          </button>
          <FullscreenProbe messages={messages} />
        </AlixProvider>
      );
    };
    render(<Host />);
    fireEvent.click(screen.getByLabelText("Full screen"));
    expect(screen.getByTestId("state").textContent).toBe("open");
    fireEvent.click(screen.getByText("add"));
    expect(screen.getByTestId("state").textContent).toBe("open");
    expect(screen.getByLabelText("messages").textContent).toContain("m2");
    expect((screen.getByLabelText("draft") as HTMLInputElement).value).toBe("");
    expect(screen.getByRole("dialog").parentElement).toBe(screen.getByTestId("host"));
  });
});

describe("export menus", () => {
  const block = {
    type: "table" as const,
    columns: [{ key: "n", label: "N", kind: "number" as const }],
    rows: [[1]],
  };

  it("hides download actions without an exporter", () => {
    render(
      <AlixProvider>
        <AlixExportsProvider value={{ enabled: true, sessionId: "ses_1" }}>
          <AlixTableBlock block={block} blockId="blk_1" />
        </AlixExportsProvider>
      </AlixProvider>,
    );
    expect(screen.queryByLabelText("Download this data")).toBeNull();
  });

  it("shows loading and error states", async () => {
    const exporter: AlixExporter = vi.fn(async () => {
      await new Promise((resolve) => setTimeout(resolve, 20));
      throw new Error("boom");
    });
    render(
      <AlixProvider>
        <AlixExportMenu
          exporter={exporter}
          label="Download this data"
          formats={["csv"]}
          params={{ sessionId: "ses_1", turnId: "run_1", blockIds: ["blk_1"] }}
        />
      </AlixProvider>,
    );
    fireEvent.click(screen.getByLabelText("Download this data"));
    fireEvent.click(screen.getByRole("menuitem"));
    expect(screen.getByLabelText("Download this data").getAttribute("aria-busy")).toBe("true");
    await waitFor(() => expect(screen.getByRole("alert")).toBeTruthy());
  });

  it("calls exporter and download for a file id", async () => {
    const download = vi.fn(async () => undefined);
    const exporter: AlixExporter = vi.fn(async () => ({ fileId: "file_1", name: "rows.csv" }));
    render(
      <AlixProvider>
        <AlixExportsProvider value={{ enabled: true, sessionId: "ses_1", exporter, download }}>
          <AlixTableBlock block={block} blockId="blk_1" turnId="run_1" />
        </AlixExportsProvider>
      </AlixProvider>,
    );
    fireEvent.click(screen.getByLabelText("Download this data"));
    fireEvent.click(screen.getByRole("menuitem", { name: "Download CSV" }));
    await waitFor(() => expect(exporter).toHaveBeenCalled());
    expect(exporter).toHaveBeenCalledWith({ sessionId: "ses_1", turnId: "run_1", blockIds: ["blk_1"], format: "csv" });
    expect(download).toHaveBeenCalledWith("file_1");
  });
});
