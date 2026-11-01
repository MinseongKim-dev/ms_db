package storage

import (
	"errors"
	"math"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
)

// Phase 1 completion criterion (B+Tree):
// With unique int64 keys, insert, search and range scan work; after
// multi-level splits (including root splits) every key is still returned in
// sorted order; and the tree survives reopening the file.
// Scope decision: delete is included, with full rebalancing (borrow/merge).
//
// Buffer pool addendum: the tree runs on a small buffer pool, and every
// operation must unpin every page it fetched (NumPinnedPages() == 0 after).

// smallFanout forces many splits and merges with few keys.
var smallFanout = &BTreeOptions{LeafMax: 4, InternalMax: 4}

// treePoolSize is deliberately tiny so the tree constantly evicts pages.
const treePoolSize = 4

func openTree(t *testing.T, opts *BTreeOptions) (*BTree, *DiskManager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "idx.db")
	dm, err := OpenDiskManager(path)
	if err != nil {
		t.Fatal(err)
	}
	bt, err := OpenBTree(NewBufferPool(dm, treePoolSize), opts)
	if err != nil {
		t.Fatal(err)
	}
	return bt, dm, path
}

// reopenTree flushes the pool, closes the file and opens it again with a
// fresh pool, so only what reached disk survives.
func reopenTree(t *testing.T, bt *BTree, dm *DiskManager, path string) (*BTree, *DiskManager) {
	t.Helper()
	if err := bt.bp.FlushAll(); err != nil {
		t.Fatal(err)
	}
	if err := dm.Close(); err != nil {
		t.Fatal(err)
	}
	dm, err := OpenDiskManager(path)
	if err != nil {
		t.Fatal(err)
	}
	bt, err = OpenBTree(NewBufferPool(dm, treePoolSize), nil)
	if err != nil {
		t.Fatal(err)
	}
	return bt, dm
}

func checkNoPins(t *testing.T, bt *BTree) {
	t.Helper()
	if n := bt.bp.NumPinnedPages(); n != 0 {
		t.Fatalf("%d pages still pinned", n)
	}
}

func ridFor(k int64) RID {
	return RID{Page: PageID(uint64(k) % 100000), Slot: SlotID(uint64(k) % 1000)}
}

func mustInsertKey(t *testing.T, bt *BTree, k int64) {
	t.Helper()
	if err := bt.Insert(k, ridFor(k)); err != nil {
		t.Fatalf("Insert(%d): %v", k, err)
	}
}

