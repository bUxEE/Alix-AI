// The assistant's spoken replies: real speech, recorded once with the operating system's own offline voice and replayed from a
// cache, never a synthetic tone. Windows uses System.Speech (the voices that ship with Windows), macOS `say`, Linux `espeak-ng`.
// A voice in the reply's language is preferred; when the machine has no voice at all, the reply is silent (`undefined`), and the
// kits still get their playback events. The recordings are made on the developer's machine and kept in a git-ignored cache
// (ui/demo-mock/.voice-cache); none is committed.

import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import type { DemoLocale } from "../prompts";

const CACHE_DIR = join(dirname(fileURLToPath(import.meta.url)), "..", "..", ".voice-cache");
const TIMEOUT_MS = 20_000;

const run = (file: string, args: readonly string[], env: NodeJS.ProcessEnv = process.env): Promise<boolean> =>
  new Promise((resolve) => {
    execFile(file, [...args], { env, timeout: TIMEOUT_MS, windowsHide: true }, (error) => resolve(error === null));
  });

// The text and the culture travel in the environment, never in the command line, so no quoting can break or inject anything.
const WINDOWS_SCRIPT = [
  "Add-Type -AssemblyName System.Speech",
  "$s = New-Object System.Speech.Synthesis.SpeechSynthesizer",
  "$v = $s.GetInstalledVoices() | Where-Object { $_.Enabled -and $_.VoiceInfo.Culture.Name -like ($env:ALIX_TTS_LANG + '*') } | Select-Object -First 1",
  "if ($v) { $s.SelectVoice($v.VoiceInfo.Name) }",
  "$s.Rate = 1",
  "$s.SetOutputToWaveFile($env:ALIX_TTS_OUT)",
  "$s.Speak($env:ALIX_TTS_TEXT)",
  "$s.Dispose()",
].join("; ");

/** Records `text` as a WAV file at `out` with the platform's voice; false when no voice could. */
const record = async (text: string, language: DemoLocale, out: string): Promise<boolean> => {
  if (process.platform === "win32") {
    return run("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", WINDOWS_SCRIPT], {
      ...process.env,
      ALIX_TTS_TEXT: text,
      ALIX_TTS_LANG: language,
      ALIX_TTS_OUT: out,
    });
  }
  if (process.platform === "darwin") {
    const voice = language === "it" ? ["-v", "Alice"] : [];
    return (await run("say", [...voice, "-o", out, "--file-format=WAVE", "--data-format=LEI16@16000", text])) || run("say", ["-o", out, "--file-format=WAVE", "--data-format=LEI16@16000", text]);
  }
  return run("espeak-ng", ["-v", language, "-w", out, text]);
};

/** The 16-bit PCM samples of a WAV file, mixed down to mono and resampled to `rate`; undefined when it is not 16-bit PCM. */
export const wavToPcm16Mono = (wav: Buffer, rate: number): Buffer | undefined => {
  if (wav.length < 12 || wav.toString("ascii", 0, 4) !== "RIFF" || wav.toString("ascii", 8, 12) !== "WAVE") return undefined;
  let channels = 0;
  let sourceRate = 0;
  let bits = 0;
  let data: Buffer | undefined;
  for (let at = 12; at + 8 <= wav.length; ) {
    const id = wav.toString("ascii", at, at + 4);
    const size = wav.readUInt32LE(at + 4);
    const body = wav.subarray(at + 8, Math.min(wav.length, at + 8 + size));
    if (id === "fmt " && body.length >= 16) {
      if (body.readUInt16LE(0) !== 1) return undefined; // PCM only
      channels = body.readUInt16LE(2);
      sourceRate = body.readUInt32LE(4);
      bits = body.readUInt16LE(14);
    } else if (id === "data") {
      data = body;
    }
    at += 8 + size + (size % 2);
  }
  if (data === undefined || bits !== 16 || channels < 1 || sourceRate <= 0) return undefined;
  const frames = Math.floor(data.length / (2 * channels));
  const mono = new Float64Array(frames);
  for (let f = 0; f < frames; f += 1) {
    let sum = 0;
    for (let c = 0; c < channels; c += 1) sum += data.readInt16LE((f * channels + c) * 2);
    mono[f] = sum / channels;
  }
  const outFrames = Math.max(1, Math.round((frames * rate) / sourceRate));
  const out = Buffer.alloc(outFrames * 2);
  for (let i = 0; i < outFrames; i += 1) {
    const position = (i * sourceRate) / rate;
    const left = Math.min(frames - 1, Math.floor(position));
    const right = Math.min(frames - 1, left + 1);
    const value = mono[left]! + (mono[right]! - mono[left]!) * (position - left);
    out.writeInt16LE(Math.max(-32768, Math.min(32767, Math.round(value))), i * 2);
  }
  return out;
};

const pending = new Map<string, Promise<Buffer | undefined>>();

/** The reply spoken as 16-bit mono PCM at `rate`, from the cache or recorded now; undefined when the machine has no voice. */
export const speechPcm = (text: string, language: DemoLocale, rate: number): Promise<Buffer | undefined> => {
  const key = createHash("sha256").update(`${process.platform}\n${language}\n${rate}\n${text}`).digest("hex").slice(0, 32);
  const known = pending.get(key);
  if (known !== undefined) return known;
  const made = (async (): Promise<Buffer | undefined> => {
    const cached = join(CACHE_DIR, `${key}.pcm`);
    try {
      return await readFile(cached);
    } catch {
      // not recorded yet
    }
    const wav = join(tmpdir(), `alix-demo-voice-${key}.wav`);
    try {
      if (!(await record(text, language, wav))) return undefined;
      const pcm = wavToPcm16Mono(await readFile(wav), rate);
      if (pcm === undefined) return undefined;
      await mkdir(CACHE_DIR, { recursive: true });
      await writeFile(cached, pcm);
      return pcm;
    } catch {
      return undefined;
    } finally {
      await rm(wav, { force: true });
    }
  })();
  pending.set(key, made);
  return made;
};
