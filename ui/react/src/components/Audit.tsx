import { useId } from "react";
import type { ReactElement, ReactNode } from "react";
import type { SettingsAuditFinding, SettingsAuditStatus, SettingsProviderHealth } from "@alixai/sdk";
import { fill } from "../i18n/messages.js";
import { useAlixIntl } from "../i18n/context.js";
import { AlixLoadError, AlixLoadable, AlixRefreshButton, AlixSkeleton, useAlixLoadState } from "./Loadable.js";

// What the audit of the installed integration package found, and how the model and service roles are doing (the Vue kit's
// AlixIntegrationAuditPanel). The audit is the core's (`GET /v1/settings/integration-audit`), the provider health is
// `GET /v1/settings/providers`; the panel shows them and decides nothing. Findings are ordered critical, warning, info, each with
// the remediation it states. Every word of a finding is the audit's own data and is shown as text.

export interface AlixIntegrationAuditPanelProps {
  audit?: SettingsAuditStatus;
  providers?: SettingsProviderHealth;
  title?: string;
  /** The host is fetching: a skeleton the first time, a scrim after that. */
  loading?: boolean;
  /** The last fetch failed. Before any success the panel shows the error with a way to try again. */
  failed?: boolean;
  /** Fetch again. Shown as a refresh button only when given. */
  onRefresh?: () => void;
  /** Draws one finding your own way (the default shows severity, status, capability, message and remediation). */
  renderFinding?: (finding: SettingsAuditFinding) => ReactNode;
  /** Replaces the line shown when there are no findings. */
  empty?: ReactNode;
}

const ORDER = { critical: 0, warning: 1, info: 2 } as const;

const severityTone = (severity: SettingsAuditFinding["severity"]): "danger" | "warn" | undefined =>
  severity === "critical" ? "danger" : severity === "warning" ? "warn" : undefined;
const statusTone = (status: string): "ok" | "warn" | "danger" => (status === "ok" ? "ok" : status === "degraded" ? "warn" : "danger");
const outcomeTone = (outcome: SettingsAuditStatus["outcome"]): "ok" | "warn" | "danger" | undefined =>
  outcome === "clear" ? "ok" : outcome === "blocked" ? "danger" : outcome === "needs_acceptance" ? "warn" : undefined;

const Finding = ({ finding }: { finding: SettingsAuditFinding }): ReactElement => {
  const text = useAlixIntl().messages.audit;
  const tone = severityTone(finding.severity);
  return (
    <>
      <div className="alix-audit__tags">
        <span className="alix-audit__tag" {...(tone !== undefined && { "data-tone": tone })}>
          {text.severity[finding.severity]}
        </span>
        <span className="alix-audit__tag">{text.status[finding.status]}</span>
        {finding.capability !== undefined && <code className="alix-audit__code">{finding.capability}</code>}
      </div>
      <p className="alix-audit__message">{finding.message}</p>
      <p className="alix-audit__remedy">
        {text.remediation}: {finding.remediation}
      </p>
    </>
  );
};

const AuditSkeleton = (): ReactElement => (
  <div className="alix-audit__summary" aria-hidden="true">
    <AlixSkeleton width="35%" />
    <AlixSkeleton />
    <AlixSkeleton width="75%" />
  </div>
);

export const AlixIntegrationAuditPanel = ({ audit, providers, title, loading = false, failed = false, onRefresh, renderFinding, empty }: AlixIntegrationAuditPanelProps): ReactElement => {
  const intl = useAlixIntl();
  const text = intl.messages.audit;
  const titleId = useId();
  const state = useAlixLoadState(loading, failed, audit !== undefined);
  const findings = [...(audit?.findings ?? [])].sort((a, b) => ORDER[a.severity] - ORDER[b.severity]);
  const outcome = outcomeTone(audit?.outcome);
  return (
    <section className="alix-audit" data-alixai="integration-audit" aria-labelledby={titleId}>
      <div className="alix-panel-head">
        <h3 id={titleId} className="alix-audit__title">
          {title ?? text.title}
        </h3>
        {onRefresh !== undefined && <AlixRefreshButton busy={loading} onRefresh={onRefresh} />}
      </div>
      <AlixLoadable state={state} skeleton={<AuditSkeleton />}>
        {failed && !state.hasLoadedOnce ? (
          <AlixLoadError message={text.failed} onRetry={onRefresh} />
        ) : audit === undefined ? null : (
          <div className="alix-audit__body">
            <div className="alix-audit__summary">
              <div className="alix-audit__tags">
                <span className="alix-audit__tag" {...(outcome !== undefined && { "data-tone": outcome })}>
                  {audit.outcome !== undefined ? text.outcome[audit.outcome] : text.notAudited}
                </span>
                {audit.package_version !== undefined && <code className="alix-audit__code">{audit.package_version}</code>}
              </div>
              <p className="alix-audit__remedy">{fill(text.counts, { critical: audit.counts.critical, warnings: audit.counts.warnings, info: audit.counts.info })}</p>
            </div>
            {findings.length > 0 ? (
              <ul className="alix-audit__findings" aria-label={text.findings}>
                {findings.map((finding) => (
                  <li key={finding.key} className="alix-audit__finding" data-severity={finding.severity}>
                    {renderFinding !== undefined ? renderFinding(finding) : <Finding finding={finding} />}
                  </li>
                ))}
              </ul>
            ) : (
              (empty ?? <p className="alix-audit__remedy">{audit.audited ? text.noFindings : text.notAudited}</p>)
            )}
            {providers !== undefined && (
              <section className="alix-audit__providers" aria-label={text.providers}>
                <h4 className="alix-audit__subtitle">{text.providers}</h4>
                <ul className="alix-audit__roles">
                  {providers.roles.map((role) => (
                    <li key={role.role} className="alix-audit__role">
                      <span>{role.role}</span>
                      <span className="alix-audit__tag" data-tone={role.configured ? statusTone(role.status) : "warn"}>
                        {role.configured ? text.provider[role.status as keyof typeof text.provider] ?? role.status : text.provider.notConfigured}
                      </span>
                    </li>
                  ))}
                </ul>
              </section>
            )}
          </div>
        )}
      </AlixLoadable>
    </section>
  );
};
