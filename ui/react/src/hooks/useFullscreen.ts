import { useEffect, useId, useRef, useState } from "react";
import type { RefObject } from "react";

const FOCUSABLE =
  "button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])";

const list = (root: HTMLElement | null): HTMLElement[] =>
  root === null ? [] : Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE)).filter((el) => el.tabIndex !== -1);

export interface UseFullscreenOptions {
  /** Controlled value. Omit for uncontrolled. */
  fullscreen?: boolean;
  /** Uncontrolled initial value. */
  defaultFullscreen?: boolean;
  onFullscreenChange?: (value: boolean) => void;
  rootRef: RefObject<HTMLElement | null>;
  toggleRef: RefObject<HTMLElement | null>;
}

export interface UseFullscreen {
  isFullscreen: boolean;
  setFullscreen: (value: boolean) => void;
  toggleFullscreen: () => void;
  labelId: string;
}

/** Body scroll lock, focus trap/restore, and Esc-to-exit for a fullscreen panel overlay. */
export const useFullscreen = ({
  fullscreen,
  defaultFullscreen = false,
  onFullscreenChange,
  rootRef,
  toggleRef,
}: UseFullscreenOptions): UseFullscreen => {
  const [internal, setInternal] = useState(defaultFullscreen);
  const controlled = fullscreen !== undefined;
  const isFullscreen = controlled ? fullscreen : internal;
  const previousFocus = useRef<HTMLElement | null>(null);
  const labelId = useId();

  const setFullscreen = (value: boolean): void => {
    if (!controlled) setInternal(value);
    onFullscreenChange?.(value);
  };
  const setFullscreenRef = useRef(setFullscreen);
  setFullscreenRef.current = setFullscreen;

  useEffect(() => {
    if (typeof document === "undefined") return undefined;
    if (!isFullscreen) return undefined;
    const previous = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = previous;
    };
  }, [isFullscreen]);

  useEffect(() => {
    if (typeof document === "undefined") return undefined;
    if (!isFullscreen) return undefined;
    previousFocus.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const root = rootRef.current;
    const items = list(root);
    (items[0] ?? root)?.focus();

    const onKeydown = (event: KeyboardEvent): void => {
      if (event.key === "Escape") {
        if (event.defaultPrevented) return;
        const target = event.target;
        if (target instanceof Element) {
          const dialog = target.closest('[role="dialog"][aria-modal="true"]');
          if (dialog !== null && dialog !== rootRef.current) return;
        }
        event.preventDefault();
        setFullscreenRef.current(false);
        return;
      }
      if (event.key !== "Tab") return;
      const focusables = list(rootRef.current);
      const first = focusables[0];
      const last = focusables[focusables.length - 1];
      if (first === undefined || last === undefined) return;
      const focused = document.activeElement;
      if (event.shiftKey && (focused === first || focused === rootRef.current)) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && focused === last) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", onKeydown);
    return () => {
      document.removeEventListener("keydown", onKeydown);
      (toggleRef.current ?? previousFocus.current)?.focus();
      previousFocus.current = null;
    };
  }, [isFullscreen, rootRef, toggleRef]);

  return {
    isFullscreen,
    setFullscreen,
    toggleFullscreen: () => setFullscreen(!isFullscreen),
    labelId,
  };
};
