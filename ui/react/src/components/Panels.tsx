import { useEffect, useId, useState } from "react";
import type { FormEvent, ReactElement, ReactNode } from "react";
import type { types } from "@alixai/sdk";
import { fill } from "../i18n/messages.js";
import { formatDateTime, formatNumber, useAlixIntl } from "../i18n/context.js";
import { AlixLoadError, AlixLoadable, AlixRefreshButton, AlixSkeleton, useAlixLoadState } from "./Loadable.js";

type SourceStatus = types.rag.SourceStatus;
type EffectiveSettings = types.settings.EffectiveSettings;
type UserSettings = types.settings.UserSettings;

// ---- source health ----

export interface AlixSourceHealthPanelProps {
  /** The status of each registered source, as the core reports it (`SourceStatus`). Undefined until the host has them. */
  sources?: readonly SourceStatus[];
  /** Replaces the "Sources" title. */
  title?: string;
  /** The host is fetching the statuses: a skeleton the first time, a scrim over the list after that. */
  loading?: boolean;
  /** The last fetch failed. Before any success the panel shows the error with a way to try again. */
  failed?: boolean;
  /** Fetch the statuses again. Shown as a refresh button (and as "Try again" after a failed first load) only when given. */
  onRefresh?: () => void;
}

const stateTone = (source: SourceStatus): "ok" | "warn" | "danger" | undefined => {
  if (source.error !== undefined || source.counts.failed > 0) return "danger";
  if (source.state === "ready") return "ok";
  if (source.state === "paused" || source.state === "disabled" || source.state === "removing") return "warn";
  return undefined;
};

const SourceSkeleton = (): ReactElement => (
  <ul className="alix-health__list" aria-hidden="true">
    {[0, 1, 2].map((row) => (
      <li key={row} className="alix-health__row">
        <AlixSkeleton width="45%" />
        <AlixSkeleton width="70%" height="0.75rem" />
      </li>
    ))}
  </ul>
);

/**
 * Which sources the assistant can use, whether each is indexed and current, and what went wrong where something did. Given
 * `onRefresh` (with `loading` and `failed`) it also refetches: the host fetches, the panel shows; it decides and changes nothing.
 */
export const AlixSourceHealthPanel = ({ sources, title, loading = false, failed = false, onRefresh }: AlixSourceHealthPanelProps): ReactElement => {
  const intl = useAlixIntl();
  const text = intl.messages.sourceHealth;
  const titleId = useId();
  const state = useAlixLoadState(loading, failed, sources !== undefined);
  const ready = (sources ?? []).filter((source) => source.state === "ready" && source.error === undefined).length;
  return (
    <section className="alix-health" data-alixai="source-health" aria-labelledby={titleId}>
      <div className="alix-panel-head">
        <div>
          <h3 id={titleId} className="alix-health__title">
            {title ?? text.title}
          </h3>
          {onRefresh !== undefined && sources !== undefined && sources.length > 0 && (
            <p className="alix-health__meta">{fill(intl.messages.sourceHealthStatus.summary, { ready, total: sources.length })}</p>
          )}
        </div>
        {onRefresh !== undefined && <AlixRefreshButton busy={loading} onRefresh={onRefresh} />}
      </div>
      <AlixLoadable state={state} skeleton={<SourceSkeleton />}>
        {failed && !state.hasLoadedOnce ? (
          <AlixLoadError message={intl.messages.sourceHealthStatus.failed} onRetry={onRefresh} />
        ) : sources === undefined || sources.length === 0 ? (
          <p className="alix-health__meta">{text.empty}</p>
        ) : (
          <ul className="alix-health__list">
            {sources.map((source) => {
              const tone = stateTone(source);
              return (
                <li key={source.source_id} className="alix-health__row" data-state={source.state}>
                  <div className="alix-health__head">
                    <span>{source.display_name}</span>
                    <span className="alix-health__state" {...(tone !== undefined && { "data-tone": tone })}>
                      {text.states[source.state]}
                    </span>
                    {source.staleness === "stale" && (
                      <span className="alix-health__state" data-tone="warn">
                        {text.stale}
                      </span>
                    )}
                    {source.availability !== "available" && (
                      <span className="alix-health__state" data-tone="danger">
                        {intl.messages.citations.availability[source.availability]}
                      </span>
                    )}
                  </div>
                  <dl className="alix-health__counts">
                    {(["discovered", "indexed", "skipped", "failed", "tombstoned"] as const).map((key) => (
                      <div key={key}>
                        <dt>{text.counts[key]}</dt>
                        <dd>{formatNumber(intl, source.counts[key])}</dd>
                      </div>
                    ))}
                  </dl>
                  <p className="alix-health__meta">
                    {text.lastSuccess}:{" "}
                    {source.last_success_at === null ? text.never : <time dateTime={source.last_success_at}>{formatDateTime(intl, source.last_success_at)}</time>}
                  </p>
                  {source.error !== undefined && (
                    <p className="alix-health__error" role="alert">
                      {source.error.message}
                    </p>
                  )}
                </li>
              );
            })}
          </ul>
        )}
      </AlixLoadable>
    </section>
  );
};

