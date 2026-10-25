// The words of the demo shell (the kit's own words come from the kit). English and Italian, no em dash.

export type DemoLang = "en" | "it";
export type SectionId = "chat" | "conversations" | "voice" | "files" | "settings" | "sources" | "audit" | "blocks";

interface SectionText {
  readonly nav: string;
  readonly title: string;
  readonly lead: string;
}

export interface DemoText {
  readonly product: string;
  readonly kit: string;
  readonly groups: { readonly conversation: string; readonly panels: string; readonly reference: string };
  readonly sections: Record<SectionId, SectionText>;
  readonly language: string;
  readonly theme: string;
  readonly themes: { readonly auto: string; readonly light: string; readonly dark: string };
  readonly offline: string;
  readonly events: string;
  readonly chat: {
    readonly variant: string;
    readonly variants: { readonly inline: string; readonly sidebar: string; readonly popup: string };
    readonly progress: string;
    readonly tryThese: string;
    readonly running: string;
    readonly confirmationOpen: string;
    readonly elsewhere: { readonly sidebar: string; readonly popup: string };
    readonly thread: string;
    readonly reconnectNote: string;
  };
  readonly conversations: { readonly open: string; readonly openLead: string; readonly failed: string; readonly delete: string; readonly current: string; readonly created: string };
  readonly voice: {
    readonly simulated: string;
    readonly simulatedHint: string;
    readonly speak: string;
    readonly mode: string;
    readonly buttonTitle: string;
    readonly buttonLead: string;
    readonly editorTitle: string;
    readonly editorLead: string;
    readonly sampleTranscript: string;
    readonly state: string;
    readonly sent: string;
    readonly discarded: string;
    readonly restart: string;
  };
  readonly files: {
    readonly composer: string;
    readonly composerLead: string;
    readonly samples: string;
    readonly sampleImage: string;
    readonly sampleCsv: string;
    readonly refusalSvg: string;
    readonly refusalHtml: string;
    readonly uploading: string;
    readonly library: string;
    readonly libraryLead: string;
    readonly viewer: string;
    readonly pick: string;
    readonly pdf: string;
    readonly pdfBlocked: string;
    readonly failed: string;
    readonly refused: string;
  };
  readonly settings: { readonly mode: string; readonly edit: string; readonly readOnly: string; readonly saved: string; readonly conflict: string; readonly cleared: string; readonly failed: string };
  readonly sources: { readonly reaudit: string; readonly reaudited: string; readonly preview: string; readonly live: string; readonly loading: string; readonly failed: string };
  readonly blocks: { readonly approved: string; readonly declined: string; readonly choose: string; readonly reset: string; readonly raise: string };
  readonly actions: { readonly onAction: string; readonly onOpenCitation: string; readonly onContinueInApp: string };
}

