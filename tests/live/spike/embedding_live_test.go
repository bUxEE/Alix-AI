//go:build live

package spike

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"testing"
	"time"
)

// retrievalSet is a small synthetic, privacy-safe retrieval check in English and Italian over the same small domains the
// benchmark corpus uses. Every query has exactly one relevant passage; the pool also holds same-domain distractors that
// share vocabulary with the relevant passage but answer a different question, so the check is not a bag-of-words match.
// The Italian passages are translations of the English ones, which makes the cross-language cases (English query, Italian
// pool) answerable too.
type retrievalPair struct{ query, passage string }

var (
	enPairs = []retrievalPair{
		{"How far in advance can I book a table?", "The restaurant accepts table reservations up to 30 days ahead and holds a table for 15 minutes after the booked time."},
		{"Is there a fee if I cancel my room the day before I arrive?", "Guests may cancel a hotel booking free of charge until 48 hours before arrival; later cancellations are charged one night."},
		{"Who decides on my holiday request and how long does it take?", "Employees request annual leave through the HR portal; the manager approves or declines within three working days."},
		{"Can I reach someone at the clinic on a national holiday for an urgent problem?", "The clinic is closed on public holidays; the emergency line stays open day and night."},
		{"When do I get my money back for a business lunch, and what proof is needed?", "Expense claims must include the original receipt and are reimbursed in the next monthly payroll."},
		{"At what hours can a truck unload goods, and what must be attached to each pallet?", "The warehouse receives deliveries on weekdays between 7 and 15; pallets must carry a printed label with the order number."},
		{"How quickly will you reply to a critical request?", "Support tickets with the priority urgent are answered within one hour; normal tickets within one working day."},
		{"What time can I eat in the morning and do I pay extra?", "Breakfast is served from 6:30 to 10:00 in the garden room and is included for guests on the flexible rate."},
		{"What should I carry when I go to see the doctor?", "Patients should arrive ten minutes before the appointment and bring a valid identity document and their insurance card."},
		{"I can't remember how to sign in, how do I get a new password?", "Forgotten passwords can be reset from the login page; the link in the email is valid for one hour."},
	}
	enDistractors = []string{
		"The restaurant offers a vegetarian tasting menu every Thursday evening.",
		"Hotel guests can use the spa between 9 and 20 for a small supplement.",
		"Employees can work remotely two days a week after agreeing it with their manager.",
		"The clinic offers vaccinations on Tuesday mornings without an appointment.",
		"Travel expenses above 500 euros need a prior written approval from the finance office.",
		"Outbound shipments are collected by the carrier at 16:00 and must be sealed and weighed before.",
		"Customers can follow the status of a ticket from the support portal and add comments.",
		"The hotel parking has 20 spaces and costs 12 euros per night, reservation required.",
	}
	itPairs = []retrievalPair{
		{"Con quanto anticipo posso riservare un tavolo?", "Il ristorante accetta prenotazioni fino a 30 giorni prima e tiene il tavolo per 15 minuti dopo l'orario prenotato."},
		{"Pago qualcosa se disdico la camera il giorno prima?", "È possibile annullare la prenotazione dell'albergo senza costi fino a 48 ore prima dell'arrivo; dopo si paga una notte."},
		{"Chi approva le mie ferie e in quanto tempo?", "I dipendenti chiedono le ferie dal portale del personale; il responsabile risponde entro tre giorni lavorativi."},
		{"Posso parlare con qualcuno in ambulatorio a Natale se è urgente?", "L'ambulatorio è chiuso nei giorni festivi; il numero per le urgenze resta attivo giorno e notte."},
		{"Quando mi rimborsano il pranzo di lavoro e cosa devo consegnare?", "Le note spese devono includere lo scontrino originale e vengono rimborsate con lo stipendio del mese successivo."},
		{"In quali orari si può scaricare la merce e cosa va applicato al bancale?", "Il magazzino riceve merci nei giorni feriali dalle 7 alle 15; ogni bancale deve avere l'etichetta con il numero d'ordine."},
		{"Quanto ci mettete a rispondere a un problema grave?", "Le richieste di assistenza urgenti ricevono risposta entro un'ora; quelle ordinarie entro un giorno lavorativo."},
		{"A che ora posso fare colazione e devo pagare a parte?", "La colazione è servita dalle 6:30 alle 10:00 nella sala giardino ed è inclusa con la tariffa flessibile."},
		{"Cosa devo portare quando vado dal medico?", "I pazienti devono presentarsi dieci minuti prima della visita con un documento d'identità valido e la tessera sanitaria."},
		{"Non ricordo come entrare, come ottengo una nuova password?", "Le password dimenticate si reimpostano dalla pagina di accesso; il link nell'email vale per un'ora."},
	}
	itDistractors = []string{
		"Il ristorante propone un menù degustazione vegetariano ogni giovedì sera.",
		"Gli ospiti dell'albergo possono usare la spa dalle 9 alle 20 con un piccolo supplemento.",
		"I dipendenti possono lavorare da casa due giorni a settimana previo accordo con il responsabile.",
		"L'ambulatorio offre le vaccinazioni il martedì mattina senza appuntamento.",
		"Le spese di viaggio oltre 500 euro richiedono un'approvazione scritta preventiva dell'ufficio amministrativo.",
		"Le spedizioni in uscita vengono ritirate dal corriere alle 16:00 e devono essere sigillate e pesate prima.",
		"I clienti possono seguire lo stato di una richiesta dal portale di assistenza e aggiungere commenti.",
		"Il parcheggio dell'albergo ha 20 posti e costa 12 euro a notte, con prenotazione obbligatoria.",
	}
)

