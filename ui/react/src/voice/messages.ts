import { useMemo } from "react";
import { useAlixIntl } from "../i18n/context.js";

// The words of the voice components, in English and Italian. The kit's own catalogue (`../i18n/messages`) has no voice group, and
// the voice work stays inside its folder, so the words live here. A host rewords any of them with the `messages` prop of a voice
// component, or the whole set with `resolveVoiceMessages`. Same keys and same English as the Vue kit.

export const voiceEn = {
  "voice.start": "Start talking",
  "voice.stop": "Stop talking",
  "voice.hold": "Hold to talk",
  "voice.processing": "Processing",
  "voice.unavailable": "Voice is not available",
  "voice.mode.title": "Voice mode",
  "voice.level": "Microphone level",
  "voice.handsFree": "Hands-free",
  "voice.end": "End voice mode",
  "voice.transcript": "What I heard",
  "voice.transcriptHint": "Correct anything that is wrong, then send it.",
  "voice.state.idle": "Ready",
  "voice.state.listening": "Listening",
  "voice.state.processing": "Processing",
  "voice.state.speaking": "Speaking",
  "voice.state.unavailable": "Voice is not available",
  "voice.state.connecting": "Connecting",
  "voice.state.reconnecting": "Reconnecting",
  "voice.state.ended": "Voice session ended",
  "voice.interrupt": "Stop speaking",
  "voice.edit": "Correct what I heard",
  "voice.sendCorrection": "Send correction",
  "voice.reset": "Back to what I heard",
  "voice.reply": "Reply",
  "voice.corrected": "Corrected",
  "voice.heard": "Heard: {text}",
  "voice.textReplies": "Replies appear as text and are not read aloud.",
  "voice.fallback.text": "Voice is not available here. Type your message instead.",
  "voice.fallback.microphone": "The microphone is blocked or missing. Allow it in your browser, or type your message.",
  "voice.confirmOnScreen": "Check the request on screen and confirm it there. Saying yes does not confirm.",
  "voice.superseded": "Your correction replaces the earlier request. If it needs confirming, confirm the new one on screen.",
  "voice.renewed": "The voice connection was renewed. Press talk to continue.",
  "voice.captureLimit": "That was as long as I can listen at once. I stopped and used what I heard.",
  "voice.sessionEnded": "The voice session ended.",
  "voice.restart": "Start voice again",
  "voice.keyHint.hold": "Hold Space or Enter to talk. Escape cancels.",
  "voice.keyHint.toggle": "Press once to start and again to stop. Escape cancels.",
  "voice.keyHint.handsFree": "Hands-free listens until you press stop.",
  "voice.orb.idle": "Voice assistant, ready",
  "voice.orb.listening": "Voice assistant, listening",
  "voice.orb.thinking": "Voice assistant, thinking",
  "voice.orb.speaking": "Voice assistant, speaking",
  "voice.orb.muted": "Voice assistant, paused",
  "voice.orb.error": "Voice assistant, not available",
  "common.send": "Send",
  "common.discard": "Discard",
  "common.retry": "Try again",
  "common.reconnecting": "Connection lost. Reconnecting…",
} as const;

export type AlixVoiceMessageKey = keyof typeof voiceEn;
export type AlixVoiceMessages = Record<AlixVoiceMessageKey, string>;

