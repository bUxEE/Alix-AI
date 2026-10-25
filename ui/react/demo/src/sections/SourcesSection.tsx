import { useState } from "react";
import type { ReactElement } from "react";
import { AlixSourceHealthPanel } from "@alixai/react";
import type { types } from "@alixai/sdk";
import { DEMO_PATHS, requestJson, useResource } from "../lib/api";
import { fill } from "../lib/text";
import { useShell } from "../shell/context";
import { Segmented } from "../shell/Segmented";

type Preview = "live" | "loading" | "failed";

interface Reaudit {
  readonly checked: number;
  readonly queued: number;
  readonly failed: number;
}

export const SourcesSection = (): ReactElement => {
  const { text, notify } = useShell();
  const [preview, setPreview] = useState<Preview>("live");
  const [auditing, setAuditing] = useState(false);
  // `GET /v1/rag/sources` lists each source with its declaration; the panel takes the statuses.
  const sources = useResource((signal) =>
    requestJson<{ sources: { status: types.rag.SourceStatus }[] }>(DEMO_PATHS.ragSources, { signal }).then((body) => body.sources.map((entry) => entry.status)),
  );

  const reaudit = async (): Promise<void> => {
    setAuditing(true);
    try {
      const result = await requestJson<Reaudit>(DEMO_PATHS.ragReaudit, { method: "POST" });
      notify("POST reaudit", fill(text.sources.reaudited, { checked: result.checked, queued: result.queued, failed: result.failed }));
    } finally {
      setAuditing(false);
      sources.reload();
    }
  };

  return (
    <div className="demo-stack">
      <div className="demo-toolbar">
        <Segmented
          label={text.sources.preview}
          value={preview}
          options={[
            { value: "live", label: text.sources.live },
            { value: "loading", label: text.sources.loading },
            { value: "failed", label: text.sources.failed },
          ]}
          onChange={setPreview}
        />
        <button type="button" className="demo-button" disabled={auditing || preview !== "live"} onClick={() => void reaudit()}>
          {text.sources.reaudit}
        </button>
      </div>
      <div className="demo-frame demo-frame--pad">
        {preview === "live" ? (
          <AlixSourceHealthPanel
            {...(sources.data !== undefined && { sources: sources.data })}
            loading={sources.loading || auditing}
            failed={sources.error !== undefined}
            onRefresh={sources.reload}
          />
        ) : (
          // The two other states, shown on their own panel so the live one keeps its data.
          <AlixSourceHealthPanel key={preview} loading={preview === "loading"} failed={preview === "failed"} onRefresh={() => setPreview("live")} />
        )}
      </div>
    </div>
  );
};
