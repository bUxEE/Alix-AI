import { computed, ref, watch } from "vue";
import type { DemoLocale } from "../../demo-mock/src/index";
import { DEMO_TEXT, type DemoTextKey } from "./text";

// The demo's own preferences: the language and the colour scheme. Module state, shared by every section. They are remembered in
// this browser only (a convenience; the demo works the same when storage is blocked).

export type DemoTheme = "system" | "light" | "dark";

const STORE_KEY = "alix-demo-preferences";

const readStored = (): { locale?: DemoLocale; theme?: DemoTheme } => {
  try {
    const parsed = JSON.parse(globalThis.localStorage?.getItem(STORE_KEY) ?? "{}") as Record<string, unknown>;
    return {
      ...(parsed["locale"] === "en" || parsed["locale"] === "it" ? { locale: parsed["locale"] } : {}),
      ...(parsed["theme"] === "system" || parsed["theme"] === "light" || parsed["theme"] === "dark" ? { theme: parsed["theme"] } : {}),
    };
  } catch {
    return {};
  }
};

const stored = readStored();
const browserLocale = (): DemoLocale => (globalThis.navigator?.language?.toLowerCase().startsWith("it") === true ? "it" : "en");

export const locale = ref<DemoLocale>(stored.locale ?? browserLocale());
export const theme = ref<DemoTheme>(stored.theme ?? "system");

watch([locale, theme], ([nextLocale, nextTheme]) => {
  try {
    globalThis.localStorage?.setItem(STORE_KEY, JSON.stringify({ locale: nextLocale, theme: nextTheme }));
  } catch {
    // storage blocked: the preference lasts for this visit only
  }
});

/** A conversation another section asked the chat panel to open (the chat section takes it and clears it). */
export const requestedThread = ref<string>();

/** The demo shell's words in the current language. `{name}` placeholders are filled from `params`. */
export const useDemoText = () => {
  const table = computed(() => DEMO_TEXT[locale.value]);
  return (key: DemoTextKey, params: Record<string, string | number> = {}): string =>
    table.value[key].replace(/\{(\w+)\}/g, (whole, name: string) => (name in params ? String(params[name]) : whole));
};
