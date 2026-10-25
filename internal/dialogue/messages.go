package dialogue

import "github.com/buxee/alix-ai/internal/verify"

// coreMsgs are the few sentences the core itself writes into a directive: the
// reason a value was refused. They are constants and numbers taken from the
// package's own field definition; they never repeat what the user wrote, so a
// refused value cannot smuggle text into a response. The verifier treats the
// messages of a directive as allowed text, and the templates print them as they
// are.
type coreMsgs struct {
	mustBeText     string
	mustBeInteger  string
	mustBeNumber   string
	mustBeBoolean  string
	mustBeDate     string
	mustBeDatetime string
	mustBeDuration string
	mustBeOneOf    string // %s: the values
	mustBeList     string
	tooShort       string // %d
	tooLong        string // %d
	belowMinimum   string // %s
	aboveMaximum   string // %s
	tooManyItems   string // %d
	noMatch        string
	notAChoice     string
}

var msgsEN = &coreMsgs{
	mustBeText:     "must be text",
	mustBeInteger:  "must be a whole number",
	mustBeNumber:   "must be a number",
	mustBeBoolean:  "must be yes or no",
	mustBeDate:     "must be a date written as YYYY-MM-DD",
	mustBeDatetime: "must be a date and time in RFC 3339 format",
	mustBeDuration: "must be a duration in ISO 8601 format",
	mustBeOneOf:    "must be one of: %s",
	mustBeList:     "must be a list",
	tooShort:       "must be at least %d characters",
	tooLong:        "must be at most %d characters",
	belowMinimum:   "must be at least %s",
	aboveMaximum:   "must be at most %s",
	tooManyItems:   "must have at most %d items",
	noMatch:        "matches no record",
	notAChoice:     "is not one of the choices offered",
}

var msgsIT = &coreMsgs{
	mustBeText:     "deve essere un testo",
	mustBeInteger:  "deve essere un numero intero",
	mustBeNumber:   "deve essere un numero",
	mustBeBoolean:  "deve essere sì o no",
	mustBeDate:     "deve essere una data nel formato AAAA-MM-GG",
	mustBeDatetime: "deve essere una data e un'ora in formato RFC 3339",
	mustBeDuration: "deve essere una durata in formato ISO 8601",
	mustBeOneOf:    "deve essere uno tra: %s",
	mustBeList:     "deve essere un elenco",
	tooShort:       "deve avere almeno %d caratteri",
	tooLong:        "deve avere al massimo %d caratteri",
	belowMinimum:   "deve essere almeno %s",
	aboveMaximum:   "deve essere al massimo %s",
	tooManyItems:   "deve avere al massimo %d elementi",
	noMatch:        "non corrisponde ad alcun record",
	notAChoice:     "non è una delle scelte proposte",
}

func messagesFor(l verify.Language) *coreMsgs {
	if l == verify.LangIT {
		return msgsIT
	}
	return msgsEN
}
