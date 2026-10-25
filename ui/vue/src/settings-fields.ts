import type { MessageKey } from "./i18n";
import type { SettingsEffective, UserSettings } from "./sdk-types";

// The preferences a user can set for themselves (`UserSettings` of the frozen v1 schema, `PUT /v1/settings/user`). Each switch names
// the member it edits, the effective value to fall back to, and whether the deployment's or tenant's policy lets the user have it
// at all. A setting can only select among approved things, switch something off for itself or ask for MORE confirmation: there is
// no member here, and so no control, that enables an undeclared tool or lowers a confirmation.

export interface SwitchField {
  /** The dotted path inside UserSettings. */
  readonly path: string;
  readonly label: MessageKey;
  readonly hint?: MessageKey;
  /** Reads the effective value (what the three levels add up to) for a switch the user has not set. */
  readonly effective: (settings: SettingsEffective) => boolean | undefined;
  /** False when policy forbids the feature: the switch is shown disabled with the reason. */
  readonly allowed?: (settings: SettingsEffective) => boolean;
}

export interface SettingsSection {
  readonly id: string;
  readonly title: MessageKey;
  readonly fields: readonly SwitchField[];
}

export const SETTINGS_SECTIONS: readonly SettingsSection[] = [
  {
    id: "replies",
    title: "settings.section.replies",
    fields: [{ path: "streaming.enabled", label: "settings.streaming", hint: "settings.streaming.hint", effective: (s) => s.streaming.enabled }],
  },
  {
    id: "voice",
    title: "settings.section.voice",
    fields: [
      { path: "voice.enabled", label: "settings.voice.enabled", effective: (s) => s.voice.enabled, allowed: (s) => s.voice.allowed },
      { path: "voice.auto_play", label: "settings.voice.autoPlay", effective: (s) => s.voice.auto_play, allowed: (s) => s.voice.allowed },
      { path: "voice.barge_in", label: "settings.voice.bargeIn", hint: "settings.voice.bargeIn.hint", effective: (s) => s.voice.barge_in, allowed: (s) => s.voice.allowed },
    ],
  },
  {
    id: "privacy",
    title: "settings.section.privacy",
    fields: [
      { path: "privacy.store_transcript", label: "settings.privacy.transcript", hint: "settings.privacy.transcript.hint", effective: (s) => s.privacy.store_transcript },
      { path: "history.auto_cleanup", label: "settings.history.autoCleanup", effective: (s) => s.history.auto_cleanup },
      { path: "sources.include_attachments", label: "settings.sources.attachments", effective: (s) => s.sources.include_attachments },
    ],
  },
  {
    id: "attachments",
    title: "settings.section.attachments",
    fields: [{ path: "attachments.accept_uploads", label: "settings.attachments.accept", effective: (s) => s.attachments.enabled, allowed: (s) => s.attachments.allowed }],
  },
  {
    id: "notifications",
    title: "settings.section.notifications",
    fields: [{ path: "notifications.enabled", label: "settings.notifications.enabled", effective: (s) => s.notifications.enabled, allowed: (s) => s.notifications.allowed }],
  },
  {
    id: "accessibility",
    title: "settings.section.accessibility",
    fields: [
      { path: "accessibility.reduced_motion", label: "settings.a11y.reducedMotion", effective: (s) => s.accessibility.reduced_motion },
      { path: "accessibility.high_contrast", label: "settings.a11y.highContrast", effective: (s) => s.accessibility.high_contrast },
      { path: "accessibility.captions", label: "settings.a11y.captions", effective: (s) => s.accessibility.captions },
    ],
  },
];

/** The text sizes offered, as whole percentages (the schema allows 100 to 300). */
export const TEXT_SCALES = [100, 125, 150, 200] as const;

export const CITATION_DISPLAYS = ["inline", "footnote", "panel"] as const;

/** The risk levels from which the user may ask for an extra confirmation (the schema allows raising it, never lowering it). */
export const CONFIRMATION_LEVELS = [1, 2, 3] as const;

const segments = (path: string): string[] => path.split(".");

export const readPath = (settings: UserSettings | undefined, path: string): unknown => {
  let current: unknown = settings;
  for (const segment of segments(path)) {
    if (typeof current !== "object" || current === null) return undefined;
    current = (current as Record<string, unknown>)[segment];
  }
  return current;
};

/** Returns a copy of `settings` with the member at `path` set (or removed when `value` is undefined); empty groups are dropped. */
export const writePath = (settings: UserSettings, path: string, value: unknown): UserSettings => {
  const copy = JSON.parse(JSON.stringify(settings)) as Record<string, unknown>;
  const parts = segments(path);
  const last = parts[parts.length - 1] as string;
  let cursor = copy;
  const trail: Array<[Record<string, unknown>, string]> = [];
  for (const part of parts.slice(0, -1)) {
    const next = cursor[part];
    const child = typeof next === "object" && next !== null && !Array.isArray(next) ? (next as Record<string, unknown>) : {};
    cursor[part] = child;
    trail.push([cursor, part]);
    cursor = child;
  }
  if (value === undefined) delete cursor[last];
  else cursor[last] = value;
  for (let i = trail.length - 1; i >= 0; i -= 1) {
    const [parent, key] = trail[i] as [Record<string, unknown>, string];
    if (Object.keys(parent[key] as Record<string, unknown>).length === 0) delete parent[key];
  }
  return copy as UserSettings;
};