// ---- settings ----

export interface AlixSettingsPanelProps {
  /**
   * The effective settings of the caller (the core computes them from the deployment, tenant and user levels). Undefined until
   * the host has them.
   */
  settings?: EffectiveSettings;
  title?: string;
  /**
   * The user's own settings document (`GET /v1/settings/user`, its `settings`). Given, the panel becomes the editor of that
   * document and reports `onSave` with the whole document and the version it replaces. Without it the panel is read only.
   */
  user?: UserSettings;
  /** The version of `user` that was read (0 when the user has no document yet). */
  version?: number;
  /** The host is writing the document: the save button waits. */
  saving?: boolean;
  /** The host is fetching the settings: a skeleton the first time, a scrim after that. */
  loading?: boolean;
  /** The last fetch failed. Before any success the panel shows the error with a way to try again. */
  failed?: boolean;
  /** The person saved: the whole document and the version it replaces. The host writes it with `settings.putUser`. */
  onSave?: (settings: UserSettings, expectedVersion: number) => void;
  /** The person asked to clear this conversation's history. Shown only when given (and only in the editor). */
  onClearHistory?: () => void;
  /** Fetch the settings again after a failed first load. */
  onRetry?: () => void;
  /** The languages offered, by their own names. Default: English and Italiano. */
  languages?: readonly { value: string; label: string }[];
  /** Anything the host adds under the groups, for example its own "Edit" control. The kit changes no setting itself. */
  children?: ReactNode;
}

interface Row {
  readonly label: string;
  readonly value: string;
  /** The `decided_by` key of the value, when more than one level can state it. */
  readonly decided?: string;
}

/** The effective settings, grouped, with which level decided each value that more than one level can state. */
const ReadOnlySettings = ({ settings }: { settings: EffectiveSettings }): ReactElement => {
  const intl = useAlixIntl();
  const text = intl.messages.settings;
  const flag = (value: boolean): string => (value ? text.on : text.off);
  const rows = text.rows;
  const groups: { id: keyof typeof text.groups; rows: Row[] }[] = [
    {
      id: "general",
      rows: [
        ...(settings.language !== undefined ? [{ label: rows.language, value: settings.language, decided: "language" }] : []),
        { label: rows.modelProfile, value: settings.model_profile, decided: "model_profile" },
      ],
    },
    {
      id: "history",
      rows: [
        { label: rows.retentionDays, value: formatNumber(intl, settings.history.retention_days), decided: "history.retention_days" },
        { label: rows.autoCleanup, value: flag(settings.history.auto_cleanup) },
        { label: rows.exportAllowed, value: flag(settings.history.export_allowed), decided: "history.export_allowed" },
      ],
    },
    { id: "privacy", rows: [{ label: rows.storeTranscript, value: flag(settings.privacy.store_transcript), decided: "privacy.store_transcript" }] },
    {
      id: "attachments",
      rows: [
        { label: rows.attachmentsEnabled, value: flag(settings.attachments.enabled), decided: "attachments.enabled" },
        { label: rows.maxFileMb, value: formatNumber(intl, settings.attachments.max_file_mb), decided: "attachments.max_file_mb" },
      ],
    },
    {
      id: "sources",
      rows: [
        { label: rows.includeAttachments, value: flag(settings.sources.include_attachments) },
        { label: rows.citationDisplay, value: text.values[settings.sources.citation_display] },
      ],
    },
    { id: "voice", rows: [{ label: rows.voiceEnabled, value: flag(settings.voice.enabled), decided: "voice.enabled" }] },
    { id: "notifications", rows: [{ label: rows.notificationsEnabled, value: flag(settings.notifications.enabled), decided: "notifications.enabled" }] },
    { id: "streaming", rows: [{ label: rows.streamUnit, value: text.values[settings.streaming.unit] }] },
    {
      id: "accessibility",
      rows: [
        { label: rows.reducedMotion, value: flag(settings.accessibility.reduced_motion) },
        { label: rows.highContrast, value: flag(settings.accessibility.high_contrast) },
        { label: rows.textScale, value: formatNumber(intl, settings.accessibility.text_scale_pct) },
        { label: rows.screenReader, value: flag(settings.accessibility.screen_reader) },
        { label: rows.captions, value: flag(settings.accessibility.captions) },
      ],
    },
  ];
  return (
    <>
      {groups.map((group) => (
        <section key={group.id} className="alix-settings__group" aria-label={text.groups[group.id]}>
          <h4 className="alix-settings__group-title">{text.groups[group.id]}</h4>
          <dl className="alix-settings__list">
            {group.rows.map((row) => {
              const level = row.decided === undefined ? undefined : settings.decided_by[row.decided];
              return (
                <div key={row.label}>
                  <dt>{row.label}</dt>
                  <dd>
                    {row.value}
                    {level !== undefined && <span className="alix-settings__source">{text.decidedBy[level]}</span>}
                  </dd>
                </div>
              );
            })}
          </dl>
        </section>
      ))}
    </>
  );
};

