import { computed, inject, provide, toValue, type ComputedRef, type InjectionKey, type MaybeRefOrGetter } from "vue";
import { en, type MessageKey } from "./en";
import { it } from "./it";

// Localisation for the kit's own words. English and Italian ship; a host adds or overrides any message for any locale through
// `provideAlix({ messages })`, and the typed key catches a misspelt key at compile time. Application text (a capability's name,
// a source's name, the model's answer) is never translated here: it is data and arrives in the language it was written in.

export type { MessageKey } from "./en";
export type Messages = Record<MessageKey, string>;

export const BUILT_IN_MESSAGES: Readonly<Record<string, Messages>> = Object.freeze({ en, it });

export interface AlixConfigOptions {
  /** A BCP 47 tag (`it`, `it-IT`). Falls back from the full tag to its language, then to English. */
  locale?: MaybeRefOrGetter<string>;
  /** Extra or replacement messages, per locale. */
  messages?: Record<string, Partial<Messages>>;
  /** The clock, in milliseconds since the epoch. Tests replace it; the default is `Date.now`. */
  now?: () => number;
  /** The IANA zone dates are shown in. Defaults to the runtime's own. Set it to make server and browser output agree. */
  timeZone?: string;
}

export interface AlixConfig {
  locale: ComputedRef<string>;
  t: (key: MessageKey, params?: Record<string, string | number>) => string;
  /** The `.one` or `.other` form of a message for `count` (`plural("citations.count", 3)`). */
  plural: (base: "citations.count" | "sources.count", count: number) => string;
  formatDateTime: (value: string | number | Date) => string;
  formatTime: (value: string | number | Date) => string;
  formatSize: (bytes: number) => string;
  formatNumber: (value: number) => string;
  now: () => number;
}

const interpolate = (template: string, params: Record<string, string | number> | undefined): string =>
  params === undefined ? template : template.replace(/\{(\w+)\}/g, (whole, name: string) => (name in params ? String(params[name]) : whole));

const toDate = (value: string | number | Date): Date => (value instanceof Date ? value : new Date(value));

export const createAlixConfig = (options: AlixConfigOptions = {}): AlixConfig => {
  const locale = computed(() => toValue(options.locale) ?? "en");

  const catalogs = (tag: string): Array<Partial<Messages> | undefined> => {
    const primary = tag.toLowerCase().split("-")[0] ?? "en";
    return [
      options.messages?.[tag],
      options.messages?.[primary],
      BUILT_IN_MESSAGES[tag],
      BUILT_IN_MESSAGES[primary],
      en,
    ];
  };

  const t: AlixConfig["t"] = (key, params) => {
    for (const catalog of catalogs(locale.value)) {
      const found = catalog?.[key];
      if (found !== undefined) return interpolate(found, params);
    }
    return key;
  };

  const plural: AlixConfig["plural"] = (base, count) => {
    const form = new Intl.PluralRules(locale.value).select(count) === "one" ? "one" : "other";
    return t(`${base}.${form}` as MessageKey, { n: count });
  };

  const zone = options.timeZone === undefined ? {} : { timeZone: options.timeZone };
  const formatDateTime: AlixConfig["formatDateTime"] = (value) => {
    const date = toDate(value);
    return Number.isNaN(date.getTime()) ? "" : new Intl.DateTimeFormat(locale.value, { dateStyle: "medium", timeStyle: "short", ...zone }).format(date);
  };
  const formatTime: AlixConfig["formatTime"] = (value) => {
    const date = toDate(value);
    return Number.isNaN(date.getTime()) ? "" : new Intl.DateTimeFormat(locale.value, { timeStyle: "short", ...zone }).format(date);
  };
  const formatNumber: AlixConfig["formatNumber"] = (value) => new Intl.NumberFormat(locale.value).format(value);
  const formatSize: AlixConfig["formatSize"] = (bytes) => {
    if (!Number.isFinite(bytes) || bytes < 0) return "";
    const units = ["B", "KB", "MB", "GB"];
    let size = bytes;
    let unit = 0;
    while (size >= 1024 && unit < units.length - 1) {
      size /= 1024;
      unit += 1;
    }
    return `${new Intl.NumberFormat(locale.value, { maximumFractionDigits: unit === 0 ? 0 : 1 }).format(size)} ${units[unit]}`;
  };

  return { locale, t, plural, formatDateTime, formatTime, formatSize, formatNumber, now: options.now ?? Date.now };
};

const KEY: InjectionKey<AlixConfig> = Symbol("alixai.config");

let fallback: AlixConfig | undefined;

/** Provides the kit's configuration (locale, messages, clock) to every kit component below the caller. */
export const provideAlix = (options: AlixConfigOptions = {}): AlixConfig => {
  const config = createAlixConfig(options);
  provide(KEY, config);
  return config;
};

/** The configuration in force: the nearest provider's, or English with the real clock. */
export const useAlix = (): AlixConfig => inject(KEY, undefined) ?? (fallback ??= createAlixConfig());
