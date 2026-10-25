import { onMounted, onBeforeUnmount, ref, type Ref } from "vue";

// Browser-only state. Nothing here touches `window`, `document` or `navigator` outside `onMounted`, so the components that use
// it render on the server with the defaults (motion allowed, online) and correct themselves on the client.

const listen = (query: string, onChange: (matches: boolean) => void): (() => void) => {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") return () => undefined;
  const media = window.matchMedia(query);
  const handler = (event: MediaQueryListEvent): void => onChange(event.matches);
  onChange(media.matches);
  media.addEventListener("change", handler);
  return () => media.removeEventListener("change", handler);
};

/** True when the user asks for reduced motion. JavaScript-driven motion (scrolling, waveforms) reads this; CSS reads the media query. */
export const useReducedMotion = (): Ref<boolean> => {
  const reduced = ref(false);
  let stop = (): void => undefined;
  onMounted(() => {
    stop = listen("(prefers-reduced-motion: reduce)", (matches) => {
      reduced.value = matches;
    });
  });
  onBeforeUnmount(() => stop());
  return reduced;
};

/** The browser's own online state, for the offline banner. Defaults to online until mounted. */
export const useOnline = (): Ref<boolean> => {
  const online = ref(true);
  const update = (): void => {
    online.value = typeof navigator === "undefined" ? true : navigator.onLine;
  };
  onMounted(() => {
    update();
    window.addEventListener("online", update);
    window.addEventListener("offline", update);
  });
  onBeforeUnmount(() => {
    if (typeof window === "undefined") return;
    window.removeEventListener("online", update);
    window.removeEventListener("offline", update);
  });
  return online;
};
