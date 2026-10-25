# Integration readiness requirements

<!-- Generated from internal/readiness/catalog/requirements.json by CatalogRenderMarkdown. Do not edit by hand: change the catalog, then run `READINESS_WRITE_DOCS=1 go test ./internal/readiness/catalog -run TestCatalogDocIsCurrent`. -->

Source of truth: [ADR 0037](../decisions/0037-integration-readiness-discovery.md) and the catalog data this page is generated from. Where this page and the ADR disagree, the ADR wins and this page is the bug.

This is the checklist `alixai readiness scan <dir>` walks through on a project directory the operator names. It is **advisory**: a readiness report never certifies, never signs, never activates and never edits a package, and a complete report is not an audit pass. The integration audit ([application packages](application-package.md)) still decides activation. Each requirement below names the audit check it feeds, so a covered checklist points at what the audit will then verify.

Catalog version 1, 33 requirements. Every title, description, rule and remediation exists in English and Italian.

## How a requirement is covered

- **Statuses.** `found` (covered, with evidence the verifier confirmed), `partial`, `missing`, `unverified` (could not be determined, or the evidence did not confirm; never counted as covered) and `contradicted` (two pieces of evidence disagree).
- **Sources.** `deterministic` (a rule found it), `model_verified` (a model proposed it and the verifier re-read and confirmed its evidence) and `model_unverified` (a model proposed it and the evidence did not confirm). Only the first two can cover a requirement.
- **Evidence.** Every finding cites a path relative to the scanned root, a line span and the file hash. A finding that does not confirm is `unverified`.
- **Levels.** `mandatory`: the audit check it feeds cannot pass without it. `recommended`: the audit reports a warning without it.

## Overview

| Requirement | Category | Level | Audit check | Covered by |
| --- | --- | --- | --- | --- |
| `req.contracts.conversation` | conversation_contract | mandatory | `package.contracts` | `deterministic`, `model_verified` |
| `req.contracts.terminology` | conversation_contract | recommended | `package.contracts` | `deterministic`, `model_verified` |
| `req.data.commit` | deterministic_data | mandatory | `application.deterministic_results` | `deterministic`, `model_verified` |
| `req.data.read` | deterministic_data | mandatory | `application.deterministic_results` | `deterministic`, `model_verified` |
| `req.data.safe_execution` | deterministic_data | mandatory | `application.safe_execution` | `deterministic`, `model_verified` |
| `req.data.typed_failures` | deterministic_data | mandatory | `application.failure_degradation` | `deterministic`, `model_verified` |
| `req.data.validate_prepare` | deterministic_data | mandatory | `application.deterministic_results` | `deterministic`, `model_verified` |
| `req.deployment.compose_and_ports` | deployment | recommended | `runtime.inspection` | `deterministic` |
| `req.deployment.inspection` | deployment | recommended | `runtime.inspection` | `deterministic`, `model_verified` |
| `req.deployment.smoke_environment` | deployment | mandatory | `application.runtime_smoke` | `deterministic`, `model_verified` |
| `req.documentation.capabilities` | documentation | mandatory | `package.documentation` | `deterministic`, `model_verified` |
| `req.documentation.fields_and_validation` | documentation | mandatory | `package.documentation` | `deterministic`, `model_verified` |
| `req.documentation.risk_and_outcomes` | documentation | mandatory | `package.documentation` | `deterministic`, `model_verified` |
| `req.mcp.server` | mcp | recommended | `application.control_surface` | `deterministic`, `model_verified` |
| `req.mcp.tool_list` | mcp | recommended | `application.control_surface` | `deterministic`, `model_verified` |
| `req.model.external_egress` | model_runtime | recommended | `package.external_inventory` | `deterministic`, `model_verified` |
| `req.model.runtime_profile` | model_runtime | recommended | `runtime.inspection` | `deterministic`, `model_verified` |
| `req.native.adapter` | native_adapter | recommended | `package.inventory` | `deterministic`, `model_verified` |
| `req.openapi.description` | openapi | recommended | `application.control_surface` | `deterministic`, `model_verified` |
| `req.package.manifest` | deployment | mandatory | `package.authenticity` | `deterministic` |
| `req.package.signatures` | deployment | mandatory | `package.authenticity` | `deterministic` |
| `req.package.tests` | deployment | recommended | `package.policy` | `deterministic` |
| `req.permissions.model` | permissions | mandatory | `application.scope_probes` | `deterministic`, `model_verified` |
| `req.permissions.policy_files` | permissions | recommended | `package.policy` | `deterministic` |
| `req.rag.citation_resolver` | rag_source | recommended | `application.rag_citations` | `deterministic`, `model_verified` |
| `req.rag.live_sources` | rag_source | recommended | `application.rag_sources` | `deterministic` |
| `req.rag.scope_per_source` | rag_source | recommended | `application.rag_scope` | `deterministic`, `model_verified` |
| `req.rag.source_candidates` | rag_source | recommended | `package.rag_inventory` | `deterministic`, `model_verified` |
| `req.rag.source_declarations` | rag_source | recommended | `package.rag_inventory` | `deterministic` |
| `req.rag.synthetic_documents` | rag_source | recommended | `application.rag_smoke` | `deterministic`, `model_verified` |
| `req.scope.principal_roles` | scope_and_auth | mandatory | `application.scope_probes` | `deterministic`, `model_verified` |
| `req.scope.tenant_workspace` | scope_and_auth | mandatory | `application.scope_probes` | `deterministic`, `model_verified` |
| `req.surface.control_surface` | deterministic_data | mandatory | `package.inventory` | `deterministic`, `model_verified` |

