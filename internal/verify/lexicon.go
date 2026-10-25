package verify

import "strings"

// The lexicons below hold the words the extractor recognises. They are checked in every
// supported language at once, whatever language the response is meant to be in: a model that
// answers an Italian user in English still has to pass the same facts check, and a union can only
// make the check stricter.

// monthFull maps a folded full month name to its number. "may" and "march" are listed in
// monthAmbiguous instead because they are also ordinary English words.
var monthFull = map[string]int{
	"january": 1, "february": 2, "april": 4, "june": 6, "july": 7, "august": 8,
	"september": 9, "october": 10, "november": 11, "december": 12,
	"gennaio": 1, "febbraio": 2, "marzo": 3, "aprile": 4, "maggio": 5, "giugno": 6, "luglio": 7,
	"agosto": 8, "settembre": 9, "ottobre": 10, "novembre": 11, "dicembre": 12,
}

// monthAmbiguous are month names that double as everyday words. They count as a month only when
// they sit next to a day or a year ("14 May", "March 2027"), never on their own ("you may").
var monthAmbiguous = map[string]int{"may": 5, "march": 3}

// monthAbbr are abbreviations. Several are ordinary words ("set", "ago", "mar", "dec", "gen"), so
// they count as a month only when they sit next to a day or a year.
var monthAbbr = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "sept": 9,
	"oct": 10, "nov": 11, "dec": 12,
	"gen": 1, "mag": 5, "giu": 6, "lug": 7, "ago": 8, "set": 9, "ott": 10, "dic": 12,
}

// weekday numbers follow time.Weekday: Sunday is 0.
var weekdayNames = map[string]int{
	"sunday": 0, "monday": 1, "tuesday": 2, "wednesday": 3, "thursday": 4, "friday": 5, "saturday": 6,
	"domenica": 0, "lunedi": 1, "martedi": 2, "mercoledi": 3, "giovedi": 4, "venerdi": 5, "sabato": 6,
}

// weekdayAbbr are abbreviated weekdays. Several are ordinary words ("sat", "sun", "wed", "dom"), so
// they count as a weekday only right in front of a date. Italian "mar" is read as March.
var weekdayAbbr = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "tues": 2, "wed": 3, "thu": 4, "thur": 4, "thurs": 4, "fri": 5, "sat": 6,
	"dom": 0, "lun": 1, "mer": 3, "gio": 4, "ven": 5, "sab": 6,
}

// dateConnectors may sit between a day and its month ("14th of October", "14 di ottobre").
var dateConnectors = map[string]bool{"of": true, "di": true, "de": true, "del": true}

// articles may sit between a weekday and the date it qualifies ("Tuesday the 14th").
var weekdayDateGap = map[string]bool{"the": true, "il": true, "lo": true, "la": true, "l": true, "on": true, "del": true}

// timePrefixes are words after which a bare 0-23 number reads as an hour ("at 10", "alle 10").
var timePrefixes = map[string]bool{"at": true, "alle": true, "ore": true, "h": true}

// scaleWords multiply the number they follow, attached or as the next word ("5k", "5 mila", "2,5
// milioni", "3 billion"), by ten to the given power. "m" is not one: after a number it is as often
// metres or minutes.
var scaleWords = map[string]int{
	"k": 3, "thousand": 3, "mila": 3, "mille": 3,
	"million": 6, "mln": 6, "milione": 6, "milioni": 6,
	"billion": 9, "bn": 9, "mld": 9, "miliardo": 9, "miliardi": 9,
}

