// The words of the kit, in English and Italian. Every visible string and every accessible name comes from here, so a host
// adds a language (or rewords one) by passing `messages` to AlixProvider; nothing is hard-coded in a component.
// `{name}` marks a value filled in by `fill`.

export interface AlixMessages {
  confirmation: {
    title: string;
    approve: string;
    decline: string;
    risk: string;
    riskLevels: readonly [string, string, string, string];
    expires: string;
    unavailable: string;
    answered: string;
    external: string;
    operations: { create: string; update: string; delete: string; send: string; other: string };
  };
  citations: {
    heading: string;
    open: string;
    page: string;
    availability: { available: string; unavailable: string; removed: string };
    staleness: { current: string; stale: string; unknown: string };
  };
  files: {
    heading: string;
    remove: string;
    preview: string;
    kinds: { image: string; audio: string; video: string; document: string; other: string };
  };
  composer: {
    pick: string;
    hint: string;
    tooLarge: string;
    removeNamed: string;
    pending: string;
  };
  actions: { confirm: string; reject: string; cancel: string; modify: string; refresh: string; retry: string; continue_in_app: string; invoke_capability: string };
  chart: { showData: string; hideData: string; series: string };
  table: { empty: string; yes: string; no: string };
  diagram: { source: string };
  choices: { send: string; freeText: string };
  /** `stages`: the words for the pipeline stages the turn-event schema names; another stage is shown as it is. */
  progress: { running: string; stages: Record<string, string> };
  sourceHealth: {
    title: string;
    empty: string;
    states: { registered: string; indexing: string; ready: string; paused: string; disabled: string; removing: string };
    counts: { discovered: string; indexed: string; skipped: string; failed: string; tombstoned: string };
    lastSuccess: string;
    never: string;
    stale: string;
  };
  settings: {
    title: string;
    on: string;
    off: string;
    decidedBy: { default: string; deployment: string; tenant: string; user: string; operator: string };
    groups: {
      general: string;
      history: string;
      privacy: string;
      attachments: string;
      sources: string;
      voice: string;
      notifications: string;
      streaming: string;
      accessibility: string;
    };
    rows: {
      language: string;
      modelProfile: string;
      retentionDays: string;
      autoCleanup: string;
      exportAllowed: string;
      storeTranscript: string;
      attachmentsEnabled: string;
      maxFileMb: string;
      includeAttachments: string;
      citationDisplay: string;
      voiceEnabled: string;
      notificationsEnabled: string;
      streamUnit: string;
      reducedMotion: string;
      highContrast: string;
      textScale: string;
      screenReader: string;
      captions: string;
    };
    values: { inline: string; footnote: string; panel: string; token: string; sentence: string };
  };
  conversations: { title: string; new: string; empty: string; untitled: string; list: string };
  chat: {
    label: string;
    placeholder: string;
    title: string;
    /** CopilotKit's own words around the chat, in the kit's language. */
    welcome: string;
    disclaimer: string;
    open: string;
    close: string;
    attach: string;
    copy: string;
  };
  common: { refresh: string; retry: string; loading: string; save: string; discard: string };
  receipt: {
    title: string;
    status: { committed: string; rejected: string; queued: string; partial: string; uncertain: string };
    errors: string;
    followUp: string;
    followUpStates: { queued: string; running: string; succeeded: string; failed: string; unknown: string };
    issued: string;
    /** The one line for a confirmation that ended without a receipt. */
    outcomes: { rejected: string; failed: string; other: string };
  };
  prepared: {
    title: string;
    stepUpTitle: string;
    changes: string;
    explicitNote: string;
    stepUpNote: string;
  };
  continueInApp: { title: string; body: string; cta: string };
  sourceHealthStatus: { summary: string; failed: string };
  audit: {
    title: string;
    failed: string;
    notAudited: string;
    noFindings: string;
    findings: string;
    remediation: string;
    counts: string;
    outcome: { clear: string; needs_acceptance: string; blocked: string };
    severity: { critical: string; warning: string; info: string };
    status: { open: string; accepted: string; informational: string };
    providers: string;
    provider: { ok: string; degraded: string; unavailable: string; notConfigured: string };
  };
  settingsEditor: {
    failed: string;
    default: string;
    policyOff: string;
    language: string;
    citations: string;
    citationValues: { inline: string; footnote: string; panel: string };
    sections: { replies: string; voice: string; privacy: string; attachments: string; notifications: string; accessibility: string };
    fields: {
      streaming: string;
      streamingHint: string;
      voiceEnabled: string;
      autoPlay: string;
      bargeIn: string;
      bargeInHint: string;
      transcript: string;
      transcriptHint: string;
      autoCleanup: string;
      includeAttachments: string;
      acceptUploads: string;
      notifications: string;
      reducedMotion: string;
      highContrast: string;
      captions: string;
      textScale: string;
    };
    retention: string;
    confirmation: string;
    confirmationFrom: string;
    confirmationHint: string;
    clearHistory: string;
  };
}

