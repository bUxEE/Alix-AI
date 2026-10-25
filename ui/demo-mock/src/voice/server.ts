import type { IncomingMessage } from "node:http";
import type { Duplex } from "node:stream";
import { WebSocketServer, type RawData, type WebSocket } from "ws";
import { newConfirmationToken, newId } from "../ids";
import type { DemoLocale } from "../prompts";
import { speechPcm } from "./speech";

// The voice WebSocket of a session (GET /v1/sessions/{session_id}/voice, schemas/voice): JSON control messages and binary PCM in,
// JSON events and binary speech out, in the order the core's voice session emits them (internal/voice/session): capture.started,
// transcript.interim while audio arrives, transcript.final when the capture ends, then turn.completed, response, playback.started,
// the speech as binary chunks and playback.done. The audio is ignored: the transcripts are scripted, one per utterance in turn.

const PATH = /^\/v1\/sessions\/([A-Za-z0-9_.-]{1,128})\/voice$/;
const SAMPLE_RATE = 16000;
const FORMAT = { encoding: "pcm_s16le", sample_rate: SAMPLE_RATE, channels: 1 } as const;
const MAX_AUDIO_BYTES = 8 * 1024 * 1024;
/** One interim transcript per this much audio (0.4 s at 16 kHz mono 16-bit). */
const INTERIM_EVERY_BYTES = 12800;
const CHUNK_MS = 100;

interface Script {
  readonly heard: string;
  readonly act: string;
  readonly reply: string;
  readonly prepares?: boolean;
}

const SCRIPTS: Record<DemoLocale, readonly Script[]> = {
  en: [
    {
      heard: "What's the status of work order WO-2291?",
      act: "present_result",
      reply: "Work order WO-2291 is open: the HVAC filters at Harbor Point are due to be replaced by 14 October, and Coolwave has the job.",
    },
    {
      heard: "Book a fire-safety inspection at Riverside for Tuesday morning",
      act: "request_confirmation",
      reply: "I've prepared the fire-safety inspection at Riverside Logistics Depot for Tuesday at 8:30. Please check it on screen and confirm it there.",
      prepares: true,
    },
    {
      heard: "How much energy did Westfield use in July?",
      act: "present_result",
      reply: "Westfield Data Hall used 797 megawatt hours in July, its highest month this year.",
    },
  ],
  it: [
    {
      heard: "Qual è lo stato dell'ordine WO-2291?",
      act: "present_result",
      reply: "L'ordine WO-2291 è aperto: i filtri HVAC di Harbor Point vanno sostituiti entro il 14 ottobre, e il lavoro è assegnato a Coolwave.",
    },
    {
      heard: "Prenota un'ispezione antincendio a Riverside per martedì mattina",
      act: "request_confirmation",
      reply: "Ho preparato l'ispezione antincendio al Riverside Logistics Depot per martedì alle 8:30. Controllala sullo schermo e confermala lì.",
      prepares: true,
    },
    {
      heard: "Quanta energia ha consumato Westfield a luglio?",
      act: "present_result",
      reply: "A luglio il Westfield Data Hall ha consumato 797 megawattora, il mese più alto dell'anno.",
    },
  ],
};

export interface VoiceServerOptions {
  /** Multiplies every delay: 1 plays speech in real time, 0 sends everything at once (tests). */
  readonly pace: number;
  /** "system" (default) speaks each reply with the machine's own voice (see speech.ts); "silent" sends silence (tests). */
  readonly voice?: "system" | "silent";
}

export interface VoiceServer {
  /** Takes the upgrade when the path is a voice socket; false otherwise (another handler, Vite's HMR for one, gets it). */
  handleUpgrade: (req: IncomingMessage, socket: Duplex, head: Buffer, path: string, query: URLSearchParams) => boolean;
  close: () => void;
}

const localeOf = (language: string | undefined): DemoLocale => (language?.toLowerCase().startsWith("it") === true ? "it" : "en");

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

