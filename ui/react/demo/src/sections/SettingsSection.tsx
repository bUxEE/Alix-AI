import { useState } from "react";
import type { ReactElement } from "react";
import { AlixSettingsPanel } from "@alixai/react";
import type { SettingsEffective, SettingsUserRecord, types } from "@alixai/sdk";
import { DEMO_PATHS, isHttpError, requestJson, useResource } from "../lib/api";
import { useShell } from "../shell/context";
import { Segmented } from "../shell/Segmented";

interface Loaded {
  readonly effective: SettingsEffective;
  readonly user: SettingsUserRecord;
}

export const SettingsSection = (): ReactElement => {
  const { text, notify } = useShell();
  const [mode, setMode] = useState<"edit" | "read">("edit");
  const [saving, setSaving] = useState(false);
  const [problem, setProblem] = useState<string | undefined>(undefined);
  const settings = useResource(async (signal): Promise<Loaded> => {
    const [effective, user] = await Promise.all([
      requestJson<SettingsEffective>(DEMO_PATHS.settingsEffective, { signal }),
      requestJson<SettingsUserRecord>(DEMO_PATHS.settingsUser, { signal }),
    ]);
    return { effective, user };
  });

  const save = async (next: types.settings.UserSettings, expectedVersion: number): Promise<void> => {
    setSaving(true);
    setProblem(undefined);
    try {
      const record = await requestJson<SettingsUserRecord>(DEMO_PATHS.settingsUser, { method: "PUT", body: JSON.stringify({ expected_version: expectedVersion, settings: next }) });
      notify("onSave", `${text.settings.saved} (v${record.version})`);
    } catch (error) {
      setProblem(isHttpError(error) && error.status === 409 ? text.settings.conflict : text.settings.failed);
    } finally {
      setSaving(false);
      settings.reload();
    }
  };

  return (
    <div className="demo-stack">
      <div className="demo-toolbar">
        <Segmented
          label={text.settings.mode}
          value={mode}
          options={[
            { value: "edit", label: text.settings.edit },
            { value: "read", label: text.settings.readOnly },
          ]}
          onChange={setMode}
        />
        {settings.data !== undefined && (
          <p className="demo-toolbar__meta">
            PUT /v1/settings/user <code>expected_version {settings.data.user.version}</code>
          </p>
        )}
      </div>
      {problem !== undefined && (
        <p className="demo-note" data-tone="warn" role="alert">
          {problem}
        </p>
      )}
      <div className="demo-frame demo-frame--pad">
        <AlixSettingsPanel
          {...(settings.data !== undefined && { settings: settings.data.effective })}
          {...(mode === "edit" && settings.data !== undefined && { user: settings.data.user.settings, version: settings.data.user.version })}
          loading={settings.loading}
          failed={settings.error !== undefined}
          saving={saving}
          onSave={(next, version) => void save(next, version)}
          onClearHistory={() => notify("onClearHistory", text.settings.cleared)}
          onRetry={settings.reload}
        />
      </div>
    </div>
  );
};