func scanAll(t *testing.T, bt *BTree) []int64 {
	t.Helper()
	var keys []int64
	err := bt.RangeScan(math.MinInt64, math.MaxInt64, func(k int64, rid RID) bool {
		if rid != ridFor(k) {
			t.Fatalf("scan: key %d has RID %v, want %v", k, rid, ridFor(k))
		}
		keys = append(keys, k)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// checkContents verifies Search and a full scan against the expected key set.
func checkContents(t *testing.T, bt *BTree, want map[int64]bool) {
	t.Helper()
	sorted := make([]int64, 0, len(want))
	for k := range want {
		sorted = append(sorted, k)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	got := scanAll(t, bt)
	if len(got) != len(sorted) {
		t.Fatalf("scan returned %d keys, want %d", len(got), len(sorted))
	}
	for i := range got {
		if got[i] != sorted[i] {
			t.Fatalf("scan[%d] = %d, want %d", i, got[i], sorted[i])
		}
	}
	for k := range want {
		rid, ok, err := bt.Search(k)
		if err != nil || !ok || rid != ridFor(k) {
			t.Fatalf("Search(%d) = %v, %v, %v", k, rid, ok, err)
		}
	}
	checkNoPins(t, bt)
}

// verifyTree checks the structural invariants of the whole tree:
// keys sorted within nodes and inside their separator bounds, every leaf at
// the same depth, non-root nodes at least half full, and the leaf chain
// visiting every leaf left to right.
func verifyTree(t *testing.T, bt *BTree) {
	t.Helper()
	checkNoPins(t, bt)
	defer checkNoPins(t, bt)
	var leaves []PageID
	leafDepth := -1
	var walk func(id PageID, lo, hi *int64, depth int, isRoot bool)
	walk = func(id PageID, lo, hi *int64, depth int, isRoot bool) {
		n, err := bt.readNode(id)
		if err != nil {
			t.Fatalf("read node %d: %v", id, err)
		}
		for i := 1; i < len(n.keys); i++ {
			if n.keys[i-1] >= n.keys[i] {
				t.Fatalf("node %d: keys not strictly increasing: %v", id, n.keys)
			}
		}
		for _, k := range n.keys {
			if (lo != nil && k < *lo) || (hi != nil && k >= *hi) {
				t.Fatalf("node %d: key %d outside bounds", id, k)
			}
		}
		if n.leaf {
			if !isRoot && len(n.keys) < bt.leafMax/2 {
				t.Fatalf("leaf %d underfull: %d keys", id, len(n.keys))
			}
			if leafDepth == -1 {
				leafDepth = depth
			} else if leafDepth != depth {
				t.Fatalf("leaf %d at depth %d, others at %d", id, depth, leafDepth)
			}
			leaves = append(leaves, id)
			return
		}
		if len(n.children) != len(n.keys)+1 {
			t.Fatalf("internal %d: %d keys but %d children", id, len(n.keys), len(n.children))
		}
		if isRoot && len(n.keys) == 0 {
			t.Fatalf("internal root %d has no keys", id)
		}
		if !isRoot && len(n.keys) < bt.internalMax/2 {
			t.Fatalf("internal %d underfull: %d keys", id, len(n.keys))
		}
		for i, c := range n.children {
			cl, ch := lo, hi
			if i > 0 {
				cl = &n.keys[i-1]
			}
			if i < len(n.keys) {
				ch = &n.keys[i]
			}
			walk(c, cl, ch, depth+1, false)
		}
	}
	walk(bt.root, nil, nil, 0, true)

	// The leaf chain must visit exactly the leaves found by the walk, in order.
	id := leaves[0]
	for i := 0; ; i++ {
		if i >= len(leaves) || id != leaves[i] {
			t.Fatalf("leaf chain diverges at position %d", i)
		}
		n, err := bt.readNode(id)
		if err != nil {
			t.Fatal(err)
		}
		if n.next == InvalidPageID {
			if i != len(leaves)-1 {
				t.Fatalf("leaf chain ends early at position %d of %d", i, len(leaves))
			}
			break
		}
		id = n.next
	}
}

func treeHeight(t *testing.T, bt *BTree) int {
	t.Helper()
	h := 1
	id := bt.root
	for {
		n, err := bt.readNode(id)
		if err != nil {
			t.Fatal(err)
		}
		if n.leaf {
			return h
		}
		id = n.children[0]
		h++
	}
}

func TestEmptyTree(t *testing.T) {
	bt, dm, _ := openTree(t, smallFanout)
	defer dm.Close()
	if _, ok, err := bt.Search(42); ok || err != nil {
		t.Fatalf("Search on empty tree: ok=%v err=%v", ok, err)
	}
	if keys := scanAll(t, bt); len(keys) != 0 {
		t.Fatalf("scan on empty tree returned %v", keys)
	}
	verifyTree(t, bt)
}

func TestSequentialInsertMultiLevelSplits(t *testing.T) {
	bt, dm, _ := openTree(t, smallFanout)
	defer dm.Close()
	want := map[int64]bool{}
	for k := int64(1); k <= 500; k++ {
		mustInsertKey(t, bt, k)
		want[k] = true
		if k%50 == 0 {
			verifyTree(t, bt)
		}
	}
	if h := treeHeight(t, bt); h < 4 {
		t.Fatalf("height %d: expected several root splits", h)
	}
	verifyTree(t, bt)
	checkContents(t, bt, want)
}

func TestDescendingAndRandomInsert(t *testing.T) {
	for _, name := range []string{"descending", "random"} {
		t.Run(name, func(t *testing.T) {
			bt, dm, _ := openTree(t, smallFanout)
			defer dm.Close()
			keys := make([]int64, 400)
			for i := range keys {
				keys[i] = int64(400 - i)
			}
			if name == "random" {
				r := rand.New(rand.NewSource(1))
				for i := range keys {
					keys[i] = r.Int63() - math.MaxInt64/2
				}
			}
			want := map[int64]bool{}
			for _, k := range keys {
				if want[k] {
					continue
				}
				mustInsertKey(t, bt, k)
				want[k] = true
			}
			verifyTree(t, bt)
			checkContents(t, bt, want)
		})
	}
}

func TestExtremeKeys(t *testing.T) {
	bt, dm, _ := openTree(t, smallFanout)
	defer dm.Close()
	want := map[int64]bool{}
	for _, k := range []int64{math.MinInt64, math.MaxInt64, 0, -1, 1, math.MinInt64 + 1, math.MaxInt64 - 1} {
		mustInsertKey(t, bt, k)
		want[k] = true
	}
	verifyTree(t, bt)
	checkContents(t, bt, want)
}

func TestDuplicateKeyRejected(t *testing.T) {
	bt, dm, _ := openTree(t, smallFanout)
	defer dm.Close()
	for k := int64(0); k < 50; k++ {
		mustInsertKey(t, bt, k)
	}
	if err := bt.Insert(25, RID{Page: 999, Slot: 9}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("got %v, want ErrDuplicateKey", err)
	}
	// The original RID must be untouched.
	if rid, ok, _ := bt.Search(25); !ok || rid != ridFor(25) {
		t.Fatalf("Search(25) after duplicate insert = %v, %v", rid, ok)
	}
	verifyTree(t, bt)
}

func TestRangeScanBounds(t *testing.T) {
	bt, dm, _ := openTree(t, smallFanout)
	defer dm.Close()
	for k := int64(0); k < 200; k += 2 { // even keys 0..198
		mustInsertKey(t, bt, k)
	}
	collect := func(lo, hi int64) []int64 {
		var out []int64
		if err := bt.RangeScan(lo, hi, func(k int64, _ RID) bool {
			out = append(out, k)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Inclusive bounds on existing keys.
	if got := collect(10, 20); !equalKeys(got, []int64{10, 12, 14, 16, 18, 20}) {
		t.Fatalf("[10,20] = %v", got)
	}
	// Bounds between keys.
	if got := collect(11, 19); !equalKeys(got, []int64{12, 14, 16, 18}) {
		t.Fatalf("[11,19] = %v", got)
	}
	// Bounds spanning many leaves.
	if got := collect(-100, 1000); len(got) != 100 {
		t.Fatalf("full range returned %d keys", len(got))
	}
	// Empty ranges.
	if got := collect(20, 10); len(got) != 0 {
		t.Fatalf("lo > hi returned %v", got)
	}
	if got := collect(199, 199); len(got) != 0 {
		t.Fatalf("[199,199] returned %v", got)
	}
	// Early stop.
	var n int
	if err := bt.RangeScan(0, 198, func(int64, RID) bool { n++; return n < 7 }); err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("early stop visited %d keys, want 7", n)
	}
}

func equalKeys(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPersistsAcrossReopenBTree(t *testing.T) {
	bt, dm, path := openTree(t, smallFanout)
	want := map[int64]bool{}
	r := rand.New(rand.NewSource(2))
	for len(want) < 300 {
		k := r.Int63n(10000)
		if want[k] {
			continue
		}
		mustInsertKey(t, bt, k)
		want[k] = true
	}
	h := treeHeight(t, bt)

	bt, dm = reopenTree(t, bt, dm, path)
	defer func() { dm.Close() }()
	if bt.leafMax != smallFanout.LeafMax || bt.internalMax != smallFanout.InternalMax {
		t.Fatalf("fanout not persisted: %d/%d", bt.leafMax, bt.internalMax)
	}
	if got := treeHeight(t, bt); got != h {
		t.Fatalf("height after reopen = %d, want %d", got, h)
	}
	verifyTree(t, bt)
	checkContents(t, bt, want)

	// The reopened tree keeps working, including further root splits.
	for k := int64(20000); k < 20500; k++ {
		mustInsertKey(t, bt, k)
		want[k] = true
	}
	bt, dm = reopenTree(t, bt, dm, path)
	verifyTree(t, bt)
	checkContents(t, bt, want)
}

func TestDefaultFanoutManyKeys(t *testing.T) {
	bt, dm, path := openTree(t, nil)
	if bt.leafMax != maxLeafKeys || bt.internalMax != maxInternalKeys {
		t.Fatalf("default fanout = %d/%d, want %d/%d", bt.leafMax, bt.internalMax, maxLeafKeys, maxInternalKeys)
	}
	r := rand.New(rand.NewSource(3))
	want := map[int64]bool{}
	for len(want) < 30000 {
		k := r.Int63()
		if want[k] {
			continue
		}
		mustInsertKey(t, bt, k)
		want[k] = true
	}
	// 30000 random keys fill about 130 leaves, which fit under one root, so
	// the page-sized tree has height 2: this checks that the root split with
	// real page encoding. Deeper trees are covered by the small-fanout tests.
	if h := treeHeight(t, bt); h < 2 {
		t.Fatalf("height %d, expected the root to have split", h)
	}
	bt, dm = reopenTree(t, bt, dm, path)
	defer dm.Close()
	verifyTree(t, bt)
	checkContents(t, bt, want)
}

func TestInvalidOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.db")
	dm, err := OpenDiskManager(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dm.Close()
	for _, o := range []BTreeOptions{
		{LeafMax: 2, InternalMax: 4},
		{LeafMax: 4, InternalMax: 2},
		{LeafMax: maxLeafKeys + 1, InternalMax: 4},
		{LeafMax: 4, InternalMax: maxInternalKeys + 1},
	} {
		o := o
		if _, err := OpenBTree(NewBufferPool(dm, treePoolSize), &o); err == nil {
			t.Fatalf("OpenBTree(%+v) succeeded, want error", o)
		}
	}
}

func TestDeleteMissingKey(t *testing.T) {
	bt, dm, _ := openTree(t, smallFanout)
	defer dm.Close()
	if err := bt.Delete(1); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Delete on empty tree: got %v", err)
	}
	for k := int64(0); k < 30; k += 3 {
		mustInsertKey(t, bt, k)
	}
	if err := bt.Delete(4); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Delete(4): got %v, want ErrKeyNotFound", err)
	}
	verifyTree(t, bt)
}

func TestDeleteAllShrinksToLeafRoot(t *testing.T) {
	for _, order := range []string{"ascending", "descending", "random"} {
		t.Run(order, func(t *testing.T) {
			bt, dm, _ := openTree(t, smallFanout)
			defer dm.Close()
			const n = 300
			keys := make([]int64, n)
			for i := range keys {
				keys[i] = int64(i)
				mustInsertKey(t, bt, keys[i])
			}
			switch order {
			case "descending":
				sort.Slice(keys, func(i, j int) bool { return keys[i] > keys[j] })
			case "random":
				rand.New(rand.NewSource(4)).Shuffle(n, func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
			}
			want := map[int64]bool{}
			for _, k := range keys {
				want[k] = true
			}
			for i, k := range keys {
				if err := bt.Delete(k); err != nil {
					t.Fatalf("Delete(%d): %v", k, err)
				}
				delete(want, k)
				if i%25 == 0 {
					verifyTree(t, bt)
					checkContents(t, bt, want)
				}
			}
			if h := treeHeight(t, bt); h != 1 {
				t.Fatalf("height after deleting everything = %d, want 1", h)
			}
			if keys := scanAll(t, bt); len(keys) != 0 {
				t.Fatalf("tree not empty: %v", keys)
			}
			// The emptied tree must be fully reusable.
			for k := int64(0); k < 100; k++ {
				mustInsertKey(t, bt, k)
				want[k] = true
			}
			verifyTree(t, bt)
			checkContents(t, bt, want)
		})
	}
}

// TestRandomInsertDeleteChurn compares the tree with a map model over many
// random inserts and deletes, checking every invariant as it goes.
func TestRandomInsertDeleteChurn(t *testing.T) {
	bt, dm, path := openTree(t, smallFanout)
	r := rand.New(rand.NewSource(5))
	model := map[int64]bool{}
	for step := 0; step < 5000; step++ {
		k := r.Int63n(600) - 300
		if r.Intn(3) == 0 || len(model) == 0 {
			// Delete (possibly a missing key).
			err := bt.Delete(k)
			switch {
			case model[k] && err != nil:
				t.Fatalf("step %d: Delete(%d): %v", step, k, err)
			case !model[k] && !errors.Is(err, ErrKeyNotFound):
				t.Fatalf("step %d: Delete(%d) of missing key: %v", step, k, err)
			}
			delete(model, k)
		} else {
			err := bt.Insert(k, ridFor(k))
			switch {
			case model[k] && !errors.Is(err, ErrDuplicateKey):
				t.Fatalf("step %d: duplicate Insert(%d): %v", step, k, err)
			case !model[k] && err != nil:
				t.Fatalf("step %d: Insert(%d): %v", step, k, err)
			}
			model[k] = true
		}
		if step%100 == 0 {
			verifyTree(t, bt)
			checkContents(t, bt, model)
		}
	}
	bt, dm = reopenTree(t, bt, dm, path)
	defer dm.Close()
	verifyTree(t, bt)
	checkContents(t, bt, model)
}

// TestBTreeWithOneFramePool shows the tree never needs more than one page
// pinned at a time: every operation works through a single-frame pool.
func TestBTreeWithOneFramePool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one.db")
	dm, err := OpenDiskManager(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dm.Close()
	bt, err := OpenBTree(NewBufferPool(dm, 1), smallFanout)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]bool{}
	for k := int64(0); k < 200; k++ {
		mustInsertKey(t, bt, k)
		want[k] = true
	}
	for k := int64(0); k < 200; k += 2 {
		if err := bt.Delete(k); err != nil {
			t.Fatalf("Delete(%d): %v", k, err)
		}
		delete(want, k)
	}
	verifyTree(t, bt)
	checkContents(t, bt, want)
}
