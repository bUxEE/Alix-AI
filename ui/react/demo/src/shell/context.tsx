import { createContext, useContext } from "react";
import type { DemoLang, DemoText, SectionId } from "../lib/text";

export interface DemoEvent {
  readonly id: number;
  readonly name: string;
  readonly detail: string;
}

export interface DemoShell {
  readonly lang: DemoLang;
  readonly theme: "auto" | "light" | "dark";
  readonly text: DemoText;
  /** The conversation the chat shows. */
  readonly threadId: string;
  readonly openThread: (threadId: string) => void;
  readonly go: (section: SectionId) => void;
  /** Shows what a host callback received, as a short-lived note. */
  readonly notify: (name: string, detail: string) => void;
}

export const ShellContext = createContext<DemoShell | undefined>(undefined);

export const useShell = (): DemoShell => {
  const shell = useContext(ShellContext);
  if (shell === undefined) throw new Error("useShell is used outside the demo shell");
  return shell;
};
