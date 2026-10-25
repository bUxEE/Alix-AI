import { createContext, useContext, useMemo } from "react";
import type { ReactElement, ReactNode } from "react";
import { resolveMessages } from "./messages.js";
import type { AlixMessages, DeepPartial } from "./messages.js";

export interface AlixIntl {
  readonly locale: string;
  readonly messages: AlixMessages;
  /** Fixes the zone dates are shown in (an IANA name). Set it when the page is rendered on a server, so both renders agree. */
  readonly timeZone?: string;
}

const DEFAULT_INTL: AlixIntl = { locale: "en", messages: resolveMessages("en") };

const IntlContext = createContext<AlixIntl>(DEFAULT_INTL);

export interface AlixProviderProps {
  /** BCP 47 tag. English and Italian are built in; any other language needs `messages` for the words it changes. */
  locale?: string;
  /** Replaces single words or whole groups; everything not named keeps the built-in text. */
  messages?: DeepPartial<AlixMessages>;
  timeZone?: string;
  children?: ReactNode;
}

/** Sets the language of the kit for everything below it. The components also work without it (English). */
export const AlixProvider = ({ locale = "en", messages, timeZone, children }: AlixProviderProps): ReactElement => {
  const value = useMemo<AlixIntl>(
    () => ({ locale, messages: resolveMessages(locale, messages), ...(timeZone !== undefined && { timeZone }) }),
    [locale, messages, timeZone],
  );
  return <IntlContext.Provider value={value}>{children}</IntlContext.Provider>;
};

export const useAlixIntl = (): AlixIntl => useContext(IntlContext);

/** The words of the kit for the current language. */
export const useAlixText = (): AlixMessages => useContext(IntlContext).messages;

export const formatDateTime = (intl: AlixIntl, iso: string): string => {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  try {
    return new Intl.DateTimeFormat(intl.locale, {
      dateStyle: "medium",
      timeStyle: "short",
      ...(intl.timeZone !== undefined && { timeZone: intl.timeZone }),
    }).format(date);
  } catch {
    return iso;
  }
};

export const formatNumber = (intl: AlixIntl, value: number): string => {
  try {
    return new Intl.NumberFormat(intl.locale).format(value);
  } catch {
    return String(value);
  }
};

export const formatBytes = (intl: AlixIntl, bytes: number): string => {
  const units = ["B", "KB", "MB", "GB"] as const;
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const shown = unit === 0 ? String(Math.round(value)) : new Intl.NumberFormat(intl.locale, { maximumFractionDigits: 1 }).format(value);
  return `${shown} ${units[unit]}`;
};