// The preferences a user can set for themselves (`UserSettings`, `PUT /v1/settings/user`), as in the Vue kit. A setting can only
// select among approved things, switch something off for oneself or ask for MORE confirmation: no control here lowers a
// protection. A feature the policy forbids is shown switched off with the reason, never hidden.

type FieldKey = keyof ReturnType<typeof useAlixIntl>["messages"]["settingsEditor"]["fields"];
type SectionKey = keyof ReturnType<typeof useAlixIntl>["messages"]["settingsEditor"]["sections"];

interface SwitchField {
  readonly path: string;
  readonly label: FieldKey;
  readonly hint?: FieldKey;
  readonly effective: (settings: EffectiveSettings) => boolean | undefined;
  readonly allowed?: (settings: EffectiveSettings) => boolean;
}

const SECTIONS: readonly { id: SectionKey; fields: readonly SwitchField[] }[] = [
  { id: "replies", fields: [{ path: "streaming.enabled", label: "streaming", hint: "streamingHint", effective: (s) => s.streaming.enabled }] },
  {
    id: "voice",
    fields: [
      { path: "voice.enabled", label: "voiceEnabled", effective: (s) => s.voice.enabled, allowed: (s) => s.voice.allowed },
      { path: "voice.auto_play", label: "autoPlay", effective: (s) => s.voice.auto_play, allowed: (s) => s.voice.allowed },
      { path: "voice.barge_in", label: "bargeIn", hint: "bargeInHint", effective: (s) => s.voice.barge_in, allowed: (s) => s.voice.allowed },
    ],
  },
  {
    id: "privacy",
    fields: [
      { path: "privacy.store_transcript", label: "transcript", hint: "transcriptHint", effective: (s) => s.privacy.store_transcript },
      { path: "history.auto_cleanup", label: "autoCleanup", effective: (s) => s.history.auto_cleanup },
      { path: "sources.include_attachments", label: "includeAttachments", effective: (s) => s.sources.include_attachments },
    ],
  },
  { id: "attachments", fields: [{ path: "attachments.accept_uploads", label: "acceptUploads", effective: (s) => s.attachments.enabled, allowed: (s) => s.attachments.allowed }] },
  { id: "notifications", fields: [{ path: "notifications.enabled", label: "notifications", effective: (s) => s.notifications.enabled, allowed: (s) => s.notifications.allowed }] },
  {
    id: "accessibility",
    fields: [
      { path: "accessibility.reduced_motion", label: "reducedMotion", effective: (s) => s.accessibility.reduced_motion },
      { path: "accessibility.high_contrast", label: "highContrast", effective: (s) => s.accessibility.high_contrast },
      { path: "accessibility.captions", label: "captions", effective: (s) => s.accessibility.captions },
    ],
  },
];

const TEXT_SCALES = [100, 125, 150, 200] as const;
const CITATION_DISPLAYS = ["inline", "footnote", "panel"] as const;
const CONFIRMATION_LEVELS = [1, 2, 3] as const;
const DEFAULT_LANGUAGES = [
  { value: "en", label: "English" },
  { value: "it", label: "Italiano" },
] as const;

const clone = (value: UserSettings | undefined): UserSettings => JSON.parse(JSON.stringify(value ?? {})) as UserSettings;

const readPath = (settings: UserSettings, path: string): unknown => {
  let current: unknown = settings;
  for (const segment of path.split(".")) {
    if (typeof current !== "object" || current === null) return undefined;
    current = (current as Record<string, unknown>)[segment];
  }
  return current;
};