type embedSet struct {
	name     string
	queries  []string
	pool     []string
	relevant []int // index into pool of the passage that answers queries[i]
}

func buildSets() []embedSet {
	mk := func(name string, qs []retrievalPair, poolPairs []retrievalPair, distract []string) embedSet {
		s := embedSet{name: name}
		for _, p := range poolPairs {
			s.pool = append(s.pool, p.passage)
		}
		s.pool = append(s.pool, distract...)
		for i, q := range qs {
			s.queries = append(s.queries, q.query)
			s.relevant = append(s.relevant, i) // the first len(pairs) pool entries are the pairs, in order
		}
		return s
	}
	return []embedSet{
		mk("en_to_en", enPairs, enPairs, enDistractors),
		mk("it_to_it", itPairs, itPairs, itDistractors),
		mk("en_to_it", enPairs, itPairs, itDistractors),
		mk("it_to_en", itPairs, enPairs, enDistractors),
	}
}

func embedBatch(env liveEnv, model string, inputs []string) ([][]float64, int64, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "input": inputs})
	start := time.Now()
	code, resp, err := httpDo(http.MethodPost, env.runtimeURL+"/v1/embeddings", body, map[string]string{"Authorization": "Bearer " + env.key})
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return nil, ms, err
	}
	if code != http.StatusOK {
		return nil, ms, fmt.Errorf("POST /v1/embeddings (%s) = %d: %s", model, code, truncate(string(resp), 400))
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, ms, err
	}
	if len(out.Data) != len(inputs) {
		return nil, ms, fmt.Errorf("asked for %d embeddings, got %d", len(inputs), len(out.Data))
	}
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })
	vecs := make([][]float64, len(out.Data))
	for i, d := range out.Data {
		vecs[i] = d.Embedding
	}
	return vecs, ms, nil
}

