// Raw PCM helpers: the audio of the voice socket is signed 16-bit little-endian samples and nothing else (containers and codecs are the
// host's business). Nothing here is specific to a runtime: a browser's Web Audio buffers and a server's capture library both reach
// `pushAudio` through them. No Node API is used (base64 is `btoa`).

/** Whole 16-bit samples as little-endian bytes, whatever the byte order of this machine. */
export const pcm16ToBytes = (samples: Int16Array): Uint8Array => {
  const bytes = new Uint8Array(samples.length * 2);
  const view = new DataView(bytes.buffer);
  for (let i = 0; i < samples.length; i += 1) view.setInt16(i * 2, samples[i] as number, true);
  return bytes;
};

/** Float samples in [-1, 1] (a Web Audio channel) as 16-bit PCM. Values outside the range are clipped. */
export const float32ToPcm16 = (input: Float32Array): Int16Array => {
  const out = new Int16Array(input.length);
  for (let i = 0; i < input.length; i += 1) {
    const s = Math.max(-1, Math.min(1, input[i] as number));
    out[i] = s < 0 ? Math.round(s * 0x8000) : Math.round(s * 0x7fff);
  }
  return out;
};

/**
 * Float samples at `fromRate` as float samples at `toRate` (default 16000, the rate the recogniser takes), by averaging the source
 * samples that fall into each output sample when going down and linear interpolation when going up. Enough for speech; not a
 * mastering-grade resampler.
 */
export const resampleFloat32 = (input: Float32Array, fromRate: number, toRate = 16000): Float32Array => {
  if (!(fromRate > 0) || !(toRate > 0)) throw new RangeError("a sample rate must be positive");
  if (fromRate === toRate || input.length === 0) return input;
  const ratio = fromRate / toRate;
  const length = Math.floor(input.length / ratio);
  const out = new Float32Array(length);
  for (let i = 0; i < length; i += 1) {
    if (ratio > 1) {
      const start = Math.floor(i * ratio);
      const end = Math.min(input.length, Math.max(start + 1, Math.floor((i + 1) * ratio)));
      let sum = 0;
      for (let j = start; j < end; j += 1) sum += input[j] as number;
      out[i] = sum / (end - start);
    } else {
      const position = i * ratio;
      const index = Math.floor(position);
      const next = Math.min(input.length - 1, index + 1);
      const fraction = position - index;
      out[i] = (input[index] as number) * (1 - fraction) + (input[next] as number) * fraction;
    }
  }
  return out;
};

/** The loudness of a chunk of PCM 16-bit little-endian audio as the RMS of its samples, 0 (silence) to 1 (full scale). For a waveform or a meter. */
export const pcmLevel = (bytes: Uint8Array): number => {
  const samples = Math.floor(bytes.length / 2);
  if (samples === 0) return 0;
  const view = new DataView(bytes.buffer, bytes.byteOffset, samples * 2);
  let sum = 0;
  for (let i = 0; i < samples; i += 1) {
    const s = view.getInt16(i * 2, true) / 0x8000;
    sum += s * s;
  }
  return Math.sqrt(sum / samples);
};

/** The base64 text of bytes, built in slices so that a long utterance does not overflow the argument limit of a call. */
export const bytesToBase64 = (bytes: Uint8Array): string => {
  const slice = 0x8000;
  const parts: string[] = [];
  for (let i = 0; i < bytes.length; i += slice) parts.push(String.fromCharCode(...bytes.subarray(i, i + slice)));
  return btoa(parts.join(""));
};

/** How many bytes `length` bytes of audio take as base64 text. */
export const base64Length = (length: number): number => Math.ceil(length / 3) * 4;

/** The milliseconds of audio in `bytes` bytes of PCM 16-bit audio at a rate and channel count. */
export const pcmDurationMs = (bytes: number, sampleRate: number, channels: number): number => (bytes / (2 * channels) / sampleRate) * 1000;
