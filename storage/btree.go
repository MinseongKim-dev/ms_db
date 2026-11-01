package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

var (
	ErrDuplicateKey = errors.New("storage: duplicate key")
	ErrKeyNotFound  = errors.New("storage: key not found")
)

// BTreeOptions sets the maximum number of keys per node. It only applies when
// a new index file is created; an existing file keeps the values stored in its
// meta page. Small values are useful in tests to force splits and merges.
type BTreeOptions struct {
	LeafMax     int // 3..maxLeafKeys
	InternalMax int // 3..maxInternalKeys
}

const (
	metaPageID PageID = 0
	btreeMagic        = 0x5442534d // "MSBT"
)

// BTree is a B+Tree index from unique int64 keys to RIDs, stored in its own
// file and accessed through a BufferPool.
//
// Page 0 is a meta page (magic | root PageID | leafMax | internalMax); every
// other page is one node. Values live only in leaves, and leaves are linked
// left to right for range scans. Internal node keys[i] separates children[i]
// (keys < keys[i]) from children[i+1] (keys >= keys[i]).
//
// Not safe for concurrent use (latch crabbing is Phase 3), and a crash in the
// middle of a split or merge can leave the file inconsistent (WAL is Phase 4).
// Pages emptied by merges are not reused yet.
//
// Every page access pins the page only for the moment it takes to decode or
// encode a node, so the tree never holds more than one pin and never leaks
// one, even on error paths.
type BTree struct {
	bp          *BufferPool
	root        PageID
	leafMax     int
	internalMax int
}

// OpenBTree opens the index stored in bp's file, creating an empty tree if the
// file is empty. opts may be nil for page-sized nodes.
func OpenBTree(bp *BufferPool, opts *BTreeOptions) (*BTree, error) {
	if bp.dm.NumPages() > 0 {
		return loadBTree(bp)
	}

	bt := &BTree{bp: bp, leafMax: maxLeafKeys, internalMax: maxInternalKeys}
	if opts != nil {
		if opts.LeafMax < 3 || opts.LeafMax > maxLeafKeys {
			return nil, fmt.Errorf("storage: LeafMax %d out of range [3, %d]", opts.LeafMax, maxLeafKeys)
		}
		if opts.InternalMax < 3 || opts.InternalMax > maxInternalKeys {
			return nil, fmt.Errorf("storage: InternalMax %d out of range [3, %d]", opts.InternalMax, maxInternalKeys)
		}
		bt.leafMax, bt.internalMax = opts.LeafMax, opts.InternalMax
	}

	meta, err := bp.NewPage()
	if err != nil {
		return nil, err
	}
	if err := bp.UnpinPage(meta.ID(), false); err != nil {
		return nil, err
	}
	if meta.ID() != metaPageID {
		return nil, fmt.Errorf("storage: meta page allocated at %d, want %d", meta.ID(), metaPageID)
	}
	rootID, err := bt.allocNode(&node{leaf: true, next: InvalidPageID})
	if err != nil {
		return nil, err
	}
	if err := bt.setRoot(rootID); err != nil {
		return nil, err
	}
	return bt, nil
}

func loadBTree(bp *BufferPool) (*BTree, error) {
	meta, err := bp.FetchPage(metaPageID)
	if err != nil {
		return nil, err
	}
	buf := meta.Data()
	bt := &BTree{
		bp:          bp,
		root:        PageID(binary.LittleEndian.Uint32(buf[4:8])),
		leafMax:     int(binary.LittleEndian.Uint16(buf[8:10])),
		internalMax: int(binary.LittleEndian.Uint16(buf[10:12])),
	}
	magic := binary.LittleEndian.Uint32(buf[0:4])
	if err := bp.UnpinPage(metaPageID, false); err != nil {
		return nil, err
	}
	if magic != btreeMagic {
		return nil, fmt.Errorf("%w: bad meta page magic", ErrCorruptNode)
	}
	return bt, nil
}

