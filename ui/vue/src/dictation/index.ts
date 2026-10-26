// Dictation (record-to-text) surface. Import from `@alixai/vue/dictation` so the chat host never pulls the voice-conversation
// orb, player or session driver. Capture helpers are shared; conversation mode stays in `@alixai/vue` / `./voice`.

export { default as AlixDictationButton } from "./AlixDictationButton.vue";
export { useAlixDictation } from "./useAlixDictation";
export type { AlixDictation, DictationPhase, UseAlixDictationOptions, VoiceModeOption } from "./useAlixDictation";
export { createAlixMicrophone } from "./capture";
export type { AlixCaptureMicrophone } from "./capture";

/** Inserts at `selectionStart` (default: end), replacing through `selectionEnd` when given. */
export const insertAtCaret = (
  current: string,
  insertion: string,
  selectionStart?: number,
  selectionEnd?: number,
): { text: string; caret: number } => {
  const piece = insertion.trim();
  if (piece === "") return { text: current, caret: selectionStart ?? current.length };
  const start = selectionStart === undefined ? current.length : Math.max(0, Math.min(current.length, selectionStart));
  const end =
    selectionEnd === undefined ? start : Math.max(start, Math.min(current.length, selectionEnd));
  const before = current.slice(0, start);
  const after = current.slice(end);
  const needsSpace = before.length > 0 && !/\s$/.test(before) && !/^\s/.test(piece);
  const glued = `${before}${needsSpace ? " " : ""}${piece}${after}`;
  const nextCaret = before.length + (needsSpace ? 1 : 0) + piece.length;
  return { text: glued, caret: nextCaret };
};
