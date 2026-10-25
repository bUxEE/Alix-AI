package apppkg

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// FuzzPackageLoad (SEC-057, SEC-059): the loader must neither panic nor hang
// on any input, and must never hand out a package that has a critical finding
// or whose signature was not verified.
//
// The first byte of the input selects what the rest is:
//
//	0       the rest is a raw archive (.tar.gz) read through FromArchive: the
//	        gzip, tar, path and limit layers;
//	n > 0   the rest REPLACES one file of the valid package, which is then
//	        signed again: the signature verifies, so the YAML reader, the
//	        schemas and every cross-check run on arbitrary content.
//
// M5-08 runs it with -fuzz=FuzzPackageLoad ./internal/apppkg/.
// SEC-057, SEC-059
func FuzzPackageLoad(f *testing.F) {
	files := valid(f)
	names := sortedNames(files)

	f.Add(append([]byte{0}, tarGz(f, files.signed(f))...))
	f.Add(append([]byte{0}, gz(f, rawTar(f, tf("../evil", "x"), tf("/abs", "x"), tarEntry{name: "l", typ: 50, link: "/etc/passwd"}))...))
	f.Add([]byte{0})
	f.Add([]byte{0, 0x1f, 0x8b, 8, 0})
	f.Add([]byte("\x00not an archive"))
	for i, n := range names {
		f.Add(append([]byte{byte(i + 1)}, files[n]...))
	}
	f.Add(append([]byte{1}, "a: &x [1]\nb: *x\n"...))
	f.Add(append([]byte{1}, "{}"...))
	f.Add(append([]byte{2}, strings.Repeat("- ", 5000)...))
	f.Add(append([]byte{3}, "\xef\xbb\xbf\r\n\x00"...))

	lim := Limits{
		MaxArchiveBytes: 1 << 20,
		MaxTotalBytes:   4 << 20,
		MaxFileBytes:    1 << 20,
		MaxFiles:        200,
		MaxYAMLBytes:    1 << 20,
		MaxYAMLNodes:    50000,
		MaxYAMLDepth:    32,
		MaxFindings:     200,
	}
	opts := loadOptions()
	opts.Limits = lim

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 || len(data) > 2<<20 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		var src Source
		if data[0] == 0 {
			src = FromArchive(bytes.NewReader(data[1:]))
		} else {
			p := files.clone()
			p[names[int(data[0]-1)%len(names)]] = bytes.Clone(data[1:])
			src = FromFS(p.signed(t))
		}
		res, err := Load(ctx, src, opts)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatalf("Load did not finish in time: %v", err)
			}
			t.Fatalf("Load returned an error for hostile input (defects are findings, not errors): %v", err)
		}
		if res == nil || res.Findings == nil {
			t.Fatal("Load returned no result or nil findings")
		}
		for _, f := range res.Findings {
			if err := f.Validate(); err != nil {
				t.Fatalf("incomplete finding: %v", err)
			}
			if strings.ContainsAny(f.Message+f.Path+f.Capability, "\x00\r\x1b") {
				t.Fatalf("a finding carries a control character: %q", f.Message)
			}
		}
		if !sorted(res.Findings) {
			t.Fatal("findings are out of order")
		}
		if res.Package != nil {
			if contracts.HasCritical(res.Findings) || res.Authenticity != AuthVerified || res.ManifestHash == "" {
				t.Fatalf("a package was returned although critical=%v authenticity=%q", contracts.HasCritical(res.Findings), res.Authenticity)
			}
		}
		if len(res.Findings) > lim.MaxFindings+1 {
			t.Fatalf("%d findings exceed the bound", len(res.Findings))
		}
	})
}

func sorted(fs []contracts.Finding) bool { return sortedFindings(fs) }

func sortedNames(files pkgFiles) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
