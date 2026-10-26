package polly

import (
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Known-answer vectors from the AWS Signature Version 4 test suite
// (credentials AKIDEXAMPLE / wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY,
// date 20150830T123600Z, region us-east-1, service "service").
//
// Sources checked 2026-10-26:
//   https://github.com/mhart/aws4/tree/master/test/aws-sig-v4-test-suite
//   (mirror of docs.aws.amazon.com/general/latest/gr/signature-v4-test-suite.html)
//   https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html
// Signing-key known answer (iam example, date 20120215):
//   AWS "Examples of deriving a signing key" (SigV4 derive-signing-key docs).

const (
	suiteAccess = "AKIDEXAMPLE"
	suiteSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	suiteDate   = "20150830T123600Z"
)

func suiteCreds() awsCreds {
	return awsCreds{AccessKey: suiteAccess, SecretKey: suiteSecret}
}

func suiteOpts() signOpts {
	return signOpts{
		Service: "service", Region: "us-east-1", Creds: suiteCreds(),
		Now: time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC),
	}
}

func TestDeriveSigningKeyIAMExample(t *testing.T) {
	// AWS docs: secret wJalr…EXAMPLEKEY, date 20120215, region us-east-1, service iam.
	got := deriveSigningKey(suiteSecret, "20120215", "us-east-1", "iam")
	want := "f4780e2d9f65fa895f9c67b32ce1baf0b0d8a43505a000a1a9e090d414db404d"
	if hex.EncodeToString(got) != want {
		t.Fatalf("signing key = %x, want %s", got, want)
	}
}

func TestSuiteGetVanilla(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Host", "example.amazonaws.com")
	req.Header.Set("X-Amz-Date", suiteDate)
	got := signV4Request(req, nil, suiteOpts())

	wantCreq := "GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	wantSTS := "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\nbb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63"
	wantAuth := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got.CanonicalRequest != wantCreq {
		t.Fatalf("canonical request:\n%s\nwant:\n%s", got.CanonicalRequest, wantCreq)
	}
	if got.StringToSign != wantSTS {
		t.Fatalf("string to sign:\n%s\nwant:\n%s", got.StringToSign, wantSTS)
	}
	if got.Signature != "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31" {
		t.Fatalf("signature = %s", got.Signature)
	}
	if got.Authorization != wantAuth {
		t.Fatalf("auth = %q", got.Authorization)
	}
	assertNoSecretLeak(t, got.Authorization, got.CanonicalRequest, got.StringToSign)
}

func TestSuiteGetVanillaQueryOrderKey(t *testing.T) {
	// get-vanilla-query-order-key: same Param1 key twice; values sorted Value1 then value2.
	req, err := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/?Param1=value2&Param1=Value1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Host", "example.amazonaws.com")
	req.Header.Set("X-Amz-Date", suiteDate)
	got := signV4Request(req, nil, suiteOpts())

	wantCreq := "GET\n/\nParam1=Value1&Param1=value2\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	wantSTS := "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\n704b4cef673542d84cdff252633f065e8daeba5f168b77116f8b1bcaf3d38f89"
	wantSig := "eedbc4e291e521cf13422ffca22be7d2eb8146eecf653089df300a15b2382bd1"
	if got.CanonicalRequest != wantCreq {
		t.Fatalf("canonical request:\n%s\nwant:\n%s", got.CanonicalRequest, wantCreq)
	}
	if got.StringToSign != wantSTS {
		t.Fatalf("string to sign:\n%s\nwant:\n%s", got.StringToSign, wantSTS)
	}
	if got.Signature != wantSig {
		t.Fatalf("signature = %s, want %s", got.Signature, wantSig)
	}
	assertNoSecretLeak(t, got.Authorization)
}

func TestSuitePostVanilla(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Host", "example.amazonaws.com")
	req.Header.Set("X-Amz-Date", suiteDate)
	got := signV4Request(req, nil, suiteOpts())

	wantCreq := "POST\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	wantSTS := "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\n553f88c9e4d10fc9e109e2aeb65f030801b70c2f6468faca261d401ae622fc87"
	wantSig := "5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b"
	if got.CanonicalRequest != wantCreq {
		t.Fatalf("canonical request:\n%s\nwant:\n%s", got.CanonicalRequest, wantCreq)
	}
	if got.StringToSign != wantSTS {
		t.Fatalf("string to sign:\n%s\nwant:\n%s", got.StringToSign, wantSTS)
	}
	if got.Signature != wantSig {
		t.Fatalf("signature = %s, want %s", got.Signature, wantSig)
	}
	assertNoSecretLeak(t, got.Authorization)
}

func TestSuitePostXWWWFormURLEncoded(t *testing.T) {
	body := []byte("Param1=value1")
	req, err := http.NewRequest(http.MethodPost, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Host", "example.amazonaws.com")
	req.Header.Set("X-Amz-Date", suiteDate)
	got := signV4Request(req, body, suiteOpts())

	wantCreq := "POST\n/\n\ncontent-type:application/x-www-form-urlencoded\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\ncontent-type;host;x-amz-date\n9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e"
	wantSTS := "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\n42a5e5bb34198acb3e84da4f085bb7927f2bc277ca766e6d19c73c2154021281"
	wantSig := "ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a"
	if got.CanonicalRequest != wantCreq {
		t.Fatalf("canonical request:\n%s\nwant:\n%s", got.CanonicalRequest, wantCreq)
	}
	if got.StringToSign != wantSTS {
		t.Fatalf("string to sign:\n%s\nwant:\n%s", got.StringToSign, wantSTS)
	}
	if got.Signature != wantSig {
		t.Fatalf("signature = %s, want %s", got.Signature, wantSig)
	}
	assertNoSecretLeak(t, got.Authorization)
}

func TestPollyJSONBodySignsPayloadHash(t *testing.T) {
	body := []byte(`{"OutputFormat":"pcm","Text":"Hi","VoiceId":"Joanna"}`)
	req, err := http.NewRequest(http.MethodPost, "https://polly.us-east-1.amazonaws.com/v1/speech", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	got := signV4Request(req, body, signOpts{
		Service: awsService, Region: "us-east-1", Creds: suiteCreds(), Now: now, PayloadHeader: true,
	})
	payloadHash := sha256Hex(body)
	if req.Header.Get("X-Amz-Content-Sha256") != payloadHash {
		t.Fatalf("payload header = %q, want %s", req.Header.Get("X-Amz-Content-Sha256"), payloadHash)
	}
	if !strings.Contains(got.CanonicalRequest, "x-amz-content-sha256:"+payloadHash) {
		t.Fatalf("canonical missing payload hash:\n%s", got.CanonicalRequest)
	}
	if !strings.Contains(got.Authorization, "SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date") {
		t.Fatalf("auth = %q", got.Authorization)
	}
	if !strings.HasSuffix(got.CanonicalRequest, "\n"+payloadHash) {
		t.Fatalf("canonical payload line wrong:\n%s", got.CanonicalRequest)
	}
	assertNoSecretLeak(t, got.Authorization, got.CanonicalRequest, got.StringToSign)
}

func TestSessionTokenHeader(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://polly.us-east-1.amazonaws.com/v1/speech", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	creds := suiteCreds()
	creds.SessionToken = "session-token-fixture"
	got := signV4Request(req, []byte(`{}`), signOpts{
		Service: awsService, Region: "us-east-1", Creds: creds,
		Now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), PayloadHeader: true,
	})
	if req.Header.Get("X-Amz-Security-Token") != "session-token-fixture" {
		t.Fatalf("token header = %q", req.Header.Get("X-Amz-Security-Token"))
	}
	if !strings.Contains(got.CanonicalRequest, "x-amz-security-token:session-token-fixture\n") {
		t.Fatalf("canonical missing token:\n%s", got.CanonicalRequest)
	}
	if strings.Contains(got.Authorization, suiteSecret) || strings.Contains(got.Authorization, "session-token-fixture") {
		// Session token is in a header, not the Authorization credential string.
		if strings.Contains(got.Authorization, suiteSecret) {
			t.Fatal("secret in Authorization")
		}
	}
}

func TestParseAWSCredsFormats(t *testing.T) {
	c, err := parseAWSCreds("AKID:secret")
	if err != nil || c.AccessKey != "AKID" || c.SecretKey != "secret" || c.SessionToken != "" {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = parseAWSCreds("AKID:secret:tok")
	if err != nil || c.SessionToken != "tok" {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{"", "nocolon", ":secret", "ak:", "a:b:c:d", "ak with space:sec"} {
		_, err := parseAWSCreds(bad)
		if err == nil || err.Error() != "malformed credential" {
			t.Fatalf("%q -> %v", bad, err)
		}
	}
}

func assertNoSecretLeak(t *testing.T, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if strings.Contains(p, suiteSecret) {
			t.Fatalf("secret leaked into %q", p)
		}
	}
}
