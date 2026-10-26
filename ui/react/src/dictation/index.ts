// Dictation (record-to-text) surface. Import from `@alixai/react/dictation` so the chat host never pulls voice-conversation
// mode (orb, player, Voice Mode, session driver). Capture helpers are shared; conversation mode stays in `@alixai/react` / `./voice`.

export { AlixDictationButton } from "./AlixDictationButton.js";
export type { AlixDictationButtonProps } from "./AlixDictationButton.js";
export { useAlixDictation } from "./useAlixDictation.js";
export type { AlixDictation, DictationPhase, UseAlixDictationOptions, VoiceModeOption } from "./useAlixDictation.js";
export { createAlixMicrophone } from "./capture.js";
export type { AlixCaptureMicrophone } from "./capture.js";
export { DICTATION_MESSAGES, dictationEn, dictationIt, resolveDictationMessages, useAlixDictationText } from "./messages.js";
export type { AlixDictationMessageKey, AlixDictationMessageOverrides, AlixDictationMessages, AlixDictationText } from "./messages.js";

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

/** Inserts recognised text at the caret (replacing any selection) in CopilotKit's composer textarea without sending. */
export const insertIntoCopilotTextarea = (panel: ParentNode | null | undefined, insertion: string): void => {
  if (panel === null || panel === undefined) return;
  const textarea = panel.querySelector("textarea");
  if (!(textarea instanceof HTMLTextAreaElement)) return;
  const start = textarea.selectionStart ?? textarea.value.length;
  const end = textarea.selectionEnd ?? start;
  const next = insertAtCaret(textarea.value, insertion, start, end);
  textarea.value = next.text;
  textarea.setSelectionRange(next.caret, next.caret);
  textarea.dispatchEvent(new InputEvent("input", { bubbles: true, inputType: "insertText", data: insertion.trim() }));
  textarea.focus();
};
