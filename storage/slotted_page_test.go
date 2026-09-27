package storage

import (
	"bytes"
	"errors"
	"math/rand"
	"path/filepath"
	"testing"
)

// Phase 1 completion criterion (SlottedPage):
// Variable-length tuples from 0 bytes up to the maximum a single page can hold
// must survive insert, delete and re-insert without corruption, and inserting
// when there is not enough space must return a clear error.

func newPage(t *testing.T) *SlottedPage {
	t.Helper()
	return InitSlottedPage(make([]byte, PageDataSize))
}

func mustInsert(t *testing.T, p *SlottedPage, tuple []byte) SlotID {
	t.Helper()
	id, err := p.Insert(tuple)
	if err != nil {
		t.Fatalf("Insert(len %d): %v", len(tuple), err)
	}
	return id
}

func mustGet(t *testing.T, p *SlottedPage, id SlotID) []byte {
	t.Helper()
	got, err := p.Get(id)
	if err != nil {
		t.Fatalf("Get(%d): %v", id, err)
	}
	return got
}

func fill(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	r.Read(b)
	return b
}

func TestMaxTupleSize(t *testing.T) {
	if MaxTupleSize != PageDataSize-slottedHeaderSize-slotSize {
		t.Fatalf("MaxTupleSize = %d, want %d", MaxTupleSize, PageDataSize-slottedHeaderSize-slotSize)
	}
}

func TestEmptyTuple(t *testing.T) {
	p := newPage(t)
	id := mustInsert(t, p, []byte{})
	got := mustGet(t, p, id)
	if len(got) != 0 {
		t.Fatalf("empty tuple read back with len %d", len(got))
	}
	// A live empty tuple must be distinguishable from a deleted slot.
	if err := p.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(id); !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("Get after delete: got %v, want ErrSlotNotFound", err)
	}
}

func TestMaxSizeTuple(t *testing.T) {
	p := newPage(t)
	r := rand.New(rand.NewSource(1))
	want := fill(r, MaxTupleSize)
	id := mustInsert(t, p, want)
	if !bytes.Equal(mustGet(t, p, id), want) {
		t.Fatal("max-size tuple corrupted")
	}
	// Even an empty tuple needs a slot, so the page is now full.
	if p.CanInsert(0) {
		t.Fatal("CanInsert(0) = true on a full page")
	}
	if _, err := p.Insert(nil); !errors.Is(err, ErrPageFull) {
		t.Fatalf("Insert into full page: got %v, want ErrPageFull", err)
	}
	if !bytes.Equal(mustGet(t, p, id), want) {
		t.Fatal("max-size tuple corrupted by rejected insert")
	}
}

func TestTupleTooLarge(t *testing.T) {
	p := newPage(t)
	if _, err := p.Insert(make([]byte, MaxTupleSize+1)); !errors.Is(err, ErrTupleTooLarge) {
		t.Fatalf("got %v, want ErrTupleTooLarge", err)
	}
	if p.NumSlots() != 0 {
		t.Fatal("rejected insert changed the page")
	}
}

