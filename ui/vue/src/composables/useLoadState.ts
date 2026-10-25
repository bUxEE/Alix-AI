import { computed, ref, watch, type ComputedRef, type Ref } from "vue";

// Loading is two situations, never one boolean. A screen that has never had data shows a skeleton shaped like the content;
// a screen that is refreshing data it already shows keeps that content and puts an overlay over it. Both are DERIVED from one
// `isLoading` flag and one `hasLoadedOnce` flag, so they cannot disagree.
//
// `hasLoadedOnce` flips on SUCCESS only. A retry after a failed first load is still a first load: there is nothing underneath
// to overlay. That is why `end` takes the outcome and the caller never writes it in a bare `finally`.

export interface LoadState {
  isLoading: Ref<boolean>;
  hasLoadedOnce: Ref<boolean>;
  /** Loading and nothing has ever loaded: show the skeleton. */
  isFirstLoad: ComputedRef<boolean>;
  /** Loading and content is already on screen: show the overlay. */
  isRefreshing: ComputedRef<boolean>;
  begin: () => void;
  end: (succeeded: boolean) => void;
  reset: () => void;
}

export const useLoadState = (): LoadState => {
  const isLoading = ref(false);
  const hasLoadedOnce = ref(false);
  const isFirstLoad = computed(() => isLoading.value && !hasLoadedOnce.value);
  const isRefreshing = computed(() => isLoading.value && hasLoadedOnce.value);
  const begin = (): void => {
    isLoading.value = true;
  };
  const end = (succeeded: boolean): void => {
    isLoading.value = false;
    if (succeeded) hasLoadedOnce.value = true;
  };
  const reset = (): void => {
    isLoading.value = false;
    hasLoadedOnce.value = false;
  };
  return { isLoading, hasLoadedOnce, isFirstLoad, isRefreshing, begin, end, reset };
};

/**
 * The same two derived states for a component whose host owns the request and passes it in as props. `loading` and `failed` come
 * from the host; whether there is data to show is `hasData`. A load that ends without data or with a failure is not "loaded".
 */
export const useLoadFromProps = (source: { loading: () => boolean; failed: () => boolean; hasData: () => boolean }): LoadState => {
  const state = useLoadState();
  watch(
    () => [source.loading(), source.failed(), source.hasData()] as const,
    ([loading, failed, hasData]) => {
      if (loading) state.begin();
      else state.end(!failed && hasData);
    },
    { immediate: true },
  );
  return state;
};