const en: DemoText = {
  product: "AlixAI",
  kit: "React kit",
  groups: { conversation: "Conversation", panels: "Panels", reference: "Reference" },
  sections: {
    chat: { nav: "Chat", title: "Chat", lead: "CopilotKit's chat with the AlixAI parts registered into it. Every reply comes from the scripted mock on this machine." },
    conversations: { nav: "Conversations", title: "Conversations", lead: "Data in, events out: the list shows the threads the host has, the host decides what switching does." },
    voice: { nav: "Voice", title: "Voice", lead: "Voice Mode and its orb on the mock's voice socket, and the talk button and transcript editor on their own." },
    files: { nav: "Files", title: "Files and previews", lead: "Attach, upload and preview. The kit never builds a file URL: the host draws every preview." },
    settings: { nav: "Settings", title: "Settings", lead: "The user's own settings as an editor, or the effective settings read only, with the level that decided each value." },
    sources: { nav: "Source health", title: "Source health", lead: "Which sources the answers can use, whether each is indexed and current, and what failed." },
    audit: { nav: "Integration audit", title: "Integration audit", lead: "What the audit of the installed package found, and how the model and service roles are doing." },
    blocks: { nav: "Blocks", title: "Blocks gallery", lead: "Every component on its own, with fixed data, outside the chat." },
  },
  language: "Language",
  theme: "Theme",
  themes: { auto: "Auto", light: "Light", dark: "Dark" },
  offline: "Local mock, no network",
  events: "Host events",
  chat: {
    variant: "Layout",
    variants: { inline: "Inline", sidebar: "Sidebar", popup: "Popup" },
    progress: "Progress line",
    tryThese: "Try these",
    running: "The assistant is answering.",
    confirmationOpen: "A confirmation is open. Approve or decline it in the chat first: AG-UI starts no new run until it is answered.",
    elsewhere: { sidebar: "The chat is in the sidebar at the edge of the page.", popup: "The chat is in the popup: open it with the button in the corner." },
    thread: "Conversation",
    reconnectNote: "The reconnect scenario is left out: CopilotKit's HttpAgent does not resume a cut stream.",
  },
  conversations: {
    open: "Open in chat",
    openLead: "Pick a conversation to continue it in the chat. New starts an empty one.",
    failed: "The conversations could not be loaded.",
    delete: "Delete",
    current: "Open in the chat",
    created: "New conversation created",
  },
  voice: {
    simulated: "Simulated microphone",
    simulatedHint: "A soft hum instead of your microphone. The mock answers with scripted transcripts either way.",
    speak: "Read replies aloud",
    mode: "Talk button",
    buttonTitle: "Talk button",
    buttonLead: "Owns no audio: it reports start, stop and cancel. Escape cancels.",
    editorTitle: "Transcript editor",
    editorLead: "What the microphone heard, editable before anything is sent.",
    sampleTranscript: "Book a fire-safety inspection at Riverside for Tuesday morning",
    state: "State",
    sent: "Sent",
    discarded: "Discarded",
    restart: "Start a new voice session",
  },
  files: {
    composer: "Attachment composer",
    composerLead: "Pick files, or make one below. The mock sniffs the type, refuses SVG and HTML, and takes up to 8 MB.",
    samples: "Make a file",
    sampleImage: "A PNG image",
    sampleCsv: "A CSV export",
    refusalSvg: "An SVG (refused)",
    refusalHtml: "An HTML page (refused)",
    uploading: "Uploading",
    library: "Files on the mock",
    libraryLead: "Seeded floor plan, chart, policies and export, plus your uploads.",
    viewer: "Preview",
    pick: "Pick a file to preview it.",
    pdf: "PDF, drawn by the browser from the file's content.",
    pdfBlocked: "This file may be previewed but not downloaded, so there is no PDF view.",
    failed: "The files could not be loaded.",
    refused: "Refused",
  },
  settings: {
    mode: "View",
    edit: "Editor",
    readOnly: "Read only",
    saved: "Settings saved",
    conflict: "The settings changed since they were read. They were read again: apply your change once more.",
    cleared: "onClearHistory: the host clears the history",
    failed: "The settings could not be saved.",
  },
  sources: { reaudit: "Re-audit sources", reaudited: "Re-audit: {checked} checked, {queued} queued, {failed} failed", preview: "Show", live: "Live data", loading: "First load", failed: "Failed load" },
  blocks: { approved: "Approved", declined: "Declined", choose: "Chose", reset: "Show again", raise: "Raise the interrupt" },
  actions: { onAction: "onAction", onOpenCitation: "onOpenCitation", onContinueInApp: "onContinueInApp" },
};

