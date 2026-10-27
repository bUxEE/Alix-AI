// The scenario registry: what the demo can show, the prompt that shows it (English and Italian) and the words that route a typed
// message to it. The demo apps list `demoPrompts` as "try these" buttons; the mock routes every message with `matchScenario`.
// This file is browser safe: no Node import.

export type DemoLocale = "en" | "it";

export interface DemoText {
  readonly en: string;
  readonly it: string;
}

export type DemoScenarioId =
  | "help"
  | "markdown"
  | "chart_bar"
  | "chart_line"
  | "table"
  | "diagram"
  | "image"
  | "citations"
  | "clarification"
  | "confirmation"
  | "step_up"
  | "progress"
  | "unavailable"
  | "refusal"
  | "long"
  | "reconnect"
  | "memory_proposal"
  | "analyst";

export interface DemoPrompt {
  readonly id: DemoScenarioId;
  /** A short label for a button. */
  readonly title: DemoText;
  /** The message that runs the scenario. */
  readonly prompt: DemoText;
  /** What the scenario shows, for a tooltip or a caption. */
  readonly shows: DemoText;
  /** Words that route a typed message here (matched at the start of a word, case and accents ignored). */
  readonly keywords: { readonly en: readonly string[]; readonly it: readonly string[] };
  /** The kits that render the whole scenario. Absent: both. */
  readonly kits?: readonly ("vue" | "react")[];
}

