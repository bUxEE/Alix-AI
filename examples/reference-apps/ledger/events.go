package ledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Application events (master plan 9.3, item 4): the ledger tells AlixAI that a record
// changed so state AlixAI holds about it can be invalidated and the sessions it
// concerns told. An event can only invalidate: AlixAI never prepares, confirms or
// commits anything because of one.
//
// The wire format is the event adapter's (adapters/events): one JSON delivery
// {"event": {...}, "signatures": [one]} where the signature is the application's
// detached Ed25519 signature over the RFC 8785 canonical JSON of the event member.
// The ledger signs with its own code, with no AlixAI import: the format is the
// contract, and the tests deliver the ledger's output to the real intake.

// EventFormat is the constant every signed event carries, so a signature made for
// anything else cannot be replayed as an event.
const EventFormat = "alixai.application-event.v1"

// MaxEventValidity is the longest an event signature may be valid; the intake
// refuses more.
const MaxEventValidity = 15 * time.Minute

// The event types the application emits, as its package declares them.
const (
	EventInvoicePaid            = "invoice.paid"
	EventInvoicePaymentRecorded = "invoice.payment_recorded"
	EventInvoiceVoided          = "invoice.voided"
	EventContractUpdated        = "contract.updated"
)

// Event is the typed application event. Every member that decides what it does is
// in it, so the signature covers all of it.
type Event struct {
	Format        string        `json:"format"`
	ID            string        `json:"id"`
	Type          string        `json:"type"`
	Kind          string        `json:"kind"`
	ApplicationID string        `json:"application_id"`
	TenantID      string        `json:"tenant_id"`
	WorkspaceID   string        `json:"workspace_id"`
	Entities      []EventEntity `json:"entities,omitempty"`
}

// EventEntity is a record an event concerns, at the version it is now.
type EventEntity struct {
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Version    string `json:"version,omitempty"`
}

// EventSink delivers one signed-to-be event. A failure leaves the event in the
// outbox for the next flush.
type EventSink interface {
	Send(ctx context.Context, e Event) error
}

// emit queues an event for a record revision. It runs under the lock, in the same
// step as the change it describes: a change without its event cannot happen.
func (l *Ledger) emit(t *tenant, teamID, eventType, entityType, entityID string, version int64) {
	l.evtSeq++
	l.outbox = append(l.outbox, Event{
		Format: EventFormat, ID: "lev_" + pad(l.evtSeq, 6), Type: eventType, Kind: "record_revision",
		ApplicationID: ApplicationID, TenantID: t.id, WorkspaceID: teamID,
		Entities: []EventEntity{{EntityType: entityType, EntityID: entityID, Version: itoa(version)}},
	})
}

// Outbox returns the events not yet delivered, oldest first.
func (l *Ledger) Outbox() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Event(nil), l.outbox...)
}

// flushOutbox delivers the queued events in order and stops at the first failure,
// so a receiver never sees a later revision before an earlier one.
func (l *Ledger) flushOutbox(ctx context.Context) error {
	if l.cfg.Events == nil {
		return nil
	}
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	for {
		l.mu.Lock()
		if len(l.outbox) == 0 {
			l.mu.Unlock()
			return nil
		}
		e := l.outbox[0]
		l.mu.Unlock()
		if err := l.cfg.Events.Send(ctx, e); err != nil {
			return err
		}
		l.mu.Lock()
		if len(l.outbox) > 0 && l.outbox[0].ID == e.ID {
			l.outbox = l.outbox[1:]
		}
		l.mu.Unlock()
	}
}

// ---- signing ----------------------------------------------------------------

// Signature is the detached signature of a delivery.
type Signature struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Signature string `json:"signature"`
	SignedAt  string `json:"signed_at"`
	ExpiresAt string `json:"expires_at"`
}

