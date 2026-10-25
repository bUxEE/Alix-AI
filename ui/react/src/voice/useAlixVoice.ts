import { useEffect, useRef, useState } from "react";
import { createAlixVoiceController } from "./controller.js";
import type { AlixVoiceController, AlixVoiceControllerOptions, AlixVoiceListener, AlixVoiceView } from "./types.js";

// The controller as a React hook. It is created in an effect (never during a server render, where there is no microphone to open),
// is rebuilt only when the session, the microphone or the player changes, and is destroyed with the component: the microphone and
// the loudspeaker are released. A new language or a toggled option must not drop a live conversation.

export interface AlixVoice {
  /** The current view, or undefined until the component is mounted or while there is no session. */
  view: AlixVoiceView | undefined;
  controller: AlixVoiceController | undefined;
}

export const useAlixVoice = (options: AlixVoiceControllerOptions | undefined, onEvent?: AlixVoiceListener): AlixVoice => {
  const [state, setState] = useState<AlixVoice>({ view: undefined, controller: undefined });
  const latest = useRef(options);
  latest.current = options;
  const handler = useRef(onEvent);
  handler.current = onEvent;
  const session = options?.session;
  const microphone = options?.microphone;
  const player = options?.player;

  useEffect(() => {
    const current = latest.current;
    if (current === undefined) {
      setState({ view: undefined, controller: undefined });
      return undefined;
    }
    const created = createAlixVoiceController(current);
    const stopView = created.subscribe(() => setState({ view: created.getView(), controller: created }));
    const stopEvents = created.on((event) => handler.current?.(event));
    setState({ view: created.getView(), controller: created });
    return () => {
      stopView();
      stopEvents();
      created.destroy();
    };
  }, [session, microphone, player]);

  return state;
};