/** A copy of `settings` with the member at `path` set (or removed when `value` is undefined); emptied groups are dropped. */
const writePath = (settings: UserSettings, path: string, value: unknown): UserSettings => {
  const copy = clone(settings) as Record<string, unknown>;
  const parts = path.split(".");
  const last = parts[parts.length - 1] as string;
  let cursor = copy;
  const trail: [Record<string, unknown>, string][] = [];
  for (const part of parts.slice(0, -1)) {
    const next = cursor[part];
    const child = typeof next === "object" && next !== null && !Array.isArray(next) ? (next as Record<string, unknown>) : {};
    cursor[part] = child;
    trail.push([cursor, part]);
    cursor = child;
  }
  if (value === undefined) delete cursor[last];
  else cursor[last] = value;
  for (let index = trail.length - 1; index >= 0; index -= 1) {
    const [parent, key] = trail[index] as [Record<string, unknown>, string];
    if (Object.keys(parent[key] as Record<string, unknown>).length === 0) delete parent[key];
  }
  return copy as UserSettings;
};

const Switch = ({ checked, disabled, label, hint, onChange }: { checked: boolean; disabled: boolean; label: string; hint?: string | undefined; onChange: (on: boolean) => void }): ReactElement => {
  const hintId = useId();
  return (
    <label className="alix-switch" {...(disabled && { "data-disabled": "" })}>
      <span className="alix-switch__text">
        <span className="alix-switch__label">{label}</span>
        {hint !== undefined && hint !== "" && (
          <span id={hintId} className="alix-switch__hint">
            {hint}
          </span>
        )}
      </span>
      <input
        type="checkbox"
        role="switch"
        className="alix-switch__input"
        checked={checked}
        disabled={disabled}
        {...(hint !== undefined && hint !== "" && { "aria-describedby": hintId })}
        onChange={(event) => onChange(event.target.checked)}
      />
    </label>
  );
};

const Select = ({ label, value, hint, onChange, children }: { label: string; value: string | number; hint?: string; onChange: (raw: string) => void; children: ReactNode }): ReactElement => (
  <label className="alix-field">
    <span className="alix-field__label">{label}</span>
    <select className="alix-field__control no-validation" value={value} onChange={(event) => onChange(event.target.value)}>
      {children}
    </select>
    {hint !== undefined && <span className="alix-switch__hint">{hint}</span>}
  </label>
);