// Signer signs events with the application's event key, which AlixAI holds the
// public half of under KeyID. The key is a secret: it is read once by the caller
// (from a file or an environment reference) and never logged or written.
type Signer struct {
	KeyID    string
	Key      ed25519.PrivateKey
	Validity time.Duration
	Clock    func() time.Time
}

// Sign returns the delivery body for e: the event and exactly one signature (a
// delivery with none is unauthenticated and with several is refused).
func (s Signer) Sign(e Event) ([]byte, error) {
	if len(s.Key) != ed25519.PrivateKeySize || s.KeyID == "" {
		return nil, errors.New("ledger: the event signer needs a key and a key id")
	}
	v := s.Validity
	if v <= 0 {
		v = 5 * time.Minute
	}
	if v > MaxEventValidity {
		return nil, errors.New("ledger: an event signature may not be valid for more than 15 minutes")
	}
	clock := s.Clock
	if clock == nil {
		clock = time.Now
	}
	e.Format = EventFormat
	body, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	canon, err := CanonicalJSON(body)
	if err != nil {
		return nil, err
	}
	at := clock().UTC().Truncate(time.Second)
	sig := Signature{
		KeyID: s.KeyID, Algorithm: "ed25519",
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(s.Key, canon)),
		SignedAt:  at.Format(time.RFC3339Nano), ExpiresAt: at.Add(v).Format(time.RFC3339Nano),
	}
	return json.Marshal(struct {
		Event      json.RawMessage `json:"event"`
		Signatures []Signature     `json:"signatures"`
	}{Event: body, Signatures: []Signature{sig}})
}

// CanonicalJSON returns the RFC 8785 canonical form of the JSON document raw, for
// the subset an event uses: objects, arrays, strings, booleans, null and integers.
// A float is refused rather than formatted, so there is one spelling of every
// value.
func CanonicalJSON(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("ledger: the document is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("ledger: data after the end of the JSON value")
	}
	var b bytes.Buffer
	if err := writeCanonical(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		if strings.ContainsAny(x.String(), ".eE") || x.String() == "-0" {
			return errors.New("ledger: only integers are canonical")
		}
		b.WriteString(x.String())
	case string:
		writeCanonicalString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalString(b, k)
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("ledger: unsupported JSON value %T", v)
	}
	return nil
}

// utf16Less orders keys by UTF-16 code units, as RFC 8785 requires.
func utf16Less(a, b string) bool {
	ua, ub := utf16Units(a), utf16Units(b)
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

func utf16Units(s string) []uint16 {
	var out []uint16
	for _, r := range s {
		if r >= 0x10000 {
			r -= 0x10000
			out = append(out, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
		} else {
			out = append(out, uint16(r))
		}
	}
	return out
}

// writeCanonicalString writes only the escapes RFC 8785 requires; everything else,
// <, > & and U+2028 included, is written as is (encoding/json would escape them).
func writeCanonicalString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// ---- delivery ---------------------------------------------------------------

// HTTPSink posts signed events to AlixAI's event intake. The URL must be loopback
// http: the application and AlixAI share a machine, and the signature, not the
// channel, is what authenticates an event.
type HTTPSink struct {
	URL    string
	Signer Signer
	Client *http.Client
}

// NewHTTPSink validates the intake URL.
func NewHTTPSink(rawURL string, s Signer) (*HTTPSink, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" || u.User != nil {
		return nil, errors.New("ledger: the event intake URL must be a plain http URL on loopback")
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); !(host == "localhost" || (ip != nil && ip.IsLoopback())) {
		return nil, errors.New("ledger: the event intake must be on loopback")
	}
	return &HTTPSink{URL: rawURL, Signer: s, Client: &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Send signs and posts one event. Any status but 2xx is a failure, and the event
// stays queued.
func (h *HTTPSink) Send(ctx context.Context, e Event) error {
	body, err := h.Signer.Sign(e)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		return errors.New("ledger: the event intake could not be reached")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("ledger: the event intake answered %d", resp.StatusCode)
	}
	return nil
}
