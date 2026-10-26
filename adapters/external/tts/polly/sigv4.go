package polly

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const awsService = "polly"

// awsCreds holds access key id, secret, and optional session token from the
// credential value "ACCESS_KEY_ID:SECRET_ACCESS_KEY" or
// "ACCESS_KEY_ID:SECRET_ACCESS_KEY:SESSION_TOKEN" (third segment sets
// X-Amz-Security-Token). Segments are split on ':' (secrets must not contain ':').
type awsCreds struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
}

func parseAWSCreds(v string) (awsCreds, error) {
	parts := strings.Split(v, ":")
	switch len(parts) {
	case 2:
		if parts[0] == "" || parts[1] == "" || strings.ContainsAny(parts[0], " \t") {
			return awsCreds{}, errors.New("malformed credential")
		}
		return awsCreds{AccessKey: parts[0], SecretKey: parts[1]}, nil
	case 3:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" || strings.ContainsAny(parts[0], " \t") {
			return awsCreds{}, errors.New("malformed credential")
		}
		return awsCreds{AccessKey: parts[0], SecretKey: parts[1], SessionToken: parts[2]}, nil
	default:
		return awsCreds{}, errors.New("malformed credential")
	}
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func deriveSigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

// signResult is the SigV4 intermediate and final values for one request.
type signResult struct {
	CanonicalRequest string
	StringToSign     string
	Signature        string
	Authorization    string
}

// signOpts configures a Signature Version 4 signing pass.
type signOpts struct {
	Service string
	Region  string
	Creds   awsCreds
	Now     time.Time
	// PayloadHeader: set X-Amz-Content-Sha256 to the body hash and sign it
	// (Polly). The AWS vanilla test-suite vectors omit this header.
	PayloadHeader bool
}

// signV4Request signs req in place and returns the exact canonical request,
// string to sign and signature. If X-Amz-Date is already set it is kept;
// otherwise Now supplies it.
func signV4Request(req *http.Request, body []byte, o signOpts) signResult {
	if body == nil {
		body = []byte{}
	}
	payloadHash := sha256Hex(body)
	amzDate := req.Header.Get("X-Amz-Date")
	if amzDate == "" {
		amzDate = o.Now.UTC().Format("20060102T150405Z")
		req.Header.Set("X-Amz-Date", amzDate)
	}
	dateStamp := amzDate[:8]

	host := req.Header.Get("Host")
	if host == "" {
		host = req.URL.Host
		req.Header.Set("Host", host)
	}
	if o.PayloadHeader {
		req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	if o.Creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", o.Creds.SessionToken)
	}

	headers := map[string]string{"host": host, "x-amz-date": amzDate}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		headers["content-type"] = ct
	}
	if v := req.Header.Get("X-Amz-Content-Sha256"); v != "" {
		headers["x-amz-content-sha256"] = v
	}
	if v := req.Header.Get("X-Amz-Security-Token"); v != "" {
		headers["x-amz-security-token"] = v
	}
	var names []string
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHdr strings.Builder
	for _, n := range names {
		canonHdr.WriteString(n)
		canonHdr.WriteByte(':')
		canonHdr.WriteString(strings.TrimSpace(headers[n]))
		canonHdr.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQuery := canonicalizeQuery(req.URL.RawQuery)

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonHdr.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := strings.Join([]string{dateStamp, o.Region, o.Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := deriveSigningKey(o.Creds.SecretKey, dateStamp, o.Region, o.Service)
	sig := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	auth := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		o.Creds.AccessKey, credentialScope, signedHeaders, sig,
	)
	req.Header.Set("Authorization", auth)
	return signResult{
		CanonicalRequest: canonicalRequest,
		StringToSign:     stringToSign,
		Signature:        sig,
		Authorization:    auth,
	}
}

// signV4 adds Authorization for a Polly SynthesizeSpeech POST (payload hash header included).
func signV4(req *http.Request, body []byte, creds awsCreds, region string, now time.Time) {
	signV4Request(req, body, signOpts{
		Service: awsService, Region: region, Creds: creds, Now: now, PayloadHeader: true,
	})
}

// canonicalizeQuery builds the SigV4 canonical query string from a raw query
// (URI-encode each name and value, sort by encoded name then value). AWS
// UriEncode uses %20 for space, never '+'.
func canonicalizeQuery(raw string) string {
	if raw == "" {
		return ""
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		// Fall back to empty rather than signing a malformed query.
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	first := true
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		ek := uriEncode(k, true)
		for _, v := range vs {
			if !first {
				b.WriteByte('&')
			}
			first = false
			b.WriteString(ek)
			b.WriteByte('=')
			b.WriteString(uriEncode(v, true))
		}
	}
	return b.String()
}

// uriEncode implements the AWS SigV4 UriEncode rules (docs checked 2026-10-26):
// https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' || (c == '/' && !encodeSlash) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}