func normalise(v []float64) ([]float64, error) {
	var n float64
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, fmt.Errorf("the embedding holds NaN or Inf")
		}
		n += x * x
	}
	if n == 0 {
		return nil, fmt.Errorf("the embedding is all zeros")
	}
	n = math.Sqrt(n)
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = x / n
	}
	return out, nil
}

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// TestSpikeEmbeddingQuality loads the Granite Embedding 97M Multilingual R2 GGUF under the pinned LocalAI llama-cpp backend
// (the `modern-bert` architecture was unverified there) in its two quantisations, the registry's Q4_K_M and the Q8_0 of the
// same repository and commit, and compares retrieval quality on the synthetic English/Italian set. It writes
// docs/evidence/m0-spike/embedding.json. A model that does not load, returns the wrong dimension or non-finite numbers
// fails the test; the quality numbers are recorded, not asserted, because the choice between the two is the report's.
func TestSpikeEmbeddingQuality(t *testing.T) {
	env := needEnv(t)
	const wantDims = 384
	models := []struct{ label, name string }{
		{"Q4_K_M", "granite-embedding-97m-multilingual-r2-q4_k_m"},
		{"Q8_0", "granite-embedding-97m-multilingual-r2-q8_0"},
	}
	sets := buildSets()

	type setResult struct {
		Set       string  `json:"set"`
		Queries   int     `json:"queries"`
		PoolSize  int     `json:"pool_size"`
		RecallAt1 float64 `json:"recall_at_1"`
		RecallAt3 float64 `json:"recall_at_3"`
		MRR       float64 `json:"mrr"`
	}
	type modelResult struct {
		Label               string      `json:"quantisation"`
		LocalAIModel        string      `json:"localai_model"`
		Dimensions          int         `json:"dimensions"`
		LoadAndFirstBatchMS int64       `json:"first_batch_ms_includes_model_load"`
		SecondBatchMS       int64       `json:"second_batch_ms"`
		Texts               int         `json:"texts_embedded"`
		Results             []setResult `json:"results"`
		MeanRecallAt1       float64     `json:"mean_recall_at_1"`
		MeanMRR             float64     `json:"mean_mrr"`
	}
	var all []modelResult
	// vectors[label][text] keeps the normalised embeddings for the Q4-versus-Q8 drift measure.
	vectors := map[string]map[string][]float64{}

	for _, m := range models {
		t.Run(m.label, func(t *testing.T) {
			var texts []string
			seen := map[string]bool{}
			for _, s := range sets {
				for _, x := range append(append([]string{}, s.queries...), s.pool...) {
					if !seen[x] {
						seen[x] = true
						texts = append(texts, x)
					}
				}
			}
			var firstMS, secondMS int64
			vecs := map[string][]float64{}
			dims := 0
			const batch = 16
			for pass := 0; pass < 2; pass++ { // pass 0 includes the model load; pass 1 is warm
				var total int64
				for i := 0; i < len(texts); i += batch {
					end := min(i+batch, len(texts))
					out, ms, err := embedBatch(env, m.name, texts[i:end])
					if err != nil {
						t.Fatalf("%s did not embed (the architecture may not load under this backend): %v", m.name, err)
					}
					total += ms
					for j, v := range out {
						nv, err := normalise(v)
						if err != nil {
							t.Fatalf("%s: %v", m.name, err)
						}
						if dims == 0 {
							dims = len(nv)
						}
						if len(nv) != dims {
							t.Fatalf("%s returned vectors of different lengths (%d, %d)", m.name, dims, len(nv))
						}
						vecs[texts[i+j]] = nv
					}
				}
				if pass == 0 {
					firstMS = total
				} else {
					secondMS = total
				}
			}
			if dims != wantDims {
				t.Errorf("%s: %d dimensions, the registry says %d", m.name, dims, wantDims)
			}
			vectors[m.label] = vecs

			mr := modelResult{Label: m.label, LocalAIModel: m.name, Dimensions: dims, LoadAndFirstBatchMS: firstMS, SecondBatchMS: secondMS, Texts: len(texts)}
			for _, s := range sets {
				r := setResult{Set: s.name, Queries: len(s.queries), PoolSize: len(s.pool)}
				for qi, q := range s.queries {
					qv := vecs[q]
					type scored struct {
						idx int
						sim float64
					}
					ranked := make([]scored, len(s.pool))
					for pi, p := range s.pool {
						ranked[pi] = scored{pi, dot(qv, vecs[p])}
					}
					sort.Slice(ranked, func(a, b int) bool { return ranked[a].sim > ranked[b].sim })
					for rank, c := range ranked {
						if c.idx == s.relevant[qi] {
							if rank == 0 {
								r.RecallAt1++
							}
							if rank < 3 {
								r.RecallAt3++
							}
							r.MRR += 1 / float64(rank+1)
							break
						}
					}
				}
				n := float64(len(s.queries))
				r.RecallAt1, r.RecallAt3, r.MRR = round3(r.RecallAt1/n), round3(r.RecallAt3/n), round3(r.MRR/n)
				mr.Results = append(mr.Results, r)
				mr.MeanRecallAt1 += r.RecallAt1 / float64(len(sets))
				mr.MeanMRR += r.MRR / float64(len(sets))
			}
			mr.MeanRecallAt1, mr.MeanMRR = round3(mr.MeanRecallAt1), round3(mr.MeanMRR)
			all = append(all, mr)
			t.Logf("%s: dims %d, mean recall@1 %.3f, mean MRR %.3f, first pass %d ms (includes load), warm pass %d ms for %d texts",
				m.label, dims, mr.MeanRecallAt1, mr.MeanMRR, firstMS, secondMS, len(texts))
		})
	}

	// Quantisation drift: cosine between the Q4 and Q8 embedding of the same text.
	drift := map[string]any{}
	if q4, q8 := vectors["Q4_K_M"], vectors["Q8_0"]; len(q4) > 0 && len(q8) > 0 {
		var sum, minSim float64
		minSim = 1
		n := 0
		for text, a := range q4 {
			b, ok := q8[text]
			if !ok || len(a) != len(b) {
				continue
			}
			c := dot(a, b)
			sum += c
			minSim = math.Min(minSim, c)
			n++
		}
		if n > 0 {
			drift = map[string]any{"texts": n, "mean_cosine_q4_vs_q8": round3(sum / float64(n)), "min_cosine_q4_vs_q8": round3(minSim)}
		}
	}
	path := writeEvidence(t, env, "embedding.json", map[string]any{
		"task":        "M0-13",
		"emulated":    true,
		"what":        "retrieval quality of Granite Embedding 97M Multilingual R2, Q4_K_M versus Q8_0, on a synthetic English/Italian set",
		"set_note":    "10 query/passage pairs per language plus 8 same-domain distractors per language; sets en_to_en, it_to_it, en_to_it (English query, Italian pool) and it_to_en; one relevant passage per query; cosine similarity of L2-normalised vectors",
		"limitations": "20 queries per language on a pool of 18: enough to see a gross failure of a quantisation, far too small to rank two close models",
		"models":      all,
		"drift":       drift,
		"measured_at": time.Now().UTC().Format(time.RFC3339),
	})
	t.Logf("wrote %s", path)
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }
