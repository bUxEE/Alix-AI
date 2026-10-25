package secretssec

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/tests/security/secutil"
)

const (
	// maxScanBytes is the largest file the scan reads. Anything bigger is a data
	// file (a lock file, a corpus), counted and reported, never silently dropped.
	maxScanBytes = 4 << 20
	// minScanFiles is the least number of files a scan must read to count. The
	// repository holds thousands; a scan that read a handful looked at the wrong
	// tree.
	minScanFiles = 200
)

// scanResult is what a repository scan covered and found.
type scanResult struct {
	Scanned      int
	Bytes        int64
	SkippedBin   int
	SkippedLarge []string
	Findings     []finding
}

// scanFiles reads the files (slash paths relative to root) with the detectors, on all
// CPUs. Read errors are findings of the scan itself, not of the repository: they fail the test.
func scanFiles(t testing.TB, root string, files []string) scanResult {
	t.Helper()
	var (
		mu  sync.Mutex
		res scanResult
		wg  sync.WaitGroup
	)
	work := make(chan string)
	for range max(1, runtime.NumCPU()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rel := range work {
				full := filepath.Join(root, filepath.FromSlash(rel))
				info, err := os.Stat(full)
				if err != nil {
					t.Errorf("stat %s: %v", rel, err)
					continue
				}
				if info.Size() > maxScanBytes {
					mu.Lock()
					res.SkippedLarge = append(res.SkippedLarge, rel)
					mu.Unlock()
					continue
				}
				data, err := os.ReadFile(full)
				if err != nil {
					t.Errorf("read %s: %v", rel, err)
					continue
				}
				if !mayHoldSecret(data) {
					mu.Lock()
					res.SkippedBin++
					mu.Unlock()
					continue
				}
				found := scanContent(rel, data)
				mu.Lock()
				res.Scanned++
				res.Bytes += int64(len(data))
				res.Findings = append(res.Findings, found...)
				mu.Unlock()
			}
		}()
	}
	for _, rel := range files {
		work <- rel
	}
	close(work)
	wg.Wait()
	sortFindings(res.Findings)
	return res
}

// trackedAmong returns the candidates git tracks. When git cannot answer (a source
// archive, no git binary) every candidate counts as tracked: the check fails closed.
func trackedAmong(t testing.TB, root string, candidates []string) []string {
	t.Helper()
	if len(candidates) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root, "ls-files", "-z", "--"}, candidates...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return candidates
	}
	var tracked []string
	for _, p := range strings.Split(out.String(), "\x00") {
		if p != "" {
			tracked = append(tracked, p)
		}
	}
	return tracked
}

// scanEnvFiles reports every environment file (other than a *.env.example) among files
// that is tracked.
func scanEnvFiles(t testing.TB, root string, files []string) []finding {
	t.Helper()
	var cands []string
	for _, rel := range files {
		if isEnvFileName(rel) {
			cands = append(cands, rel)
		}
	}
	var out []finding
	for _, rel := range trackedAmong(t, root, cands) {
		out = append(out, finding{Path: rel, Line: 1, Detector: detEnvFile, Excerpt: "an environment file is tracked"})
	}
	return out
}

// ---- planted violations, built at run time ----------------------------------------------

// The planted secrets are assembled from pieces and random bytes so that this file holds no
// secret-shaped literal: the scan of the repository reads it, and must not flag it.

