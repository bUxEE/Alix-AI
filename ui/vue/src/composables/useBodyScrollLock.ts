import { onBeforeUnmount, watch, type Ref } from "vue";

/** Locks `document.body` scroll while `locked` is true. Restores the previous overflow on unlock or unmount. */
export const useBodyScrollLock = (locked: Ref<boolean>): void => {
  let previous: string | undefined;
  const apply = (on: boolean): void => {
    if (typeof document === "undefined") return;
    if (on) {
      if (previous === undefined) previous = document.body.style.overflow;
      document.body.style.overflow = "hidden";
      return;
    }
    if (previous === undefined) return;
    document.body.style.overflow = previous;
    previous = undefined;
  };
  watch(locked, apply, { immediate: true });
  onBeforeUnmount(() => apply(false));
};
