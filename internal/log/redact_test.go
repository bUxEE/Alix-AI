package log

import (
	"strings"
	"testing"
)

func TestRedactStringPatterns(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // exact result
	}{
		{"bearer", "Bearer abc.def-ghi_jkl==", "Bearer " + Redacted},
		{"bearer lower case in sentence", "sent bearer abc123 ok", "sent bearer " + Redacted + " ok"},
		{"authorization header line", "Authorization: Bearer abc\nHost: h", "Authorization: " + Redacted + "\nHost: h"},
		{"basic authorization", "authorization: Basic dXNlcjpwdw==", "authorization: " + Redacted},
		{"cookie header", "Cookie: a=1; b=2", "Cookie: " + Redacted},
		{"key=value", "token=abc&x=1", "token=" + Redacted + "&x=1"},
		{"key = quoted value", `password = "p w"`, "password = " + Redacted},
		{"authorization=Basic x", "authorization=Basic abc==", "authorization=" + Redacted},
		{"json member", `{"api_key":"k","n":1}`, `{"api_key":"` + Redacted + `","n":1}`},
		{"url userinfo", "postgres://u:pw@h:5432/db", "postgres://" + Redacted + "@h:5432/db"},
		{"jwt", "t=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln", "t=" + Redacted},
		{"truncated pem", "-----BEGIN RSA PRIVATE KEY-----\nMIIB", Redacted},
		{"plain text untouched", "listening on 127.0.0.1:7470 after 3 retries", "listening on 127.0.0.1:7470 after 3 retries"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactString(tc.in)
			if got != tc.want {
				t.Errorf("RedactString(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if again := RedactString(got); again != got {
				t.Errorf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestSensitiveKey(t *testing.T) {
	r := NewRedactor([]string{"Card-Number"})
	for _, k := range []string{
		"token", "Token", "access_token", "refreshToken", "secret", "client_secret", "password", "passwd",
		"authorization", "Proxy-Authorization", "api_key", "apiKey", "API-KEY", "x-api-key", "cookie", "Set-Cookie",
		"private_key", "credentials", "card_number", "CARDNUMBER",
	} {
		if !r.SensitiveKey(k) {
			t.Errorf("SensitiveKey(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"", "user_id", "name", "status", "path", "cardnumber_note", "number"} {
		if r.SensitiveKey(k) {
			t.Errorf("SensitiveKey(%q) = true, want false", k)
		}
	}
	if strings.Contains(RedactString("api key rotation scheduled"), Redacted) {
		t.Errorf("free text without a key=value pair must not be redacted")
	}
}
