package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

var (
	ErrPageNotFound     = errors.New("storage: page not found")
	ErrChecksumMismatch = errors.New("storage: page checksum mismatch (torn or corrupt page)")
	ErrInvalidPageSize  = errors.New("storage: buffer length must equal PageDataSize")
)

// DiskManager reads and writes fixed-size pages in a single file.
//
// On-disk page layout (PageSize bytes):
//
//	[0:4)        CRC32 (IEEE, little-endian) of bytes [4:PageSize)
//	[4:PageSize) caller data (PageDataSize bytes)
//
// Page i lives at file offset i*PageSize. Pages are never freed, so page IDs
// are allocated by appending and are unique for the lifetime of the file.
type DiskManager struct {
	file *os.File

	mu       sync.Mutex // guards numPages
	numPages uint32
}

// OpenDiskManager opens or creates the database file at path.
func OpenDiskManager(path string) (*DiskManager, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	// Round up so a partially written trailing page (torn write) still gets an
	// ID; reading it fails the checksum instead of the data silently vanishing.
	n := (info.Size() + PageSize - 1) / PageSize
	if n >= int64(InvalidPageID) {
		f.Close()
		return nil, fmt.Errorf("storage: file %s too large (%d pages)", path, n)
	}
	return &DiskManager{file: f, numPages: uint32(n)}, nil
}

// NumPages returns the number of allocated pages.
func (dm *DiskManager) NumPages() int {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	return int(dm.numPages)
}

// AllocatePage appends a zero-filled page and returns its ID.
func (dm *DiskManager) AllocatePage() (PageID, error) {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	id := PageID(dm.numPages)
	if id == InvalidPageID {
		return InvalidPageID, errors.New("storage: page ID space exhausted")
	}
	// Write a checksummed zero page so a fresh page reads back cleanly.
	if err := dm.writeRaw(id, make([]byte, PageDataSize)); err != nil {
		return InvalidPageID, err
	}
	dm.numPages++
	return id, nil
}

// WritePage stores data (exactly PageDataSize bytes) in page id.
func (dm *DiskManager) WritePage(id PageID, data []byte) error {
	if len(data) != PageDataSize {
		return fmt.Errorf("%w: got %d", ErrInvalidPageSize, len(data))
	}
	if err := dm.checkExists(id); err != nil {
		return err
	}
	return dm.writeRaw(id, data)
}

// ReadPage reads page id into buf (exactly PageDataSize bytes). It returns
// ErrChecksumMismatch if the stored checksum does not match the data.
func (dm *DiskManager) ReadPage(id PageID, buf []byte) error {
	if len(buf) != PageDataSize {
		return fmt.Errorf("%w: got %d", ErrInvalidPageSize, len(buf))
	}
	if err := dm.checkExists(id); err != nil {
		return err
	}

	raw := make([]byte, PageSize)
	n, err := dm.file.ReadAt(raw, int64(id)*PageSize)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n < PageSize {
		return fmt.Errorf("%w: page %d is %d bytes", ErrChecksumMismatch, id, n)
	}
	stored := binary.LittleEndian.Uint32(raw[:ChecksumSize])
	if crc32.ChecksumIEEE(raw[ChecksumSize:]) != stored {
		return fmt.Errorf("%w: page %d", ErrChecksumMismatch, id)
	}
	copy(buf, raw[ChecksumSize:])
	return nil
}

// Close closes the underlying file.
func (dm *DiskManager) Close() error {
	return dm.file.Close()
}

func (dm *DiskManager) checkExists(id PageID) error {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if uint32(id) >= dm.numPages {
		return fmt.Errorf("%w: id %d (have %d pages)", ErrPageNotFound, id, dm.numPages)
	}
	return nil
}

// writeRaw writes checksum+data as one PageSize write at the page's offset.
func (dm *DiskManager) writeRaw(id PageID, data []byte) error {
	raw := make([]byte, PageSize)
	copy(raw[ChecksumSize:], data)
	binary.LittleEndian.PutUint32(raw[:ChecksumSize], crc32.ChecksumIEEE(raw[ChecksumSize:]))
	_, err := dm.file.WriteAt(raw, int64(id)*PageSize)
	return err
}
