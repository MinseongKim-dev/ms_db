package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// SlotID identifies a tuple within a page. It stays stable for the tuple's
// lifetime, even when compaction moves the tuple's bytes.
type SlotID uint16

var (
	ErrPageFull           = errors.New("storage: not enough free space in page")
	ErrTupleTooLarge      = errors.New("storage: tuple larger than MaxTupleSize")
	ErrSlotNotFound       = errors.New("storage: slot does not exist or was deleted")
	ErrCorruptSlottedPage = errors.New("storage: corrupt slotted page header")
)

const (
	slottedHeaderSize = 4 // numSlots uint16 | freeSpaceEnd uint16
	slotSize          = 4 // offset uint16 | length uint16

	// MaxTupleSize is the largest tuple that fits in an empty page.
	MaxTupleSize = PageDataSize - slottedHeaderSize - slotSize

	// deletedOffset marks a free slot. Live tuples always sit after the
	// header, so offset 0 is never a valid tuple position.
	deletedOffset = 0
)

// SlottedPage stores variable-length tuples in a page buffer.
//
// Layout (little-endian):
//
//	[0:2)  numSlots
//	[2:4)  freeSpaceEnd: start of the tuple data region
//	[4:4+4*numSlots)  slot array; slot i = offset uint16 | length uint16
//	...free space...
//	[freeSpaceEnd:len) tuple data, growing downward from the end
//
// The slot array grows up and tuple data grows down, meeting in the middle.
// Delete marks the slot free (trimming trailing free slots); tuple space is
// reclaimed lazily by compaction when an insert needs contiguous room.
type SlottedPage struct {
	data []byte
}

// InitSlottedPage formats buf as an empty slotted page.
func InitSlottedPage(buf []byte) *SlottedPage {
	if len(buf) < slottedHeaderSize+slotSize || len(buf) > 0xFFFF {
		panic(fmt.Sprintf("storage: invalid slotted page buffer size %d", len(buf)))
	}
	p := &SlottedPage{data: buf}
	p.setNumSlots(0)
	p.setFreeSpaceEnd(len(buf))
	return p
}

// LoadSlottedPage wraps an existing page buffer, validating its header so a
// corrupt page is rejected instead of causing out-of-range reads later.
func LoadSlottedPage(buf []byte) (*SlottedPage, error) {
	if len(buf) < slottedHeaderSize+slotSize || len(buf) > 0xFFFF {
		return nil, fmt.Errorf("%w: buffer size %d", ErrCorruptSlottedPage, len(buf))
	}
	p := &SlottedPage{data: buf}
	end := p.freeSpaceEnd()
	if p.headerEnd() > end || end > len(buf) {
		return nil, fmt.Errorf("%w: numSlots %d, freeSpaceEnd %d", ErrCorruptSlottedPage, p.numSlots(), end)
	}
	for i := 0; i < p.numSlots(); i++ {
		off, n := p.slot(i)
		if off == deletedOffset {
			continue
		}
		if off < end || off+n > len(buf) {
			return nil, fmt.Errorf("%w: slot %d at [%d:%d)", ErrCorruptSlottedPage, i, off, off+n)
		}
	}
	return p, nil
}

// Bytes returns the underlying page buffer (for writing via DiskManager).
func (p *SlottedPage) Bytes() []byte { return p.data }

// NumSlots returns the size of the slot array, including deleted slots.
func (p *SlottedPage) NumSlots() int { return p.numSlots() }

// CanInsert reports whether Insert would accept a tuple of n bytes.
func (p *SlottedPage) CanInsert(n int) bool {
	return n >= 0 && n <= MaxTupleSize && n <= p.available()
}

// available returns the largest tuple Insert could store, counting space
// reclaimable by compaction and the cost of a new slot if no deleted slot can
// be reused. It is negative when not even an empty tuple fits; it is kept
// internal because clamping it to 0 would make "a 0-byte tuple fits" and
// "nothing fits" look the same.
func (p *SlottedPage) available() int {
	free := len(p.data) - p.headerEnd() - p.liveBytes()
	if p.findDeletedSlot() < 0 {
		free -= slotSize
	}
	return free
}

