package storage

import (
	"container/list"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrBufferPoolFull = errors.New("storage: every buffer pool frame is pinned")
	ErrPageNotPinned  = errors.New("storage: page is not resident or not pinned")
)

// Page is one buffer pool frame holding a copy of a disk page. A caller may
// use Data() only between the FetchPage/NewPage that returned the page and the
// matching UnpinPage.
//
// The pool does not latch page contents: two goroutines must not modify the
// same pinned page at once, nor modify it while FlushPage/FlushAll runs (page
// latches arrive with concurrency control in Phase 3).
type Page struct {
	id       PageID
	data     []byte
	pinCount int  // guarded by BufferPool.mu
	dirty    bool // guarded by BufferPool.mu
}

// ID returns the page's ID on disk.
func (p *Page) ID() PageID { return p.id }

// Data returns the page contents (PageDataSize bytes).
func (p *Page) Data() []byte { return p.data }

// BufferPool caches a fixed number of disk pages in memory.
//
// A page stays in its frame while pinned (pin count > 0). When a frame is
// needed, the pool takes a free frame if there is one, otherwise evicts the
// least recently unpinned page, writing it back first if it is dirty. Every
// FetchPage/NewPage must be matched by exactly one UnpinPage, or the frame
// can never be reused.
//
// All bookkeeping, including the disk I/O for misses and evictions, happens
// under one mutex. That serializes I/O but keeps a page from being read and
// evicted at the same time.
type BufferPool struct {
	dm *DiskManager

	mu        sync.Mutex
	frames    []*Page
	pageTable map[PageID]int        // resident page -> frame index
	free      []int                 // frames holding no page
	lru       *list.List            // unpinned frames; front = most recently unpinned
	lruElem   map[int]*list.Element // frame index -> its element in lru
}

// NewBufferPool creates a pool of size frames over dm.
func NewBufferPool(dm *DiskManager, size int) *BufferPool {
	if size < 1 {
		panic(fmt.Sprintf("storage: buffer pool size %d < 1", size))
	}
	bp := &BufferPool{
		dm:        dm,
		frames:    make([]*Page, size),
		pageTable: make(map[PageID]int, size),
		free:      make([]int, 0, size),
		lru:       list.New(),
		lruElem:   make(map[int]*list.Element, size),
	}
	for i := range bp.frames {
		bp.frames[i] = &Page{id: InvalidPageID, data: make([]byte, PageDataSize)}
		bp.free = append(bp.free, i)
	}
	return bp
}

// FetchPage pins page id, reading it from disk if it is not resident.
func (bp *BufferPool) FetchPage(id PageID) (*Page, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	if f, ok := bp.pageTable[id]; ok {
		p := bp.frames[f]
		bp.pin(f)
		return p, nil
	}

	f, err := bp.takeFrame()
	if err != nil {
		return nil, err
	}
	p := bp.frames[f]
	if err := bp.dm.ReadPage(id, p.data); err != nil {
		bp.free = append(bp.free, f) // give the frame back unused
		return nil, err
	}
	p.id, p.dirty = id, false
	bp.pageTable[id] = f
	bp.pin(f)
	return p, nil
}

// NewPage allocates a new zeroed page on disk and returns it pinned. It checks
// for a free frame first, so a full pool does not leave an orphaned disk page.
func (bp *BufferPool) NewPage() (*Page, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	f, err := bp.takeFrame()
	if err != nil {
		return nil, err
	}
	id, err := bp.dm.AllocatePage()
	if err != nil {
		bp.free = append(bp.free, f)
		return nil, err
	}
	p := bp.frames[f]
	clear(p.data) // AllocatePage already wrote a zero page, so it is clean
	p.id, p.dirty = id, false
	bp.pageTable[id] = f
	bp.pin(f)
	return p, nil
}

// UnpinPage releases one pin on page id. dirty marks the page as modified;
// once set, the flag stays until the page is written back.
func (bp *BufferPool) UnpinPage(id PageID, dirty bool) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	f, ok := bp.pageTable[id]
	if !ok || bp.frames[f].pinCount == 0 {
		return fmt.Errorf("%w: page %d", ErrPageNotPinned, id)
	}
	p := bp.frames[f]
	if dirty {
		p.dirty = true
	}
	p.pinCount--
	if p.pinCount == 0 {
		bp.lruElem[f] = bp.lru.PushFront(f)
	}
	return nil
}

// FlushPage writes page id to disk if it is resident and dirty.
func (bp *BufferPool) FlushPage(id PageID) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	f, ok := bp.pageTable[id]
	if !ok {
		return nil
	}
	return bp.writeBack(bp.frames[f])
}

// FlushAll writes every dirty resident page to disk.
func (bp *BufferPool) FlushAll() error {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	for _, f := range bp.pageTable {
		if err := bp.writeBack(bp.frames[f]); err != nil {
			return err
		}
	}
	return nil
}

// NumPinnedPages returns how many resident pages have a pin count above zero.
// Tests use it to catch leaked pins.
func (bp *BufferPool) NumPinnedPages() int {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	n := 0
	for _, f := range bp.pageTable {
		if bp.frames[f].pinCount > 0 {
			n++
		}
	}
	return n
}

// pin adds a pin to frame f, removing it from the eviction list if it was
// there. Callers hold mu.
func (bp *BufferPool) pin(f int) {
	if e, ok := bp.lruElem[f]; ok {
		bp.lru.Remove(e)
		delete(bp.lruElem, f)
	}
	bp.frames[f].pinCount++
}

// takeFrame returns a frame holding no page: a free one, or else the least
// recently unpinned one after evicting its page. Callers hold mu.
func (bp *BufferPool) takeFrame() (int, error) {
	if n := len(bp.free); n > 0 {
		f := bp.free[n-1]
		bp.free = bp.free[:n-1]
		return f, nil
	}
	e := bp.lru.Back()
	if e == nil {
		return 0, ErrBufferPoolFull
	}
	f := e.Value.(int)
	victim := bp.frames[f]
	// Write back before forgetting the page; if the write fails the page
	// stays resident and unpinned, so no data is lost.
	if err := bp.writeBack(victim); err != nil {
		return 0, err
	}
	bp.lru.Remove(e)
	delete(bp.lruElem, f)
	delete(bp.pageTable, victim.id)
	victim.id = InvalidPageID
	return f, nil
}

// writeBack writes p to disk if dirty. Callers hold mu.
func (bp *BufferPool) writeBack(p *Page) error {
	if !p.dirty {
		return nil
	}
	if err := bp.dm.WritePage(p.id, p.data); err != nil {
		return err
	}
	p.dirty = false
	return nil
}
