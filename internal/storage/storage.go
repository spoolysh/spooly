package storage

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash"
	"hash/crc32"
	"io"
)

// ErrNotFound reports that a backend holds no bytes for the requested id.
var ErrNotFound = errors.New("object not found")

// Storage is the abstraction over byte-storage backends (memory, local disk,
// S3, GCS). It holds raw messages keyed by message ID; the database holds
// what's parsed from them, and where their parts are, to read with GetRange.
type Storage interface {
	// Put stores data under id, overwriting any existing content, and
	// reports what was written. contentType is stamped on the object where
	// the backend supports it (S3/GCS) as a write-only courtesy for direct
	// bucket access; serving always uses the database value.
	Put(ctx context.Context, id, contentType string, r io.Reader) (Stored, error)

	// Get retrieves an object's bytes by ID, or an error wrapping ErrNotFound.
	// The caller must close the returned ReadCloser.
	Get(ctx context.Context, id string) (io.ReadCloser, error)

	// GetRange retrieves a byte range by ID, or an error wrapping ErrNotFound.
	// The caller must close the returned ReadCloser.
	GetRange(ctx context.Context, id string, start, length int64) (io.ReadCloser, error)

	// Delete removes an object. Deleting a missing object is not an error.
	Delete(ctx context.Context, id string) error
}

// Stored describes the bytes a Put wrote.
type Stored struct {
	Size int64
	// Checksum is the CRC32C of the bytes, encoded by EncodeCRC32C. Backends
	// report it natively where they can (GCS, S3) and compute it otherwise.
	Checksum string
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// NewCRC32C returns a hash computing the CRC32C (Castagnoli) checksum.
func NewCRC32C() hash.Hash32 {
	return crc32.New(castagnoli)
}

// EncodeCRC32C encodes a CRC32C checksum the way GCS and S3 report it: the
// 4-byte big-endian value, base64-encoded.
func EncodeCRC32C(sum uint32) string {
	return base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, sum))
}
