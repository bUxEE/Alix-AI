import type { AlixMicrophone } from "@alixai/vue";

// A stand-in microphone for the voice demo, so it works on a machine with no microphone, in a browser that refuses one, or
// without anyone speaking. It produces a soft synthetic murmur in bursts (about 2.4 s of "speech", then 1.2 s of silence), at the
// session's 16 kHz, so the level meter moves and hands-free mode can tell where an utterance ends. The mock ignores the audio
// itself: its transcripts are scripted.

const RATE = 16000;
const CHUNK = 1600; // 100 ms
const SPEECH_CHUNKS = 24;
const CYCLE_CHUNKS = 36;

export const createSimulatedMicrophone = (): AlixMicrophone => {
  let timer: ReturnType<typeof setInterval> | undefined;
  let tick = 0;
  let sample = 0;

  const stop = (): void => {
    if (timer !== undefined) clearInterval(timer);
    timer = undefined;
  };

  const chunk = (): Float32Array => {
    const samples = new Float32Array(CHUNK);
    const speaking = tick % CYCLE_CHUNKS < SPEECH_CHUNKS;
    for (let i = 0; i < CHUNK; i += 1) {
      const t = (sample + i) / RATE;
      const syllables = 0.5 + 0.5 * Math.sin(2 * Math.PI * 3.2 * t);
      const voice = Math.sin(2 * Math.PI * 165 * t) * 0.6 + Math.sin(2 * Math.PI * 330 * t) * 0.25;
      const noise = (Math.random() * 2 - 1) * 0.004;
      samples[i] = (speaking ? 0.32 * syllables * voice : 0) + noise;
    }
    sample += CHUNK;
    tick += 1;
    return samples;
  };

  return {
    start: async (onChunk) => {
      stop();
      tick = 0;
      timer = setInterval(() => onChunk(chunk(), RATE), 100);
    },
    stop,
  };
};

/**
 * A voice level with the rhythm of the murmur above (syllables, phrases, pauses), every 100 ms like a microphone's chunks, for an
 * orb shown without a voice session. Returns the function that stops it.
 */
export const startSimulatedLevel = (onLevel: (level: number) => void): (() => void) => {
  let tick = 0;
  const timer = setInterval(() => {
    const t = tick / 10;
    const speaking = tick % CYCLE_CHUNKS < SPEECH_CHUNKS;
    const syllables = 0.5 + 0.5 * Math.sin(2 * Math.PI * 3.2 * t);
    onLevel(speaking ? Math.min(1, 0.25 + 0.55 * syllables + Math.random() * 0.15) : 0.02);
    tick += 1;
  }, 100);
  return () => clearInterval(timer);
};