// currencies maps a folded currency word to an ISO 4217 code.
var currencyWords = map[string]string{
	"euro": "EUR", "euros": "EUR", "eur": "EUR",
	"dollar": "USD", "dollars": "USD", "dollaro": "USD", "dollari": "USD", "usd": "USD",
	"pound": "GBP", "pounds": "GBP", "sterling": "GBP", "sterlina": "GBP", "sterline": "GBP", "gbp": "GBP",
	"yen": "JPY", "jpy": "JPY",
	"franc": "CHF", "francs": "CHF", "franco": "CHF", "franchi": "CHF", "chf": "CHF",
	"cad": "CAD", "aud": "AUD", "nzd": "NZD", "sek": "SEK", "nok": "NOK", "dkk": "DKK",
	"pln": "PLN", "czk": "CZK", "huf": "HUF", "ron": "RON", "bgn": "BGN", "inr": "INR",
	"cny": "CNY", "rmb": "CNY", "brl": "BRL", "mxn": "MXN", "zar": "ZAR", "try": "TRY",
	"rub": "RUB", "krw": "KRW", "hkd": "HKD", "sgd": "SGD", "aed": "AED", "ils": "ILS",
}

// currencySymbols maps a currency symbol to an ISO 4217 code.
var currencySymbols = map[rune]string{
	'€': "EUR", '$': "USD", '£': "GBP", '¥': "JPY", '₹': "INR", '₩': "KRW", '₽': "RUB",
	'₺': "TRY", '₪': "ILS", '₫': "VND", '₴': "UAH", '₦': "NGN", '฿': "THB",
}

// completionWords are words that state that something has been done. A response containing one
// asserts a completed action, so it is allowed only against a committed ActionReceipt. The set is
// past participles and completion adverbs; base forms ("confirm", "book") are not listed because
// the requests for confirmation legitimately use them.
var completionWords = buildCompletionWords()

func buildCompletionWords() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		booked scheduled rescheduled cancelled canceled created completed confirmed saved sent updated
		deleted removed registered processed approved changed modified paid ordered reserved added
		executed performed applied charged refunded transferred issued done successfully
		finalized finalised arranged secured enrolled
	`) {
		m[w] = true
	}
	// Italian participles inflect for gender and number: -o, -a, -i, -e.
	for _, stem := range strings.Fields(`
		prenotat riprenotat programmat cancellat annullat creat completat confermat salvat inviat
		aggiornat eliminat rimoss registrat pagat approvat modificat ordinat riservat eseguit
		effettuat applicat addebitat rimborsat trasferit emess aggiunt spostat inoltrat
		finit conclus fissat finalizzat sistemat iscritt
	`) {
		for _, suffix := range []string{"o", "a", "i", "e"} {
			m[stem+suffix] = true
		}
	}
	for _, w := range strings.Fields(`fatto fatta fatti fatte riuscito riuscita`) {
		m[w] = true
	}
	return m
}

// completionPhrases are multi-word completion claims, matched on the folded text.
var completionPhrases = []string{"all set", "con successo", "ecco fatto", "tutto a posto", "tutto fatto"}

// negators cancel a completion word that follows them in the same clause ("was not booked",
// "nothing was saved", "non è stato prenotato"). Words are folded and apostrophes removed, so
// "couldn't" is "couldnt".
var negators = map[string]bool{
	"not": true, "no": true, "never": true, "nothing": true, "cannot": true, "unable": true,
	"hasnt": true, "isnt": true, "wasnt": true, "werent": true, "didnt": true, "couldnt": true,
	"wouldnt": true, "havent": true, "hadnt": true, "cant": true, "wont": true, "arent": true,
	"non": true, "mai": true, "nulla": true, "niente": true, "nessun": true, "nessuno": true,
	"nessuna": true,
}

// negationBridge are the only words allowed between a negator and the completion word it negates:
// auxiliaries and adverbs ("has not yet been booked", "non è ancora stato prenotato"). Anything
// else between them and the completion word still counts as a claim.
var negationBridge = map[string]bool{
	"been": true, "be": true, "being": true, "was": true, "were": true, "is": true, "are": true,
	"has": true, "have": true, "had": true, "yet": true, "still": true, "ever": true, "even": true,
	"actually": true, "successfully": true, "stato": true, "stata": true, "stati": true,
	"state": true, "e": true, "sono": true, "era": true, "erano": true, "ancora": true,
	"essere": true, "mai": true,
}
