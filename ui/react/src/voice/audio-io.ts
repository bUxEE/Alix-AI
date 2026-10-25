import type { voice } from "@alixai/sdk";
import type { AlixMicrophone, AlixPlayer } from "./types.js";

// The browser's microphone and loudspeaker, behind the two small ports the controller uses. Nothing here runs until a person
// presses talk (the microphone) or speech arrives (the player), so importing or rendering on a server touches no browser API.
// No audio leaves the page except through the voice session, and none is kept.

interface BrowserWindow {
  AudioContext?: typeof AudioContext;
  webkitAudioContext?: typeof AudioContext;
}

const audioContextClass = (): typeof AudioContext | undefined => {
  if (typeof window === "undefined") return undefined;
  const host = window as unknown as BrowserWindow;
  return host.AudioContext ?? host.webkitAudioContext;
};

/** Frames of this many samples reach the controller: about 85 ms at 48 kHz, short enough for the level bars to feel live. */
const FRAME = 4096;

export const createAlixMicrophone = (): AlixMicrophone => {
  let generation = 0;
  let release = (): void => undefined;

  const stop = (): void => {
    generation += 1; // a start that is still waiting for permission is abandoned
    release();
    release = (): void => undefined;
  };

  const start: AlixMicrophone["start"] = async (onChunk) => {
    stop();
    const mine = generation;
    const devices = typeof navigator === "undefined" ? undefined : navigator.mediaDevices;
    const Context = audioContextClass();
    if (devices === undefined || typeof devices.getUserMedia !== "function" || Context === undefined) {
      throw new Error("this browser has no microphone access here (it needs a secure page)");
    }
    // Echo cancellation matters: the microphone sits next to the loudspeaker while the assistant speaks.
    const stream = await devices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true } });
    if (mine !== generation) {
      for (const track of stream.getTracks()) track.stop();
      return;
    }
    const context = new Context();
    const source = context.createMediaStreamSource(stream);
    const node = context.createScriptProcessor(FRAME, 1, 1);
    const silent = context.createGain();
    silent.gain.value = 0;
    node.onaudioprocess = (event) => {
      onChunk(new Float32Array(event.inputBuffer.getChannelData(0)), context.sampleRate);
    };
    source.connect(node);
    node.connect(silent);
    silent.connect(context.destination);
    release = (): void => {
      node.onaudioprocess = null;
      try {
        source.disconnect();
        node.disconnect();
        silent.disconnect();
      } catch {
        // already disconnected
      }
      for (const track of stream.getTracks()) track.stop();
      void context.close().catch(() => undefined);
    };
    if (context.state === "suspended") await context.resume();
  };

  return { start, stop };
};

export const createAlixPlayer = (): AlixPlayer => {
  let context: AudioContext | undefined;
  let cursor = 0;
  let carry: number | undefined;
  const playing = new Set<AudioBufferSourceNode>();
  const idleListeners = new Set<() => void>();

  const notifyIdle = (): void => {
    for (const listener of [...idleListeners]) listener();
  };

  const enqueue: AlixPlayer["enqueue"] = (format: voice.AudioFormat, data) => {
    if (format.encoding !== "pcm_s16le" || data.byteLength === 0) return;
    const Context = audioContextClass();
    if (Context === undefined) return;
    context ??= new Context();
    const active = context;
    if (active.state === "suspended") void active.resume().catch(() => undefined);

    // A chunk may end in the middle of a sample: the odd byte waits for the next chunk.
    let bytes = data;
    if (carry !== undefined) {
      const joined = new Uint8Array(bytes.byteLength + 1);
      joined[0] = carry;
      joined.set(bytes, 1);
      bytes = joined;
      carry = undefined;
    }
    const frameBytes = 2 * format.channels;
    const whole = bytes.byteLength - (bytes.byteLength % frameBytes);
    if (whole < bytes.byteLength) carry = bytes[bytes.byteLength - 1];
    const frames = whole / frameBytes;
    if (frames === 0) return;

    const buffer = active.createBuffer(format.channels, frames, format.sample_rate);
    const view = new DataView(bytes.buffer, bytes.byteOffset, whole);
    for (let channel = 0; channel < format.channels; channel += 1) {
      const out = buffer.getChannelData(channel);
      for (let i = 0; i < frames; i += 1) out[i] = view.getInt16((i * format.channels + channel) * 2, true) / 0x8000;
    }
    const source = active.createBufferSource();
    source.buffer = buffer;
    source.connect(active.destination);
    const at = Math.max(cursor, active.currentTime + 0.02);
    source.start(at);
    cursor = at + buffer.duration;
    playing.add(source);
    source.onended = (): void => {
      playing.delete(source);
      if (playing.size === 0) notifyIdle();
    };
  };

  const cancel: AlixPlayer["cancel"] = () => {
    const had = playing.size > 0;
    for (const source of playing) {
      source.onended = null;
      try {
        source.stop();
      } catch {
        // already ended
      }
    }
    playing.clear();
    cursor = 0;
    carry = undefined;
    if (had) notifyIdle();
  };

  const close: AlixPlayer["close"] = () => {
    cancel();
    idleListeners.clear();
    void context?.close().catch(() => undefined);
    context = undefined;
  };

  return {
    enqueue,
    cancel,
    close,
    isPlaying: () => playing.size > 0,
    onIdle: (listener) => {
      idleListeners.add(listener);
      return () => idleListeners.delete(listener);
    },
  };
};
