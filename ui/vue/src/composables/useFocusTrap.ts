import { nextTick, onBeforeUnmount, watch, type Ref } from "vue";

const FOCUSABLE = "button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])";

const list = (root: HTMLElement | null): HTMLElement[] =>
  root === null ? [] : Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE)).filter((el) => !el.hasAttribute("disabled") && el.tabIndex !== -1);

/**
 * While `active` is true, Tab cycles inside `root` and focus is moved into it on enter.
 * On exit, focus returns to `restore` (or the previously focused element).
 */
export const useFocusTrap = (root: Ref<HTMLElement | null>, active: Ref<boolean>, restore?: Ref<HTMLElement | null>): void => {
  let previous: HTMLElement | null = null;

  const onKeydown = (event: KeyboardEvent): void => {
    if (!active.value || event.key !== "Tab") return;
    const items = list(root.value);
    const first = items[0];
    const last = items[items.length - 1];
    if (first === undefined || last === undefined) return;
    const focused = document.activeElement;
    if (event.shiftKey && (focused === first || focused === root.value)) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && focused === last) {
      event.preventDefault();
      first.focus();
    }
  };

  watch(
    active,
    async (on, was) => {
      if (typeof document === "undefined") return;
      if (on) {
        previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
        document.addEventListener("keydown", onKeydown);
        await nextTick();
        const items = list(root.value);
        (items[0] ?? root.value)?.focus();
        return;
      }
      document.removeEventListener("keydown", onKeydown);
      if (was) (restore?.value ?? previous)?.focus();
      previous = null;
    },
    { flush: "post" },
  );

  onBeforeUnmount(() => {
    if (typeof document === "undefined") return;
    document.removeEventListener("keydown", onKeydown);
  });
};
