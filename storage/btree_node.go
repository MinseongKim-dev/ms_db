package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// RID (record ID) locates a tuple: the page it is in and its slot there.
type RID struct {
	Page PageID
	Slot SlotID
}

var ErrCorruptNode = errors.New("storage: corrupt B+Tree node")

const (
	nodeTypeLeaf     = 1
	nodeTypeInternal = 2

	// Node header: type(1) | numKeys(2) | next leaf PageID(4) | pad(1).
	nodeHeaderSize = 8

	leafEntrySize     = 8 + 4 + 2 // key | RID.Page | RID.Slot
	internalEntrySize = 8 + 4     // key | child PageID

	// Largest key counts a node page can hold. An internal node stores one
	// more child than keys: child0 | (key1, child1) | (key2, child2) ...
	maxLeafKeys     = (PageDataSize - nodeHeaderSize) / leafEntrySize
	maxInternalKeys = (PageDataSize - nodeHeaderSize - 4) / internalEntrySize
)

// node is the decoded, in-memory form of one B+Tree page. The tree reads a
// node, edits these slices, and encodes it back; it never edits page bytes
// in place.
type node struct {
	leaf     bool
	keys     []int64
	rids     []RID    // leaf only: rids[i] belongs to keys[i]
	children []PageID // internal only: len(children) == len(keys)+1
	next     PageID   // leaf only: right sibling, or InvalidPageID
}

// encode writes n into buf (PageDataSize bytes).
func (n *node) encode(buf []byte) {
	for i := range buf {
		buf[i] = 0
	}
	if n.leaf {
		if len(n.keys) > maxLeafKeys {
			panic(fmt.Sprintf("storage: leaf with %d keys does not fit a page", len(n.keys)))
		}
		buf[0] = nodeTypeLeaf
	} else {
		if len(n.keys) > maxInternalKeys {
			panic(fmt.Sprintf("storage: internal node with %d keys does not fit a page", len(n.keys)))
		}
		buf[0] = nodeTypeInternal
	}
	binary.LittleEndian.PutUint16(buf[1:3], uint16(len(n.keys)))
	binary.LittleEndian.PutUint32(buf[3:7], uint32(n.next))

	off := nodeHeaderSize
	if n.leaf {
		for i, k := range n.keys {
			binary.LittleEndian.PutUint64(buf[off:], uint64(k))
			binary.LittleEndian.PutUint32(buf[off+8:], uint32(n.rids[i].Page))
			binary.LittleEndian.PutUint16(buf[off+12:], uint16(n.rids[i].Slot))
			off += leafEntrySize
		}
		return
	}
	binary.LittleEndian.PutUint32(buf[off:], uint32(n.children[0]))
	off += 4
	for i, k := range n.keys {
		binary.LittleEndian.PutUint64(buf[off:], uint64(k))
		binary.LittleEndian.PutUint32(buf[off+8:], uint32(n.children[i+1]))
		off += internalEntrySize
	}
}

// decodeNode parses a node page, rejecting headers that would read past the
// page.
func decodeNode(buf []byte) (*node, error) {
	n := &node{}
	count := int(binary.LittleEndian.Uint16(buf[1:3]))
	n.next = PageID(binary.LittleEndian.Uint32(buf[3:7]))
	off := nodeHeaderSize

	switch buf[0] {
	case nodeTypeLeaf:
		if count > maxLeafKeys {
			return nil, fmt.Errorf("%w: leaf claims %d keys", ErrCorruptNode, count)
		}
		n.leaf = true
		n.keys = make([]int64, count)
		n.rids = make([]RID, count)
		for i := 0; i < count; i++ {
			n.keys[i] = int64(binary.LittleEndian.Uint64(buf[off:]))
			n.rids[i] = RID{
				Page: PageID(binary.LittleEndian.Uint32(buf[off+8:])),
				Slot: SlotID(binary.LittleEndian.Uint16(buf[off+12:])),
			}
			off += leafEntrySize
		}
	case nodeTypeInternal:
		if count > maxInternalKeys {
			return nil, fmt.Errorf("%w: internal node claims %d keys", ErrCorruptNode, count)
		}
		n.keys = make([]int64, count)
		n.children = make([]PageID, count+1)
		n.children[0] = PageID(binary.LittleEndian.Uint32(buf[off:]))
		off += 4
		for i := 0; i < count; i++ {
			n.keys[i] = int64(binary.LittleEndian.Uint64(buf[off:]))
			n.children[i+1] = PageID(binary.LittleEndian.Uint32(buf[off+8:]))
			off += internalEntrySize
		}
	default:
		return nil, fmt.Errorf("%w: unknown node type %d", ErrCorruptNode, buf[0])
	}
	return n, nil
}
