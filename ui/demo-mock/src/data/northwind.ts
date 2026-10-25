import type { Citation } from "@alixai/sdk";
import { fixedId } from "../ids";
import type { DemoLocale, DemoText } from "../prompts";
import { SEEDED_FILES } from "../files/store";

// The fictional company every scenario talks about: Northwind Facilities, a facility-management firm that runs four sites. The
// figures are consistent across scenarios (the table, the charts, the report and the CSV fixture tell the same story).

export const t = (locale: DemoLocale, text: DemoText): string => text[locale];

export const SITES = {
  harbor: "Harbor Point Office Tower",
  riverside: "Riverside Logistics Depot",
  elm: "Elm Street Medical Center",
  westfield: "Westfield Data Hall",
} as const;

export const SOURCE_IDS = Object.freeze({
  maintenancePolicy: fixedId("src", 1),
  hvacContract: fixedId("src", 2),
  fireSchedule: fixedId("src", 3),
  ventilation: fixedId("src", 4),
  supplierPortal: fixedId("src", 5),
});

const policyClaim: DemoText = {
  en: "Air handling unit filters are replaced every 3 months, or earlier when the pressure drop exceeds 250 Pa.",
  it: "I filtri delle unità di trattamento aria si sostituiscono ogni 3 mesi, o prima se la perdita di carico supera 250 Pa.",
};
const classClaim: DemoText = {
  en: "Offices use ePM1 55% (F7) filters on supply air; clinical areas use ePM1 80% (F9).",
  it: "Gli uffici usano filtri ePM1 55% (F7) sull'aria di mandata; le aree cliniche ePM1 80% (F9).",
};
const contractClaim: DemoText = {
  en: "Coolwave supplies the filters and includes up to 4 replacements per unit per year in the fee; extra ones are billed at cost plus 8%.",
  it: "Coolwave fornisce i filtri e include fino a 4 sostituzioni per unità all'anno nel canone; le ulteriori sono fatturate al costo più l'8%.",
};
const ventilationClaim: DemoText = {
  en: "At Elm Street Medical Center the pressure drop of clinical filters is checked monthly.",
  it: "All'Elm Street Medical Center la perdita di carico dei filtri clinici è controllata ogni mese.",
};
const fireClaim: DemoText = {
  en: "The full fire-safety inspection of Riverside Logistics Depot is due by 31 October.",
  it: "L'ispezione antincendio completa del Riverside Logistics Depot va fatta entro il 31 ottobre.",
};

/** The citations of the policy answer: a policy (two locators), the supplier contract, and a stale site document. */
export const filterCitations = (locale: DemoLocale): Citation[] => [
  {
    source_id: SOURCE_IDS.maintenancePolicy,
    display_name: t(locale, { en: "Preventive Maintenance Policy", it: "Politica di manutenzione preventiva" }),
    version: "v12",
    locator: { page: 4, section: "3.2" },
    availability: "available",
    staleness: "current",
    open_ref: { kind: "file", file_id: SEEDED_FILES.maintenancePolicy },
    claims: [t(locale, policyClaim), t(locale, classClaim)],
  },
  {
    source_id: SOURCE_IDS.hvacContract,
    display_name: t(locale, { en: "HVAC Service Contract (Coolwave Ltd)", it: "Contratto di servizio HVAC (Coolwave Ltd)" }),
    version: "2025-03",
    locator: { page: 11, section: "Schedule B" },
    availability: "available",
    staleness: "current",
    open_ref: { kind: "file", file_id: SEEDED_FILES.hvacContract },
    claims: [t(locale, contractClaim)],
  },
  {
    source_id: SOURCE_IDS.ventilation,
    display_name: t(locale, { en: "Elm Street ventilation requirements", it: "Requisiti di ventilazione Elm Street" }),
    version: "v3",
    locator: { page: 2, section: "2" },
    availability: "available",
    staleness: "stale",
    open_ref: { kind: "file", file_id: SEEDED_FILES.ventilation },
    claims: [t(locale, ventilationClaim)],
  },
];