func TestFillUntilFull(t *testing.T) {
	p := newPage(t)
	r := rand.New(rand.NewSource(2))
	want := map[SlotID][]byte{}
	for {
		tuple := fill(r, r.Intn(200))
		id, err := p.Insert(tuple)
		if errors.Is(err, ErrPageFull) {
			if p.CanInsert(len(tuple)) {
				t.Fatalf("ErrPageFull but CanInsert(%d) = true", len(tuple))
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		want[id] = tuple
	}
	for id, w := range want {
		if !bytes.Equal(mustGet(t, p, id), w) {
			t.Fatalf("slot %d corrupted", id)
		}
	}
}

func TestFailedInsertLeavesPageUnchanged(t *testing.T) {
	p := newPage(t)
	r := rand.New(rand.NewSource(3))
	a := fill(r, 1000)
	idA := mustInsert(t, p, a)
	before := append([]byte(nil), p.Bytes()...)

	if _, err := p.Insert(make([]byte, MaxTupleSize)); !errors.Is(err, ErrPageFull) {
		t.Fatalf("got %v, want ErrPageFull", err)
	}
	if !bytes.Equal(before, p.Bytes()) {
		t.Fatal("failed insert modified the page")
	}
	if !bytes.Equal(mustGet(t, p, idA), a) {
		t.Fatal("existing tuple corrupted by failed insert")
	}
}

func TestCanInsertBounds(t *testing.T) {
	p := newPage(t)
	if !p.CanInsert(0) || !p.CanInsert(MaxTupleSize) {
		t.Fatal("empty page must accept 0 and MaxTupleSize")
	}
	if p.CanInsert(MaxTupleSize+1) || p.CanInsert(-1) {
		t.Fatal("CanInsert accepted an impossible size")
	}
}

func TestDeleteTrimsOnlyTrailingSlots(t *testing.T) {
	p := newPage(t)
	a := mustInsert(t, p, []byte("a"))
	b := mustInsert(t, p, []byte("b"))
	c := mustInsert(t, p, []byte("c"))

	// Deleting a middle slot must not shift later slot IDs.
	if err := p.Delete(b); err != nil {
		t.Fatal(err)
	}
	if p.NumSlots() != 3 {
		t.Fatalf("NumSlots after middle delete = %d, want 3", p.NumSlots())
	}
	if !bytes.Equal(mustGet(t, p, c), []byte("c")) {
		t.Fatal("slot c moved after deleting b")
	}

	// Deleting the last slot trims it and the deleted slot before it.
	if err := p.Delete(c); err != nil {
		t.Fatal(err)
	}
	if p.NumSlots() != 1 {
		t.Fatalf("NumSlots after trailing delete = %d, want 1", p.NumSlots())
	}
	if !bytes.Equal(mustGet(t, p, a), []byte("a")) {
		t.Fatal("slot a corrupted by trimming")
	}
	for _, id := range []SlotID{b, c} {
		if _, err := p.Get(id); !errors.Is(err, ErrSlotNotFound) {
			t.Fatalf("Get(%d) after trim: got %v, want ErrSlotNotFound", id, err)
		}
	}
}

func TestInvalidSlot(t *testing.T) {
	p := newPage(t)
	if _, err := p.Get(0); !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("Get on empty page: got %v", err)
	}
	id := mustInsert(t, p, []byte("x"))
	if _, err := p.Get(id + 1); !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("Get past end: got %v", err)
	}
	if err := p.Delete(id + 1); !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("Delete past end: got %v", err)
	}
	if err := p.Delete(id); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(id); !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("double Delete: got %v", err)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	p := newPage(t)
	id := mustInsert(t, p, []byte("hello"))
	got := mustGet(t, p, id)
	got[0] = 'X'
	if !bytes.Equal(mustGet(t, p, id), []byte("hello")) {
		t.Fatal("mutating Get result changed the page")
	}
}

func TestReinsertAfterDeleteNeedsCompaction(t *testing.T) {
	p := newPage(t)
	r := rand.New(rand.NewSource(4))

	// Fill with 100-byte tuples, then free every other one. The freed space is
	// fragmented, so a 500-byte tuple only fits after compaction.
	var ids []SlotID
	want := map[SlotID][]byte{}
	for {
		tuple := fill(r, 100)
		id, err := p.Insert(tuple)
		if errors.Is(err, ErrPageFull) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		want[id] = tuple
	}
	for i, id := range ids {
		if i%2 == 0 {
			if err := p.Delete(id); err != nil {
				t.Fatal(err)
			}
			delete(want, id)
		}
	}

	big := fill(r, 500)
	bigID := mustInsert(t, p, big)
	want[bigID] = big

	// Surviving tuples keep their slot IDs after compaction.
	for id, w := range want {
		if !bytes.Equal(mustGet(t, p, id), w) {
			t.Fatalf("slot %d corrupted after compaction", id)
		}
	}
}

