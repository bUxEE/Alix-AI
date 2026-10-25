import { useEffect, useState } from "react";
import type { ReactElement, ReactNode } from "react";
import { useAlixText } from "../i18n/context.js";

// The two loading states of a panel, derived from one flag: a first load (nothing on screen yet) shows a skeleton shaped like the
// content; a refetch keeps the previous content readable under a scrim. `hasLoadedOnce` flips on success only, so a retry after a
// failed first load is still a first load (there is nothing to overlay).

export interface AlixLoadState {
  readonly isLoading: boolean;
  readonly hasLoadedOnce: boolean;
  readonly isFirstLoad: boolean;
  readonly isRefreshing: boolean;
}

/** Derives the load state from a panel's props: `loading`, `failed`, and whether there is data. */
export const useAlixLoadState = (loading: boolean, failed: boolean, hasData: boolean): AlixLoadState => {
  const succeeded = hasData && !loading && !failed;
  const [hasLoadedOnce, setLoaded] = useState(succeeded);
  useEffect(() => {
    if (succeeded) setLoaded(true);
  }, [succeeded]);
  const loaded = hasLoadedOnce || succeeded;
  return { isLoading: loading, hasLoadedOnce: loaded, isFirstLoad: loading && !loaded, isRefreshing: loading && loaded };
};

export interface AlixLoadableProps {
  state: AlixLoadState;
  /** Shown on the first load, in place of the content: shape it like the content. */
  skeleton: ReactNode;
  children?: ReactNode;
}

/** Skeleton on the first load, the content under a scrim and a spinner on a refetch, the content otherwise. */
export const AlixLoadable = ({ state, skeleton, children }: AlixLoadableProps): ReactElement => {
  const text = useAlixText().common;
  if (state.isFirstLoad) {
    return (
      <div className="alix-loadable" aria-busy="true">
        <span className="alix-loadable__label">{text.loading}</span>
        {skeleton}
      </div>
    );
  }
  return (
    <div className="alix-loadable" {...(state.isRefreshing && { "aria-busy": "true" })}>
      {children}
      {state.isRefreshing && (
        <div className="alix-loadable__overlay" role="status">
          <span className="alix-loadable__spinner" aria-hidden="true" />
          <span className="alix-loadable__label">{text.loading}</span>
        </div>
      )}
    </div>
  );
};

/** A placeholder bar for a skeleton. */
export const AlixSkeleton = ({ width = "100%", height }: { width?: string; height?: string }): ReactElement => (
  <span className="alix-skeleton" aria-hidden="true" style={{ width, ...(height !== undefined && { height }) }} />
);

/** The refresh control of a panel header. */
export const AlixRefreshButton = ({ busy, onRefresh }: { busy: boolean; onRefresh: () => void }): ReactElement => {
  const text = useAlixText().common;
  return (
    <button type="button" className="alix-refresh" aria-label={text.refresh} title={text.refresh} disabled={busy} onClick={onRefresh}>
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
        <path d="M20 11a8 8 0 1 0-2.34 5.66" />
        <path d="M20 4v7h-7" />
      </svg>
    </button>
  );
};

/** The error of a panel that never loaded, with the way to try again. */
export const AlixLoadError = ({ message, onRetry }: { message: string; onRetry?: (() => void) | undefined }): ReactElement => {
  const text = useAlixText().common;
  return (
    <div className="alix-load-error" role="alert">
      <span>{message}</span>
      {onRetry !== undefined && (
        <button type="button" className="alix-load-error__retry" onClick={onRetry}>
          {text.retry}
        </button>
      )}
    </div>
  );
};
