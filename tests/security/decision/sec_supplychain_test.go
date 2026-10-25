package decisionsec

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/decision/lifecycle"
	"github.com/buxee/alix-ai/tests/security/secutil"
)

// SEC-111
//
// CLM-v0.1-8B.pt is a PyTorch checkpoint, and a .pt file is a pickle: loading it can run code. Whether the
// contrastive-lm service loads it with a weights-only loader is NOT verified here (the wheel is not in the
// repository and this suite makes no network call): that part of SEC-111 is a human review, recorded in
// docs/evidence/d-licence-review.md. This test pins what the deployment does about the risk without that
// knowledge: the file is fetched at an exact commit, hashed against the registry before the service starts, mounted
// read-only, and never downloaded by the service itself.
func TestSecDecisionCLMCheckpointIsPickleGuardedByPinsAndReadOnlyMounts(t *testing.T) {
	read := func(rel string) string { return string(secutil.ReadFile(t, rel)) }
	pins := read("deploy/container/clm/config/pins.txt")
	entry := read("deploy/container/clm/entrypoint.sh")
	compose := read("deploy/container/clm/compose.yaml")
	registry := read("models/registry/clm-v0.1-8b.yaml")

	t.Run("the checkpoint and the service wheel are pinned by commit and by sha256, never by a floating reference", func(t *testing.T) {
		hex40, hex64 := regexp.MustCompile(`^[0-9a-f]{40}$`), regexp.MustCompile(`^[0-9a-f]{64}$`)
		found := map[string]bool{}
		for _, line := range strings.Split(pins, "\n") {
			f := strings.Fields(line)
			if len(f) != 6 || strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			role, rev, sum, file, url := f[0], f[2], f[3], f[4], f[5]
			if role != "head" && role != "wheel" && role != "encoder" {
				t.Errorf("unknown pin role %q", role)
			}
			if !hex64.MatchString(sum) {
				t.Errorf("%s: sha256 %q is not 64 lower-case hex", file, sum)
			}
			if role != "wheel" && !hex40.MatchString(rev) {
				t.Errorf("%s: revision %q is not a 40-hex commit", file, rev)
			}
			if !strings.HasPrefix(url, "https://") || strings.Contains(url, "/main/") || strings.Contains(url, "/latest/") {
				t.Errorf("%s: url %q is not an https url at an exact revision", file, url)
			}
			if role != "wheel" && !strings.Contains(url, rev) {
				t.Errorf("%s: url does not name the pinned revision %s", file, rev)
			}
			found[role+":"+file] = true
		}
		if !found["head:CLM_v0.1-8B.pt"] || !found["wheel:contrastive_lm-0.1.0-py3-none-any.whl"] {
			t.Fatalf("the checkpoint or the wheel is not pinned: %v", found)
		}
	})

	t.Run("the entrypoint checks the registry and every hash before it starts the service, and fails closed", func(t *testing.T) {
		body := entry[strings.Index(entry, "  clm)"):]
		order := []string{`check_registry "$CLM_ID" head`, `verify_dir "$CLM_DIR" head wheel`, "exec python3"}
		last := -1
		for _, marker := range order {
			i := strings.Index(body, marker)
			if i < 0 {
				t.Fatalf("the clm branch of entrypoint.sh has no %q", marker)
			}
			if i < last {
				t.Fatalf("%q comes before an earlier check: the service would start before the checkpoint is verified", marker)
			}
			last = i
		}
		for _, must := range []string{"set -euo pipefail", "--no-download", "sha256sum", "holds a symlink; refused", "does not hold exactly the pinned files"} {
			if !strings.Contains(entry, must) {
				t.Errorf("entrypoint.sh lost %q", must)
			}
		}
		for _, mustNot := range []string{"pip install", "pip3 install", "curl ", "wget ", "torch.load", "pickle"} {
			if strings.Contains(entry, mustNot) {
				t.Errorf("entrypoint.sh contains %q", mustNot)
			}
		}
		if strings.Contains(entry, "--ckpt") && !strings.Contains(entry, `--ckpt "$CLM_DIR/CLM_v0.1-8B.pt"`) {
			t.Error("the checkpoint path is not the verified one")
		}
	})

	t.Run("the running service mounts the model files read-only on a read-only root with no capabilities", func(t *testing.T) {
		svc := compose[strings.Index(compose, "clm:"):]
		for _, must := range []string{"/models/clm:ro", "read_only: true", "no-new-privileges:true", "cap_drop:", "- ALL", `ALIXAI_FETCH_MODELS: "false"`, `HF_HUB_OFFLINE: "1"`, "127.0.0.1:"} {
			if !strings.Contains(svc, must) {
				t.Errorf("compose.yaml lost %q for the clm service", must)
			}
		}
		for _, line := range strings.Split(compose, "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "- ") && strings.Contains(l, "/models/") && !strings.HasSuffix(l, ":ro") && !strings.Contains(l, ":ro ") {
				t.Errorf("a model directory is mounted writable in the serving compose file: %s", l)
			}
		}
	})

	t.Run("the registry records the checkpoint as a candidate, hash-pinned, with no profile defaulting to it", func(t *testing.T) {
		for _, must := range []string{"default_for: []", "pickle", "sha256: b2b4a8c9c2d39263eff78a351eb909a342ce9b3bf21a3f07c1d1bf15f1c4eda5"} {
			if !strings.Contains(registry, must) {
				t.Errorf("models/registry/clm-v0.1-8b.yaml lost %q", must)
			}
		}
		pinned := regexp.MustCompile(`b2b4a8c9c2d39263eff78a351eb909a342ce9b3bf21a3f07c1d1bf15f1c4eda5`)
		if !pinned.MatchString(pins) {
			t.Error("the registry hash is not the pins.txt hash")
		}
	})

	t.Run("what the lifecycle hashes is what a pickle needs hashed: size and sha256 of every loaded file", func(t *testing.T) {
		// A guard against a refactor that drops the hash from the manifest entry: the type carries both.
		ty := reflect.TypeOf(lifecycle.Revision{}.Manifest.Files).Elem()
		for _, f := range []string{"Name", "SHA256", "SizeBytes"} {
			if _, ok := ty.FieldByName(f); !ok {
				t.Errorf("the manifest file entry has no %s", f)
			}
		}
	})
}
