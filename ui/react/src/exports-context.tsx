import { createContext, useContext } from "react";
import type { ReactElement, ReactNode } from "react";
import type { AlixDownload, AlixExporter } from "./export.js";

export interface AlixExportsContextValue {
  readonly enabled: boolean;
  readonly sessionId: string;
  readonly exporter?: AlixExporter;
  readonly download?: AlixDownload;
}

const AlixExportsContext = createContext<AlixExportsContextValue>({ enabled: false, sessionId: "" });

export const AlixExportsProvider = ({
  value,
  children,
}: {
  value: AlixExportsContextValue;
  children: ReactNode;
}): ReactElement => <AlixExportsContext.Provider value={value}>{children}</AlixExportsContext.Provider>;

export const useAlixExports = (): AlixExportsContextValue => useContext(AlixExportsContext);
