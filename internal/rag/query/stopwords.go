package query

import (
	"sort"
	"strings"
)

// IsStopWord reports whether w is one of the function words, English and
// Italian, that retrieval drops from a query's OR-expression: they occur in
// nearly every chunk, so as OR terms they make BM25 score the whole scope for no
// ranking gain. The comparison ignores case. It is the one list the ingest side
// (and anything else that has to agree with the query pipeline on what a
// content word is) reads, instead of keeping a copy that could drift from it.
//
// The list is terms.go's stopWords, which mirrors the one internal/rag/index
// applies inside Search; a package that cannot import this one (the index
// itself) keeps its own, and TestStopWordsAreTheIndexs checks the two agree.
func IsStopWord(w string) bool { return stopWords[strings.ToLower(w)] }

// StopWords returns the stop words in sorted order. The slice is the caller's:
// changing it changes nothing here.
func StopWords() []string {
	out := make([]string, 0, len(stopWords))
	for w := range stopWords {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}
