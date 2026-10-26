import { float32ToPcm16, resampleFloat32 } from "./audio.js";

/** Target rate for dictation WAV bodies (schemas/voice/transcription-capabilities). */
export const DICTATION_SAMPLE_RATE_HZ = 16_000;

/**
 * Encodes float samples as a WAV (PCM s16le, mono, 16 kHz) for `POST /v1/sessions/{session_id}/dictations`.
 * Resamples when `sampleRate` is not 16000. Empty input yields a valid empty-data WAV header only.
 */
export const encodeWav16kMono = (samples: Float32Array, sampleRate: number): Uint8Array => {
  if (!(sampleRate > 0) || !Number.isFinite(sampleRate)) {
    throw new RangeError("sampleRate must be a positive number");
  }
  const mono = resampleFloat32(samples, sampleRate, DICTATION_SAMPLE_RATE_HZ);
  const pcm = float32ToPcm16(mono);
  const dataBytes = pcm.length * 2;
  const buffer = new ArrayBuffer(44 + dataBytes);
  const view = new DataView(buffer);
  const writeAscii = (offset: number, text: string): void => {
    for (let i = 0; i < text.length; i += 1) view.setUint8(offset + i, text.charCodeAt(i));
  };
  writeAscii(0, "RIFF");
  view.setUint32(4, 36 + dataBytes, true);
  writeAscii(8, "WAVE");
  writeAscii(12, "fmt ");
  view.setUint32(16, 16, true); // PCM fmt chunk size
  view.setUint16(20, 1, true); // PCM
  view.setUint16(22, 1, true); // mono
  view.setUint32(24, DICTATION_SAMPLE_RATE_HZ, true);
  view.setUint32(28, DICTATION_SAMPLE_RATE_HZ * 2, true); // byte rate
  view.setUint16(32, 2, true); // block align
  view.setUint16(34, 16, true); // bits per sample
  writeAscii(36, "data");
  view.setUint32(40, dataBytes, true);
  const out = new Uint8Array(buffer);
  out.set(new Uint8Array(pcm.buffer, pcm.byteOffset, dataBytes), 44);
  return out;
};
