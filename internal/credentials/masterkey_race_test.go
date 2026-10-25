package credentials

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// L4: serve and an operator command can both find no master key and create one.
// Exactly one key may result, and every creator must get that key: a creator
// that went on with its own key would encrypt records nobody can open again.
func TestMasterKeyCreationIsExclusive(t *testing.T) {
	for round := 0; round < 5; round++ {
		path := filepath.Join(t.TempDir(), "keys", "master.key")
		const n = 8
		keys := make([][]byte, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				keys[i], errs[i] = KeyFileSource{Path: path}.MasterKey(bg, true)
			}(i)
		}
		close(start)
		wg.Wait()
		for i := 0; i < n; i++ {
			if errs[i] != nil {
				t.Fatalf("round %d creator %d: %v", round, i, errs[i])
			}
			if !bytes.Equal(keys[i], keys[0]) {
				t.Fatalf("round %d: creators got different master keys", round)
			}
		}
		onDisk, err := KeyFileSource{Path: path}.MasterKey(bg, false)
		if err != nil || !bytes.Equal(onDisk, keys[0]) {
			t.Fatalf("round %d: the key on disk is not the one handed out: %v", round, err)
		}
		if err := checkOwnerOnly(path, false); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		entries, _ := os.ReadDir(filepath.Dir(path))
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".tmp" {
				t.Errorf("round %d: a temporary file was left behind: %s", round, e.Name())
			}
		}
	}
}