// randString returns n random characters of the alphabet.
func randString(t testing.TB, alphabet string, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	out := make([]byte, n)
	for i := range b {
		out[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(out)
}

const (
	alnum     = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	upperNum  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b64Chars  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	urlChars  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	hexLower  = "0123456789abcdef"
	secretLen = 32
)

// mixed returns a random value with letters and digits and enough entropy to pass any
// threshold, drawing again until it does.
func mixed(t testing.TB, alphabet string, n int) string {
	t.Helper()
	for range 50 {
		v := randString(t, alphabet, n)
		if entropy(v) >= 4.0 && secretLike(v, 3.5, true) {
			return v
		}
	}
	t.Fatalf("no random value of %d characters passed the entropy check", n)
	return ""
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// planted is one violation: where it appears (a file name decides the env-file rule),
// what the file says, and the detector that must flag it.
type planted struct {
	name     string
	path     string
	content  string
	detector string
}

// pem returns a private key block of the given kind with a random body.
func pem(t testing.TB, kind string) string {
	t.Helper()
	body := make([]byte, 48)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	enc := base64.StdEncoding.EncodeToString(body)
	return "-----" + "BEGIN " + kind + "-----\n" + enc + "\n-----" + "END " + kind + "-----\n"
}

// jwt returns a JWT-shaped string: header and payload are base64url JSON, the signature is random.
func jwt(t testing.TB) string {
	t.Helper()
	return b64url([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." +
		b64url([]byte(`{"sub":"`+randString(t, alnum, 12)+`","iat":1700000000}`)) + "." + randString(t, urlChars, 43)
}

// plants returns the planted violations of one run: every detector is represented, in the
// shapes a leak takes in a repository (a Go string, YAML, a shell line, an env file).
func plants(t testing.TB) []planted {
	t.Helper()
	key := pem(t, "PRIVATE KEY")
	rsa := pem(t, "RSA PRIVATE KEY")
	ssh := pem(t, "OPENSSH PRIVATE KEY")
	escaped := strings.ReplaceAll(strings.TrimSpace(pem(t, "EC PRIVATE KEY")), "\n", `\n`)
	token := jwt(t)
	return []planted{
		{"PKCS#8 private key in a key file", "deploy/keys/service.pem", key, detPrivateKey},
		{"RSA private key", "docs/howto/signing.md", "Use this key:\n\n" + rsa, detPrivateKey},
		{"OpenSSH private key", "deploy/ssh/id_deploy", ssh, detPrivateKey},
		{"private key kept in a Go string", "internal/x/keys.go", "package x\n\nconst key = \"" + escaped + "\"\n", detPrivateKey},
		{"AWS access key id", "deploy/terraform/vars.tf", "access_key = \"" + "AK" + "IA" + randString(t, upperNum, 16) + "\"\n", detAWSKey},
		{"AWS session key id", "scripts/upload.sh", "export AWS_KEY=" + "AS" + "IA" + randString(t, upperNum, 16) + "\n", detAWSKey},
		{"GitHub personal access token", ".github/workflows/release.yml", "      GH: " + "gh" + "p_" + randString(t, alnum, 36) + "\n", detGitHub},
		{"GitHub fine-grained token", "scripts/publish.sh", "TOKEN=" + "github" + "_pat_" + randString(t, alnum, 40) + "\n", detGitHub},
		{"api key assigned in Go", "internal/x/client.go", "const api" + "Key = \"" + mixed(t, alnum, secretLen) + "\"\n", detGeneric},
		{"password in YAML", "deploy/compose/app.yml", "database:\n  password: " + mixed(t, alnum, 24) + "\n", detGeneric},
		{"client secret in JSON", "config/auth.json", "{\"client_secret\": \"" + mixed(t, urlChars, 28) + "\"}\n", detGeneric},
		{"token in a shell script", "scripts/call.sh", "ADMIN_TOKEN=" + mixed(t, alnum, 30) + "\n", detGeneric},
		{"JWT in a fixture", "examples/fixtures/session.json", "{\"id_token\": \"" + token + "\"}\n", detJWT},
		{"JWT in source", "internal/x/jwt.go", "var sample = \"" + token + "\"\n", detJWT},
		{"long hex under a secret key", "deploy/app.yaml", "session_secret: " + randString(t, hexLower, 64) + "\n", detBlob},
		{"long base64 under an encryption key", "config/enc.yaml", "encryption_key: " + randString(t, b64Chars[:62], 48) + "=\n", detBlob},
		{"bearer literal in a curl line", "docs/howto/api.md", "curl -H \"Authorization: Bearer " + mixed(t, alnum, 40) + "\" http://localhost\n", detBearer},
		{"bearer literal in a header map", "internal/x/call.go", "h := map[string]string{\"Authorization\": \"Bearer " + mixed(t, urlChars, 36) + "\"}\n", detBearer},
	}
}

// cleanSamples are inline texts that look like secrets without being one: no detector may
// fire on any of them.
func cleanSamples(t testing.TB) map[string]string {
	t.Helper()
	pemKey := pem(t, "PRIVATE KEY")
	tok := jwt(t)
	return map[string]string{
		"placeholder password":        "password: change-me-please-change-me-now\n",
		"template variable":           "token: ${ALIXAI_ADMIN_TOKEN_VALUE_FROM_VAULT}\n",
		"secret reference":            "api_key_ref: env:ALIXAI_LOCALAI_API_KEY_VALUE\n",
		"angle placeholder":           "secret: <paste-the-secret-value-here-1234>\n",
		"your-token placeholder":      "token = \"your-api-token-goes-here-0123456789\"\n",
		"example value":               "client_secret: example-client-secret-0123456789\n",
		"zero entropy":                "password = \"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"\n",
		"identifier":                  "secret: secretStoreClientImplementationName\n",
		"code expression":             "token := parseTokenFromAuthorizationHeader(request.Header.Get(\"Authorization\"))\n",
		"url":                         "token_endpoint: https://idp.example.org/oauth2/v1/token/endpoint-0123\n",
		"file path":                   "token_file: /run/secrets/alixai/admin-token-0123456789\n",
		"sha256 under a plain key":    "digest: " + randString(t, hexLower, 64) + "\n",
		"integrity hash":              "integrity: sha512-" + randString(t, b64Chars[:62], 86) + "==\n",
		"aws documentation example":   "key = AKIAIOSFODNN7EXAMPLE\n",
		"short token":                 "token: abc123\n",
		"bearer placeholder":          "curl -H \"Authorization: Bearer ${TOKEN}\" http://localhost\n",
		"bearer with no value":        "Authorization: Bearer <token>\n",
		"header concatenation":        "req.Header.Set(\"Authorization\", \"Bearer \" + tok)\n",
		"marker without a key body":   "check that the file starts with -----" + "BEGIN PRIVATE KEY-----\n",
		"truncated key fixture":       "-----" + "BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0B\n-----" + "END PRIVATE KEY-----\n",
		"test-only key, marker above": "TEST-ONLY KEY. NOT A SECRET.\n\n" + pemKey,
		"test-only jwt, marker above": "// TEST-ONLY: a published sample, signs nothing\nvar sample = \"" + tok + "\"\n",
		"env example":                 "ALIXAI_TOKEN=\n",
	}
}

// TestSecSecretDetectorsFlagPlantedSecretsAndOnlyThose is the control of the repository scan: every
// detector fires on a planted secret of its kind, none fires on clean text that merely looks like
// one, and the TEST-ONLY marker clears a key only when it is in the content next to it.
//
// SEC-084
func TestSecSecretDetectorsFlagPlantedSecretsAndOnlyThose(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range plants(t) {
		got := scanContent(p.path, []byte(p.content))
		hit := false
		for _, f := range got {
			if f.Detector == p.detector {
				hit = true
			}
			if strings.Contains(f.String(), p.content) {
				t.Errorf("%s: the finding prints the whole secret: %s", p.name, f)
			}
		}
		if !hit {
			t.Errorf("%s: the %s detector did not fire (findings: %v)", p.name, p.detector, got)
		}
		seen[p.detector] = true
	}
	for _, det := range []string{detPrivateKey, detAWSKey, detGitHub, detGeneric, detJWT, detBlob, detBearer} {
		if !seen[det] {
			t.Errorf("detector %s has no planted violation", det)
		}
	}

	for name, text := range cleanSamples(t) {
		if got := scanContent("clean/"+name+".txt", []byte(text)); len(got) != 0 {
			t.Errorf("clean sample %q is flagged: %v\n%s", name, got, text)
		}
	}

	t.Run("environment files", func(t *testing.T) {
		for _, name := range []string{".env", "deploy/prod.env", "deploy/.env.production", "compose/app.env", ".env.local"} {
			if !isEnvFileName(name) {
				t.Errorf("%s is not recognised as an environment file", name)
			}
		}
		for _, name := range []string{"deploy/app.env.example", ".env.example", "docs/environment.md", "internal/config/env.go", "deploy/envoy.yaml"} {
			if isEnvFileName(name) {
				t.Errorf("%s is wrongly recognised as an environment file", name)
			}
		}
		// A tracked one is found through git itself: plant one in a throwaway repository.
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git is not installed: the tracked-file check is covered by the fail-closed branch below")
		}
		dir := t.TempDir()
		gitIn := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		gitIn("init", "-q")
		for _, f := range []string{"prod.env", "app.env.example", "local.env"} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("A=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		gitIn("add", "prod.env", "app.env.example") // local.env stays untracked
		got := scanEnvFiles(t, dir, []string{"prod.env", "app.env.example", "local.env"})
		if len(got) != 1 || got[0].Path != "prod.env" || got[0].Detector != detEnvFile {
			t.Errorf("tracked env files = %v, want exactly prod.env", got)
		}
		// Where git cannot answer, every candidate counts: fail closed.
		if got := trackedAmong(t, filepath.Join(dir, "no-such-dir"), []string{"x.env"}); len(got) != 1 {
			t.Errorf("without git's answer the candidate is not treated as tracked: %v", got)
		}
	})

	t.Run("the TEST-ONLY marker must be in the content, near the key", func(t *testing.T) {
		key := pem(t, "PRIVATE KEY")
		if got := scanContent("keys/a.pem", []byte(key)); len(got) != 1 {
			t.Fatalf("an unmarked key = %v, want one finding", got)
		}
		if got := scanContent("keys/a.pem", []byte("TEST-ONLY KEY. NOT A SECRET.\n"+key)); len(got) != 0 {
			t.Errorf("a key under its marker is flagged: %v", got)
		}
		// The marker far from the key (more than the window below it, and not at the head of the
		// file) does not clear it: a marker somewhere in a large file vouches for nothing.
		far := strings.Repeat("// filler\n", 20)
		if got := scanContent("keys/b.go", []byte(far+"// TEST-ONLY somewhere else\n"+far+key)); len(got) != 1 {
			t.Errorf("a distant marker cleared a key: %v", got)
		}
		// A path called testdata or "test" clears nothing.
		if got := scanContent("testdata/test/keys/a.pem", []byte(key)); len(got) != 1 {
			t.Errorf("a path name cleared a key: %v", got)
		}
		tok := jwt(t)
		if got := scanContent("x/testdata/t.json", []byte("{\"t\":\""+tok+"\"}")); len(got) != 1 {
			t.Errorf("an unmarked JWT under testdata = %v, want one finding", got)
		}
	})
}

// TestSecSecretScanSeesTheRealTestKeyOnlyThroughItsMarker runs the key detector on the repository's one
// committed private key, the published test key, as it is and with its marker removed in memory: the
// first passes because the file says it is TEST-ONLY, the second is found. It proves the scan reads real
// repository content, and that the marker is what clears the key.
//
// SEC-084
func TestSecSecretScanSeesTheRealTestKeyOnlyThroughItsMarker(t *testing.T) {
	const rel = "tools/devkeys/test-only-publisher.key.pem"
	raw := secutil.ReadFile(t, rel)
	if got := scanContent(rel, raw); len(got) != 0 {
		t.Errorf("the published test key is flagged although it carries its marker: %v", got)
	}
	stripped := bytes.ReplaceAll(raw, []byte(testOnlyMarker), []byte("PUBLISHED"))
	got := scanContent(rel, stripped)
	if len(got) != 1 || got[0].Detector != detPrivateKey {
		t.Fatalf("the published key without its marker = %v, want one %s finding", got, detPrivateKey)
	}
}

// TestSecRepoSecretScan scans every text file of the repository for committed secrets: private
// key blocks (unless the content itself says TEST-ONLY), cloud and GitHub tokens, high-entropy
// literals assigned to secret-named keys, long hex or base64 under such keys, JWTs, bearer
// literals and tracked environment files. A finding is reported with its path and line and fails
// the test; nothing is allowlisted by path.
//
// The repository history and built images are scanned by the CI job (gitleaks over the history,
// the image scanner over the images); this test is the tree scan that runs on every commit.
//
// SEC-084
func TestSecRepoSecretScan(t *testing.T) {
	root := secutil.RepoRoot(t)
	files := secutil.RepoFiles(t)

	res := scanFiles(t, root, files)
	res.Findings = append(res.Findings, scanEnvFiles(t, root, files)...)
	sortFindings(res.Findings)

	t.Logf("scanned %d files (%d bytes); skipped %d binary and %d over %d bytes", res.Scanned, res.Bytes, res.SkippedBin, len(res.SkippedLarge), maxScanBytes)
	for _, p := range res.SkippedLarge {
		t.Logf("not scanned (over the size cap): %s", p)
	}
	if res.Scanned < minScanFiles {
		t.Fatalf("the scan read only %d files, want at least %d: it is not looking at the repository", res.Scanned, minScanFiles)
	}
	// The scan really reads what it claims to: well-known files of three kinds are in it.
	scanned := map[string]bool{}
	for _, f := range files {
		scanned[f] = true
	}
	for _, must := range []string{"go.mod", "tools/devkeys/test-only-publisher.key.pem", "internal/log/redact.go"} {
		if !scanned[must] {
			t.Errorf("%s is not among the files the scan covers", must)
		}
	}
	for _, f := range res.Findings {
		t.Errorf("suspected secret: %s", f)
	}
	if len(res.Findings) > 0 {
		t.Errorf("%d suspected secret(s) in the repository; remove them, or, for a key that is published on purpose, say TEST-ONLY next to it in the file", len(res.Findings))
	}
}

// hexOf is used by the log scanner tests.
func hexOf(b []byte) string { return hex.EncodeToString(b) }