export const demoPrompts: readonly DemoPrompt[] = [
  {
    id: "help",
    title: { en: "What can you do?", it: "Cosa sai fare?" },
    prompt: { en: "What can you do?", it: "Cosa sai fare?" },
    shows: { en: "The list of demo prompts, and follow-up suggestions.", it: "L'elenco dei prompt di prova e i suggerimenti." },
    keywords: { en: ["help", "what can you do", "demo", "examples", "prompts"], it: ["aiuto", "cosa sai fare", "cosa puoi fare", "esempi"] },
  },
  {
    id: "markdown",
    title: { en: "Streamed Markdown", it: "Markdown in streaming" },
    prompt: { en: "Give me an overview of our maintenance services", it: "Dammi una panoramica dei servizi di manutenzione" },
    shows: { en: "A streamed answer with headings, lists, bold, code, a quote and a link.", it: "Una risposta in streaming con titoli, elenchi, grassetto, codice, citazione e link." },
    keywords: { en: ["overview", "markdown", "summary", "services"], it: ["panoramica", "riepilogo", "servizi"] },
  },
  {
    id: "chart_bar",
    title: { en: "Bar chart", it: "Grafico a barre" },
    prompt: { en: "Show open work orders per site as a bar chart", it: "Mostra gli ordini di lavoro aperti per sede in un grafico a barre" },
    shows: { en: "A chart block (bar, two series).", it: "Un blocco grafico (barre, due serie)." },
    keywords: { en: ["bar chart", "per site", "chart", "graph"], it: ["grafico a barre", "per sede", "grafico"] },
  },
  {
    id: "chart_line",
    title: { en: "Line chart", it: "Grafico a linee" },
    prompt: { en: "Plot monthly energy use at Harbor Point and Westfield", it: "Traccia i consumi energetici mensili di Harbor Point e Westfield" },
    shows: { en: "A chart block (line, monthly series).", it: "Un blocco grafico (linee, serie mensile)." },
    keywords: { en: ["energy", "line chart", "trend", "monthly", "plot"], it: ["energia", "energetic", "consumi", "andamento", "mensil", "traccia"] },
  },
  {
    id: "table",
    title: { en: "Table", it: "Tabella" },
    prompt: { en: "List the open work orders", it: "Elenca gli ordini di lavoro aperti" },
    shows: { en: "A table block with text, number, date and boolean columns.", it: "Un blocco tabella con colonne di testo, numeri, date e sì/no." },
    keywords: { en: ["table", "list", "open work orders"], it: ["tabella", "elenca", "ordini di lavoro aperti", "ordini aperti"] },
  },
  {
    id: "diagram",
    title: { en: "Diagram", it: "Diagramma" },
    prompt: { en: "Draw the work-order process as a diagram", it: "Disegna il processo degli ordini di lavoro come diagramma" },
    shows: { en: "A diagram block (Mermaid source, drawn by the host's renderer).", it: "Un blocco diagramma (sorgente Mermaid, disegnato dal renderer dell'host)." },
    keywords: { en: ["diagram", "process", "flow", "draw"], it: ["diagramma", "processo", "flusso", "disegna"] },
  },
  {
    id: "image",
    title: { en: "Images", it: "Immagini" },
    prompt: { en: "Show me the floor plan of Harbor Point level 3", it: "Mostrami la planimetria del piano 3 di Harbor Point" },
    shows: { en: "Image files in the conversation, previewed through the files API.", it: "File immagine nella conversazione, con anteprima dalle API dei file." },
    keywords: { en: ["floor plan", "image", "photo", "picture"], it: ["planimetria", "immagine", "immagini", "foto"] },
  },
  {
    id: "citations",
    title: { en: "Answer with sources", it: "Risposta con fonti" },
    prompt: { en: "What does the maintenance policy say about HVAC filters?", it: "Cosa dice la politica di manutenzione sui filtri HVAC?" },
    shows: { en: "A grounded answer with citations (pages, sections, a stale source).", it: "Una risposta fondata con citazioni (pagine, sezioni, una fonte non aggiornata)." },
    keywords: { en: ["policy", "sources", "cite", "according to", "contract says", "warranty"], it: ["politica", "fonti", "cita", "secondo", "garanzia"] },
  },
  {
    id: "clarification",
    title: { en: "Clarification", it: "Chiarimento" },
    prompt: { en: "Schedule a fire-safety inspection at Riverside Depot", it: "Programma un'ispezione antincendio al deposito Riverside" },
    shows: { en: "A question with choices; the choice leads to a confirmation.", it: "Una domanda con scelte; la scelta porta a una conferma." },
    keywords: { en: ["schedule", "inspection", "book"], it: ["programma", "ispezione", "prenota"] },
  },
  {
    id: "confirmation",
    title: { en: "Action with confirmation", it: "Azione da confermare" },
    prompt: { en: "Replace the HVAC filters at Harbor Point", it: "Sostituisci i filtri HVAC a Harbor Point" },
    shows: { en: "A prepared action (risk level 2): the card, the confirmation sheet, then a receipt or a declined message.", it: "Un'azione preparata (rischio 2): la scheda, la conferma, poi la ricevuta o l'annullamento." },
    keywords: { en: ["replace", "create a work order", "dispatch", "send a technician"], it: ["sostituisci", "crea un ordine", "invia un tecnico"] },
  },
  {
    id: "step_up",
    title: { en: "Continue in the app", it: "Continua nell'app" },
    prompt: { en: "Terminate the cleaning contract with Brightline", it: "Disdici il contratto di pulizia con Brightline" },
    shows: { en: "A risk level 3 action that the assistant hands over to the application.", it: "Un'azione di rischio 3 che l'assistente passa all'applicazione." },
    keywords: { en: ["terminate", "end the contract", "step up", "step-up"], it: ["disdici", "recedi", "termina il contratto"] },
  },
  {
    id: "progress",
    title: { en: "Action in progress", it: "Azione in corso" },
    prompt: { en: "Export the Q3 maintenance report", it: "Esporta il report di manutenzione del terzo trimestre" },
    shows: { en: "Pipeline steps, a progress line that fills up, a queued receipt and an action card.", it: "Fasi, una barra di avanzamento, una ricevuta in coda e una scheda azione." },
    keywords: { en: ["export", "download the report"], it: ["esporta", "scarica il report"] },
  },
  {
    id: "unavailable",
    title: { en: "Service unavailable", it: "Servizio non disponibile" },
    prompt: { en: "Check the live sensor data for Westfield Data Hall", it: "Controlla i dati live dei sensori del Westfield Data Hall" },
    shows: { en: "An error state: the system behind the assistant does not answer.", it: "Uno stato di errore: il sistema dietro l'assistente non risponde." },
    keywords: { en: ["sensor", "live", "outage", "unavailable"], it: ["sensori", "dal vivo", "non disponibile", "guasto"] },
  },
  {
    id: "refusal",
    title: { en: "Refusal", it: "Rifiuto" },
    prompt: { en: "Export everyone's salary details", it: "Esporta gli stipendi di tutti i dipendenti" },
    shows: { en: "A request the assistant refuses: it is not something the application allows.", it: "Una richiesta rifiutata: l'applicazione non la consente." },
    keywords: { en: ["salary", "salaries", "password", "delete all"], it: ["stipendi", "stipendio", "password", "cancella tutti"] },
  },
  {
    id: "long",
    title: { en: "Full report", it: "Report completo" },
    prompt: { en: "Give me the full Q3 facilities report", it: "Dammi il report completo del terzo trimestre" },
    shows: { en: "Text, a table, three charts and citations in one answer.", it: "Testo, una tabella, tre grafici e citazioni in una sola risposta." },
    keywords: { en: ["full report", "full q3", "quarterly report", "everything"], it: ["report completo", "rapporto completo", "tutto"] },
  },
  {
    id: "reconnect",
    title: { en: "Dropped connection", it: "Connessione interrotta" },
    prompt: { en: "Simulate a dropped connection", it: "Simula una connessione interrotta" },
    shows: {
      en: "The stream is cut halfway once; the Vue client reconnects with its cursor and nothing is shown twice.",
      it: "Lo stream si interrompe a metà una volta; il client Vue si riconnette con il cursore e nulla appare due volte.",
    },
    keywords: { en: ["dropped connection", "reconnect", "flaky"], it: ["connessione interrotta", "riconnetti", "instabile"] },
    kits: ["vue"],
  },
  {
    id: "memory_proposal",
    title: { en: "Remember this?", it: "Vuoi ricordarlo?" },
    prompt: { en: "Remember that I prefer concise answers", it: "Ricorda che preferisco risposte concise" },
    shows: {
      en: "A memory proposal card (Remember / Don't remember). Needs the chat panel `memory` prop.",
      it: "Una card di proposta memoria (Ricorda / Non ricordare). Serve la prop `memory` del pannello chat.",
    },
    keywords: { en: ["remember that", "prefer concise", "memory proposal"], it: ["ricorda che", "preferisco risposte", "proposta memoria"] },
  },
  {
    id: "analyst",
    title: { en: "Analyst blocks", it: "Blocchi analyst" },
    prompt: { en: "Show me an analyst summary with every new block", it: "Mostrami un riepilogo analyst con tutti i nuovi blocchi" },
    shows: {
      en: "KPI, timeline, record card, provenance, draft, scatter/heatmap/combo charts and source labels.",
      it: "KPI, timeline, scheda record, provenance, bozza, grafici scatter/heatmap/combo e etichette di fonte.",
    },
    keywords: { en: ["analyst", "kpi", "provenance", "every new block"], it: ["analyst", "kpi", "provenance", "tutti i nuovi blocchi"] },
  },
];

