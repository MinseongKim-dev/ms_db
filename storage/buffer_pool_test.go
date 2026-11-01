package storage

import (
	"bytes"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// Phase 1 addendum completion criterion (buffer pool):
// Even with more pages than the pool holds, only unpinned pages are evicted,
// in LRU order; dirty pages are written to disk when evicted or flushed; when
// every frame is pinned, FetchPage returns a clear error; and many goroutines
// fetching and unpinning at once keep pin counts exact with no data races.

func newPool(t *testing.T, size int) (*BufferPool, *DiskManager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bp.db")
	dm, err := OpenDiskManager(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	return NewBufferPool(dm, size), dm, path
}

func mustNewPage(t *testing.T, bp *BufferPool) *Page {
	t.Helper()
	p, err := bp.NewPage()
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	return p
}

func mustFetch(t *testing.T, bp *BufferPool, id PageID) *Page {
	t.Helper()
	p, err := bp.FetchPage(id)
	if err != nil {
		t.Fatalf("FetchPage(%d): %v", id, err)
	}
	return p
}

func mustUnpin(t *testing.T, bp *BufferPool, id PageID, dirty bool) {
	t.Helper()
	if err := bp.UnpinPage(id, dirty); err != nil {
		t.Fatalf("UnpinPage(%d): %v", id, err)
	}
}

func resident(bp *BufferPool, id PageID) bool {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	_, ok := bp.pageTable[id]
	return ok
}

func TestNewPageIsZeroedAndPinned(t *testing.T) {
	bp, _, _ := newPool(t, 4)
	p := mustNewPage(t, bp)
	if len(p.Data()) != PageDataSize {
		t.Fatalf("page data is %d bytes, want %d", len(p.Data()), PageDataSize)
	}
	if !bytes.Equal(p.Data(), make([]byte, PageDataSize)) {
		t.Fatal("new page is not zeroed")
	}
	if bp.NumPinnedPages() != 1 {
		t.Fatalf("NumPinnedPages = %d, want 1", bp.NumPinnedPages())
	}
	mustUnpin(t, bp, p.ID(), false)
	if bp.NumPinnedPages() != 0 {
		t.Fatalf("NumPinnedPages after unpin = %d, want 0", bp.NumPinnedPages())
	}
}

func TestFlushPersistsAcrossReopen(t *testing.T) {
	bp, dm, path := newPool(t, 4)
	p := mustNewPage(t, bp)
	copy(p.Data(), "hello buffer pool")
	id := p.ID()
	mustUnpin(t, bp, id, true)
	if err := bp.FlushAll(); err != nil {
		t.Fatal(err)
	}
	dm.Close()

	dm2, err := OpenDiskManager(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dm2.Close()
	buf := make([]byte, PageDataSize)
	if err := dm2.ReadPage(id, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf, []byte("hello buffer pool")) {
		t.Fatal("flushed data not on disk")
	}
}

func TestEvictsLeastRecentlyUnpinned(t *testing.T) {
	bp, _, _ := newPool(t, 3)
	var ids []PageID
	for i := 0; i < 3; i++ {
		ids = append(ids, mustNewPage(t, bp).ID())
	}
	for _, id := range ids { // LRU order: ids[0], ids[1], ids[2]
		mustUnpin(t, bp, id, false)
	}
	// Touching ids[0] makes it most recent: order ids[1], ids[2], ids[0].
	mustFetch(t, bp, ids[0])
	mustUnpin(t, bp, ids[0], false)

	p := mustNewPage(t, bp)
	if resident(bp, ids[1]) {
		t.Fatal("expected least recently used page to be evicted")
	}
	for _, id := range []PageID{ids[0], ids[2], p.ID()} {
		if !resident(bp, id) {
			t.Fatalf("page %d was evicted, expected it to stay", id)
		}
	}
}

func TestPinnedPagesAreNeverEvicted(t *testing.T) {
	bp, dm, _ := newPool(t, 2)
	a := mustNewPage(t, bp)
	b := mustNewPage(t, bp)
	before := dm.NumPages()

	if _, err := bp.NewPage(); !errors.Is(err, ErrBufferPoolFull) {
		t.Fatalf("NewPage with all frames pinned: got %v, want ErrBufferPoolFull", err)
	}
	if dm.NumPages() != before {
		t.Fatal("failed NewPage still allocated a disk page")
	}
	// Page 0 and 1 are resident; fetching a third, non-resident page must fail.
	c, err := dm.AllocatePage()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bp.FetchPage(c); !errors.Is(err, ErrBufferPoolFull) {
		t.Fatalf("FetchPage with all frames pinned: got %v, want ErrBufferPoolFull", err)
	}
	if !resident(bp, a.ID()) || !resident(bp, b.ID()) {
		t.Fatal("pinned page was evicted")
	}

	mustUnpin(t, bp, a.ID(), false)
	mustFetch(t, bp, c) // now succeeds by evicting a
	if resident(bp, a.ID()) {
		t.Fatal("expected the unpinned page to be evicted")
	}
}

func TestFetchResidentPageWhenFull(t *testing.T) {
	bp, _, _ := newPool(t, 1)
	p := mustNewPage(t, bp)
	// A resident page can be pinned again even though no frame is free.
	q := mustFetch(t, bp, p.ID())
	if q != p {
		t.Fatal("fetching a resident page returned a different frame")
	}
	mustUnpin(t, bp, p.ID(), false)
	mustUnpin(t, bp, p.ID(), false)
}

func TestPinCountRequiresMatchingUnpins(t *testing.T) {
	bp, _, _ := newPool(t, 1)
	p := mustNewPage(t, bp)
	mustFetch(t, bp, p.ID()) // pin count 2
	mustUnpin(t, bp, p.ID(), false)
	if _, err := bp.NewPage(); !errors.Is(err, ErrBufferPoolFull) {
		t.Fatalf("page with pin count 1 was evictable: %v", err)
	}
	mustUnpin(t, bp, p.ID(), false)
	mustUnpin(t, bp, mustNewPage(t, bp).ID(), false)
}

func TestUnpinErrors(t *testing.T) {
	bp, _, _ := newPool(t, 2)
	if err := bp.UnpinPage(7, false); !errors.Is(err, ErrPageNotPinned) {
		t.Fatalf("unpin of non-resident page: got %v", err)
	}
	p := mustNewPage(t, bp)
	mustUnpin(t, bp, p.ID(), false)
	if err := bp.UnpinPage(p.ID(), false); !errors.Is(err, ErrPageNotPinned) {
		t.Fatalf("double unpin: got %v", err)
	}
}

func TestDirtyPageWrittenBackOnEviction(t *testing.T) {
	bp, dm, _ := newPool(t, 1)
	p := mustNewPage(t, bp)
	id := p.ID()
	copy(p.Data(), "dirty bytes")
	mustUnpin(t, bp, id, true)

	// The dirty flag is sticky: a later clean unpin must not clear it.
	mustFetch(t, bp, id)
	mustUnpin(t, bp, id, false)

	mustUnpin(t, bp, mustNewPage(t, bp).ID(), false) // evicts id
	if resident(bp, id) {
		t.Fatal("expected page to be evicted")
	}
	buf := make([]byte, PageDataSize)
	if err := dm.ReadPage(id, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf, []byte("dirty bytes")) {
		t.Fatal("dirty page was not written back on eviction")
	}
	// Fetching it again reads the written-back data.
	q := mustFetch(t, bp, id)
	if !bytes.HasPrefix(q.Data(), []byte("dirty bytes")) {
		t.Fatal("re-fetched page lost its data")
	}
	mustUnpin(t, bp, id, false)
}

func TestFetchMissingPageDoesNotLeakFrame(t *testing.T) {
	bp, _, _ := newPool(t, 1)
	if _, err := bp.FetchPage(42); !errors.Is(err, ErrPageNotFound) {
		t.Fatalf("got %v, want ErrPageNotFound", err)
	}
	// The single frame must still be usable.
	mustUnpin(t, bp, mustNewPage(t, bp).ID(), false)
	if bp.NumPinnedPages() != 0 {
		t.Fatal("failed fetch left a pinned frame")
	}
}

func TestManyPagesThroughSmallPool(t *testing.T) {
	bp, _, _ := newPool(t, 4)
	const n = 50
	ids := make([]PageID, n)
	for i := range ids {
		p := mustNewPage(t, bp)
		p.Data()[0] = byte(i)
		p.Data()[PageDataSize-1] = byte(i * 7)
		ids[i] = p.ID()
		mustUnpin(t, bp, ids[i], true)
	}
	for i, id := range ids {
		p := mustFetch(t, bp, id)
		if p.Data()[0] != byte(i) || p.Data()[PageDataSize-1] != byte(i*7) {
			t.Fatalf("page %d corrupted after passing through a 4-frame pool", id)
		}
		mustUnpin(t, bp, id, false)
	}
}

// TestConcurrentFetchUnpin has many goroutines pin and unpin pages from a set
// larger than the pool. Run with -race. Pages are only read after creation, so
// any corruption or race comes from the pool itself.
func TestConcurrentFetchUnpin(t *testing.T) {
	const workers, pages, rounds = 8, 40, 300
	bp, _, _ := newPool(t, workers+1) // each worker pins at most one page
	ids := make([]PageID, pages)
	for i := range ids {
		p := mustNewPage(t, bp)
		for j := range p.Data() {
			p.Data()[j] = byte(i)
		}
		ids[i] = p.ID()
		mustUnpin(t, bp, ids[i], true)
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers+1)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				i := (w*31 + r*17) % pages
				p, err := bp.FetchPage(ids[i])
				if err != nil {
					errs <- err
					return
				}
				d := p.Data()
				if p.ID() != ids[i] || d[0] != byte(i) || d[PageDataSize-1] != byte(i) {
					errs <- errors.New("fetched page has wrong identity or contents")
					bp.UnpinPage(ids[i], false)
					return
				}
				if err := bp.UnpinPage(ids[i], r%5 == 0); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	// A flusher runs alongside the workers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for r := 0; r < 20; r++ {
			if err := bp.FlushAll(); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if bp.NumPinnedPages() != 0 {
		t.Fatalf("NumPinnedPages = %d after all unpins, want 0", bp.NumPinnedPages())
	}
}
