package storage

import (
	"bytes"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Phase 1 completion criterion (DiskManager):
// 4KB pages written and read back must be byte-identical and survive reopening
// the file; reading a page whose CRC32 does not match (torn page) must return an
// error; newly allocated page IDs must never collide; reading a page that does
// not exist must return a clear error.

func openTemp(t *testing.T) (*DiskManager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	dm, err := OpenDiskManager(path)
	if err != nil {
		t.Fatalf("OpenDiskManager: %v", err)
	}
	return dm, path
}

func randomPage(r *rand.Rand) []byte {
	buf := make([]byte, PageDataSize)
	r.Read(buf)
	return buf
}

func TestPageSizeIs4KB(t *testing.T) {
	if PageSize != 4096 {
		t.Fatalf("PageSize = %d, want 4096", PageSize)
	}
	if PageDataSize != PageSize-ChecksumSize {
		t.Fatalf("PageDataSize = %d, want %d", PageDataSize, PageSize-ChecksumSize)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	dm, _ := openTemp(t)
	defer dm.Close()
	r := rand.New(rand.NewSource(1))

	const n = 8
	want := make([][]byte, n)
	for i := 0; i < n; i++ {
		id, err := dm.AllocatePage()
		if err != nil {
			t.Fatalf("AllocatePage: %v", err)
		}
		want[i] = randomPage(r)
		if err := dm.WritePage(id, want[i]); err != nil {
			t.Fatalf("WritePage(%d): %v", id, err)
		}
	}
	// Read back after all writes so a write that spills into a neighbouring
	// page would be detected.
	got := make([]byte, PageDataSize)
	for i := 0; i < n; i++ {
		if err := dm.ReadPage(PageID(i), got); err != nil {
			t.Fatalf("ReadPage(%d): %v", i, err)
		}
		if !bytes.Equal(got, want[i]) {
			t.Fatalf("page %d: content mismatch", i)
		}
	}
}

func TestOverwritePage(t *testing.T) {
	dm, _ := openTemp(t)
	defer dm.Close()
	r := rand.New(rand.NewSource(2))

	id, _ := dm.AllocatePage()
	first, second := randomPage(r), randomPage(r)
	if err := dm.WritePage(id, first); err != nil {
		t.Fatal(err)
	}
	if err := dm.WritePage(id, second); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, PageDataSize)
	if err := dm.ReadPage(id, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, second) {
		t.Fatal("overwrite not visible")
	}
}

func TestExtremeContents(t *testing.T) {
	dm, _ := openTemp(t)
	defer dm.Close()

	allOnes := bytes.Repeat([]byte{0xFF}, PageDataSize)
	zeros := make([]byte, PageDataSize)
	for _, want := range [][]byte{allOnes, zeros} {
		id, _ := dm.AllocatePage()
		if err := dm.WritePage(id, want); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, PageDataSize)
		if err := dm.ReadPage(id, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("page %d: content mismatch", id)
		}
	}
}

