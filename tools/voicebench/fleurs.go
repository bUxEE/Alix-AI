package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// FLEURS (google/fleurs, CC-BY-4.0): the human read speech that gates STT quality (protocol.md). The dataset revision is pinned in the
// URLs; the audio is downloaded to a gitignored directory and pinned by sha256 in the manifest, which is what is committed.
const (
	fleursRevision    = "70bb2e84b976b7e960aa89f1c648e09c59f894dd"
	fleursSplit       = "test"
	fleursLicence     = "CC-BY-4.0"
	fleursLicenceURL  = "https://creativecommons.org/licenses/by/4.0/"
	fleursAttribution = "Conneau et al., FLEURS: Few-shot Learning Evaluation of Universal Representations of Speech, 2022 (google/fleurs on the Hugging Face Hub)"
	fleursSelection   = "the first N clips, in the order they appear in test.tar.gz, that have a row in test.tsv; the reference is the transcription column"
)

// fleursLangs maps the benchmark language to the FLEURS configuration.
var fleursLangs = []struct{ Lang, Config string }{{"en", "en_us"}, {"it", "it_it"}}

// FleursClip is one pinned clip.
type FleursClip struct {
	ID         string  `json:"id"`
	Lang       string  `json:"lang"`
	File       string  `json:"file"`
	SHA256     string  `json:"sha256"`
	SizeBytes  int64   `json:"size_bytes"`
	DurationMS float64 `json:"duration_ms"`
	SentenceID string  `json:"sentence_id"`
	RefRaw     string  `json:"reference_raw"`
	Ref        string  `json:"reference"`
}

// FleursManifest pins the clips used by a run.
type FleursManifest struct {
	Dataset     string       `json:"dataset"`
	Revision    string       `json:"revision"`
	Split       string       `json:"split"`
	Licence     string       `json:"licence"`
	LicenceURL  string       `json:"licence_url"`
	Attribution string       `json:"attribution"`
	Selection   string       `json:"selection"`
	Clips       []FleursClip `json:"clips"`
}

func fleursURL(rel string) string {
	return "https://huggingface.co/datasets/google/fleurs/resolve/" + fleursRevision + "/" + rel
}

func readFleursManifest(p string) (FleursManifest, error) {
	var m FleursManifest
	b, err := os.ReadFile(p)
	if err != nil {
		return m, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("%s: %w", p, err)
	}
	if m.Revision != fleursRevision {
		return m, fmt.Errorf("%s pins revision %s, not %s", p, m.Revision, fleursRevision)
	}
	return m, nil
}

type tsvRow struct{ sentenceID, raw, ref string }

// fleursTSV reads test.tsv: columns id, file name, raw transcription, transcription, phonemes, samples, gender (no header).
func fleursTSV(ctx context.Context, cfg string) (map[string]tsvRow, error) {
	body, err := httpGet(ctx, fleursURL("data/"+cfg+"/"+fleursSplit+".tsv"))
	if err != nil {
		return nil, err
	}
	defer body.Close()
	rows := map[string]tsvRow{}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) < 4 {
			continue
		}
		if _, dup := rows[f[1]]; !dup {
			rows[f[1]] = tsvRow{sentenceID: f[0], raw: f[2], ref: f[3]}
		}
	}
	return rows, sc.Err()
}

func httpGet(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// fetchFleurs streams the test archive of one configuration, keeps the first n clips that have a transcript row, and stops reading.
func fetchFleurs(ctx context.Context, lang, cfg string, n int, outDir string) ([]FleursClip, error) {
	rows, err := fleursTSV(ctx, cfg)
	if err != nil {
		return nil, err
	}
	body, err := httpGet(ctx, fleursURL("data/"+cfg+"/audio/"+fleursSplit+".tar.gz"))
	if err != nil {
		return nil, err
	}
	defer body.Close() // closing early abandons the rest of the archive
	gz, err := gzip.NewReader(body)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(outDir, lang)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var clips []FleursClip
	tr := tar.NewReader(gz)
	for len(clips) < n {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		base := path.Base(h.Name)
		row, ok := rows[base]
		if h.Typeflag != tar.TypeReg || !strings.HasSuffix(base, ".wav") || !ok {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, 16<<20))
		if err != nil {
			return nil, err
		}
		pcm, rate, err := ParseWAV(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", base, err)
		}
		if err := os.WriteFile(filepath.Join(dir, base), data, 0o644); err != nil {
			return nil, err
		}
		clips = append(clips, FleursClip{
			ID: strings.TrimSuffix(base, ".wav"), Lang: lang, File: lang + "/" + base, SHA256: sha256Hex(data), SizeBytes: int64(len(data)),
			DurationMS: pcmMS(pcm, rate), SentenceID: row.sentenceID, RefRaw: row.raw, Ref: row.ref,
		})
	}
	if len(clips) < n {
		return nil, fmt.Errorf("%s: the archive held only %d usable clips, wanted %d", cfg, len(clips), n)
	}
	return clips, nil
}

func fetchFleursCommand(args []string) error {
	fs := flag.NewFlagSet("fetch-fleurs", flag.ContinueOnError)
	out := fs.String("out", "benchmarks/tmp/voice/fleurs", "directory for the clips (gitignored; never committed)")
	manifest := fs.String("manifest", "docs/evidence/m4-voice/fleurs-manifest.json", "manifest to write (committed)")
	n := fs.Int("n", 40, "clips per language")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	m := FleursManifest{
		Dataset: "google/fleurs", Revision: fleursRevision, Split: fleursSplit, Licence: fleursLicence, LicenceURL: fleursLicenceURL,
		Attribution: fleursAttribution, Selection: strings.Replace(fleursSelection, "first N", fmt.Sprintf("first %d", *n), 1),
	}
	for _, l := range fleursLangs {
		clips, err := fetchFleurs(ctx, l.Lang, l.Config, *n, *out)
		if err != nil {
			return err
		}
		marks := 0
		for _, c := range clips {
			for _, r := range c.Ref {
				if unicode.IsMark(r) {
					marks++
				}
			}
		}
		fmt.Fprintf(os.Stderr, "%s: %d clips, %d combining marks in the references\n", l.Lang, len(clips), marks)
		m.Clips = append(m.Clips, clips...)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*manifest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(*manifest, append(b, '\n'), 0o644)
}