// Insert stores a copy of tuple and returns its slot ID. It returns
// ErrTupleTooLarge or ErrPageFull without modifying the page on failure.
func (p *SlottedPage) Insert(tuple []byte) (SlotID, error) {
	if len(tuple) > MaxTupleSize {
		return 0, fmt.Errorf("%w: %d > %d", ErrTupleTooLarge, len(tuple), MaxTupleSize)
	}
	if !p.CanInsert(len(tuple)) {
		return 0, fmt.Errorf("%w: need %d, have %d", ErrPageFull, len(tuple), p.available())
	}

	slot := p.findDeletedSlot()
	newHeaderEnd := p.headerEnd()
	if slot < 0 {
		slot = p.numSlots()
		newHeaderEnd += slotSize
	}
	// Enough total space is guaranteed above; compact if it is fragmented.
	if p.freeSpaceEnd()-newHeaderEnd < len(tuple) {
		p.compact()
	}

	off := p.freeSpaceEnd() - len(tuple)
	copy(p.data[off:], tuple)
	p.setFreeSpaceEnd(off)
	if slot == p.numSlots() {
		p.setNumSlots(slot + 1)
	}
	p.setSlot(slot, off, len(tuple))
	return SlotID(slot), nil
}

// Get returns a copy of the tuple in slot id.
func (p *SlottedPage) Get(id SlotID) ([]byte, error) {
	off, n, err := p.liveSlot(id)
	if err != nil {
		return nil, err
	}
	out := make([]byte, n)
	copy(out, p.data[off:off+n])
	return out, nil
}

// Delete frees slot id. The slot may be reused by a later Insert.
func (p *SlottedPage) Delete(id SlotID) error {
	if _, _, err := p.liveSlot(id); err != nil {
		return err
	}
	p.setSlot(int(id), deletedOffset, 0)
	// Shrink the slot array past trailing deleted slots so their header space
	// is returned. Deleted slots in the middle must stay: removing them would
	// shift the IDs of every later slot and break RIDs held elsewhere.
	n := p.numSlots()
	for n > 0 {
		if off, _ := p.slot(n - 1); off != deletedOffset {
			break
		}
		n--
	}
	p.setNumSlots(n)
	return nil
}

func (p *SlottedPage) liveSlot(id SlotID) (off, n int, err error) {
	if int(id) >= p.numSlots() {
		return 0, 0, fmt.Errorf("%w: %d (numSlots %d)", ErrSlotNotFound, id, p.numSlots())
	}
	off, n = p.slot(int(id))
	if off == deletedOffset {
		return 0, 0, fmt.Errorf("%w: %d is deleted", ErrSlotNotFound, id)
	}
	return off, n, nil
}

// compact moves all live tuples to the end of the page so free space is one
// contiguous block. Slot IDs are unchanged; only their offsets move.
func (p *SlottedPage) compact() {
	type live struct{ slot, n int }
	var tuples []live
	tmp := make([]byte, 0, p.liveBytes())
	for i := 0; i < p.numSlots(); i++ {
		off, n := p.slot(i)
		if off == deletedOffset {
			continue
		}
		tmp = append(tmp, p.data[off:off+n]...)
		tuples = append(tuples, live{i, n})
	}
	end := len(p.data)
	pos := 0
	for _, t := range tuples {
		end -= t.n
		copy(p.data[end:], tmp[pos:pos+t.n])
		p.setSlot(t.slot, end, t.n)
		pos += t.n
	}
	p.setFreeSpaceEnd(end)
}

func (p *SlottedPage) liveBytes() int {
	total := 0
	for i := 0; i < p.numSlots(); i++ {
		if off, n := p.slot(i); off != deletedOffset {
			total += n
		}
	}
	return total
}

func (p *SlottedPage) findDeletedSlot() int {
	for i := 0; i < p.numSlots(); i++ {
		if off, _ := p.slot(i); off == deletedOffset {
			return i
		}
	}
	return -1
}

func (p *SlottedPage) headerEnd() int { return slottedHeaderSize + slotSize*p.numSlots() }

func (p *SlottedPage) numSlots() int { return int(binary.LittleEndian.Uint16(p.data[0:2])) }
func (p *SlottedPage) setNumSlots(n int) {
	binary.LittleEndian.PutUint16(p.data[0:2], uint16(n))
}

func (p *SlottedPage) freeSpaceEnd() int { return int(binary.LittleEndian.Uint16(p.data[2:4])) }
func (p *SlottedPage) setFreeSpaceEnd(off int) {
	binary.LittleEndian.PutUint16(p.data[2:4], uint16(off))
}

func (p *SlottedPage) slot(i int) (off, n int) {
	b := p.data[slottedHeaderSize+slotSize*i:]
	return int(binary.LittleEndian.Uint16(b[0:2])), int(binary.LittleEndian.Uint16(b[2:4]))
}

func (p *SlottedPage) setSlot(i, off, n int) {
	b := p.data[slottedHeaderSize+slotSize*i:]
	binary.LittleEndian.PutUint16(b[0:2], uint16(off))
	binary.LittleEndian.PutUint16(b[2:4], uint16(n))
}
