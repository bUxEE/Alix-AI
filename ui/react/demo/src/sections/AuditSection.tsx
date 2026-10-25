import type { ReactElement } from "react";
import { AlixIntegrationAuditPanel } from "@alixai/react";
import type { SettingsAuditStatus, SettingsProviderHealth } from "@alixai/sdk";
import { DEMO_PATHS, requestJson, useResource } from "../lib/api";

interface Loaded {
  readonly audit: SettingsAuditStatus;
  readonly providers: SettingsProviderHealth;
}

export const AuditSection = (): ReactElement => {
  const loaded = useResource(async (signal): Promise<Loaded> => {
    const [audit, providers] = await Promise.all([
      requestJson<SettingsAuditStatus>(DEMO_PATHS.integrationAudit, { signal }),
      requestJson<SettingsProviderHealth>(DEMO_PATHS.providerHealth, { signal }),
    ]);
    return { audit, providers };
  });
  return (
    <div className="demo-frame demo-frame--pad demo-narrow">
      <AlixIntegrationAuditPanel
        {...(loaded.data !== undefined && { audit: loaded.data.audit, providers: loaded.data.providers })}
        loading={loaded.loading}
        failed={loaded.error !== undefined}
        onRefresh={loaded.reload}
      />
    </div>
  );
};
