package storage

import "math"

// PageID identifies a page by its position in the database file.
type PageID uint32

// InvalidPageID is never returned by AllocatePage.
const InvalidPageID PageID = math.MaxUint32

const (
	// PageSize is the size of one page on disk, including its checksum.
	PageSize = 4096
	// ChecksumSize is the size of the CRC32 stored at the start of each page.
	ChecksumSize = 4
	// PageDataSize is the number of bytes available to callers per page.
	PageDataSize = PageSize - ChecksumSize
)
