import { onBeforeUnmount, onMounted, shallowRef, toValue, watch, type MaybeRefOrGetter, type ShallowRef } from "vue";
import { createAlixVoiceController } from "./controller";
import type { AlixVoiceController, AlixVoiceControllerOptions, AlixVoiceListener, AlixVoiceView } from "./types";

// The controller as a Vue composable. It is created after mount (never on a server, where there is no microphone to open), is
// rebuilt when the session changes, and is destroyed with the component: the microphone and the loudspeaker are released.

export interface AlixVoice {
  /** The current view, or undefined until the component is mounted or while there is no session. */
  view: ShallowRef<AlixVoiceView | undefined>;
  controller: ShallowRef<AlixVoiceController | undefined>;
}

export const useAlixVoice = (options: MaybeRefOrGetter<AlixVoiceControllerOptions | undefined>, onEvent?: AlixVoiceListener): AlixVoice => {
  const view = shallowRef<AlixVoiceView>();
  const controller = shallowRef<AlixVoiceController>();
  let release = (): void => undefined;

  const build = (): void => {
    release();
    release = (): void => undefined;
    controller.value = undefined;
    view.value = undefined;
    const resolved = toValue(options);
    if (resolved === undefined) return;
    const created = createAlixVoiceController(resolved);
    const stopView = created.subscribe(() => {
      view.value = created.getView();
    });
    const stopEvents = onEvent === undefined ? (): void => undefined : created.on(onEvent);
    controller.value = created;
    view.value = created.getView();
    release = (): void => {
      stopView();
      stopEvents();
      created.destroy();
    };
  };

  onMounted(() => {
    build();
    // Only a different session, microphone or player rebuilds: a new language or a toggled prop must not drop a live conversation.
    watch([() => toValue(options)?.session, () => toValue(options)?.microphone, () => toValue(options)?.player], build);
  });
  onBeforeUnmount(() => {
    release();
    release = (): void => undefined;
  });

  return { view, controller };
};