const it: DemoText = {
  product: "AlixAI",
  kit: "Kit React",
  groups: { conversation: "Conversazione", panels: "Pannelli", reference: "Riferimento" },
  sections: {
    chat: { nav: "Chat", title: "Chat", lead: "La chat di CopilotKit con le parti AlixAI registrate. Ogni risposta arriva dal mock su questa macchina." },
    conversations: { nav: "Conversazioni", title: "Conversazioni", lead: "Dati in ingresso, eventi in uscita: l'elenco mostra i thread dell'host, che decide cosa fa il cambio." },
    voice: { nav: "Voce", title: "Voce", lead: "Voice Mode e la sua sfera sul socket vocale del mock, e il pulsante e l'editor della trascrizione da soli." },
    files: { nav: "File", title: "File e anteprime", lead: "Allega, carica e visualizza. Il kit non costruisce mai un URL: ogni anteprima la disegna l'host." },
    settings: { nav: "Impostazioni", title: "Impostazioni", lead: "Le impostazioni dell'utente come editor, oppure quelle effettive in sola lettura, con il livello che ha deciso ogni valore." },
    sources: { nav: "Stato delle fonti", title: "Stato delle fonti", lead: "Quali fonti possono usare le risposte, se sono indicizzate e aggiornate, e cosa non è riuscito." },
    audit: { nav: "Verifica integrazione", title: "Verifica dell'integrazione", lead: "Cosa ha trovato la verifica del pacchetto installato, e come stanno i ruoli di modello e servizi." },
    blocks: { nav: "Blocchi", title: "Galleria dei blocchi", lead: "Ogni componente da solo, con dati fissi, fuori dalla chat." },
  },
  language: "Lingua",
  theme: "Tema",
  themes: { auto: "Auto", light: "Chiaro", dark: "Scuro" },
  offline: "Mock locale, nessuna rete",
  events: "Eventi per l'host",
  chat: {
    variant: "Disposizione",
    variants: { inline: "Nella pagina", sidebar: "Barra laterale", popup: "Popup" },
    progress: "Riga di avanzamento",
    tryThese: "Prova questi",
    running: "L'assistente sta rispondendo.",
    confirmationOpen: "C'è una conferma aperta. Approvala o rifiutala nella chat: AG-UI non avvia un'altra esecuzione finché non ha risposta.",
    elsewhere: { sidebar: "La chat è nella barra laterale al bordo della pagina.", popup: "La chat è nel popup: aprila con il pulsante nell'angolo." },
    thread: "Conversazione",
    reconnectNote: "Lo scenario di riconnessione è escluso: l'HttpAgent di CopilotKit non riprende un flusso interrotto.",
  },
  conversations: {
    open: "Apri nella chat",
    openLead: "Scegli una conversazione per continuarla nella chat. Nuova ne avvia una vuota.",
    failed: "Non è stato possibile caricare le conversazioni.",
    delete: "Elimina",
    current: "Aperta nella chat",
    created: "Nuova conversazione creata",
  },
  voice: {
    simulated: "Microfono simulato",
    simulatedHint: "Un ronzio al posto del tuo microfono. Il mock risponde comunque con trascrizioni preparate.",
    speak: "Leggi le risposte ad alta voce",
    mode: "Pulsante",
    buttonTitle: "Pulsante per parlare",
    buttonLead: "Non gestisce audio: segnala avvio, fine e annullamento. Esc annulla.",
    editorTitle: "Editor della trascrizione",
    editorLead: "Quello che il microfono ha sentito, modificabile prima che parta qualcosa.",
    sampleTranscript: "Prenota un'ispezione antincendio a Riverside per martedì mattina",
    state: "Stato",
    sent: "Inviato",
    discarded: "Scartato",
    restart: "Avvia una nuova sessione vocale",
  },
  files: {
    composer: "Composizione allegati",
    composerLead: "Scegli dei file, o creane uno qui sotto. Il mock riconosce il tipo, rifiuta SVG e HTML e accetta fino a 8 MB.",
    samples: "Crea un file",
    sampleImage: "Un'immagine PNG",
    sampleCsv: "Un export CSV",
    refusalSvg: "Un SVG (rifiutato)",
    refusalHtml: "Una pagina HTML (rifiutata)",
    uploading: "Caricamento",
    library: "File sul mock",
    libraryLead: "Planimetria, grafico, regolamenti ed export già presenti, più i tuoi caricamenti.",
    viewer: "Anteprima",
    pick: "Scegli un file per vederne l'anteprima.",
    pdf: "PDF, disegnato dal browser a partire dal contenuto del file.",
    pdfBlocked: "Questo file si può visualizzare in anteprima ma non scaricare, quindi niente vista PDF.",
    failed: "Non è stato possibile caricare i file.",
    refused: "Rifiutato",
  },
  settings: {
    mode: "Vista",
    edit: "Editor",
    readOnly: "Sola lettura",
    saved: "Impostazioni salvate",
    conflict: "Le impostazioni sono cambiate dopo la lettura. Le ho rilette: applica di nuovo la modifica.",
    cleared: "onClearHistory: l'host cancella la cronologia",
    failed: "Non è stato possibile salvare le impostazioni.",
  },
  sources: { reaudit: "Riverifica le fonti", reaudited: "Riverifica: {checked} controllate, {queued} in coda, {failed} non riuscite", preview: "Mostra", live: "Dati reali", loading: "Primo caricamento", failed: "Caricamento fallito" },
  blocks: { approved: "Approvata", declined: "Rifiutata", choose: "Scelto", reset: "Mostra di nuovo", raise: "Solleva l'interruzione" },
  actions: { onAction: "onAction", onOpenCitation: "onOpenCitation", onContinueInApp: "onContinueInApp" },
};

export const DEMO_TEXT: Record<DemoLang, DemoText> = { en, it };

export const fill = (template: string, values: Record<string, string | number>): string => template.replace(/\{(\w+)\}/g, (marker, name: string) => (name in values ? String(values[name]) : marker));
