/** Mic capture port shared with voice, defined here so dictation does not import the conversation audio module. */
export interface AlixCaptureMicrophone {
  start: (onChunk: (samples: Float32Array, sampleRate: number) => void) => Promise<void>;
  stop: () => void;
}

interface BrowserWindow {
  AudioContext?: typeof AudioContext;
  webkitAudioContext?: typeof AudioContext;
}

const audioContextClass = (): typeof AudioContext | undefined => {
  if (typeof window === "undefined") return undefined;
  const host = window as unknown as BrowserWindow;
  return host.AudioContext ?? host.webkitAudioContext;
};

const FRAME = 4096;

export const createAlixMicrophone = (): AlixCaptureMicrophone => {
  let generation = 0;
  let release = (): void => undefined;

  const stop = (): void => {
    generation += 1;
    release();
    release = (): void => undefined;
  };

  const start: AlixCaptureMicrophone["start"] = async (onChunk) => {
    stop();
    const mine = generation;
    const devices = typeof navigator === "undefined" ? undefined : navigator.mediaDevices;
    const Context = audioContextClass();
    if (devices === undefined || typeof devices.getUserMedia !== "function" || Context === undefined) {
      throw new Error("this browser has no microphone access here (it needs a secure page)");
    }
    const stream = await devices.getUserMedia({
      audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true },
    });
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