## Requirements by audit check

### `application.control_surface`

#### `req.mcp.server`

- Category: `mcp`. Level: `recommended`. Covered by: `deterministic`, `model_verified`.

**English.** MCP server. If the application exposes an MCP server, its transport and endpoint are declared. MCP is the preferred way to discover capabilities, but AlixAI filters and wraps its tools and never gives a model the whole server.

- Rule: A rule finds an MCP server configuration or entry point in the project files. A verified model finding must cite the file that starts or describes the server. Readiness opens no connection to it.
- If missing: Declare the MCP server in the manifest, or choose OpenAPI or a native adapter as the transport.

**Italiano.** Server MCP. Se l'applicazione espone un server MCP, trasporto ed endpoint sono dichiarati. MCP e' il modo preferito per scoprire le capability, ma AlixAI filtra e incapsula i suoi strumenti e non da' mai al modello l'intero server.

- Regola: Una regola trova una configurazione o un punto d'ingresso di un server MCP nei file del progetto. Un riscontro verificato del modello deve citare il file che avvia o descrive il server. La readiness non apre alcuna connessione.
- Se manca: Dichiarare il server MCP nel manifesto, oppure scegliere OpenAPI o un adapter nativo come trasporto.

#### `req.mcp.tool_list`

- Category: `mcp`. Level: `recommended`. Covered by: `deterministic`, `model_verified`.

**English.** MCP tool list. The tools an MCP server offers are listed with typed input schemas, so the audit can compare them with the documentation and the capability registry and report undocumented, over-broad or ambiguous operations.

- Rule: A rule finds tool registrations with input schemas in the source. A verified model finding must cite the span that registers or lists each tool.
- If missing: Give every MCP tool a name, a description and a typed input schema, and list the tools in the package.

**Italiano.** Elenco degli strumenti MCP. Gli strumenti offerti da un server MCP sono elencati con schemi di input tipizzati, cosi' l'audit puo' confrontarli con documentazione e registro delle capability e segnalare operazioni non documentate, troppo ampie o ambigue.

- Regola: Una regola trova nel sorgente le registrazioni degli strumenti con schemi di input. Un riscontro verificato del modello deve citare il passaggio che registra o elenca ciascuno strumento.
- Se manca: Dare a ogni strumento MCP un nome, una descrizione e uno schema di input tipizzato, ed elencare gli strumenti nel pacchetto.

#### `req.openapi.description`

- Category: `openapi`. Level: `recommended`. Covered by: `deterministic`, `model_verified`.

**English.** OpenAPI or API description. If the application exposes an HTTP API, an OpenAPI description (or another machine-readable one) lists its operations, parameters and responses, and the package maps the approved subset to capabilities.

- Rule: A rule finds an OpenAPI or Swagger document and counts its operations. A verified model finding must cite the file that describes the API when it is only prose.
- If missing: Publish an OpenAPI description of the approved operations and map each to a capability.