func TestDeleteAllRestoresFullCapacity(t *testing.T) {
	p := newPage(t)
	r := rand.New(rand.NewSource(5))
	var ids []SlotID
	for i := 0; i < 30; i++ {
		ids = append(ids, mustInsert(t, p, fill(r, r.Intn(100))))
	}
	for _, id := range ids {
		if err := p.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	// No leaked space: after churn, an empty page accepts a max-size tuple
	// again (deleted slots are reused, so no slot-array space is lost).
	want := fill(r, MaxTupleSize)
	id := mustInsert(t, p, want)
	if !bytes.Equal(mustGet(t, p, id), want) {
		t.Fatal("max tuple corrupted after churn")
	}
}

// TestRandomChurn checks the page against a map model over many random
// inserts and deletes of sizes spanning 0..MaxTupleSize.
func TestRandomChurn(t *testing.T) {
	p := newPage(t)
	r := rand.New(rand.NewSource(6))
	model := map[SlotID][]byte{}

	sizes := func() int {
		switch r.Intn(10) {
		case 0:
			return 0
		case 1:
			return MaxTupleSize - r.Intn(8)
		case 2:
			return 1000 + r.Intn(2000)
		default:
			return r.Intn(64)
		}
	}

	for step := 0; step < 20000; step++ {
		if len(model) > 0 && r.Intn(2) == 0 {
			// Delete a random live tuple.
			var victim SlotID
			k := r.Intn(len(model))
			for id := range model {
				if k == 0 {
					victim = id
					break
				}
				k--
			}
			if err := p.Delete(victim); err != nil {
				t.Fatalf("step %d: Delete(%d): %v", step, victim, err)
			}
			delete(model, victim)
		} else {
			tuple := fill(r, sizes())
			fits := p.CanInsert(len(tuple))
			id, err := p.Insert(tuple)
			switch {
			case errors.Is(err, ErrPageFull):
				if fits {
					t.Fatalf("step %d: ErrPageFull but CanInsert(%d) was true", step, len(tuple))
				}
			case err != nil:
				t.Fatalf("step %d: Insert: %v", step, err)
			default:
				if !fits {
					t.Fatalf("step %d: inserted %d bytes but CanInsert was false", step, len(tuple))
				}
				if _, dup := model[id]; dup {
					t.Fatalf("step %d: Insert returned live slot %d", step, id)
				}
				model[id] = tuple
			}
		}

		if step%100 == 0 {
			for id, w := range model {
				got, err := p.Get(id)
				if err != nil || !bytes.Equal(got, w) {
					t.Fatalf("step %d: slot %d corrupted (err %v)", step, id, err)
				}
			}
		}
	}
}

func TestSlottedPageSurvivesDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sp.db")
	dm, err := OpenDiskManager(path)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(7))

	p := newPage(t)
	want := map[SlotID][]byte{}
	for _, n := range []int{0, 1, 17, 1000, 2000} {
		tuple := fill(r, n)
		want[mustInsert(t, p, tuple)] = tuple
	}
	pid, _ := dm.AllocatePage()
	if err := dm.WritePage(pid, p.Bytes()); err != nil {
		t.Fatal(err)
	}
	dm.Close()

	dm, err = OpenDiskManager(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dm.Close()
	buf := make([]byte, PageDataSize)
	if err := dm.ReadPage(pid, buf); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSlottedPage(buf)
	if err != nil {
		t.Fatal(err)
	}
	for id, w := range want {
		if !bytes.Equal(mustGet(t, loaded, id), w) {
			t.Fatalf("slot %d corrupted after disk round trip", id)
		}
	}
}

func TestLoadRejectsCorruptHeader(t *testing.T) {
	buf := make([]byte, PageDataSize)
	InitSlottedPage(buf)
	// Claim more slots than fit in the page.
	buf[0], buf[1] = 0xFF, 0xFF
	if _, err := LoadSlottedPage(buf); !errors.Is(err, ErrCorruptSlottedPage) {
		t.Fatalf("got %v, want ErrCorruptSlottedPage", err)
	}
}
