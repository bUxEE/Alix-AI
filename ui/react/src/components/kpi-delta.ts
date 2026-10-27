/** Signed, accessible KPI delta display (ADR 0047). */
export type KpiDeltaView = { text: string; arrow: string; aria: string };

export const formatKpiDelta = (
  delta: string | number | null | undefined,
  formatNumber: (n: number) => string,
): KpiDeltaView | null => {
  if (delta === null || delta === undefined || delta === "") return null;
  let n: number | null = null;
  let text: string;
  if (typeof delta === "number") {
    n = delta;
    text = (delta > 0 ? "+" : "") + formatNumber(delta);
  } else {
    text = String(delta).trim();
    const m = text.match(/^([+\-−]?)\s*([0-9]+(?:[.,][0-9]+)?)/);
    if (m?.[2]) {
      const sign = m[1] === "-" || m[1] === "−" ? -1 : 1;
      n = sign * Number(m[2].replace(",", "."));
      if (!text.startsWith("+") && !text.startsWith("-") && !text.startsWith("−") && n > 0) {
        text = "+" + text;
      }
      if (text.startsWith("-")) text = "−" + text.slice(1);
    }
  }
  if (n === null || n === 0 || Number.isNaN(n)) {
    return { text, arrow: "", aria: `change ${text}` };
  }
  if (n > 0) return { text, arrow: "↑ ", aria: `up ${text}` };
  return { text, arrow: "↓ ", aria: `down ${text}` };
};