export const en: AlixMessages = {
  confirmation: {
    title: "Please confirm",
    approve: "Approve",
    decline: "Decline",
    risk: "Risk level",
    riskLevels: ["Read only", "Low impact", "Consequential", "High risk"],
    expires: "Valid until",
    unavailable: "This confirmation cannot be shown. You can decline it.",
    answered: "Your answer was sent.",
    external: "Leaves your application",
    operations: { create: "Create", update: "Update", delete: "Delete", send: "Send", other: "Change" },
  },
  citations: {
    heading: "Sources",
    open: "Open {name}",
    page: "p. {page}",
    availability: { available: "Available", unavailable: "Unavailable", removed: "Removed" },
    staleness: { current: "Current", stale: "Source changed since", unknown: "Version unknown" },
  },
  files: {
    heading: "Attachments",
    remove: "Remove",
    preview: "Preview of {name}",
    kinds: { image: "Image", audio: "Audio", video: "Video", document: "Document", other: "File" },
  },
  composer: {
    pick: "Attach files",
    hint: "Up to {max} MB each. The application decides what it accepts.",
    tooLarge: "{name} is larger than {max} MB.",
    removeNamed: "Remove {name}",
    pending: "Files ready to send",
  },
  actions: { confirm: "Confirm", reject: "Reject", cancel: "Cancel", modify: "Change", refresh: "Refresh", retry: "Try again", continue_in_app: "Continue in the app", invoke_capability: "Run" },
  chart: { showData: "Show data", hideData: "Hide data", series: "Series" },
  table: { empty: "No rows", yes: "Yes", no: "No" },
  diagram: { source: "Diagram source" },
  choices: { send: "Send", freeText: "Or write your own answer" },
  progress: {
    running: "Working",
    stages: { interpreting: "Understanding the request", retrieving: "Searching the knowledge sources", sources: "Reading the sources", validating: "Checking with the application", preparing: "Preparing the action", committing: "Applying the change", tool_call: "Asking the application", queued: "Queued", generating: "Writing the answer", verifying: "Verifying the answer" },
  },
  sourceHealth: {
    title: "Sources",
    empty: "No sources are registered.",
    states: {
      registered: "Registered",
      indexing: "Indexing",
      ready: "Ready",
      paused: "Paused",
      disabled: "Disabled",
      removing: "Removing",
    },
    counts: { discovered: "Found", indexed: "Indexed", skipped: "Skipped", failed: "Failed", tombstoned: "Removed" },
    lastSuccess: "Last indexed",
    never: "Never",
    stale: "Content changed since the last index",
  },
  settings: {
    title: "Settings",
    on: "On",
    off: "Off",
    decidedBy: { default: "Default", deployment: "Deployment", tenant: "Organisation", user: "You", operator: "Operator" },
    groups: {
      general: "General",
      history: "History",
      privacy: "Privacy",
      attachments: "Attachments",
      sources: "Sources",
      voice: "Voice",
      notifications: "Notifications",
      streaming: "Streaming",
      accessibility: "Accessibility",
    },
    rows: {
      language: "Language",
      modelProfile: "Model profile",
      retentionDays: "Keep history for (days)",
      autoCleanup: "Clean up automatically",
      exportAllowed: "Export allowed",
      storeTranscript: "Store transcripts",
      attachmentsEnabled: "Attachments",
      maxFileMb: "Largest file (MB)",
      includeAttachments: "Use attachments as sources",
      citationDisplay: "Show citations",
      voiceEnabled: "Voice",
      notificationsEnabled: "Notifications",
      streamUnit: "Stream by",
      reducedMotion: "Reduced motion",
      highContrast: "High contrast",
      textScale: "Text size (%)",
      screenReader: "Screen reader support",
      captions: "Captions",
    },
    values: { inline: "Inline", footnote: "Footnote", panel: "Panel", token: "Token", sentence: "Sentence" },
  },
  conversations: { title: "Conversations", new: "New", empty: "No conversations yet.", untitled: "Untitled", list: "Conversation list" },
  chat: {
    label: "AlixAI assistant",
    placeholder: "Ask something",
    title: "Assistant",
    welcome: "How can I help you today?",
    disclaimer: "Answers come from your sources and your application. Check anything important.",
    open: "Open the assistant",
    close: "Close the assistant",
    attach: "Add",
    copy: "Copy",
  },
  common: { refresh: "Refresh", retry: "Try again", loading: "Loading", save: "Save", discard: "Discard" },
  receipt: {
    title: "Result",
    status: { committed: "Done", rejected: "Not done", queued: "Queued", partial: "Partly done", uncertain: "Result uncertain" },
    errors: "Problems",
    followUp: "Follow-up: {state}",
    followUpStates: { queued: "queued", running: "running", succeeded: "done", failed: "failed", unknown: "unknown" },
    issued: "Recorded {time}",
    outcomes: { rejected: "Declined. Nothing was changed.", failed: "This was not carried out.", other: "The confirmation ended: {status}." },
  },
  prepared: {
    title: "Prepared action",
    stepUpTitle: "Needs the application",
    changes: "The changes ({count})",
    explicitNote: "Nothing happens until you approve it.",
    stepUpNote: "This chat cannot complete it: it continues in the application.",
  },
  continueInApp: {
    title: "Continue in the application",
    body: "This action needs a stronger check than this chat can give. Continue in the application to complete it.",
    cta: "Continue in the application",
  },
  sourceHealthStatus: { summary: "{ready} of {total} ready", failed: "The source status could not be loaded." },
  audit: {
    title: "Integration audit",
    failed: "The audit could not be loaded.",
    notAudited: "Not audited yet",
    noFindings: "The audit found nothing to report.",
    findings: "Findings",
    remediation: "What to do",
    counts: "{critical} critical, {warnings} warnings, {info} notes",
    outcome: { clear: "Clear", needs_acceptance: "Needs acceptance", blocked: "Blocked" },
    severity: { critical: "Critical", warning: "Warning", info: "Note" },
    status: { open: "Open", accepted: "Accepted", informational: "For information" },
    providers: "Model and service health",
    provider: { ok: "Working", degraded: "Degraded", unavailable: "Unavailable", notConfigured: "Not set up" },
  },
  settingsEditor: {
    failed: "The settings could not be loaded.",
    default: "Use the default",
    policyOff: "Turned off by your organisation",
    language: "Language",
    citations: "Show sources as",
    citationValues: { inline: "Inline chips", footnote: "A list", panel: "Cards" },
    sections: { replies: "Replies", voice: "Voice", privacy: "Privacy and history", attachments: "Files", notifications: "Notifications", accessibility: "Accessibility" },
    fields: {
      streaming: "Show replies as they are written",
      streamingHint: "Turn off to see each reply only when it is complete.",
      voiceEnabled: "Voice",
      autoPlay: "Read replies aloud",
      bargeIn: "Let me interrupt",
      bargeInHint: "Speaking while the assistant talks stops it.",
      transcript: "Keep my conversation history",
      transcriptHint: "Turn off and new conversations are not stored.",
      autoCleanup: "Delete old history automatically",
      includeAttachments: "Use my attached files as sources",
      acceptUploads: "Allow file uploads",
      notifications: "Notify me when something is ready",
      reducedMotion: "Reduce motion",
      highContrast: "Higher contrast",
      captions: "Show captions",
      textScale: "Text size",
    },
    retention: "History is kept for {days} days.",
    confirmation: "Ask me to confirm more actions",
    confirmationFrom: "From risk level {level} upward",
    confirmationHint: "This can only add confirmations, never remove them.",
    clearHistory: "Clear this conversation's history",
  },
};