// The order in which scenarios claim a message: the more specific first.
const MATCH_ORDER: readonly DemoScenarioId[] = [
  "help",
  "reconnect",
  "memory_proposal",
  "analyst",
  "refusal",
  "unavailable",
  "step_up",
  "confirmation",
  "clarification",
  "long",
  "progress",
  "citations",
  "image",
  "diagram",
  "chart_line",
  "chart_bar",
  "table",
  "markdown",
];

const fold = (text: string): string =>
  text
    .normalize("NFD")
    .replace(/[̀-ͯ]/g, "")
    .toLowerCase()
    .replace(/[’`]/g, "'");

// A keyword matches at the start of a word: "live" is in "live data" and not in "delivery".
const hasWord = (folded: string, keyword: string): boolean => {
  const k = fold(keyword);
  let from = 0;
  for (;;) {
    const at = folded.indexOf(k, from);
    if (at === -1) return false;
    if (at === 0 || !/[a-z0-9]/.test(folded[at - 1] as string)) return true;
    from = at + 1;
  }
};

const IT_WORDS = ["il", "lo", "la", "gli", "le", "di", "del", "della", "dei", "che", "per", "una", "un", "cosa", "sono", "mostra", "dammi", "quali", "come", "nel", "al", "con", "puoi"];
const EN_WORDS = ["the", "of", "for", "and", "what", "show", "me", "is", "are", "my", "our", "how", "can", "with", "please", "give"];

const countWords = (folded: string, words: readonly string[]): number => {
  const tokens = folded.split(/[^a-z0-9']+/);
  return tokens.filter((token) => words.includes(token)).length;
};

/** The language a message is written in, as far as a few common words tell: English unless it reads as Italian. */
export const detectLocale = (text: string): DemoLocale => {
  const folded = fold(text);
  return countWords(folded, IT_WORDS) > countWords(folded, EN_WORDS) ? "it" : "en";
};

export interface ScenarioMatch {
  readonly id: DemoScenarioId | undefined;
  /** The language the matching words were in, when only one language matched. */
  readonly locale: DemoLocale | undefined;
}

/** The scenario a typed message runs (undefined when nothing matches) and the language its words suggest. */
export const matchScenario = (text: string): ScenarioMatch => {
  const folded = fold(text);
  for (const id of MATCH_ORDER) {
    const prompt = demoPrompts.find((entry) => entry.id === id);
    if (prompt === undefined) continue;
    const en = prompt.keywords.en.some((keyword) => hasWord(folded, keyword));
    const it = prompt.keywords.it.some((keyword) => hasWord(folded, keyword));
    if (en || it) return { id, locale: en && !it ? "en" : it && !en ? "it" : undefined };
  }
  return { id: undefined, locale: undefined };
};

/** The demo prompt of a scenario. */
export const demoPrompt = (id: DemoScenarioId): DemoPrompt => {
  const found = demoPrompts.find((entry) => entry.id === id);
  if (found === undefined) throw new Error(`no demo prompt ${id}`);
  return found;
};
