import type { AlixMicrophone } from "@alixai/react";

// A stand-in microphone: a soft hum with a slow swell, in 100 ms chunks at 16 kHz, so Voice Mode can be tried on a machine with no
// microphone (or without granting one). The mock ignores the audio and answers with its scripted transcripts either way.

const RATE = 16000;
const CHUNK = RATE / 10;

/**
 * A voice level with the rhythm of speech (syllables, phrases, pauses), every 100 ms like a microphone's chunks, for an orb shown
 * without a voice session. Returns the function that stops it.
 */
export const startSimulatedLevel = (onLevel: (level: number) => void): (() => void) => {
  let tick = 0;
  const timer = setInterval(() => {
    const speaking = tick % 36 < 24;
    const syllables = 0.5 + 0.5 * Math.sin((2 * Math.PI * 3.2 * tick) / 10);
    onLevel(speaking ? Math.min(1, 0.25 + 0.55 * syllables + Math.random() * 0.15) : 0.02);
    tick += 1;
  }, 100);
  return () => clearInterval(timer);
};

export const createSimulatedMicrophone = (): AlixMicrophone => {
  let timer: ReturnType<typeof setInterval> | undefined;
  let phase = 0;
  let tick = 0;
  return {
    start: async (onChunk) => {
      if (timer !== undefined) return;
      timer = setInterval(() => {
        const samples = new Float32Array(CHUNK);
        const swell = 0.25 + 0.2 * Math.sin(tick / 3);
        for (let index = 0; index < CHUNK; index += 1) {
          phase += (2 * Math.PI * (180 + 40 * Math.sin(tick / 5))) / RATE;
          samples[index] = swell * Math.sin(phase) * (0.6 + 0.4 * Math.random());
        }
        tick += 1;
        onChunk(samples, RATE);
      }, 100);
    },
    stop: () => {
      if (timer !== undefined) clearInterval(timer);
      timer = undefined;
    },
  };
};