export const voiceIt: AlixVoiceMessages = {
  "voice.start": "Inizia a parlare",
  "voice.stop": "Smetti di parlare",
  "voice.hold": "Tieni premuto per parlare",
  "voice.processing": "Elaborazione",
  "voice.unavailable": "La voce non è disponibile",
  "voice.mode.title": "Modalità vocale",
  "voice.level": "Livello del microfono",
  "voice.handsFree": "Vivavoce",
  "voice.end": "Termina la modalità vocale",
  "voice.transcript": "Cosa ho sentito",
  "voice.transcriptHint": "Correggi ciò che non va, poi invialo.",
  "voice.state.idle": "Pronto",
  "voice.state.listening": "In ascolto",
  "voice.state.processing": "Elaborazione",
  "voice.state.speaking": "Sto parlando",
  "voice.state.unavailable": "La voce non è disponibile",
  "voice.state.connecting": "Connessione",
  "voice.state.reconnecting": "Riconnessione",
  "voice.state.ended": "Sessione vocale terminata",
  "voice.interrupt": "Interrompi",
  "voice.edit": "Correggi quello che ho sentito",
  "voice.sendCorrection": "Invia la correzione",
  "voice.reset": "Torna a quello che ho sentito",
  "voice.reply": "Risposta",
  "voice.corrected": "Corretto",
  "voice.heard": "Ho sentito: {text}",
  "voice.textReplies": "Le risposte compaiono come testo e non vengono lette ad alta voce.",
  "voice.fallback.text": "La voce non è disponibile qui. Scrivi il tuo messaggio.",
  "voice.fallback.microphone": "Il microfono è bloccato o assente. Consentilo nel browser, oppure scrivi il messaggio.",
  "voice.confirmOnScreen": "Controlla la richiesta sullo schermo e confermala lì. Dire sì non conferma.",
  "voice.superseded": "La tua correzione sostituisce la richiesta precedente. Se serve una conferma, conferma quella nuova sullo schermo.",
  "voice.renewed": "La connessione vocale è stata rinnovata. Premi parla per continuare.",
  "voice.captureLimit": "Questo è il massimo che posso ascoltare in una volta. Mi sono fermato e ho usato quello che ho sentito.",
  "voice.sessionEnded": "La sessione vocale è terminata.",
  "voice.restart": "Riavvia la voce",
  "voice.keyHint.hold": "Tieni premuto Spazio o Invio per parlare. Esc annulla.",
  "voice.keyHint.toggle": "Premi una volta per iniziare e di nuovo per fermare. Esc annulla.",
  "voice.keyHint.handsFree": "Il vivavoce ascolta finché non premi stop.",
  "voice.orb.idle": "Assistente vocale, pronto",
  "voice.orb.listening": "Assistente vocale, in ascolto",
  "voice.orb.thinking": "Assistente vocale, sta pensando",
  "voice.orb.speaking": "Assistente vocale, sta parlando",
  "voice.orb.muted": "Assistente vocale, in pausa",
  "voice.orb.error": "Assistente vocale, non disponibile",
  "common.send": "Invia",
  "common.discard": "Scarta",
  "common.retry": "Riprova",
  "common.reconnecting": "Connessione persa. Riconnessione in corso…",
};

export const VOICE_MESSAGES: Readonly<Record<string, AlixVoiceMessages>> = Object.freeze({ en: voiceEn, it: voiceIt });

export type AlixVoiceMessageOverrides = Partial<AlixVoiceMessages>;

const fillText = (template: string, params: Record<string, string | number> | undefined): string =>
  params === undefined ? template : template.replace(/\{(\w+)\}/g, (whole, name: string) => (name in params ? String(params[name]) : whole));

/** The words for a locale: the language of the tag, else English, with the host's own words over them. */
export const resolveVoiceMessages = (locale: string | undefined, overrides?: AlixVoiceMessageOverrides): AlixVoiceMessages => {
  const tag = (locale ?? "en").toLowerCase();
  const base = VOICE_MESSAGES[tag] ?? VOICE_MESSAGES[tag.split("-")[0] ?? "en"] ?? voiceEn;
  if (overrides === undefined) return base;
  const merged: Record<string, string> = { ...base };
  for (const [key, value] of Object.entries(overrides)) {
    if (key in voiceEn && typeof value === "string") merged[key] = value;
  }
  return merged as AlixVoiceMessages;
};

export type AlixVoiceText = (key: AlixVoiceMessageKey, params?: Record<string, string | number>) => string;

/** The voice words for the current language, as a function: `t("voice.heard", { text })`. */
export const useAlixVoiceText = (overrides?: AlixVoiceMessageOverrides): AlixVoiceText => {
  const { locale } = useAlixIntl();
  const messages = useMemo(() => resolveVoiceMessages(locale, overrides), [locale, overrides]);
  return useMemo(() => (key, params) => fillText(messages[key], params), [messages]);
};