/** The citations of the quarterly report: the work-order record, the fire schedule and a source that is offline. */
export const reportCitations = (locale: DemoLocale): Citation[] => [
  {
    source_id: SOURCE_IDS.fireSchedule,
    display_name: t(locale, { en: "Fire Safety Inspection Schedule 2026", it: "Calendario ispezioni antincendio 2026" }),
    version: "2026-09-30",
    locator: { page: 3, section: "Q4" },
    availability: "available",
    staleness: "current",
    open_ref: { kind: "file", file_id: SEEDED_FILES.fireSchedule },
    claims: [t(locale, fireClaim)],
  },
  {
    source_id: SOURCE_IDS.maintenancePolicy,
    display_name: t(locale, { en: "Preventive Maintenance Policy", it: "Politica di manutenzione preventiva" }),
    version: "v12",
    locator: { page: 5, section: "4" },
    availability: "available",
    staleness: "current",
    open_ref: { kind: "file", file_id: SEEDED_FILES.maintenancePolicy },
    claims: [t(locale, { en: "P1 work orders are answered within 4 hours.", it: "Gli ordini P1 ricevono risposta entro 4 ore." })],
  },
  {
    source_id: SOURCE_IDS.supplierPortal,
    display_name: t(locale, { en: "Supplier portal: work-order history", it: "Portale fornitori: storico ordini" }),
    version: "2026-10-01",
    locator: { record: "WO-2301" },
    availability: "unavailable",
    staleness: "unknown",
    open_ref: { kind: "record", entity_type: "work_order", entity_id: "WO-2301" },
    claims: [t(locale, { en: "WO-2301 at Westfield Data Hall is overdue.", it: "WO-2301 al Westfield Data Hall è in ritardo." })],
  },
];

export interface WorkOrderRow {
  readonly id: string;
  readonly site: string;
  readonly category: DemoText;
  readonly priority: "P1" | "P2" | "P3";
  readonly due: string;
  readonly overdue: boolean;
  readonly hours: number;
}

export const OPEN_WORK_ORDERS: readonly WorkOrderRow[] = [
  { id: "WO-2291", site: SITES.harbor, category: { en: "HVAC", it: "HVAC" }, priority: "P2", due: "2026-10-14", overdue: false, hours: 3.5 },
  { id: "WO-2294", site: SITES.riverside, category: { en: "Fire safety", it: "Antincendio" }, priority: "P1", due: "2026-10-03", overdue: true, hours: 6 },
  { id: "WO-2297", site: SITES.elm, category: { en: "Plumbing", it: "Idraulica" }, priority: "P3", due: "2026-10-20", overdue: false, hours: 2 },
  { id: "WO-2301", site: SITES.westfield, category: { en: "Electrical", it: "Elettrico" }, priority: "P1", due: "2026-10-06", overdue: true, hours: 8 },
  { id: "WO-2305", site: SITES.harbor, category: { en: "Lifts", it: "Ascensori" }, priority: "P2", due: "2026-10-12", overdue: false, hours: 4 },
  { id: "WO-2309", site: SITES.riverside, category: { en: "Doors and access", it: "Porte e accessi" }, priority: "P3", due: "2026-10-24", overdue: false, hours: 1.5 },
  { id: "WO-2312", site: SITES.westfield, category: { en: "Cooling", it: "Raffreddamento" }, priority: "P2", due: "2026-10-15", overdue: false, hours: 5 },
];

/** Open work orders per site, split into urgent (P1) and planned (P2/P3). */
export const OPEN_BY_SITE = [
  { site: "Harbor Point", urgent: 1, planned: 4 },
  { site: "Riverside", urgent: 2, planned: 3 },
  { site: "Elm Street", urgent: 0, planned: 2 },
  { site: "Westfield", urgent: 3, planned: 2 },
] as const;

/** Monthly energy use in MWh, April to September 2026 (the same figures as the energy chart image). */
export const ENERGY_MWH = [
  { month: "2026-04", harbor: 412, westfield: 688 },
  { month: "2026-05", harbor: 398, westfield: 702 },
  { month: "2026-06", harbor: 455, westfield: 741 },
  { month: "2026-07", harbor: 503, westfield: 797 },
  { month: "2026-08", harbor: 496, westfield: 802 },
  { month: "2026-09", harbor: 430, westfield: 735 },
] as const;

/** Q3 work orders per site: opened, closed, overdue at the end of the quarter, mean days to close. */
export const Q3_BY_SITE = [
  { site: SITES.harbor, opened: 64, closed: 61, overdue: 1, days: 2.4 },
  { site: SITES.riverside, opened: 48, closed: 41, overdue: 2, days: 3.9 },
  { site: SITES.elm, opened: 37, closed: 36, overdue: 0, days: 1.8 },
  { site: SITES.westfield, opened: 52, closed: 46, overdue: 3, days: 3.1 },
] as const;

/** Q3 maintenance spend by category, in thousands of euros. */
export const Q3_SPEND = [
  { category: { en: "HVAC", it: "HVAC" }, keur: 182 },
  { category: { en: "Electrical", it: "Elettrico" }, keur: 96 },
  { category: { en: "Fire safety", it: "Antincendio" }, keur: 58 },
  { category: { en: "Lifts", it: "Ascensori" }, keur: 44 },
  { category: { en: "Other", it: "Altro" }, keur: 61 },
] as const;
