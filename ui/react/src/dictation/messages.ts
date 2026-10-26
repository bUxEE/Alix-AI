import { useMemo } from "react";
import { useAlixIntl } from "../i18n/context.js";

export const dictationEn = {
  "dictation.start": "Dictate",
  "dictation.stop": "Stop dictation",
  "dictation.cancel": "Cancel dictation",
  "dictation.recording": "Recording, {seconds}s",
  "dictation.transcribing": "Turning speech into text",
  "dictation.requesting": "Waiting for the microphone",
  "dictation.error.permission": "Microphone access was denied. Allow it in the browser, or type instead.",
  "dictation.error.unavailable": "Dictation is not available right now.",
  "dictation.error.silent": "That clip was too short or silent. Try again.",
  "dictation.error.generic": "Dictation did not work. Try again, or type instead.",
  "dictation.live.recording": "Recording",
  "dictation.live.done": "Text inserted into the message box",
} as const;

export type AlixDictationMessageKey = keyof typeof dictationEn;
export type AlixDictationMessages = Record<AlixDictationMessageKey, string>;

export const dictationIt: AlixDictationMessages = {
  "dictation.start": "Dettatura",
  "dictation.stop": "Interrompi la dettatura",
  "dictation.cancel": "Annulla la dettatura",
  "dictation.recording": "Registrazione, {seconds}s",
  "dictation.transcribing": "Trascrizione in corso",
  "dictation.requesting": "Attesa del microfono",
  "dictation.error.permission": "Accesso al microfono negato. Consentilo nel browser, oppure scrivi.",
  "dictation.error.unavailable": "La dettatura non è disponibile in questo momento.",
  "dictation.error.silent": "La registrazione era troppo breve o silenziosa. Riprova.",
  "dictation.error.generic": "La dettatura non è riuscita. Riprova, oppure scrivi.",
  "dictation.live.recording": "Registrazione",
  "dictation.live.done": "Testo inserito nel messaggio",
};

export const DICTATION_MESSAGES: Readonly<Record<string, AlixDictationMessages>> = Object.freeze({ en: dictationEn, it: dictationIt });

export type AlixDictationMessageOverrides = Partial<AlixDictationMessages>;

const fillText = (template: string, params: Record<string, string | number> | undefined): string =>
  params === undefined ? template : template.replace(/\{(\w+)\}/g, (whole, name: string) => (name in params ? String(params[name]) : whole));

export const resolveDictationMessages = (locale: string | undefined, overrides?: AlixDictationMessageOverrides): AlixDictationMessages => {
  const tag = (locale ?? "en").toLowerCase();
  const base = DICTATION_MESSAGES[tag] ?? DICTATION_MESSAGES[tag.split("-")[0] ?? "en"] ?? dictationEn;
  if (overrides === undefined) return base;
  const merged: Record<string, string> = { ...base };
  for (const [key, value] of Object.entries(overrides)) {
    if (key in dictationEn && typeof value === "string") merged[key] = value;
  }
  return merged as AlixDictationMessages;
};

export type AlixDictationText = (key: AlixDictationMessageKey, params?: Record<string, string | number>) => string;

export const useAlixDictationText = (overrides?: AlixDictationMessageOverrides): AlixDictationText => {
  const { locale } = useAlixIntl();
  const messages = useMemo(() => resolveDictationMessages(locale, overrides), [locale, overrides]);
  return useMemo(() => (key, params) => fillText(messages[key], params), [messages]);
};
