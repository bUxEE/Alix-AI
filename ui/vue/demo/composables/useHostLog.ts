import { shallowRef, type ShallowRef } from "vue";

// A short, newest-first record of what the kit handed to the host page (see DemoHostLog).

export type HostLogEntry<T> = T & { readonly id: number; readonly at: string };

export interface HostLog<T> {
  entries: ShallowRef<readonly HostLogEntry<T>[]>;
  push: (entry: T) => void;
  clear: () => void;
}

let sequence = 0;
const clock = (): string => new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });

export const useHostLog = <T extends object>(limit = 8): HostLog<T> => {
  const entries = shallowRef<readonly HostLogEntry<T>[]>([]);
  const push = (entry: T): void => {
    sequence += 1;
    entries.value = [{ ...entry, id: sequence, at: clock() }, ...entries.value].slice(0, limit);
  };
  const clear = (): void => {
    entries.value = [];
  };
  return { entries, push, clear };
};