export const it: AlixMessages = {
  confirmation: {
    title: "Conferma richiesta",
    approve: "Approva",
    decline: "Rifiuta",
    risk: "Livello di rischio",
    riskLevels: ["Sola lettura", "Impatto basso", "Con conseguenze", "Rischio alto"],
    expires: "Valida fino a",
    unavailable: "Questa conferma non può essere mostrata. Puoi rifiutarla.",
    answered: "La tua risposta è stata inviata.",
    external: "Esce dalla tua applicazione",
    operations: { create: "Crea", update: "Aggiorna", delete: "Elimina", send: "Invia", other: "Modifica" },
  },
  citations: {
    heading: "Fonti",
    open: "Apri {name}",
    page: "pag. {page}",
    availability: { available: "Disponibile", unavailable: "Non disponibile", removed: "Rimossa" },
    staleness: { current: "Aggiornata", stale: "Fonte modificata dopo", unknown: "Versione sconosciuta" },
  },
  files: {
    heading: "Allegati",
    remove: "Rimuovi",
    preview: "Anteprima di {name}",
    kinds: { image: "Immagine", audio: "Audio", video: "Video", document: "Documento", other: "File" },
  },
  composer: {
    pick: "Allega file",
    hint: "Fino a {max} MB ciascuno. L'applicazione decide cosa accetta.",
    tooLarge: "{name} supera {max} MB.",
    removeNamed: "Rimuovi {name}",
    pending: "File pronti per l'invio",
  },
  actions: { confirm: "Conferma", reject: "Rifiuta", cancel: "Annulla", modify: "Modifica", refresh: "Aggiorna", retry: "Riprova", continue_in_app: "Continua nell'app", invoke_capability: "Esegui" },
  chart: { showData: "Mostra i dati", hideData: "Nascondi i dati", series: "Serie" },
  table: { empty: "Nessuna riga", yes: "Sì", no: "No" },
  diagram: { source: "Sorgente del diagramma" },
  choices: { send: "Invia", freeText: "Oppure scrivi la tua risposta" },
  progress: {
    running: "In corso",
    stages: { interpreting: "Comprensione della richiesta", retrieving: "Ricerca nelle fonti", sources: "Lettura delle fonti", validating: "Verifica con l'applicazione", preparing: "Preparazione dell'azione", committing: "Applicazione della modifica", tool_call: "Interrogazione dell'applicazione", queued: "In coda", generating: "Scrittura della risposta", verifying: "Verifica della risposta" },
  },
  sourceHealth: {
    title: "Fonti",
    empty: "Nessuna fonte registrata.",
    states: {
      registered: "Registrata",
      indexing: "Indicizzazione",
      ready: "Pronta",
      paused: "In pausa",
      disabled: "Disattivata",
      removing: "Rimozione",
    },
    counts: { discovered: "Trovati", indexed: "Indicizzati", skipped: "Saltati", failed: "Falliti", tombstoned: "Rimossi" },
    lastSuccess: "Ultima indicizzazione",
    never: "Mai",
    stale: "Il contenuto è cambiato dall'ultima indicizzazione",
  },
  settings: {
    title: "Impostazioni",
    on: "Attivo",
    off: "Disattivo",
    decidedBy: { default: "Predefinito", deployment: "Installazione", tenant: "Organizzazione", user: "Tu", operator: "Operatore" },
    groups: {
      general: "Generali",
      history: "Cronologia",
      privacy: "Privacy",
      attachments: "Allegati",
      sources: "Fonti",
      voice: "Voce",
      notifications: "Notifiche",
      streaming: "Streaming",
      accessibility: "Accessibilità",
    },
    rows: {
      language: "Lingua",
      modelProfile: "Profilo del modello",
      retentionDays: "Conserva la cronologia (giorni)",
      autoCleanup: "Pulizia automatica",
      exportAllowed: "Esportazione consentita",
      storeTranscript: "Salva le trascrizioni",
      attachmentsEnabled: "Allegati",
      maxFileMb: "File più grande (MB)",
      includeAttachments: "Usa gli allegati come fonti",
      citationDisplay: "Mostra le citazioni",
      voiceEnabled: "Voce",
      notificationsEnabled: "Notifiche",
      streamUnit: "Flusso per",
      reducedMotion: "Movimento ridotto",
      highContrast: "Contrasto elevato",
      textScale: "Dimensione del testo (%)",
      screenReader: "Supporto per lettori di schermo",
      captions: "Sottotitoli",
    },
    values: { inline: "Nel testo", footnote: "Nota a piè di pagina", panel: "Pannello", token: "Token", sentence: "Frase" },
  },
  conversations: { title: "Conversazioni", new: "Nuova", empty: "Nessuna conversazione.", untitled: "Senza titolo", list: "Elenco delle conversazioni" },
  chat: {
    label: "Assistente AlixAI",
    placeholder: "Chiedi qualcosa",
    title: "Assistente",
    welcome: "Come posso aiutarti oggi?",
    disclaimer: "Le risposte vengono dalle tue fonti e dalla tua applicazione. Verifica ciò che è importante.",
    open: "Apri l'assistente",
    close: "Chiudi l'assistente",
    attach: "Aggiungi",
    copy: "Copia",
  },
  common: { refresh: "Aggiorna", retry: "Riprova", loading: "Caricamento", save: "Salva", discard: "Scarta" },
  receipt: {
    title: "Esito",
    status: { committed: "Fatto", rejected: "Non eseguita", queued: "In coda", partial: "Eseguita in parte", uncertain: "Esito incerto" },
    errors: "Problemi",
    followUp: "Seguito: {state}",
    followUpStates: { queued: "in coda", running: "in corso", succeeded: "completato", failed: "non riuscito", unknown: "sconosciuto" },
    issued: "Registrato il {time}",
    outcomes: { rejected: "Rifiutata. Non è stato modificato nulla.", failed: "Non è stata eseguita.", other: "La conferma si è chiusa: {status}." },
  },
  prepared: {
    title: "Azione preparata",
    stepUpTitle: "Serve l'applicazione",
    changes: "Le modifiche ({count})",
    explicitNote: "Non succede nulla finché non la approvi.",
    stepUpNote: "Questa chat non può completarla: prosegue nell'applicazione.",
  },
  continueInApp: {
    title: "Continua nell'applicazione",
    body: "Questa azione richiede un controllo più forte di quello che questa chat può dare. Continua nell'applicazione per completarla.",
    cta: "Continua nell'applicazione",
  },
  sourceHealthStatus: { summary: "{ready} pronte su {total}", failed: "Non è stato possibile caricare lo stato delle fonti." },
  audit: {
    title: "Verifica dell'integrazione",
    failed: "Non è stato possibile caricare la verifica.",
    notAudited: "Non ancora verificata",
    noFindings: "La verifica non ha rilevato nulla.",
    findings: "Rilievi",
    remediation: "Cosa fare",
    counts: "{critical} critici, {warnings} avvisi, {info} note",
    outcome: { clear: "Nessun problema", needs_acceptance: "Richiede accettazione", blocked: "Bloccata" },
    severity: { critical: "Critico", warning: "Avviso", info: "Nota" },
    status: { open: "Aperto", accepted: "Accettato", informational: "Informativo" },
    providers: "Stato di modelli e servizi",
    provider: { ok: "Funzionante", degraded: "Degradato", unavailable: "Non disponibile", notConfigured: "Non configurato" },
  },
  settingsEditor: {
    failed: "Non è stato possibile caricare le impostazioni.",
    default: "Usa il valore predefinito",
    policyOff: "Disattivato dalla tua organizzazione",
    language: "Lingua",
    citations: "Mostra le fonti come",
    citationValues: { inline: "Etichette in linea", footnote: "Un elenco", panel: "Schede" },
    sections: { replies: "Risposte", voice: "Voce", privacy: "Privacy e cronologia", attachments: "File", notifications: "Notifiche", accessibility: "Accessibilità" },
    fields: {
      streaming: "Mostra le risposte mentre vengono scritte",
      streamingHint: "Disattiva per vedere ogni risposta solo quando è completa.",
      voiceEnabled: "Voce",
      autoPlay: "Leggi le risposte ad alta voce",
      bargeIn: "Lasciami interrompere",
      bargeInHint: "Se parli mentre l'assistente parla, si interrompe.",
      transcript: "Conserva la cronologia delle mie conversazioni",
      transcriptHint: "Se disattivi, le nuove conversazioni non vengono salvate.",
      autoCleanup: "Elimina automaticamente la cronologia vecchia",
      includeAttachments: "Usa i miei file allegati come fonti",
      acceptUploads: "Consenti il caricamento di file",
      notifications: "Avvisami quando qualcosa è pronto",
      reducedMotion: "Riduci i movimenti",
      highContrast: "Contrasto più alto",
      captions: "Mostra i sottotitoli",
      textScale: "Dimensione del testo",
    },
    retention: "La cronologia viene conservata per {days} giorni.",
    confirmation: "Chiedimi di confermare più azioni",
    confirmationFrom: "Dal livello di rischio {level} in su",
    confirmationHint: "Può solo aggiungere conferme, mai toglierle.",
    clearHistory: "Cancella la cronologia di questa conversazione",
  },
};

