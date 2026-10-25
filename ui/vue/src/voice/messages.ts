import { useAlix, type MessageKey } from "../i18n";

// The words only the voice components need. The kit's own catalogue (`../i18n`) keeps the keys M7-01 gave the voice shells
// (`voice.start`, `voice.state.idle` ...); everything newer lives here, in English and Italian, so the voice work stays inside
// its folder. A host rewords any of them the usual way (`provideAlix({ messages: { it: { "voice.interrupt": "..." } } })`): the
// host's text wins, then these, then English.

const en = {
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
} as const;

const it: Record<keyof typeof en, string> = {
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
};

export const VOICE_MESSAGES: Readonly<Record<string, Readonly<Record<keyof typeof en, string>>>> = Object.freeze({ en, it });

export type AlixVoiceMessageKey = keyof typeof en;
type AnyKey = AlixVoiceMessageKey | MessageKey;

const fill = (template: string, params: Record<string, string | number> | undefined): string =>
  params === undefined ? template : template.replace(/\{(\w+)\}/g, (whole, name: string) => (name in params ? String(params[name]) : whole));

/** The kit's `t`, widened to the voice keys: the host's text, then the kit's, then the voice catalogue in the locale, then English. */
export const useAlixVoiceText = (): ((key: AnyKey, params?: Record<string, string | number>) => string) => {
  const config = useAlix();
  return (key, params) => {
    const found = config.t(key as MessageKey, params);
    if (found !== key) return found;
    const tag = config.locale.value.toLowerCase();
    const local = VOICE_MESSAGES[tag] ?? VOICE_MESSAGES[tag.split("-")[0] ?? "en"] ?? en;
    return fill((local as Record<string, string>)[key] ?? (en as Record<string, string>)[key] ?? key, params);
  };
};