func (bt *BTree) setRoot(id PageID) error {
	meta, err := bt.bp.FetchPage(metaPageID)
	if err != nil {
		return err
	}
	buf := meta.Data()
	binary.LittleEndian.PutUint32(buf[0:4], btreeMagic)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(id))
	binary.LittleEndian.PutUint16(buf[8:10], uint16(bt.leafMax))
	binary.LittleEndian.PutUint16(buf[10:12], uint16(bt.internalMax))
	if err := bt.bp.UnpinPage(metaPageID, true); err != nil {
		return err
	}
	bt.root = id
	return nil
}

// Search returns the RID stored under key.
func (bt *BTree) Search(key int64) (RID, bool, error) {
	leaf, err := bt.findLeaf(key)
	if err != nil {
		return RID{}, false, err
	}
	i := lowerBound(leaf.keys, key)
	if i < len(leaf.keys) && leaf.keys[i] == key {
		return leaf.rids[i], true, nil
	}
	return RID{}, false, nil
}

// RangeScan calls fn for every key in [lo, hi] in ascending order, stopping
// early if fn returns false.
func (bt *BTree) RangeScan(lo, hi int64, fn func(key int64, rid RID) bool) error {
	if lo > hi {
		return nil
	}
	leaf, err := bt.findLeaf(lo)
	if err != nil {
		return err
	}
	for i := lowerBound(leaf.keys, lo); ; i = 0 {
		for ; i < len(leaf.keys); i++ {
			if leaf.keys[i] > hi || !fn(leaf.keys[i], leaf.rids[i]) {
				return nil
			}
		}
		if leaf.next == InvalidPageID {
			return nil
		}
		if leaf, err = bt.readNode(leaf.next); err != nil {
			return err
		}
	}
}

// findLeaf descends from the root to the leaf whose range contains key.
func (bt *BTree) findLeaf(key int64) (*node, error) {
	n, err := bt.readNode(bt.root)
	for err == nil && !n.leaf {
		n, err = bt.readNode(n.children[childIndex(n.keys, key)])
	}
	return n, err
}

// Insert adds key -> rid. It returns ErrDuplicateKey if key already exists.
func (bt *BTree) Insert(key int64, rid RID) error {
	split, err := bt.insert(bt.root, key, rid)
	if err != nil || split == nil {
		return err
	}
	// The root split: grow the tree by one level with a new root above it.
	newRoot, err := bt.allocNode(&node{
		keys:     []int64{split.key},
		children: []PageID{bt.root, split.right},
	})
	if err != nil {
		return err
	}
	return bt.setRoot(newRoot)
}

// splitResult tells the parent that a child split: key separates the old
// child from its new right sibling.
type splitResult struct {
	key   int64
	right PageID
}

func (bt *BTree) insert(id PageID, key int64, rid RID) (*splitResult, error) {
	n, err := bt.readNode(id)
	if err != nil {
		return nil, err
	}

	if n.leaf {
		i := lowerBound(n.keys, key)
		if i < len(n.keys) && n.keys[i] == key {
			return nil, fmt.Errorf("%w: %d", ErrDuplicateKey, key)
		}
		n.keys = insertAt(n.keys, i, key)
		n.rids = insertAt(n.rids, i, rid)
		if len(n.keys) <= bt.leafMax {
			return nil, bt.writeNode(id, n)
		}
		// Leaf split: the right half's first key is copied up to the parent
		// and also stays in the leaf, because values live only in leaves.
		mid := len(n.keys) / 2
		right := &node{
			leaf: true,
			keys: append([]int64(nil), n.keys[mid:]...),
			rids: append([]RID(nil), n.rids[mid:]...),
			next: n.next,
		}
		rightID, err := bt.allocNode(right)
		if err != nil {
			return nil, err
		}
		n.keys, n.rids, n.next = n.keys[:mid], n.rids[:mid], rightID
		if err := bt.writeNode(id, n); err != nil {
			return nil, err
		}
		return &splitResult{key: right.keys[0], right: rightID}, nil
	}

	i := childIndex(n.keys, key)
	split, err := bt.insert(n.children[i], key, rid)
	if err != nil || split == nil {
		return nil, err
	}
	n.keys = insertAt(n.keys, i, split.key)
	n.children = insertAt(n.children, i+1, split.right)
	if len(n.keys) <= bt.internalMax {
		return nil, bt.writeNode(id, n)
	}
	// Internal split: the middle key moves up to the parent and is removed
	// here, since a separator only needs to exist in one place.
	mid := len(n.keys) / 2
	up := n.keys[mid]
	right := &node{
		keys:     append([]int64(nil), n.keys[mid+1:]...),
		children: append([]PageID(nil), n.children[mid+1:]...),
		next:     InvalidPageID,
	}
	rightID, err := bt.allocNode(right)
	if err != nil {
		return nil, err
	}
	n.keys, n.children = n.keys[:mid], n.children[:mid+1]
	if err := bt.writeNode(id, n); err != nil {
		return nil, err
	}
	return &splitResult{key: up, right: rightID}, nil
}