export const BUILT_IN_MESSAGES: Readonly<Record<string, AlixMessages>> = Object.freeze({ en, it });

export type DeepPartial<T> = T extends readonly unknown[] ? T : T extends object ? { [K in keyof T]?: DeepPartial<T[K]> } : T;

const isPlain = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const merge = (base: unknown, patch: unknown): unknown => {
  if (!isPlain(base) || !isPlain(patch)) return patch === undefined ? base : patch;
  const out: Record<string, unknown> = { ...base };
  for (const key of Object.keys(patch)) {
    if (key === "__proto__" || key === "constructor" || key === "prototype") continue;
    out[key] = merge(base[key], patch[key]);
  }
  return out;
};

/** The messages of a locale ("it", "it-IT"), English when the language is not built in, with the host's overrides on top. */
export const resolveMessages = (locale: string | undefined, overrides?: DeepPartial<AlixMessages>): AlixMessages => {
  const language = (locale ?? "en").toLowerCase().split(/[-_]/)[0] ?? "en";
  const base = BUILT_IN_MESSAGES[language] ?? en;
  return overrides === undefined ? base : (merge(base, overrides) as AlixMessages);
};

/** Fills `{name}` markers. A marker without a value stays as written, so a missing value is visible rather than blank. */
export const fill = (template: string, values: Readonly<Record<string, string | number>>): string =>
  template.replace(/\{(\w+)\}/g, (marker, name: string) => (name in values ? String(values[name]) : marker));
