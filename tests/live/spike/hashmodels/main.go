// Command hashmodels re-hashes the model files an operator fetched and compares each with the pin in the model
// registry (M0-13: "RE-HASH every downloaded file locally with models.HashFile"). The registry's hashes came from
// the Hugging Face hub's metadata, so this is the first time they are compared with bytes that were actually on a
// disk here. It writes a JSON report for docs/evidence/m0-spike/ and exits 1 on any mismatch or missing file.
//
//	go run ./tests/live/spike/hashmodels -dir deploy/container/localai/data/models \
//	    -out docs/evidence/m0-spike/model-hashes.json \
//	    -also granite-embedding-97M-multilingual-r2-Q8_0.gguf=25155b89...:115061088
//
// -also names a file that is not a registry artefact (the Q8 embedding comparison) with the hash and size it must
// have, as filename=sha256:size.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/models/registry"
)

type extra []string

func (e *extra) String() string     { return strings.Join(*e, ",") }
func (e *extra) Set(v string) error { *e = append(*e, v); return nil }

// row is one hashed file in the report.
type row struct {
	RegistryID     string  `json:"registry_id"`
	Filename       string  `json:"filename"`
	SizeLocal      int64   `json:"size_bytes_local"`
	SizePinned     *int64  `json:"size_bytes_pinned"`
	SHA256Local    string  `json:"sha256_local"`
	SHA256Pinned   *string `json:"sha256_pinned"`
	Match          bool    `json:"match"`
	HashedWith     string  `json:"hashed_with"`
	HashMillis     int64   `json:"hash_ms"`
	NotInRegistry  bool    `json:"not_a_registry_artefact,omitempty"`
	ProblemDetails string  `json:"problem,omitempty"`
}

type report struct {
	Schema    string `json:"schema"`
	HashedAt  string `json:"hashed_at"`
	ModelsDir string `json:"models_dir"`
	Files     []row  `json:"files"`
	AllMatch  bool   `json:"all_match"`
}

var alsoRE = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)=([0-9a-f]{64}):([0-9]+)$`)

func main() {
	dir := flag.String("dir", "", "directory that holds the model files (required)")
	out := flag.String("out", "", "write the JSON report here (default: stdout only)")
	var also extra
	flag.Var(&also, "also", "filename=sha256:size of a file that is not a registry artefact (repeatable)")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "hashmodels: -dir is required")
		os.Exit(2)
	}
	reg, err := models.LoadFS(registry.FS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hashmodels: the shipped registry is invalid: %v\n", err)
		os.Exit(1)
	}

	var rows []row
	hash := func(id, name string, size *int64, sum *string, notInRegistry bool) {
		r := row{RegistryID: id, Filename: name, SizePinned: size, SHA256Pinned: sum, HashedWith: "internal/models.HashFile", NotInRegistry: notInRegistry}
		start := time.Now()
		got, n, err := models.HashFile(context.Background(), filepath.Join(*dir, name))
		r.HashMillis = time.Since(start).Milliseconds()
		switch {
		case err != nil:
			r.ProblemDetails = err.Error()
		default:
			r.SHA256Local, r.SizeLocal = got, n
			r.Match = sum != nil && size != nil && got == *sum && n == *size
			if !r.Match {
				r.ProblemDetails = "hash or size differs from the pin"
			}
		}
		rows = append(rows, r)
	}

	entries := reg.Entries()
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	for _, e := range entries {
		if e.Artifact.Filename == nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(*dir, *e.Artifact.Filename)); err != nil {
			rows = append(rows, row{RegistryID: e.ID, Filename: *e.Artifact.Filename, SHA256Pinned: e.Artifact.SHA256,
				SizePinned: e.Artifact.SizeBytes, ProblemDetails: "file is not in the models directory"})
			continue
		}
		hash(e.ID, *e.Artifact.Filename, e.Artifact.SizeBytes, e.Artifact.SHA256, false)
	}
	for _, a := range also {
		m := alsoRE.FindStringSubmatch(a)
		if m == nil {
			fmt.Fprintf(os.Stderr, "hashmodels: -also %q is not filename=sha256:size\n", a)
			os.Exit(2)
		}
		size, _ := strconv.ParseInt(m[3], 10, 64)
		sum := m[2]
		hash("", m[1], &size, &sum, true)
	}

	rep := report{Schema: "alixai-m0-13-model-hashes-v1", HashedAt: time.Now().UTC().Format(time.RFC3339), ModelsDir: filepath.ToSlash(*dir), Files: rows, AllMatch: true}
	for _, r := range rows {
		if !r.Match {
			rep.AllMatch = false
		}
		state := "MATCH   "
		if !r.Match {
			state = "MISMATCH"
		}
		fmt.Printf("%s %-52s %12d bytes  %s  %s\n", state, r.Filename, r.SizeLocal, r.SHA256Local, r.ProblemDetails)
	}
	if *out != "" {
		data, err := json.MarshalIndent(rep, "", "  ")
		if err == nil {
			err = os.WriteFile(*out, append(data, '\n'), 0o644)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "hashmodels: %v\n", err)
			os.Exit(1)
		}
	}
	if !rep.AllMatch {
		os.Exit(1)
	}
}
