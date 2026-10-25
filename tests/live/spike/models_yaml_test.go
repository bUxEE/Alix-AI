package spike

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/models/registry"
)

// The spike's LocalAI model definitions (localai/models.yaml) pin a URL and a sha256 for every file they download. For the
// registry's models those pins must be the registry's: the registry is the authority, and a hash that drifted in this file
// would make LocalAI (and the fetch script) accept bytes the registry does not.
func TestSpikeModelsYAMLMatchesRegistry(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("localai", "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var defs []struct {
		Name          string `yaml:"name"`
		DownloadFiles []struct {
			Filename string `yaml:"filename"`
			URI      string `yaml:"uri"`
			SHA256   string `yaml:"sha256"`
		} `yaml:"download_files"`
	}
	if err := yaml.Unmarshal(raw, &defs); err != nil {
		t.Fatal(err)
	}
	reg, err := models.LoadFS(registry.FS)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, d := range defs {
		entry, ok := reg.Get(d.Name)
		if !ok {
			continue // alixai-smoke, the Q8 comparison file and the failure fixtures are not registry ids
		}
		if len(d.DownloadFiles) != 1 {
			t.Errorf("%s: want exactly one download file, have %d", d.Name, len(d.DownloadFiles))
			continue
		}
		f := d.DownloadFiles[0]
		a := entry.Artifact
		if a.Filename == nil || a.SHA256 == nil || a.Revision == nil || a.RepoURL == nil {
			t.Errorf("%s: the registry entry does not pin the artefact completely", d.Name)
			continue
		}
		if f.Filename != *a.Filename || f.SHA256 != *a.SHA256 {
			t.Errorf("%s: models.yaml pins %s / %s, the registry pins %s / %s", d.Name, f.Filename, f.SHA256, *a.Filename, *a.SHA256)
		}
		if want := *a.RepoURL + "/resolve/" + *a.Revision + "/" + *a.Filename; f.URI != want {
			t.Errorf("%s: models.yaml fetches %s, the registry says %s", d.Name, f.URI, want)
		}
		checked++
	}
	if checked < 5 {
		t.Fatalf("only %d registry-backed definitions were compared; the file does not hold the candidates", checked)
	}
}
