package verify

import "github.com/buxee/alix-ai/pkg/contracts"

// messages is the wording of the deterministic templates in one language. Every sentence is plain
// on purpose: the templates are the fallback when a model's phrasing fails verification and the
// whole response in degraded mode, and they must be safe by construction. A template states
// completion ("completed") only for a committed receipt and contains no digit of its own.
type messages struct {
	months   [12]string
	yes, no  string
	utc      string
	needMore string
	// %s is the label of the field asked for.
	chooseOne, provide, whichOne string
	yesNo                        string
	// %s is a comma-separated list of field labels.
	stillNeeded string
	invalid     string
	disambig    string
	confirm     string
	confirmAsk  string
	// %s is a list of verbs.
	confirmOptions string
	stepUp         string
	continueHere   string
	present        string
	presentNothing string
	committed      string
	rejected       string
	queued         string
	partial        string
	uncertain      string
	// %s is the follow-up state.
	followUp   string
	details    string // heading of the problems a receipt reports
	requested  string // heading of the facts of a request whose outcome is not a completed one
	failure    string
	refuse     string
	helpIntro  string
	helpNone   string
	noEvidence string
	cancelled  string
	sources    string
	// %s is a list of labels.
	alsoYouCan string
	// verbs name the permitted next actions in the confirmation line.
	verbConfirm, verbReject, verbModify, verbCancel, verbRefresh, verbRetry string
	// and joins the last two verbs of a list.
	and string
	// followStates maps a follow-up state to words.
	followQueued, followRunning, followSucceeded, followFailed, followUnknown string
	// reasons maps an error code to a sentence; every code in contracts.AllErrCodes has one.
	reasons map[contracts.ErrCode]string
	generic string
}

var enMessages = messages{
	months: [12]string{"January", "February", "March", "April", "May", "June", "July", "August",
		"September", "October", "November", "December"},
	yes: "Yes", no: "No", utc: "UTC",
	needMore:  "I need a bit more information to continue.",
	chooseOne: "Please choose %s:", provide: "Please provide %s.", whichOne: "Please tell me which %s you mean.",
	yesNo:          "Please answer yes or no.",
	stillNeeded:    "Still needed: %s.",
	invalid:        "I couldn't accept that.",
	disambig:       "More than one option matches. Please choose one:",
	confirm:        "Please check these details:",
	confirmAsk:     "Shall I go ahead?",
	confirmOptions: "You can %s.",
	stepUp:         "This needs extra verification, so it has to be handled in the application itself.",
	continueHere:   "Please continue there.",
	present:        "Here is what I found:",
	presentNothing: "There is nothing to show.",
	committed:      "This has been completed.",
	rejected:       "This was not carried out. No changes were made.",
	queued:         "Your request has been accepted and is being processed. It is not finished yet.",
	partial:        "Only part of this was carried out.",
	uncertain: "I couldn't confirm whether this was carried out. Please check in the application before " +
		"trying again, and do not repeat it blindly.",
	followUp:    "Follow-up: %s.",
	details:     "Problems:",
	requested:   "Request details:",
	failure:     "Sorry, this could not be carried out.",
	refuse:      "I'm not able to do that.",
	helpIntro:   "I can help with the following:",
	helpNone:    "I can help with this application. What would you like to do?",
	noEvidence:  "I couldn't find anything in the available sources that answers this.",
	cancelled:   "Okay, I've stopped that request. No changes were made.",
	sources:     "Sources:",
	alsoYouCan:  "You can also: %s.",
	verbConfirm: "confirm", verbReject: "decline", verbModify: "change it", verbCancel: "cancel",
	verbRefresh: "refresh it", verbRetry: "try again",
	and:          "or",
	followQueued: "waiting to start", followRunning: "in progress", followSucceeded: "succeeded",
	followFailed: "failed", followUnknown: "status unknown",
	generic: "Something went wrong.",
	reasons: map[contracts.ErrCode]string{
		contracts.ErrScopeMissing:               "I can't tell which account or workspace this belongs to.",
		contracts.ErrUnauthenticated:            "I couldn't verify who you are. Please sign in again.",
		contracts.ErrPermissionDenied:           "You don't have permission to do that.",
		contracts.ErrScopeViolation:             "That is outside what you are allowed to access here.",
		contracts.ErrSchemaInvalid:              "I received information I couldn't use.",
		contracts.ErrUnknownCapability:          "That isn't something I can do in this application.",
		contracts.ErrAmbiguous:                  "More than one option matches.",
		contracts.ErrMissingFields:              "Some required details are missing.",
		contracts.ErrValidationFailed:           "Some of the details are not valid.",
		contracts.ErrStale:                      "The information is out of date. Please start again.",
		contracts.ErrExpired:                    "This request has expired. Please start again.",
		contracts.ErrReplayed:                   "This request was already used.",
		contracts.ErrConfirmationRequired:       "This needs your confirmation first.",
		contracts.ErrStepUpRequired:             "This needs extra verification in the application itself.",
		contracts.ErrConflict:                   "This conflicts with the current state.",
		contracts.ErrUnavailable:                "The service is not available right now. Please try again later.",
		contracts.ErrDegraded:                   "The service is running in a limited mode right now.",
		contracts.ErrTimeout:                    "The service took too long to answer. Please try again.",
		contracts.ErrRateLimited:                "There are too many requests right now. Please wait a moment and try again.",
		contracts.ErrResourceExhausted:          "The system is busy right now. Please try again later.",
		contracts.ErrNoSupportedEvidence:        "I couldn't find anything in the available sources that supports an answer.",
		contracts.ErrUncertainOutcome:           "I couldn't confirm what happened. Please check in the application before trying again.",
		contracts.ErrPartialFailure:             "Only part of this was carried out.",
		contracts.ErrQuarantined:                "This isn't available right now.",
		contracts.ErrOutOfDomain:                "That is outside what I can help with in this application.",
		contracts.ErrAbilityDisabled:            "That analysis ability is not enabled here.",
		contracts.ErrModelNotAnalystCapable:     "The current model cannot run analyst turns.",
		contracts.ErrInsufficientData:           "There is not enough data to complete that calculation.",
		contracts.ErrWebQueryRefused:            "That web search was refused because it would expose restricted data.",
		contracts.ErrAnalystCapExceeded:         "This analysis hit a configured limit for the turn.",
		contracts.ErrUnitMismatch:               "Those figures use incompatible units or currencies.",
		contracts.ErrForecastUnavailable:        "A forecast is not available for that series.",
		contracts.ErrDomainClassificationFailed: "I could not classify whether that question is in domain.",
		contracts.ErrInternal:                   "Something went wrong on our side.",
	},
}