// Delete removes key, rebalancing underfull nodes by borrowing from or merging
// with a sibling. It returns ErrKeyNotFound if key is absent.
func (bt *BTree) Delete(key int64) error {
	if _, err := bt.delete(bt.root, key); err != nil {
		return err
	}
	// A merge may have taken the root's last separator: the root then has a
	// single child, which becomes the new root (the tree shrinks one level).
	root, err := bt.readNode(bt.root)
	if err != nil {
		return err
	}
	if !root.leaf && len(root.keys) == 0 {
		return bt.setRoot(root.children[0])
	}
	return nil
}

// delete removes key from the subtree at id and reports whether that node is
// now below minimum occupancy. The caller (its parent) fixes the underflow.
func (bt *BTree) delete(id PageID, key int64) (underflow bool, err error) {
	n, err := bt.readNode(id)
	if err != nil {
		return false, err
	}

	if n.leaf {
		i := lowerBound(n.keys, key)
		if i == len(n.keys) || n.keys[i] != key {
			return false, fmt.Errorf("%w: %d", ErrKeyNotFound, key)
		}
		n.keys = removeAt(n.keys, i)
		n.rids = removeAt(n.rids, i)
		return len(n.keys) < bt.minKeys(n), bt.writeNode(id, n)
	}

	i := childIndex(n.keys, key)
	childUnder, err := bt.delete(n.children[i], key)
	if err != nil || !childUnder {
		return false, err
	}
	if err := bt.rebalance(n, i); err != nil {
		return false, err
	}
	return len(n.keys) < bt.minKeys(n), bt.writeNode(id, n)
}

// minKeys is the fewest keys a non-root node may hold. Splits never produce
// less, and merging an underfull node (min-1) with a sibling at min always
// fits in one node.
func (bt *BTree) minKeys(n *node) int {
	if n.leaf {
		return bt.leafMax / 2
	}
	return bt.internalMax / 2
}

// rebalance fixes parent.children[i], which has one key fewer than the
// minimum. It prefers borrowing one entry from a sibling (no structural
// change) and merges only when both siblings are at the minimum. It updates
// parent in memory; the caller writes parent.
func (bt *BTree) rebalance(parent *node, i int) error {
	childID := parent.children[i]
	child, err := bt.readNode(childID)
	if err != nil {
		return err
	}

	var left, right *node
	var leftID, rightID PageID
	if i > 0 {
		leftID = parent.children[i-1]
		if left, err = bt.readNode(leftID); err != nil {
			return err
		}
		if len(left.keys) > bt.minKeys(left) {
			borrowFromLeft(parent, i, left, child)
			return bt.writeNodes(leftID, left, childID, child)
		}
	}
	if i < len(parent.children)-1 {
		rightID = parent.children[i+1]
		if right, err = bt.readNode(rightID); err != nil {
			return err
		}
		if len(right.keys) > bt.minKeys(right) {
			borrowFromRight(parent, i, child, right)
			return bt.writeNodes(childID, child, rightID, right)
		}
	}

	// Both siblings are at the minimum: merge with one of them. The emptied
	// right-hand page is dropped from the tree (not yet recycled).
	if left != nil {
		mergeInto(left, child, parent.keys[i-1])
		parent.keys = removeAt(parent.keys, i-1)
		parent.children = removeAt(parent.children, i)
		return bt.writeNode(leftID, left)
	}
	mergeInto(child, right, parent.keys[i])
	parent.keys = removeAt(parent.keys, i)
	parent.children = removeAt(parent.children, i+1)
	return bt.writeNode(childID, child)
}

