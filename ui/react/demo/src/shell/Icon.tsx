import type { ReactElement } from "react";

// Line icons for the shell, drawn here (no icon package): 24 grid, 1.75 stroke, round joins.

const PATHS = {
  chat: ["M5 5h14a1 1 0 0 1 1 1v9a1 1 0 0 1-1 1H10l-4 3v-3H5a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1z"],
  conversations: ["M4 6h16", "M4 12h16", "M4 18h10"],
  memory: ["M12 3l7 3v5c0 4.5-3 8.5-7 10-4-1.5-7-5.5-7-10V6z", "M9 12l2 2 4-4"],
  voice: ["M12 3a3 3 0 0 0-3 3v6a3 3 0 0 0 6 0V6a3 3 0 0 0-3-3z", "M5 11a7 7 0 0 0 14 0", "M12 18v3"],
  files: ["M14 3H7a1 1 0 0 0-1 1v16a1 1 0 0 0 1 1h10a1 1 0 0 0 1-1V7z", "M14 3v4h4"],
  settings: ["M4 7h10", "M18 7h2", "M4 17h4", "M12 17h8", "M16 5v4", "M10 15v4"],
  sources: ["M3 12h4l3-7 4 14 3-7h4"],
  audit: ["M12 3l7 3v5c0 4.5-3 8.5-7 10-4-1.5-7-5.5-7-10V6z", "M9 12l2 2 4-4"],
  blocks: ["M4 4h7v7H4z", "M13 4h7v4h-7z", "M13 10h7v10h-7z", "M4 13h7v7H4z"],
  sun: ["M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8z", "M12 2v2", "M12 20v2", "M4.9 4.9l1.4 1.4", "M17.7 17.7l1.4 1.4", "M2 12h2", "M20 12h2", "M4.9 19.1l1.4-1.4", "M17.7 6.3l1.4-1.4"],
  check: ["M5 12.5l4.5 4.5L19 7.5"],
  close: ["M6 6l12 12", "M18 6L6 18"],
  arrow: ["M5 12h14", "M13 6l6 6-6 6"],
  plus: ["M12 5v14", "M5 12h14"],
  dot: ["M12 11a1 1 0 1 0 0 2 1 1 0 0 0 0-2z"],
} as const;

export type IconName = keyof typeof PATHS;

export const Icon = ({ name, size = 18 }: { name: IconName; size?: number }): ReactElement => (
  <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.75} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
    {PATHS[name].map((d) => (
      <path key={d} d={d} />
    ))}
  </svg>
);
