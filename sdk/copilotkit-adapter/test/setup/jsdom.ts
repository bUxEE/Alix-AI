// jsdom lacks a few browser APIs CopilotKit's components call. They are stubbed here, only when a window exists.
if (typeof window !== "undefined") {
  if (typeof window.matchMedia !== "function") {
    window.matchMedia = ((query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addListener: () => undefined,
      removeListener: () => undefined,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
      dispatchEvent: () => false,
    })) as typeof window.matchMedia;
  }
  const noopObserver = class {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
    takeRecords(): [] {
      return [];
    }
  };
  const holder = globalThis as unknown as Record<string, unknown>;
  holder["ResizeObserver"] ??= noopObserver;
  holder["IntersectionObserver"] ??= noopObserver;
  Element.prototype.scrollIntoView ??= (): void => undefined;
  Element.prototype.scrollTo ??= (): void => undefined;
}