// borrowFromLeft moves the left sibling's last entry into child.
func borrowFromLeft(parent *node, i int, left, child *node) {
	last := len(left.keys) - 1
	if child.leaf {
		child.keys = insertAt(child.keys, 0, left.keys[last])
		child.rids = insertAt(child.rids, 0, left.rids[last])
		left.keys, left.rids = left.keys[:last], left.rids[:last]
		parent.keys[i-1] = child.keys[0]
		return
	}
	// Rotate right through the parent: the separator comes down into child,
	// the left sibling's last key goes up to replace it.
	child.keys = insertAt(child.keys, 0, parent.keys[i-1])
	child.children = insertAt(child.children, 0, left.children[last+1])
	parent.keys[i-1] = left.keys[last]
	left.keys, left.children = left.keys[:last], left.children[:last+1]
}

// borrowFromRight moves the right sibling's first entry into child.
func borrowFromRight(parent *node, i int, child, right *node) {
	if child.leaf {
		child.keys = append(child.keys, right.keys[0])
		child.rids = append(child.rids, right.rids[0])
		right.keys, right.rids = removeAt(right.keys, 0), removeAt(right.rids, 0)
		parent.keys[i] = right.keys[0]
		return
	}
	// Rotate left through the parent.
	child.keys = append(child.keys, parent.keys[i])
	child.children = append(child.children, right.children[0])
	parent.keys[i] = right.keys[0]
	right.keys, right.children = removeAt(right.keys, 0), removeAt(right.children, 0)
}

// mergeInto appends right's entries to left. For internal nodes the parent's
// separator comes down between them; for leaves it is simply dropped.
func mergeInto(left, right *node, sep int64) {
	if left.leaf {
		left.keys = append(left.keys, right.keys...)
		left.rids = append(left.rids, right.rids...)
		left.next = right.next
		return
	}
	left.keys = append(append(left.keys, sep), right.keys...)
	left.children = append(left.children, right.children...)
}

// readNode decodes page id into a node. decodeNode copies everything out of
// the page, so the pin is released before returning.
func (bt *BTree) readNode(id PageID) (*node, error) {
	p, err := bt.bp.FetchPage(id)
	if err != nil {
		return nil, err
	}
	n, err := decodeNode(p.Data())
	if uerr := bt.bp.UnpinPage(id, false); uerr != nil && err == nil {
		err = uerr
	}
	if err != nil {
		return nil, fmt.Errorf("page %d: %w", id, err)
	}
	return n, nil
}

func (bt *BTree) writeNode(id PageID, n *node) error {
	p, err := bt.bp.FetchPage(id)
	if err != nil {
		return err
	}
	n.encode(p.Data())
	return bt.bp.UnpinPage(id, true)
}

func (bt *BTree) writeNodes(id1 PageID, n1 *node, id2 PageID, n2 *node) error {
	if err := bt.writeNode(id1, n1); err != nil {
		return err
	}
	return bt.writeNode(id2, n2)
}

func (bt *BTree) allocNode(n *node) (PageID, error) {
	p, err := bt.bp.NewPage()
	if err != nil {
		return InvalidPageID, err
	}
	n.encode(p.Data())
	return p.ID(), bt.bp.UnpinPage(p.ID(), true)
}

// lowerBound returns the first index whose key is >= key.
func lowerBound(keys []int64, key int64) int {
	return sort.Search(len(keys), func(i int) bool { return keys[i] >= key })
}

// childIndex returns which child of an internal node covers key: the first
// separator greater than key bounds it from above.
func childIndex(keys []int64, key int64) int {
	return sort.Search(len(keys), func(i int) bool { return keys[i] > key })
}

func insertAt[T any](s []T, i int, v T) []T {
	var zero T
	s = append(s, zero)
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

func removeAt[T any](s []T, i int) []T {
	return append(s[:i], s[i+1:]...)
}