var itMessages = messages{
	months: [12]string{"gennaio", "febbraio", "marzo", "aprile", "maggio", "giugno", "luglio", "agosto",
		"settembre", "ottobre", "novembre", "dicembre"},
	yes: "Sì", no: "No", utc: "UTC",
	needMore:  "Mi servono ancora alcune informazioni per continuare.",
	chooseOne: "Scegli %s:", provide: "Indica %s.", whichOne: "Dimmi a quale %s ti riferisci.",
	yesNo:          "Rispondi sì o no.",
	stillNeeded:    "Mancano ancora: %s.",
	invalid:        "Non posso accettare questo valore.",
	disambig:       "Più opzioni corrispondono. Scegline una:",
	confirm:        "Controlla questi dettagli:",
	confirmAsk:     "Procedo?",
	confirmOptions: "Puoi %s.",
	stepUp:         "Serve una verifica aggiuntiva, quindi va portata a termine direttamente nell'applicazione.",
	continueHere:   "Continua nell'applicazione.",
	present:        "Ecco cosa ho trovato:",
	presentNothing: "Non c'è nulla da mostrare.",
	committed:      "Operazione completata.",
	rejected:       "L'operazione non è stata portata a termine. Nessuna modifica è stata apportata.",
	queued:         "La richiesta è stata accettata ed è in elaborazione. Non è ancora terminata.",
	partial:        "Solo una parte dell'operazione è stata portata a termine.",
	uncertain: "Non posso verificare se l'operazione è stata portata a termine. Controlla nell'applicazione " +
		"prima di riprovare e non ripeterla alla cieca.",
	followUp:    "Passaggio successivo: %s.",
	details:     "Problemi:",
	requested:   "Dettagli della richiesta:",
	failure:     "Mi dispiace, non è stato possibile portarla a termine.",
	refuse:      "Non posso farlo.",
	helpIntro:   "Posso aiutarti con queste attività:",
	helpNone:    "Posso aiutarti con questa applicazione. Cosa vuoi fare?",
	noEvidence:  "Non ho trovato nulla nelle fonti disponibili che risponda a questa domanda.",
	cancelled:   "Va bene, ho interrotto la richiesta. Nessuna modifica è stata apportata.",
	sources:     "Fonti:",
	alsoYouCan:  "Puoi anche: %s.",
	verbConfirm: "confermare", verbReject: "rifiutare", verbModify: "cambiarla", verbCancel: "annullare",
	verbRefresh: "ricaricarla", verbRetry: "riprovare",
	and:          "oppure",
	followQueued: "in attesa", followRunning: "in corso", followSucceeded: "terminato con esito positivo",
	followFailed: "fallito", followUnknown: "stato sconosciuto",
	generic: "Qualcosa è andato storto.",
	reasons: map[contracts.ErrCode]string{
		contracts.ErrScopeMissing:               "Non riesco a capire a quale account o area di lavoro appartiene la richiesta.",
		contracts.ErrUnauthenticated:            "Non posso verificare la tua identità. Accedi di nuovo.",
		contracts.ErrPermissionDenied:           "Non hai l'autorizzazione per farlo.",
		contracts.ErrScopeViolation:             "Questo è al di fuori di ciò a cui puoi accedere qui.",
		contracts.ErrSchemaInvalid:              "Ho ricevuto informazioni che non posso usare.",
		contracts.ErrUnknownCapability:          "Questa non è un'operazione che posso svolgere in questa applicazione.",
		contracts.ErrAmbiguous:                  "Più opzioni corrispondono.",
		contracts.ErrMissingFields:              "Mancano alcuni dettagli obbligatori.",
		contracts.ErrValidationFailed:           "Alcuni dettagli non sono validi.",
		contracts.ErrStale:                      "Le informazioni sono obsolete. Ricomincia.",
		contracts.ErrExpired:                    "La richiesta è scaduta. Ricomincia.",
		contracts.ErrReplayed:                   "Questa richiesta è già stata utilizzata.",
		contracts.ErrConfirmationRequired:       "Serve prima la tua conferma.",
		contracts.ErrStepUpRequired:             "Serve una verifica aggiuntiva direttamente nell'applicazione.",
		contracts.ErrConflict:                   "Questo è in conflitto con lo stato attuale.",
		contracts.ErrUnavailable:                "Il servizio non è disponibile al momento. Riprova più tardi.",
		contracts.ErrDegraded:                   "Il servizio funziona in modalità limitata al momento.",
		contracts.ErrTimeout:                    "Il servizio ha impiegato troppo tempo a rispondere. Riprova.",
		contracts.ErrRateLimited:                "Ci sono troppe richieste al momento. Attendi un attimo e riprova.",
		contracts.ErrResourceExhausted:          "Il sistema è occupato al momento. Riprova più tardi.",
		contracts.ErrNoSupportedEvidence:        "Non ho trovato nulla nelle fonti disponibili che supporti una risposta.",
		contracts.ErrUncertainOutcome:           "Non posso confermare cosa è successo. Controlla nell'applicazione prima di riprovare.",
		contracts.ErrPartialFailure:             "Solo una parte dell'operazione è stata portata a termine.",
		contracts.ErrQuarantined:                "Questo non è disponibile al momento.",
		contracts.ErrOutOfDomain:                "Questo è al di fuori di ciò su cui posso aiutarti in questa applicazione.",
		contracts.ErrAbilityDisabled:            "Questa capacità di analisi non è abilitata qui.",
		contracts.ErrModelNotAnalystCapable:     "Il modello attuale non può eseguire turni analyst.",
		contracts.ErrInsufficientData:           "Non ci sono dati sufficienti per completare quel calcolo.",
		contracts.ErrWebQueryRefused:            "La ricerca web non è consentita: conterrebbe dati confidenziali.",
		contracts.ErrAnalystCapExceeded:         "Questa analisi ha raggiunto un limite configurato per il turno.",
		contracts.ErrUnitMismatch:               "Queste cifre usano unità o valute incompatibili.",
		contracts.ErrForecastUnavailable:        "Una previsione non è disponibile per quella serie.",
		contracts.ErrDomainClassificationFailed: "Non sono riuscito a classificare se la domanda è nel dominio.",
		contracts.ErrInternal:                   "Si è verificato un problema da parte nostra.",
	},
}

func messagesFor(l Language) *messages {
	if l == LangIT {
		return &itMessages
	}
	return &enMessages
}
