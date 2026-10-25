// Calendar arithmetic in a site's own time zone. Instants are stored in UTC;
// "today", "tomorrow at 09:30" and the display of a slot are always the site's
// local wall clock, decided here and never by the model.

const formatterCache = new Map<string, Intl.DateTimeFormat>();

const formatterFor = (timeZone: string): Intl.DateTimeFormat => {
  let formatter = formatterCache.get(timeZone);
  if (formatter === undefined) {
    formatter = new Intl.DateTimeFormat("en-CA", {
      timeZone,
      hourCycle: "h23",
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
    formatterCache.set(timeZone, formatter);
  }
  return formatter;
};

export type LocalParts = { year: number; month: number; day: number; hour: number; minute: number; second: number };

export const localParts = (instant: Date, timeZone: string): LocalParts => {
  const parts: Record<string, number> = {};
  for (const part of formatterFor(timeZone).formatToParts(instant)) {
    if (part.type !== "literal") {
      parts[part.type] = Number(part.value);
    }
  }
  return {
    year: parts.year ?? 0,
    month: parts.month ?? 0,
    day: parts.day ?? 0,
    hour: parts.hour ?? 0,
    minute: parts.minute ?? 0,
    second: parts.second ?? 0,
  };
};

const pad = (value: number, width = 2): string => String(value).padStart(width, "0");

export const localDate = (instant: Date, timeZone: string): string => {
  const p = localParts(instant, timeZone);
  return `${pad(p.year, 4)}-${pad(p.month)}-${pad(p.day)}`;
};

export const localTime = (instant: Date, timeZone: string): string => {
  const p = localParts(instant, timeZone);
  return `${pad(p.hour)}:${pad(p.minute)}`;
};

// "2026-10-14 10:30", language neutral.
export const localDisplay = (instant: Date, timeZone: string): string =>
  `${localDate(instant, timeZone)} ${localTime(instant, timeZone)}`;

export const DATE_PATTERN = /^(\d{4})-(\d{2})-(\d{2})$/;

export const isCalendarDate = (value: string): boolean => {
  const match = DATE_PATTERN.exec(value);
  if (match === null) {
    return false;
  }
  const [year, month, day] = [Number(match[1]), Number(match[2]), Number(match[3])];
  const probe = new Date(Date.UTC(year, month - 1, day));
  return probe.getUTCFullYear() === year && probe.getUTCMonth() === month - 1 && probe.getUTCDate() === day;
};

export const addDays = (dateText: string, days: number): string => {
  const match = DATE_PATTERN.exec(dateText);
  if (match === null) {
    throw new Error("addDays: not a date");
  }
  const base = new Date(Date.UTC(Number(match[1]), Number(match[2]) - 1, Number(match[3])));
  base.setUTCDate(base.getUTCDate() + days);
  return base.toISOString().slice(0, 10);
};

export const daysBetween = (fromDate: string, toDate: string): number => {
  const a = Date.parse(`${fromDate}T00:00:00Z`);
  const b = Date.parse(`${toDate}T00:00:00Z`);
  return Math.round((b - a) / 86_400_000);
};

// The UTC instant at which the site's wall clock reads dateText timeText.
// Two passes handle the offset change on a daylight-saving day.
export const zonedToUtc = (dateText: string, timeText: string, timeZone: string): Date => {
  const dateMatch = DATE_PATTERN.exec(dateText);
  const timeMatch = /^(\d{2}):(\d{2})$/.exec(timeText);
  if (dateMatch === null || timeMatch === null) {
    throw new Error("zonedToUtc: not a date and time");
  }
  const wantedAsUtc = Date.UTC(
    Number(dateMatch[1]),
    Number(dateMatch[2]) - 1,
    Number(dateMatch[3]),
    Number(timeMatch[1]),
    Number(timeMatch[2]),
  );
  let guess = wantedAsUtc;
  for (let pass = 0; pass < 2; pass += 1) {
    const p = localParts(new Date(guess), timeZone);
    const shownAsUtc = Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute);
    guess += wantedAsUtc - shownAsUtc;
  }
  return new Date(guess);
};
