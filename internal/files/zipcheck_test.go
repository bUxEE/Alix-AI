package files

import (
	"bytes"
	"encoding/binary"
	"io"
	"sync/atomic"
	"testing"
)

// countingReaderAt counts the reads made of an archive.
type countingReaderAt struct {
	r     io.ReaderAt
	reads atomic.Int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.reads.Add(1)
	return c.r.ReadAt(p, off)
}

// withDeclaredEntries returns a copy of a ZIP archive whose end of central directory declares n entries.
func withDeclaredEntries(t *testing.T, archive []byte, n uint16) []byte {
	t.Helper()
	out := bytes.Clone(archive)
	i := bytes.LastIndex(out, []byte{0x50, 0x4b, 0x05, 0x06})
	if i < 0 || len(out)-i < 22 {
		t.Fatal("no end of central directory")
	}
	binary.LittleEndian.PutUint16(out[i+8:], n)
	binary.LittleEndian.PutUint16(out[i+10:], n)
	return out
}

// The entry count an OOXML archive declares is checked from its end of central directory record before
// archive/zip reads the directory: an archive that declares more entries than the bound, or a ZIP64 marker count,
// is refused after one read of its tail, and an ordinary document still passes.
func TestOOXMLEntryCountIsCheckedBeforeTheDirectoryIsRead(t *testing.T) {
	spec := typeTable[docxMIME]
	good := docxBytes(t)
	if n, ok := zipDeclaredEntries(bytes.NewReader(good), int64(len(good))); !ok || n != 2 {
		t.Fatalf("declared entries of a two-part document = %d, %v", n, ok)
	}
	if err := verifyContainer(spec, bytes.NewReader(good), int64(len(good))); err != nil {
		t.Fatalf("a valid document was refused: %v", err)
	}
	for name, n := range map[string]uint16{"over the bound": maxZipEntries + 1, "a ZIP64 marker": 0xFFFF} {
		bad := withDeclaredEntries(t, good, n)
		c := &countingReaderAt{r: bytes.NewReader(bad)}
		if err := verifyContainer(spec, c, int64(len(bad))); err == nil {
			t.Errorf("%s: the archive was accepted", name)
		}
		if got := c.reads.Load(); got != 1 {
			t.Errorf("%s: %d reads of the archive, want only the one of its tail", name, got)
		}
	}
	for name, raw := range map[string][]byte{"too short": []byte("PK"), "no record": bytes.Repeat([]byte("x"), 100)} {
		if _, ok := zipDeclaredEntries(bytes.NewReader(raw), int64(len(raw))); ok {
			t.Errorf("%s: a declared count was read", name)
		}
	}
}