export const createVoiceServer = (options: VoiceServerOptions): VoiceServer => {
  const wss = new WebSocketServer({ noServer: true, maxPayload: 1024 * 1024 });

  const connection = (ws: WebSocket, sessionId: string, query: URLSearchParams): void => {
    const speak = query.get("speak") !== "false";
    const sessionLanguage = query.get("language") ?? undefined;
    const timers = new Set<ReturnType<typeof setTimeout>>();
    let scriptIndex = 0;
    let capture: { id: string; bytes: number; seq: number; script: Script; language: DemoLocale } | undefined;
    let playback: { id: string; turnId: string; cancelled: boolean } | undefined;
    let latest: { id: string; text: string; turnId: string; preparationId: string | undefined; language: DemoLocale } | undefined;

    const later = (ms: number, run: () => void): void => {
      const timer = setTimeout(() => {
        timers.delete(timer);
        if (ws.readyState === ws.OPEN) run();
      }, Math.round(ms * options.pace));
      timers.add(timer);
    };
    const send = (event: Record<string, unknown>): void => {
      if (ws.readyState === ws.OPEN) ws.send(JSON.stringify(event));
    };
    const fail = (code: string, message: string, extra: Record<string, unknown> = {}): void =>
      send({ type: "error", error: { code, message, retryable: false }, ...extra });

    const stopPlayback = (reason: "client" | "barge_in"): void => {
      if (playback === undefined) return;
      playback.cancelled = true;
      send({ type: "playback.cancelled", playback_id: playback.id, turn_id: playback.turnId, reason });
      playback = undefined;
    };

    const speakReply = (turnId: string, text: string, language: DemoLocale): void => {
      if (!speak) return;
      const current = { id: newId("pbk"), turnId, cancelled: false };
      playback = current;
      send({ type: "playback.started", playback_id: current.id, turn_id: turnId, format: FORMAT });
      // The reply's own words in a real voice; with no voice on this machine, silence as long as the words would take.
      const silence = (): Buffer => Buffer.alloc(Math.round((Math.min(6000, Math.max(1500, text.length * 55)) / 1000) * SAMPLE_RATE) * 2);
      const spoken = options.voice === "silent" ? Promise.resolve(undefined) : speechPcm(text, language, SAMPLE_RATE);
      void spoken.then((pcm) => stream(pcm ?? silence()));
      const chunkBytes = (SAMPLE_RATE * 2 * CHUNK_MS) / 1000;
      let audio: Buffer = Buffer.alloc(0);
      let offset = 0;
      const stream = (pcm: Buffer): void => {
        audio = pcm;
        later(120, next);
      };
      const next = (): void => {
        if (current.cancelled) return;
        if (offset >= audio.length) {
          send({ type: "playback.done", playback_id: current.id, turn_id: turnId });
          if (playback === current) playback = undefined;
          return;
        }
        ws.send(audio.subarray(offset, offset + chunkBytes));
        offset += chunkBytes;
        later(CHUNK_MS, next);
      };
    };

    /** The turn of a final transcript: turn.completed, then the verified reply, then its speech. */
    const runTurn = (utteranceId: string, text: string, script: Script | undefined, language: DemoLocale, corrects?: { turnId: string; preparationId: string | undefined }): void => {
      const turnId = newId("trn");
      const preparationId = script?.prepares === true && corrects === undefined ? newId("prep") : undefined;
      const reply =
        script !== undefined && corrects === undefined
          ? script.reply
          : language === "it"
            ? `Ho usato la tua correzione: "${text}". Se serve una conferma, la trovi sullo schermo.`
            : `I've used your correction: "${text}". If it needs confirming, you'll find it on screen.`;
      later(450, () => {
        send({
          type: "turn.completed",
          utterance_id: utteranceId,
          turn_id: turnId,
          act: corrects === undefined ? (script?.act ?? "present_result") : "present_result",
          failed: false,
          ...(preparationId !== undefined && { preparation_id: preparationId, confirmation: { preparation_id: preparationId, token: newConfirmationToken() } }),
          ...(corrects !== undefined && { corrects_turn_id: corrects.turnId }),
          ...(corrects?.preparationId !== undefined && { superseded_preparation_id: corrects.preparationId }),
        });
        send({
          type: "response",
          utterance_id: utteranceId,
          turn_id: turnId,
          text: reply,
          source: "model",
          speech: speak ? "queued" : "skipped",
          ...(!speak && { speech_reason: "speech_disabled" }),
        });
        latest = { id: utteranceId, text, turnId, preparationId, language };
        speakReply(turnId, reply, language);
      });
    };

    const startCapture = (language: DemoLocale): string => {
      stopPlayback("barge_in");
      const scripts = SCRIPTS[language];
      const script = scripts[scriptIndex % scripts.length] as Script;
      scriptIndex += 1;
      capture = { id: newId("utt"), bytes: 0, seq: 0, script, language };
      send({ type: "capture.started", utterance_id: capture.id });
      return capture.id;
    };

    const endCapture = (): void => {
      const done = capture;
      capture = undefined;
      if (done === undefined) {
        fail("validation_failed", "no capture is in progress");
        return;
      }
      if (done.bytes === 0) {
        send({ type: "capture.cancelled", utterance_id: done.id, reason: "no_speech" });
        return;
      }
      later(300, () => {
        send({ type: "transcript.final", utterance_id: done.id, text: done.script.heard, language: done.language });
        runTurn(done.id, done.script.heard, done.script, done.language);
      });
    };

    const onAudio = (bytes: number): void => {
      const current = capture;
      if (current === undefined) return; // audio outside a capture is dropped, as the core does
      const before = current.bytes;
      current.bytes += bytes;
      if (current.bytes > MAX_AUDIO_BYTES) {
        capture = undefined;
        fail("validation_failed", "the utterance is longer than the session allows", { utterance_id: current.id });
        send({ type: "capture.cancelled", utterance_id: current.id, reason: "invalid_audio" });
        return;
      }
      if (Math.floor(current.bytes / INTERIM_EVERY_BYTES) > Math.floor(before / INTERIM_EVERY_BYTES)) {
        const words = current.script.heard.split(" ");
        current.seq += 1;
        const shown = Math.min(words.length - 1, current.seq * 2);
        if (shown > 0) send({ type: "transcript.interim", utterance_id: current.id, seq: current.seq, text: words.slice(0, shown).join(" ") });
      }
    };

    const onMessage = (message: Record<string, unknown>): void => {
      switch (message["type"]) {
        case "capture.start":
          startCapture(localeOf(typeof message["language"] === "string" ? message["language"] : sessionLanguage));
          return;
        case "capture.end":
          endCapture();
          return;
        case "capture.cancel": {
          const current = capture;
          capture = undefined;
          if (current !== undefined) send({ type: "capture.cancelled", utterance_id: current.id, reason: "client" });
          return;
        }
        case "audio.submit": {
          const audio = typeof message["audio"] === "string" ? Buffer.from(message["audio"], "base64") : Buffer.alloc(0);
          startCapture(localeOf(typeof message["language"] === "string" ? message["language"] : sessionLanguage));
          onAudio(audio.length);
          endCapture();
          return;
        }
        case "transcript.correct": {
          const text = typeof message["text"] === "string" ? message["text"].trim() : "";
          if (latest === undefined || message["utterance_id"] !== latest.id) {
            fail("stale", "only the latest utterance can be corrected");
            return;
          }
          if (text === "" || text === latest.text) {
            fail("validation_failed", "the correction does not change the transcript", { utterance_id: latest.id });
            return;
          }
          const previous = latest;
          stopPlayback("client");
          send({ type: "transcript.corrected", utterance_id: previous.id, text, previous_text: previous.text });
          runTurn(previous.id, text, undefined, previous.language, { turnId: previous.turnId, preparationId: previous.preparationId });
          return;
        }
        case "playback.cancel":
          stopPlayback("client");
          return;
        default:
          fail("validation_failed", "unknown message type");
      }
    };

    ws.on("message", (data: RawData, isBinary: boolean) => {
      if (isBinary) {
        const size = Array.isArray(data) ? data.reduce((sum, part) => sum + part.length, 0) : data instanceof ArrayBuffer ? data.byteLength : data.length;
        onAudio(size);
        return;
      }
      let parsed: unknown;
      try {
        parsed = JSON.parse(data.toString());
      } catch {
        parsed = undefined;
      }
      if (!isRecord(parsed)) {
        fail("validation_failed", "a control message is a JSON object");
        return;
      }
      onMessage(parsed);
    });
    ws.on("close", () => {
      for (const timer of timers) clearTimeout(timer);
      timers.clear();
      if (playback !== undefined) playback.cancelled = true;
    });

    // A moment after the handshake, never in the same packet: a client attaches its message listener once its `open` has run,
    // and Node delivers a message that arrives with the handshake right after `open`, before an awaiting client is listening.
    const greeting = setTimeout(() => {
      timers.delete(greeting);
      send({
        type: "session.started",
        voice_session_id: newId("vss"),
        session_id: sessionId,
        speech: speak,
        retain_raw_audio: false,
        max_chunk_bytes: 32768,
        max_audio_bytes: MAX_AUDIO_BYTES,
        language: localeOf(sessionLanguage),
        pack_id: "demo-voice-en-it",
      });
    }, 30);
    timers.add(greeting);
  };

  const handleUpgrade = (req: IncomingMessage, socket: Duplex, head: Buffer, path: string, query: URLSearchParams): boolean => {
    const match = PATH.exec(path);
    if (match === null) return false;
    const sessionId = match[1] as string;
    wss.handleUpgrade(req, socket, head, (ws) => connection(ws, sessionId, query));
    return true;
  };

  return {
    handleUpgrade,
    close: () => {
      for (const client of wss.clients) client.terminate();
      wss.close();
    },
  };
};