const SettingsEditor = ({
  settings,
  user,
  version,
  saving,
  onSave,
  onClearHistory,
  languages,
  children,
}: {
  settings: EffectiveSettings;
  user: UserSettings;
  version: number;
  saving: boolean;
  onSave?: AlixSettingsPanelProps["onSave"];
  onClearHistory?: (() => void) | undefined;
  languages: readonly { value: string; label: string }[];
  children?: ReactNode;
}): ReactElement => {
  const intl = useAlixIntl();
  const text = intl.messages.settingsEditor;
  const common = intl.messages.common;
  const [draft, setDraft] = useState<UserSettings>(() => clone(user));
  // A new document from the host (saved, or read again) replaces the draft.
  useEffect(() => setDraft(clone(user)), [user]);
  const dirty = JSON.stringify(draft) !== JSON.stringify(user);
  const set = (path: string, value: unknown): void => setDraft((current) => writePath(current, path, value));
  const valueOf = (field: SwitchField): boolean => {
    const own = readPath(draft, field.path);
    return typeof own === "boolean" ? own : (field.effective(settings) ?? false);
  };
  const allowed = (field: SwitchField): boolean => field.allowed === undefined || field.allowed(settings);
  const language = (readPath(draft, "language") as string | undefined) ?? "";
  const display = (readPath(draft, "sources.citation_display") as string | undefined) ?? settings.sources.citation_display;
  const scale = (readPath(draft, "accessibility.text_scale_pct") as number | undefined) ?? settings.accessibility.text_scale_pct;
  const level = (readPath(draft, "confirmation.extra_from_risk_level") as number | undefined) ?? "";
  const submit = (event: FormEvent<HTMLFormElement>): void => {
    event.preventDefault();
    if (dirty && !saving) onSave?.(draft, version);
  };
  return (
    <form className="alix-settings__form" onSubmit={submit}>
      <div className="alix-settings__sections">
        {SECTIONS.map((section) => (
          <fieldset key={section.id} className="alix-settings__fieldset">
            <legend className="alix-settings__legend">{text.sections[section.id]}</legend>
            {section.fields.map((field) => (
              <Switch
                key={field.path}
                checked={valueOf(field)}
                disabled={!allowed(field)}
                label={text.fields[field.label]}
                hint={!allowed(field) ? text.policyOff : field.hint !== undefined ? text.fields[field.hint] : undefined}
                onChange={(on) => set(field.path, on)}
              />
            ))}
            {section.id === "replies" && (
              <>
                <Select label={text.language} value={language} onChange={(raw) => set("language", raw === "" ? undefined : raw)}>
                  <option value="">{text.default}</option>
                  {languages.map((entry) => (
                    <option key={entry.value} value={entry.value}>
                      {entry.label}
                    </option>
                  ))}
                </Select>
                <Select label={text.citations} value={display} onChange={(raw) => set("sources.citation_display", raw)}>
                  {CITATION_DISPLAYS.map((entry) => (
                    <option key={entry} value={entry}>
                      {text.citationValues[entry]}
                    </option>
                  ))}
                </Select>
              </>
            )}
            {section.id === "accessibility" && (
              <Select label={text.fields.textScale} value={scale} onChange={(raw) => set("accessibility.text_scale_pct", Number(raw))}>
                {TEXT_SCALES.map((entry) => (
                  <option key={entry} value={entry}>
                    {entry}%
                  </option>
                ))}
              </Select>
            )}
            {section.id === "privacy" && (
              <>
                <p className="alix-switch__hint">{fill(text.retention, { days: formatNumber(intl, settings.history.retention_days) })}</p>
                <Select
                  label={text.confirmation}
                  value={level}
                  hint={text.confirmationHint}
                  onChange={(raw) => set("confirmation.extra_from_risk_level", raw === "" ? undefined : Number(raw))}
                >
                  <option value="">{text.default}</option>
                  {CONFIRMATION_LEVELS.map((entry) => (
                    <option key={entry} value={entry}>
                      {fill(text.confirmationFrom, { level: entry })}
                    </option>
                  ))}
                </Select>
                {onClearHistory !== undefined && (
                  <button type="button" className="alix-settings__danger" onClick={onClearHistory}>
                    {text.clearHistory}
                  </button>
                )}
              </>
            )}
          </fieldset>
        ))}
      </div>
      {children}
      <div className="alix-settings__actions">
        <button type="button" className="alix-settings__button" disabled={!dirty || saving} onClick={() => setDraft(clone(user))}>
          {common.discard}
        </button>
        <button type="submit" className="alix-settings__button" data-variant="primary" disabled={!dirty || saving} aria-busy={saving}>
          {saving && <span className="alix-loadable__spinner" aria-hidden="true" />}
          {common.save}
        </button>
      </div>
    </form>
  );
};

const SettingsSkeleton = (): ReactElement => (
  <div className="alix-settings__sections" aria-hidden="true">
    {[0, 1, 2, 3].map((index) => (
      <div key={index} className="alix-settings__fieldset">
        <AlixSkeleton width="40%" />
        <AlixSkeleton />
        <AlixSkeleton width="80%" />
      </div>
    ))}
  </div>
);

/**
 * Settings and privacy controls. Given only the effective settings it is a read-only view of what the deployment, the tenant and
 * the user add up to. Given the user's own document as well (`user`, with its `version`) it is the editor of that document.
 */
export const AlixSettingsPanel = ({
  settings,
  title,
  user,
  version = 0,
  saving = false,
  loading = false,
  failed = false,
  onSave,
  onClearHistory,
  onRetry,
  languages = DEFAULT_LANGUAGES,
  children,
}: AlixSettingsPanelProps): ReactElement => {
  const intl = useAlixIntl();
  const text = intl.messages.settings;
  const titleId = useId();
  const state = useAlixLoadState(loading, failed, settings !== undefined);
  const body = (): ReactNode => {
    if (failed && !state.hasLoadedOnce) return <AlixLoadError message={intl.messages.settingsEditor.failed} onRetry={onRetry} />;
    if (settings === undefined) return null;
    if (user === undefined)
      return (
        <>
          <ReadOnlySettings settings={settings} />
          {children}
        </>
      );
    return (
      <SettingsEditor settings={settings} user={user} version={version} saving={saving} onClearHistory={onClearHistory} languages={languages} {...(onSave !== undefined && { onSave })}>
        {children}
      </SettingsEditor>
    );
  };
  return (
    <section className="alix-settings" data-alixai="settings" {...(user !== undefined && { "data-mode": "edit" })} aria-labelledby={titleId}>
      <h3 id={titleId} className="alix-settings__title">
        {title ?? text.title}
      </h3>
      <AlixLoadable state={state} skeleton={<SettingsSkeleton />}>
        {body()}
      </AlixLoadable>
    </section>
  );
};
