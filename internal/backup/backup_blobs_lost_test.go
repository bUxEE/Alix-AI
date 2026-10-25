package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A blob the data directory had already lost (its row is still live) is no
// compaction race: the backup holds the data directory as it is and says which
// blobs it lacks, instead of refusing every backup from then on.
func TestBackupOfADataDirectoryThatAlreadyLostABlob(t *testing.T) {
	e := newEnv(t)
	kept := e.putFile("fil_01J000000000000000000000AC", strings.Repeat("c", 64))
	lost := e.putFile("fil_01J000000000000000000000AD", strings.Repeat("d", 64))
	if err := os.Remove(filepath.Join(e.dataDir, filepath.FromSlash(strings.TrimPrefix(lost, TreeRoot+"/")))); err != nil {
		t.Fatal(err)
	}

	res := e.backup(t.TempDir())
	if len(res.LostBlobs) != 1 || res.LostBlobs[0] != lost {
		t.Fatalf("LostBlobs = %v, want [%s]", res.LostBlobs, lost)
	}
	listed := map[string]bool{}
	for _, f := range res.Manifest.Signed.Files {
		listed[f.Path] = true
	}
	if !listed[kept] || listed[lost] {
		t.Errorf("the backup lists %v", res.Manifest.Signed.Files)
	}
	if _, err := Verify(ctx, res.Dir, VerifyOptions{Keys: e.keys, Clock: e.clk}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}