**Italiano.** Descrizione OpenAPI o delle API. Se l'applicazione espone un'API HTTP, una descrizione OpenAPI (o un'altra leggibile da macchina) elenca operazioni, parametri e risposte, e il pacchetto associa il sottoinsieme approvato alle capability.

- Regola: Una regola trova un documento OpenAPI o Swagger e ne conta le operazioni. Un riscontro verificato del modello deve citare il file che descrive l'API quando e' solo testo.
- Se manca: Pubblicare una descrizione OpenAPI delle operazioni approvate e associare ciascuna a una capability.

### `application.deterministic_results`

#### `req.data.read`

- Category: `deterministic_data`. Level: `mandatory`. Covered by: `deterministic`, `model_verified`.

**English.** Deterministic read operations. Reads return typed, stable results with status, identifiers, canonical values, errors and freshness, so no decision depends on model prose.

- Rule: A rule finds read operations whose responses are schema-typed. A verified model finding must cite the span that defines a read and its response shape.
- If missing: Expose each read as an operation with a typed response schema instead of free text.

**Italiano.** Operazioni di lettura deterministiche. Le letture restituiscono risultati tipizzati e stabili con stato, identificatori, valori canonici, errori e freschezza, cosi' nessuna decisione dipende dalla prosa del modello.

- Regola: Una regola trova operazioni di lettura con risposte tipizzate da schema. Un riscontro verificato del modello deve citare il passaggio che definisce una lettura e la forma della risposta.
- Se manca: Esporre ogni lettura come operazione con uno schema di risposta tipizzato invece di testo libero.

#### `req.data.validate_prepare`

- Category: `deterministic_data`. Level: `mandatory`. Covered by: `deterministic`, `model_verified`.

**English.** Validate and prepare operations. Every consequential action has a validate and a prepare operation that return a typed result, so the user can confirm exactly what will happen before anything is committed.

- Rule: A rule finds validate and prepare operations paired with a commit. A verified model finding must cite the spans that define them.
- If missing: Split each consequential operation into validate, prepare and commit, each with a typed result.

**Italiano.** Operazioni di validazione e preparazione. Ogni azione rilevante ha un'operazione di validazione e una di preparazione che restituiscono un risultato tipizzato, cosi' l'utente puo' confermare esattamente cio' che accadra' prima di ogni commit.

- Regola: Una regola trova operazioni di validazione e preparazione abbinate a un commit. Un riscontro verificato del modello deve citare i passaggi che le definiscono.
- Se manca: Dividere ogni operazione rilevante in validazione, preparazione e commit, ciascuna con un risultato tipizzato.

#### `req.data.commit`

- Category: `deterministic_data`. Level: `mandatory`. Covered by: `deterministic`, `model_verified`.

**English.** Commit with a receipt. A commit returns a typed result and an action receipt that names what changed, so success is never a generic message.

- Rule: A rule finds commit operations that return identifiers or a receipt. A verified model finding must cite the span that defines the commit result.
- If missing: Return the changed record IDs and canonical values from every commit.

**Italiano.** Commit con ricevuta. Un commit restituisce un risultato tipizzato e una ricevuta dell'azione che indica cosa e' cambiato, cosi' il successo non e' mai un messaggio generico.

- Regola: Una regola trova operazioni di commit che restituiscono identificatori o una ricevuta. Un riscontro verificato del modello deve citare il passaggio che definisce il risultato del commit.
- Se manca: Restituire da ogni commit gli ID dei record modificati e i valori canonici.

### `application.failure_degradation`

#### `req.data.typed_failures`

- Category: `deterministic_data`. Level: `mandatory`. Covered by: `deterministic`, `model_verified`.

**English.** Typed failures and degradation. The application reports unavailable dependencies, queued work, partial results and uncertain outcomes in typed form and never returns a success-looking message when the operation did not complete.

- Rule: A rule finds an error-code enumeration and typed error responses in the operation definitions. A verified model finding must cite the span that lists the failure states.
- If missing: Define typed error and partial-result responses and stop returning generic success text on failure.

**Italiano.** Errori e degradazione tipizzati. L'applicazione segnala dipendenze non disponibili, lavori in coda, risultati parziali ed esiti incerti in forma tipizzata e non restituisce mai un messaggio apparentemente di successo quando l'operazione non e' stata completata.

- Regola: Una regola trova un'enumerazione di codici di errore e risposte di errore tipizzate nelle definizioni delle operazioni. Un riscontro verificato del modello deve citare il passaggio che elenca gli stati di errore.
- Se manca: Definire risposte tipizzate di errore e di risultato parziale e smettere di restituire testo generico di successo in caso di errore.

### `application.rag_citations`

#### `req.rag.citation_resolver`

- Category: `rag_source`. Level: `recommended`. Covered by: `deterministic`, `model_verified`.

**English.** Citation resolver. Each source says how a citation resolves, with a permission check, to the original document, so a successful answer can show its authorised provenance.

- Rule: A rule finds a citation resolver declaration per source. A verified model finding must cite the span that describes how a document is opened from its identifier.
- If missing: Add a citation resolver to each source declaration: a stable document ID and a way to open it as the asking user.

**Italiano.** Risolutore di citazioni. Ogni fonte indica come una citazione si risolve, con un controllo dei permessi, nel documento originale, cosi' una risposta riuscita puo' mostrare la provenienza autorizzata.

- Regola: Una regola trova una dichiarazione del risolutore di citazioni per fonte. Un riscontro verificato del modello deve citare il passaggio che descrive come si apre un documento dal suo identificatore.
- Se manca: Aggiungere un risolutore di citazioni a ogni dichiarazione di fonte: un ID documento stabile e un modo per aprirlo come l'utente che chiede.

### `application.rag_scope`

#### `req.rag.scope_per_source`

- Category: `rag_source`. Level: `recommended`. Covered by: `deterministic`, `model_verified`.

**English.** Scope on every source. Every source and document carries tenant, workspace and access-control information, so unauthorised chunks never reach the model.

- Rule: A rule finds scope and access fields in source declarations or document metadata. A verified model finding must cite the span that states who may read the documents.
- If missing: Record tenant, workspace and access rules for each source and its documents.

**Italiano.** Ambito su ogni fonte. Ogni fonte e documento porta informazioni di tenant, workspace e controllo degli accessi, cosi' i frammenti non autorizzati non raggiungono mai il modello.

- Regola: Una regola trova campi di ambito e accesso nelle dichiarazioni di fonte o nei metadati dei documenti. Un riscontro verificato del modello deve citare il passaggio che dichiara chi puo' leggere i documenti.
- Se manca: Registrare tenant, workspace e regole di accesso per ogni fonte e per i suoi documenti.

### `application.rag_smoke`

#### `req.rag.synthetic_documents`

- Category: `rag_source`. Level: `recommended`. Covered by: `deterministic`, `model_verified`.

**English.** Synthetic documents for the smoke test. A small set of synthetic documents with known answers exists for the disposable-tenant smoke test, so retrieval, grounded answers, citations and cleanup can be checked without real data.

- Rule: A rule finds fixture documents in tests. A verified model finding must cite the span that names them.
- If missing: Add synthetic documents with known questions and answers to the tests directory.

**Italiano.** Documenti sintetici per lo smoke test. Esiste un piccolo insieme di documenti sintetici con risposte note per lo smoke test su tenant usa e getta, cosi' recupero, risposte fondate, citazioni e pulizia si verificano senza dati reali.

- Regola: Una regola trova documenti di prova in tests. Un riscontro verificato del modello deve citare il passaggio che li nomina.
- Se manca: Aggiungere alla cartella tests documenti sintetici con domande e risposte note.

### `application.rag_sources`

#### `req.rag.live_sources`

- Category: `rag_source`. Level: `recommended`. Covered by: `deterministic`.

**English.** Reachable sources, none undeclared. Every declared source is reachable and no source exists that the package does not declare, so the audit can verify declared fields against the live source.

- Rule: A rule compares source candidates found in the project with the declarations. Reachability is verified by the audit, not by readiness; without that it stays unverified.
- If missing: Declare every source found in the project or remove the ones that should not be indexed.

**Italiano.** Fonti raggiungibili, nessuna non dichiarata. Ogni fonte dichiarata e' raggiungibile e non esistono fonti non dichiarate nel pacchetto, cosi' l'audit puo' verificare i campi dichiarati sulla fonte reale.

- Regola: Una regola confronta le fonti candidate trovate nel progetto con le dichiarazioni. La raggiungibilita' e' verificata dall'audit, non dalla readiness; senza di essa resta non verificata.
- Se manca: Dichiarare ogni fonte trovata nel progetto o rimuovere quelle che non devono essere indicizzate.

### `application.runtime_smoke`

#### `req.deployment.smoke_environment`

- Category: `deployment`. Level: `mandatory`. Covered by: `deterministic`, `model_verified`.

**English.** Disposable tenant for the smoke test. The application can create a disposable tenant with synthetic records and remove it afterwards, so the runtime smoke test exercises the text, voice, RAG and action paths without touching real data.

- Rule: A rule finds a seed or provisioning script for a test tenant. A verified model finding must cite the span that documents creating and removing one.
- If missing: Provide a way to create and delete a disposable tenant with synthetic data.

**Italiano.** Tenant usa e getta per lo smoke test. L'applicazione puo' creare un tenant usa e getta con record sintetici e rimuoverlo poi, cosi' lo smoke test esercita i percorsi testo, voce, RAG e azioni senza toccare dati reali.

- Regola: Una regola trova uno script di seed o provisioning per un tenant di prova. Un riscontro verificato del modello deve citare il passaggio che documenta come crearlo e rimuoverlo.
- Se manca: Fornire un modo per creare ed eliminare un tenant usa e getta con dati sintetici.

### `application.safe_execution`

#### `req.data.safe_execution`

- Category: `deterministic_data`. Level: `mandatory`. Covered by: `deterministic`, `model_verified`.

**English.** Idempotency, expiry and cancellation. Consequential operations support idempotency keys, prepared-action expiry, cancellation, stale-record detection and recovery from an uncertain response.

- Rule: A rule finds idempotency-key, expiry and version or etag handling in the operation definitions. A verified model finding must cite the span that describes them.
- If missing: Add an idempotency key and a record version check to every commit, and document expiry and cancellation.

**Italiano.** Idempotenza, scadenza e annullamento. Le operazioni rilevanti supportano chiavi di idempotenza, scadenza delle azioni preparate, annullamento, rilevamento di record obsoleti e recupero da una risposta incerta.

- Regola: Una regola trova gestione di chiavi di idempotenza, scadenza e versione o etag nelle definizioni delle operazioni. Un riscontro verificato del modello deve citare il passaggio che le descrive.
- Se manca: Aggiungere una chiave di idempotenza e un controllo di versione del record a ogni commit, e documentare scadenza e annullamento.

### `application.scope_probes`

#### `req.scope.tenant_workspace`

- Category: `scope_and_auth`. Level: `mandatory`. Covered by: `deterministic`, `model_verified`.

**English.** Tenant and workspace model. The project has an explicit tenant and workspace model, with every record and document bound to a scope, so the audit can run positive and negative cross-scope probes.

- Rule: A rule finds tenant and workspace columns, fields or claims in schemas and migrations. A verified model finding must cite the span that states the scope model.
- If missing: Declare the tenant and workspace model in the manifest and make every query scope-filtered.

**Italiano.** Modello di tenant e workspace. Il progetto ha un modello esplicito di tenant e workspace, con ogni record e documento legato a un ambito, cosi' l'audit puo' eseguire sonde di ambito positive e negative.

- Regola: Una regola trova colonne, campi o claim di tenant e workspace in schemi e migrazioni. Un riscontro verificato del modello deve citare il passaggio che dichiara il modello degli ambiti.
- Se manca: Dichiarare il modello di tenant e workspace nel manifesto e filtrare per ambito ogni interrogazione.

#### `req.scope.principal_roles`

- Category: `scope_and_auth`. Level: `mandatory`. Covered by: `deterministic`, `model_verified`.

**English.** Identity and roles. The application identifies the acting principal and their roles and can exchange that identity context with AlixAI, so the application, not the model, decides who may do what.

- Rule: A rule finds role definitions and an authentication or context-exchange mechanism. A verified model finding must cite the span that describes them.
- If missing: Document how the principal and roles are passed to AlixAI and declare the authentication exchange in the manifest.

**Italiano.** Identita' e ruoli. L'applicazione identifica il principal che agisce e i suoi ruoli e puo' scambiare questo contesto di identita' con AlixAI, cosi' e' l'applicazione, non il modello, a decidere chi puo' fare cosa.

- Regola: Una regola trova definizioni di ruoli e un meccanismo di autenticazione o scambio di contesto. Un riscontro verificato del modello deve citare il passaggio che li descrive.
- Se manca: Documentare come principal e ruoli vengono passati ad AlixAI e dichiarare lo scambio di autenticazione nel manifesto.

#### `req.permissions.model`

- Category: `permissions`. Level: `mandatory`. Covered by: `deterministic`, `model_verified`.

**English.** Permission model. The application's permissions are explicit and testable per operation, so the audit can find missing, excessive or contradictory permissions. Readiness reports what exists and grants nothing.

- Rule: A rule finds permission checks or role-to-operation tables. A verified model finding must cite the span that states who may run which operation.
- If missing: Write down which role may run which operation and enforce it in the application.

**Italiano.** Modello dei permessi. I permessi dell'applicazione sono espliciti e verificabili per operazione, cosi' l'audit puo' trovare permessi mancanti, eccessivi o contraddittori. La readiness riporta cio' che esiste e non concede nulla.

- Regola: Una regola trova controlli di permesso o tabelle ruolo-operazione. Un riscontro verificato del modello deve citare il passaggio che dichiara chi puo' eseguire quale operazione.
- Se manca: Mettere per iscritto quale ruolo puo' eseguire quale operazione e farlo rispettare dall'applicazione.

### `package.authenticity`

#### `req.package.manifest`

- Category: `deployment`. Level: `mandatory`. Covered by: `deterministic`.

**English.** Application manifest. The package root holds an alixai.yaml manifest that declares the application ID and version, the integration package version, the compatibility range, the transports and endpoints, the tenant and workspace model and the supported languages. The audit loads the package from this file.

- Rule: A rule finds alixai.yaml at the root and parses its declared versions and compatibility range. A model finding cannot stand in for the file.
- If missing: Create alixai.yaml at the package root with the application ID, versions, compatibility range, transports, scope model and languages (docs/integration/application-package.md).

**Italiano.** Manifesto dell'applicazione. La radice del pacchetto contiene un manifesto alixai.yaml che dichiara ID e versione dell'applicazione, versione del pacchetto di integrazione, intervallo di compatibilita', trasporti ed endpoint, modello di tenant e workspace e lingue supportate. L'audit carica il pacchetto da questo file.

- Regola: Una regola trova alixai.yaml nella radice e ne legge versioni e intervallo di compatibilita' dichiarati. Un riscontro del modello non puo' sostituire il file.
- Se manca: Creare alixai.yaml nella radice del pacchetto con ID applicazione, versioni, intervallo di compatibilita', trasporti, modello degli ambiti e lingue (docs/integration/application-package.md).

#### `req.package.signatures`

- Category: `deployment`. Level: `mandatory`. Covered by: `deterministic`.

**English.** Signatures and file hashes. The package has a signatures directory with a signature manifest that covers the manifest, schemas, documentation, conversation contracts and adapter configuration, so the audit can hash-verify every file. Readiness never signs: the application owner does.

- Rule: A rule finds a signatures directory with a signature manifest. Whether the signature is valid is the audit's decision, not readiness's.
- If missing: Add a signatures directory and sign the package with the application owner's key before the audit.

**Italiano.** Firme e hash dei file. Il pacchetto ha una cartella signatures con un manifesto di firma che copre manifesto, schemi, documentazione, contratti conversazionali e configurazione degli adapter, cosi' l'audit puo' verificare l'hash di ogni file. La readiness non firma mai: firma il proprietario dell'applicazione.

- Regola: Una regola trova una cartella signatures con un manifesto di firma. La validita' della firma e' decisa dall'audit, non dalla readiness.
- Se manca: Aggiungere una cartella signatures e firmare il pacchetto con la chiave del proprietario dell'applicazione prima dell'audit.

### `package.contracts`

#### `req.contracts.conversation`

- Category: `conversation_contract`. Level: `mandatory`. Covered by: `deterministic`, `model_verified`.

**English.** Conversation contract per capability. Every ordinary user capability has a versioned conversation contract covering required fields, clarifications, corrections, cancellations, confirmations, success and failure responses and permitted next actions.

- Rule: A rule finds files in conversation-contracts per capability. A verified model finding must cite existing material, such as a flow description, that the operator can turn into a contract.
- If missing: Write a conversation contract for each user capability in the conversation-contracts directory and sign it with the package.

**Italiano.** Contratto conversazionale per capability. Ogni capability utente ordinaria ha un contratto conversazionale versionato che copre campi obbligatori, chiarimenti, correzioni, annullamenti, conferme, risposte di successo e di errore e prossime azioni consentite.

- Regola: Una regola trova i file in conversation-contracts per capability. Un riscontro verificato del modello deve citare materiale esistente, come la descrizione di un flusso, che l'operatore puo' trasformare in contratto.
- Se manca: Scrivere un contratto conversazionale per ogni capability utente nella cartella conversation-contracts e firmarlo con il pacchetto.

#### `req.contracts.terminology`

- Category: `conversation_contract`. Level: `recommended`. Covered by: `deterministic`, `model_verified`.

**English.** Terminology and languages. The application's own vocabulary is collected in a terminology directory, with English and Italian terms, so users' words map to the application's entities and fields.

- Rule: A rule finds terminology files. A verified model finding must cite a glossary or naming section of the project's documentation.
- If missing: Add a terminology directory with the entity, field and status names users say, in English and Italian.

**Italiano.** Terminologia e lingue. Il vocabolario proprio dell'applicazione e' raccolto in una cartella terminology, con termini in inglese e italiano, cosi' le parole degli utenti si collegano a entita' e campi dell'applicazione.

- Regola: Una regola trova i file di terminologia. Un riscontro verificato del modello deve citare un glossario o una sezione sui nomi nella documentazione del progetto.
- Se manca: Aggiungere una cartella terminology con i nomi di entita', campi e stati usati dagli utenti, in inglese e italiano.

### `package.documentation`

#### `req.documentation.capabilities`

- Category: `documentation`. Level: `mandatory`. Covered by: `deterministic`, `model_verified`.

**English.** Documentation for every capability. Every capability AlixAI will operate has documentation that says what it does and for whom. A capability without documentation is a failed capability, not a prompt for the model to guess.

- Rule: A rule matches documentation files to capability IDs. A verified model finding must cite the documentation span that describes the capability.
- If missing: Write one documentation entry per capability in the documentation directory and reference it from the capability contract.

**Italiano.** Documentazione per ogni capability. Ogni capability che AlixAI gestira' ha una documentazione che dice cosa fa e per chi. Una capability senza documentazione e' una capability fallita, non un invito al modello a indovinare.

- Regola: Una regola associa i file di documentazione agli ID delle capability. Un riscontro verificato del modello deve citare il passaggio di documentazione che descrive la capability.
- Se manca: Scrivere una voce di documentazione per ogni capability nella cartella documentation e richiamarla dal contratto della capability.

#### `req.documentation.fields_and_validation`

- Category: `documentation`. Level: `mandatory`. Covered by: `deterministic`, `model_verified`.

**English.** Fields and validation rules. The documentation describes each capability's required and optional fields, their types, defaults and validation rules, and the error codes the application returns.

- Rule: A rule finds field and error-code tables or schema files per capability. A verified model finding must cite the span that lists the fields and rules.
- If missing: Document every field with type, default, validation rule and error code, or point the documentation at the schema that defines them.

**Italiano.** Campi e regole di validazione. La documentazione descrive per ogni capability i campi obbligatori e opzionali, i tipi, i valori predefiniti, le regole di validazione e i codici di errore restituiti dall'applicazione.

- Regola: Una regola trova tabelle di campi e codici di errore o file di schema per capability. Un riscontro verificato del modello deve citare il passaggio che elenca campi e regole.
- Se manca: Documentare ogni campo con tipo, valore predefinito, regola di validazione e codice di errore, oppure far puntare la documentazione allo schema che li definisce.

#### `req.documentation.risk_and_outcomes`

- Category: `documentation`. Level: `mandatory`. Covered by: `deterministic`, `model_verified`.

**English.** Risk, confirmation, failures and outcomes. The documentation states each capability's risk level, the confirmation rule, the failure states and the outcome the user sees, so a consequential action is never left to the model's judgement.

- Rule: A rule finds risk and confirmation entries per capability. A verified model finding must cite the span that states them.
- If missing: Add risk level, confirmation rule, failure states and user-visible outcome to the documentation of every capability that changes data.

**Italiano.** Rischio, conferma, errori ed esiti. La documentazione indica per ogni capability il livello di rischio, la regola di conferma, gli stati di errore e l'esito visibile all'utente, cosi' un'azione rilevante non e' mai lasciata al giudizio del modello.

- Regola: Una regola trova voci di rischio e conferma per capability. Un riscontro verificato del modello deve citare il passaggio che le dichiara.
- Se manca: Aggiungere livello di rischio, regola di conferma, stati di errore ed esito visibile all'utente alla documentazione di ogni capability che modifica dati.

### `package.external_inventory`

#### `req.model.external_egress`

- Category: `model_runtime`. Level: `recommended`. Covered by: `deterministic`, `model_verified`.

**English.** Data that could leave the deployment. Any external provider, outbound network permission or data class that could leave the deployment is declared with its data classification, so the audit can list what could leave and the operator can decide.

- Rule: A rule finds network permissions and provider references in the manifest and configuration. A verified model finding must cite the span that names an outbound destination.
- If missing: Declare every outbound destination and the data class sent there, or state that none is used.

**Italiano.** Dati che potrebbero uscire dal deployment. Ogni provider esterno, permesso di rete in uscita o classe di dati che potrebbe uscire dal deployment e' dichiarato con la sua classificazione, cosi' l'audit puo' elencare cio' che potrebbe uscire e l'operatore decidere.

- Regola: Una regola trova permessi di rete e riferimenti a provider nel manifesto e nella configurazione. Un riscontro verificato del modello deve citare il passaggio che nomina una destinazione in uscita.
- Se manca: Dichiarare ogni destinazione in uscita e la classe di dati inviata, oppure indicare che non ne viene usata nessuna.

### `package.inventory`

#### `req.native.adapter`

- Category: `native_adapter`. Level: `recommended`. Covered by: `deterministic`, `model_verified`.

**English.** Native adapter operations. If the host embeds a language-specific bridge or local IPC instead of MCP or HTTP, its declared operations are listed so each can be mapped to a capability and inventoried.

- Rule: A rule finds adapter entry points that use an AlixAI SDK or declare operations. A verified model finding must cite the span that declares them.
- If missing: Declare the native adapter and its operations in the manifest and list each as a capability.

**Italiano.** Operazioni dell'adapter nativo. Se l'host integra un ponte specifico del linguaggio o un IPC locale al posto di MCP o HTTP, le sue operazioni dichiarate sono elencate cosi' ciascuna puo' essere associata a una capability e inventariata.

- Regola: Una regola trova punti d'ingresso di adapter che usano un SDK AlixAI o dichiarano operazioni. Un riscontro verificato del modello deve citare il passaggio che le dichiara.
- Se manca: Dichiarare l'adapter nativo e le sue operazioni nel manifesto ed elencare ciascuna come capability.

#### `req.surface.control_surface`

- Category: `deterministic_data`. Level: `mandatory`. Covered by: `deterministic`, `model_verified`.

**English.** Declared capabilities and owning adapters. The capabilities intended for AlixAI are declared in the capabilities directory, each with a stable ID, an owning adapter and a transport. Everything AlixAI should be able to do is reachable through an approved MCP, OpenAPI or native contract.

- Rule: A rule finds capability declarations with an adapter and transport. A verified model finding must cite the span that names an operation and the transport that serves it.
- If missing: Add a capability file for every operation AlixAI should perform and name its adapter and transport.

**Italiano.** Capability dichiarate e adapter proprietari. Le capability destinate ad AlixAI sono dichiarate nella cartella capabilities, ciascuna con un ID stabile, un adapter proprietario e un trasporto. Tutto cio' che AlixAI deve poter fare e' raggiungibile tramite un contratto MCP, OpenAPI o nativo approvato.

- Regola: Una regola trova dichiarazioni di capability con adapter e trasporto. Un riscontro verificato del modello deve citare il passaggio che nomina un'operazione e il trasporto che la serve.
- Se manca: Aggiungere un file di capability per ogni operazione che AlixAI deve eseguire e indicarne adapter e trasporto.

### `package.policy`

#### `req.package.tests`

- Category: `deployment`. Level: `recommended`. Covered by: `deterministic`.

**English.** Package tests. The package has a tests directory with the cases the application owner expects to hold: multilingual examples and capability tests. Examples are used for testing, never as the only runtime policy.

- Rule: A rule finds a tests directory with at least one case file.
- If missing: Add a tests directory with example conversations and capability cases in English and Italian.

**Italiano.** Test del pacchetto. Il pacchetto ha una cartella tests con i casi che il proprietario si aspetta: esempi multilingua e test delle capability. Gli esempi servono ai test, mai come unica policy a runtime.

- Regola: Una regola trova una cartella tests con almeno un file di casi.
- Se manca: Aggiungere una cartella tests con conversazioni di esempio e casi per capability in inglese e italiano.

#### `req.permissions.policy_files`

- Category: `permissions`. Level: `recommended`. Covered by: `deterministic`.

**English.** Policy documents. The package keeps its policy documents (field visibility, confirmation and approval rules for the operator) in a policies directory, so policy conflicts can be detected before activation.

- Rule: A rule finds policy documents in a policies directory.
- If missing: Move the application's policy rules into the policies directory of the package.

**Italiano.** Documenti di policy. Il pacchetto tiene i documenti di policy (visibilita' dei campi, regole di conferma per l'operatore) in una cartella policies, cosi' i conflitti di policy si rilevano prima dell'attivazione.

- Regola: Una regola trova documenti di policy in una cartella policies.
- Se manca: Spostare le regole di policy dell'applicazione nella cartella policies del pacchetto.

### `package.rag_inventory`

#### `req.rag.source_candidates`

- Category: `rag_source`. Level: `recommended`. Covered by: `deterministic`, `model_verified`.

**English.** Document and knowledge sources. Folders, mounted volumes, API sources or document providers the assistant should answer from are identified, so each can be declared with an owner and a scope. Undeclared sources are reported by the audit.

- Rule: A rule finds document folders, volume mounts or provider configuration. A verified model finding must cite the span that names the source.
- If missing: List the document sources the assistant should use, or state that there are none.

**Italiano.** Fonti di documenti e conoscenza. Cartelle, volumi montati, fonti API o provider di documenti da cui l'assistente deve rispondere sono identificati, cosi' ciascuno puo' essere dichiarato con proprietario e ambito. Le fonti non dichiarate sono segnalate dall'audit.

- Regola: Una regola trova cartelle di documenti, montaggi di volumi o configurazione di provider. Un riscontro verificato del modello deve citare il passaggio che nomina la fonte.
- Se manca: Elencare le fonti documentali che l'assistente deve usare, oppure dichiarare che non ce ne sono.

#### `req.rag.source_declarations`

- Category: `rag_source`. Level: `recommended`. Covered by: `deterministic`.

**English.** Declared sources with owner and scope. Each RAG source is declared in rag-sources with an owner, application, tenant and workspace scope, include and exclude rules, a credential reference, a refresh policy, deletion behaviour and a citation resolver.

- Rule: A rule finds source declarations in the rag-sources directory with those fields. Credentials are references only; a secret value is never read.
- If missing: Create one declaration per source in rag-sources and fill every field the source declaration schema requires (docs/integration/application-package.md).

**Italiano.** Fonti dichiarate con proprietario e ambito. Ogni fonte RAG e' dichiarata in rag-sources con proprietario, ambito di applicazione, tenant e workspace, regole di inclusione ed esclusione, riferimento alle credenziali, politica di aggiornamento, comportamento di cancellazione e risolutore di citazioni.

- Regola: Una regola trova nella cartella rag-sources dichiarazioni di fonte con quei campi. Le credenziali sono solo riferimenti; un valore segreto non viene mai letto.
- Se manca: Creare una dichiarazione per fonte in rag-sources e compilare ogni campo richiesto dallo schema della dichiarazione di fonte (docs/integration/application-package.md).

### `runtime.inspection`

#### `req.model.runtime_profile`

- Category: `model_runtime`. Level: `recommended`. Covered by: `deterministic`, `model_verified`.

**English.** Model and runtime needs. The project states which model profile and runtime it needs (languages, voice, memory and latency limits), so the audit can compare it with the deployment's resource limits and profile.

- Rule: A rule finds model and profile requirements in the manifest. A verified model finding must cite the span that states them.
- If missing: State the model profile, languages and resource limits the application needs in the manifest.

**Italiano.** Esigenze di modello e runtime. Il progetto indica quale profilo di modello e runtime richiede (lingue, voce, limiti di memoria e latenza), cosi' l'audit puo' confrontarlo con i limiti di risorse e il profilo del deployment.

- Regola: Una regola trova i requisiti di modello e profilo nel manifesto. Un riscontro verificato del modello deve citare il passaggio che li dichiara.
- Se manca: Indicare nel manifesto profilo di modello, lingue e limiti di risorse richiesti dall'applicazione.

#### `req.deployment.inspection`

- Category: `deployment`. Level: `recommended`. Covered by: `deterministic`, `model_verified`.

**English.** Inspection interface. The application provides an inspection adapter or a documented management API that reports versions, enabled modules, health and readiness, configured scopes, storage paths and network permissions. AlixAI does not scan anything the application does not expose.

- Rule: A rule finds a health or management endpoint definition. A verified model finding must cite the span that documents the inspection interface. Readiness does not call it.
- If missing: Expose a health and management endpoint, or an inspection adapter, and document what it reports.

**Italiano.** Interfaccia di ispezione. L'applicazione fornisce un adapter di ispezione o un'API di gestione documentata che riporta versioni, moduli abilitati, salute e prontezza, ambiti configurati, percorsi di archiviazione e permessi di rete. AlixAI non scansiona nulla che l'applicazione non esponga.

- Regola: Una regola trova la definizione di un endpoint di salute o gestione. Un riscontro verificato del modello deve citare il passaggio che documenta l'interfaccia di ispezione. La readiness non la chiama.
- Se manca: Esporre un endpoint di salute e gestione, o un adapter di ispezione, e documentare cosa riporta.

#### `req.deployment.compose_and_ports`

- Category: `deployment`. Level: `recommended`. Covered by: `deterministic`.

**English.** Deployment files, migrations and ports. Compose files, migrations and published ports are present and consistent, so the audit can match the declared deployment with the running one.

- Rule: A rule finds compose files, migration directories and port declarations, and reports a contradiction when two files disagree.
- If missing: Commit the compose files and migrations, and keep one source of truth for ports.

**Italiano.** File di deployment, migrazioni e porte. File compose, migrazioni e porte pubblicate sono presenti e coerenti, cosi' l'audit puo' confrontare il deployment dichiarato con quello in esecuzione.

- Regola: Una regola trova file compose, cartelle di migrazioni e dichiarazioni di porte, e segnala una contraddizione quando due file non concordano.
- Se manca: Versionare i file compose e le migrazioni e mantenere una sola fonte di verita' per le porte.