func TestFreshPageIsZeroAndValid(t *testing.T) {
	dm, _ := openTemp(t)
	defer dm.Close()

	id, _ := dm.AllocatePage()
	got := make([]byte, PageDataSize)
	if err := dm.ReadPage(id, got); err != nil {
		t.Fatalf("reading freshly allocated page: %v", err)
	}
	if !bytes.Equal(got, make([]byte, PageDataSize)) {
		t.Fatal("freshly allocated page is not zeroed")
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	dm, path := openTemp(t)
	r := rand.New(rand.NewSource(3))

	const n = 5
	want := make([][]byte, n)
	for i := 0; i < n; i++ {
		id, _ := dm.AllocatePage()
		want[i] = randomPage(r)
		if err := dm.WritePage(id, want[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := dm.Close(); err != nil {
		t.Fatal(err)
	}

	dm, err := OpenDiskManager(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dm.Close()
	if dm.NumPages() != n {
		t.Fatalf("NumPages after reopen = %d, want %d", dm.NumPages(), n)
	}
	got := make([]byte, PageDataSize)
	for i := 0; i < n; i++ {
		if err := dm.ReadPage(PageID(i), got); err != nil {
			t.Fatalf("ReadPage(%d): %v", i, err)
		}
		if !bytes.Equal(got, want[i]) {
			t.Fatalf("page %d: content mismatch after reopen", i)
		}
	}
}

func TestAllocateUniqueIDs(t *testing.T) {
	dm, path := openTemp(t)

	seen := map[PageID]bool{}
	for i := 0; i < 10; i++ {
		id, err := dm.AllocatePage()
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("duplicate page id %d", id)
		}
		seen[id] = true
	}
	dm.Close()

	// IDs allocated after reopening must not reuse existing pages.
	dm, err := OpenDiskManager(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dm.Close()
	for i := 0; i < 10; i++ {
		id, err := dm.AllocatePage()
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("page id %d reused after reopen", id)
		}
		seen[id] = true
	}
}

func TestAllocateUniqueIDsConcurrent(t *testing.T) {
	dm, _ := openTemp(t)
	defer dm.Close()

	const workers, perWorker = 8, 50
	ids := make(chan PageID, workers*perWorker)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id, err := dm.AllocatePage()
				if err != nil {
					t.Error(err)
					return
				}
				ids <- id
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := map[PageID]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate page id %d under concurrent allocation", id)
		}
		seen[id] = true
	}
	if dm.NumPages() != workers*perWorker {
		t.Fatalf("NumPages = %d, want %d", dm.NumPages(), workers*perWorker)
	}
}

func TestNonexistentPage(t *testing.T) {
	dm, _ := openTemp(t)
	defer dm.Close()
	buf := make([]byte, PageDataSize)

	if err := dm.ReadPage(0, buf); !errors.Is(err, ErrPageNotFound) {
		t.Fatalf("ReadPage on empty file: got %v, want ErrPageNotFound", err)
	}
	id, _ := dm.AllocatePage()
	if err := dm.ReadPage(id+1, buf); !errors.Is(err, ErrPageNotFound) {
		t.Fatalf("ReadPage past end: got %v, want ErrPageNotFound", err)
	}
	if err := dm.WritePage(id+1, buf); !errors.Is(err, ErrPageNotFound) {
		t.Fatalf("WritePage past end: got %v, want ErrPageNotFound", err)
	}
	if err := dm.ReadPage(InvalidPageID, buf); !errors.Is(err, ErrPageNotFound) {
		t.Fatalf("ReadPage(InvalidPageID): got %v, want ErrPageNotFound", err)
	}
}

func TestWrongBufferSize(t *testing.T) {
	dm, _ := openTemp(t)
	defer dm.Close()
	id, _ := dm.AllocatePage()

	for _, size := range []int{0, PageDataSize - 1, PageDataSize + 1, PageSize} {
		buf := make([]byte, size)
		if err := dm.WritePage(id, buf); !errors.Is(err, ErrInvalidPageSize) {
			t.Fatalf("WritePage len %d: got %v, want ErrInvalidPageSize", size, err)
		}
		if err := dm.ReadPage(id, buf); !errors.Is(err, ErrInvalidPageSize) {
			t.Fatalf("ReadPage len %d: got %v, want ErrInvalidPageSize", size, err)
		}
	}
}

// corruptAndReopen writes page 0, closes the file, applies mutate to the raw
// file, reopens it and returns the error from reading page 0.
func corruptAndReopen(t *testing.T, mutate func(f *os.File)) error {
	t.Helper()
	dm, path := openTemp(t)
	id, _ := dm.AllocatePage()
	if err := dm.WritePage(id, randomPage(rand.New(rand.NewSource(4)))); err != nil {
		t.Fatal(err)
	}
	dm.Close()

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	mutate(f)
	f.Close()

	dm, err = OpenDiskManager(path)
	if err != nil {
		t.Fatalf("reopen after corruption: %v", err)
	}
	defer dm.Close()
	return dm.ReadPage(id, make([]byte, PageDataSize))
}

func flipByte(f *os.File, off int64) {
	b := make([]byte, 1)
	f.ReadAt(b, off)
	b[0] ^= 0xFF
	f.WriteAt(b, off)
}

func TestChecksumMismatchInData(t *testing.T) {
	err := corruptAndReopen(t, func(f *os.File) { flipByte(f, PageSize/2) })
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("got %v, want ErrChecksumMismatch", err)
	}
}

func TestChecksumMismatchInChecksumField(t *testing.T) {
	err := corruptAndReopen(t, func(f *os.File) { flipByte(f, 0) })
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("got %v, want ErrChecksumMismatch", err)
	}
}

func TestTornWriteTruncatedPage(t *testing.T) {
	// Simulates a crash midway through writing the last page.
	err := corruptAndReopen(t, func(f *os.File) { f.Truncate(PageSize / 2) })
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("got %v, want ErrChecksumMismatch", err)
	}
}
