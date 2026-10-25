package ledger

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The contracts document API. The ledger keeps the contracts of each team, and
// serves them read-only over loopback HTTP in the shape AlixAI's application
// document API source provider reads (internal/rag/providers/httpapi):
//
//	GET  /?application_id&tenant_id&workspace_id&limit[&cursor]   one page of items
//	GET  /contracts/<slug>.md?<same scope>                        the item's bytes
//	HEAD /contracts/<slug>.md?<same scope>                        its metadata
//
// Every request carries the verified scope of AlixAI and a bearer credential; the
// ledger answers for that organisation and team only and reauthorises the scope
// itself. A contract of another team, of another organisation or that does not
// exist is the same 404, so a key proves nothing about what exists elsewhere. The
// version of an item is its content address (sha256:<hex>), which the provider
// verifies against the bytes it read.

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// contractKey is the item key of a contract slug.
func contractKey(slug string) string { return "contracts/" + slug + ".md" }

func (ct *contract) address() string {
	sum := sha256.Sum256([]byte(ct.body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// putContract stores or replaces the contract of a team under a slug. It runs under
// the lock; the public PutContract adds the event.
func (l *Ledger) putContract(t *tenant, teamID, slug, title, body string) *contract {
	slug = strings.ToLower(strings.TrimSpace(slug))
	key := contractKey(slug)
	for _, ct := range t.contracts {
		if ct.team == teamID && ct.key == key {
			ct.title, ct.body, ct.modified = title, body, l.now()
			ct.version++
			return ct
		}
	}
	ct := &contract{id: l.nextID("con"), key: key, team: teamID, title: title, body: body, version: 1, modified: l.now()}
	t.contracts[ct.id] = ct
	return ct
}

// PutContract adds or updates a contract of a team. It is the application's own
// back office, not a capability: AlixAI cannot reach it. A change emits the
// contract.updated event, which tells AlixAI the contracts source changed.
func (l *Ledger) PutContract(tenantID, teamID, slug, title, body string) error {
	if !slugPattern.MatchString(strings.ToLower(slug)) {
		return errors.New("ledger: a contract slug is lowercase letters, digits and hyphens")
	}
	l.mu.Lock()
	t := l.tenants[tenantID]
	if t == nil || t.teams[teamID] == nil {
		l.mu.Unlock()
		return errors.New("ledger: unknown organisation or team")
	}
	ct := l.putContract(t, teamID, slug, title, "# "+title+"\n\n"+strings.TrimSpace(body)+"\n")
	l.emit(t, teamID, EventContractUpdated, "contract", ct.id, ct.version)
	l.mu.Unlock()
	return l.flushOutbox(context.Background())
}

// DocumentAPI returns the handler of the contracts document API. The credential is
// required: a handler with no token is refused rather than served open.
func (l *Ledger) DocumentAPI(token string) (http.Handler, error) {
	if len(token) < 16 {
		return nil, errors.New("ledger: the document API needs a credential of at least 16 characters")
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		if q.Get("application_id") != ApplicationID || q.Get("tenant_id") == "" || q.Get("workspace_id") == "" {
			http.Error(w, "the request names no scope", http.StatusBadRequest)
			return
		}
		l.mu.Lock()
		t := l.tenants[q.Get("tenant_id")]
		var items []*contract
		if t != nil && t.teams[q.Get("workspace_id")] != nil {
			for _, ct := range t.contracts {
				if ct.team == q.Get("workspace_id") {
					items = append(items, ct)
				}
			}
		}
		known := t != nil && t.teams[q.Get("workspace_id")] != nil
		// Copy what is served so the response is written without the lock.
		snapshot := make([]contract, 0, len(items))
		for _, ct := range items {
			snapshot = append(snapshot, *ct)
		}
		l.mu.Unlock()
		if !known {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].key < snapshot[j].key })
		if r.URL.Path == "/" || r.URL.Path == "" {
			serveList(w, r, snapshot)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/")
		for i := range snapshot {
			if snapshot[i].key == key {
				serveItem(w, r, &snapshot[i])
				return
			}
		}
		http.NotFound(w, r)
	}), nil
}

func serveList(w http.ResponseWriter, r *http.Request, items []contract) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "bad limit", http.StatusBadRequest)
			return
		}
		limit = min(n, 500)
	}
	after := ""
	if c := r.URL.Query().Get("cursor"); c != "" {
		b, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil {
			http.Error(w, "bad cursor", http.StatusBadRequest)
			return
		}
		after = string(b)
	}
	type item struct {
		Key        string `json:"key"`
		Version    string `json:"version"`
		Size       int    `json:"size"`
		ModifiedAt string `json:"modified_at"`
	}
	out := struct {
		Items      []item  `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}{Items: []item{}}
	for _, ct := range items {
		if ct.key <= after {
			continue
		}
		if len(out.Items) == limit {
			next := base64.RawURLEncoding.EncodeToString([]byte(out.Items[len(out.Items)-1].Key))
			out.NextCursor = &next
			break
		}
		out.Items = append(out.Items, item{Key: ct.key, Version: ct.address(), Size: len(ct.body), ModifiedAt: ct.modified.UTC().Format(time.RFC3339)})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func serveItem(w http.ResponseWriter, r *http.Request, ct *contract) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("ETag", `"`+ct.address()+`"`)
	w.Header().Set("Last-Modified", ct.modified.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.Itoa(len(ct.body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(ct.body))
}
