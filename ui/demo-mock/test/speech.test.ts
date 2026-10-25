import { describe, expect, it } from "vitest";
import { wavToPcm16Mono } from "../src/voice/speech";

const wav = (rate: number, channels: number, frames: readonly number[][]): Buffer => {
  const data = Buffer.alloc(frames.length * channels * 2);
  frames.forEach((frame, f) => frame.forEach((value, c) => data.writeInt16LE(value, (f * channels + c) * 2)));
  const fmt = Buffer.alloc(16);
  fmt.writeUInt16LE(1, 0);
  fmt.writeUInt16LE(channels, 2);
  fmt.writeUInt32LE(rate, 4);
  fmt.writeUInt32LE(rate * channels * 2, 8);
  fmt.writeUInt16LE(channels * 2, 12);
  fmt.writeUInt16LE(16, 14);
  const chunk = (id: string, body: Buffer): Buffer => {
    const head = Buffer.alloc(8);
    head.write(id, 0, "ascii");
    head.writeUInt32LE(body.length, 4);
    return Buffer.concat([head, body]);
  };
  const body = Buffer.concat([Buffer.from("WAVE", "ascii"), chunk("fmt ", fmt), chunk("LIST", Buffer.alloc(4)), chunk("data", data)]);
  const riff = Buffer.alloc(8);
  riff.write("RIFF", 0, "ascii");
  riff.writeUInt32LE(body.length, 4);
  return Buffer.concat([riff, body]);
};

const samples = (pcm: Buffer): number[] => Array.from({ length: pcm.length / 2 }, (_, i) => pcm.readInt16LE(i * 2));

describe("the recorded reply's audio", () => {
  it("keeps 16 kHz mono as it is, skipping chunks it does not know", () => {
    expect(samples(wavToPcm16Mono(wav(16000, 1, [[100], [-200], [300]]), 16000)!)).toEqual([100, -200, 300]);
  });

  it("mixes stereo down to mono and resamples 32 kHz to 16 kHz", () => {
    const out = samples(wavToPcm16Mono(wav(32000, 2, [[100, 300], [0, 0], [-400, 0], [0, 0]]), 16000)!);
    expect(out).toEqual([200, -200]);
  });

  it("refuses what is not 16-bit PCM WAV", () => {
    expect(wavToPcm16Mono(Buffer.from("not a wav file at all"), 16000)).toBeUndefined();
  });
});
