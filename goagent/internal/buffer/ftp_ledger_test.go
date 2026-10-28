package buffer

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestFTPLedgerRoundTripsAcrossReopen covers load/mark/forget, the upsert of
// a rewritten path's stamp, and persistence across a reopen, with more rows
// than one write chunk.
func TestFTPLedgerRoundTripsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buffer.sqlite")
	b, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := ledgerChunk*2 + 7
	marks := make([]FTPMark, n)
	for i := range marks {
		marks[i] = FTPMark{Path: fmt.Sprintf("/ftp/Log_%06d.gz", i), Stamp: FTPStamp{Size: int64(i), ModTime: int64(i) * 1e9}}
	}
	if err := b.MarkFTPIngested(marks); err != nil {
		t.Fatalf("mark: %v", err)
	}
	rewritten := FTPMark{Path: marks[0].Path, Stamp: FTPStamp{Size: 999, ModTime: 42}}
	if err := b.MarkFTPIngested([]FTPMark{rewritten}); err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	if err := b.ForgetFTPIngested([]string{marks[1].Path, marks[2].Path, "/ftp/never-marked.gz"}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	b.Close()

	b, err = Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	got, err := b.LoadFTPIngested()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != n-2 {
		t.Fatalf("rows = %d, want %d", len(got), n-2)
	}
	if got[rewritten.Path] != rewritten.Stamp {
		t.Fatalf("rewritten stamp = %+v, want %+v", got[rewritten.Path], rewritten.Stamp)
	}
	if _, ok := got[marks[1].Path]; ok {
		t.Fatalf("forgotten path still loaded")
	}
	if got[marks[n-1].Path] != marks[n-1].Stamp {
		t.Fatalf("last chunk row = %+v, want %+v", got[marks[n-1].Path], marks[n-1].Stamp)
	}
}
