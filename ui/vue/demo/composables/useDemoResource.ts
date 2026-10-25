import { useLoadState, type LoadState } from "@alixai/vue";
import { ref, shallowRef, type Ref, type ShallowRef } from "vue";

// One thing the demo reads from the mock, with the kit's two loading states: `state.isFirstLoad` (nothing on screen yet, show a
// skeleton) and `state.isRefreshing` (keep what is shown, put an overlay over it). Both derive from one flag; `hasLoadedOnce`
// flips on success only, so a retry after a failed first load is still a first load.

export interface DemoResource<T> {
  data: ShallowRef<T | undefined>;
  error: Ref<string | undefined>;
  state: LoadState;
  refresh: () => Promise<void>;
}

export const useDemoResource = <T>(read: (signal: AbortSignal) => Promise<T>): DemoResource<T> => {
  const data = shallowRef<T>();
  const error = ref<string>();
  const state = useLoadState();
  let current: AbortController | undefined;

  const refresh = async (): Promise<void> => {
    current?.abort();
    const own = new AbortController();
    current = own;
    state.begin();
    try {
      const value = await read(own.signal);
      if (own.signal.aborted) return;
      data.value = value;
      error.value = undefined;
      state.end(true);
    } catch (failure) {
      if (own.signal.aborted) return;
      error.value = failure instanceof Error ? failure.message : String(failure);
      state.end(false);
    }
  };

  return { data, error, state, refresh };
};
