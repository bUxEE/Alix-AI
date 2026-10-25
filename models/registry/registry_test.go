package registry_test

import (
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/models/registry"
)

// TestEmbeddedRegistryMatchesFiles: the registry compiled into the binary is
// valid and is the same set of entries as the files on disk.
func TestEmbeddedRegistryMatchesFiles(t *testing.T) {
	embedded, err := models.LoadFS(registry.FS)
	if err != nil {
		t.Fatalf("embedded registry is invalid: %v", err)
	}
	onDisk, err := models.LoadDir(".")
	if err != nil {
		t.Fatalf("registry directory is invalid: %v", err)
	}
	if embedded.Len() == 0 {
		t.Fatal("embedded registry is empty")
	}
	if diff := cmp.Diff(onDisk.Entries(), embedded.Entries()); diff != "" {
		t.Fatalf("embedded and on-disk entries differ (-disk +embedded):\n%s", diff)
	}
	// Guard against a test that passes because both sides are empty or the
	// files were not embedded: every YAML file on disk is an entry.
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range files {
		if len(f.Name()) > 5 && f.Name()[len(f.Name())-5:] == ".yaml" {
			n++
		}
	}
	if n != embedded.Len() {
		t.Fatalf("%d yaml files on disk, %d embedded entries", n, embedded.Len())
	}
}
