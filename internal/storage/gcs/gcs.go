package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"google.golang.org/api/googleapi"
	gcsapi "google.golang.org/api/storage/v1"

	"github.com/spoolysh/spooly/internal/storage"
)

// Storage stores file bytes in Google Cloud Storage, through its JSON API.
type Storage struct {
	objects *gcsapi.ObjectsService
	bucket  string
}

func New(service *gcsapi.Service, bucket string) *Storage {
	return &Storage{objects: service.Objects, bucket: bucket}
}

func (s *Storage) Put(ctx context.Context, id, contentType string, r io.Reader) (storage.Stored, error) {
	var opts []googleapi.MediaOption
	if contentType != "" {
		opts = append(opts, googleapi.ContentType(contentType))
	}
	obj, err := s.objects.Insert(s.bucket, &gcsapi.Object{Name: id, ContentType: contentType}).
		Media(r, opts...).Context(ctx).Do()
	if err != nil {
		return storage.Stored{}, fmt.Errorf("writing to GCS: %w", err)
	}
	// GCS computes a CRC32C for every object.
	return storage.Stored{Size: int64(obj.Size), Checksum: obj.Crc32c}, nil
}

func (s *Storage) Get(ctx context.Context, id string) (io.ReadCloser, error) {
	resp, err := s.objects.Get(s.bucket, id).Context(ctx).Download()
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", storage.ErrNotFound, id)
		}
		return nil, fmt.Errorf("opening GCS reader: %w", err)
	}
	return resp.Body, nil
}

func (s *Storage) GetRange(ctx context.Context, id string, start, length int64) (io.ReadCloser, error) {
	call := s.objects.Get(s.bucket, id).Context(ctx)
	call.Header().Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+length-1))
	resp, err := call.Download()
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", storage.ErrNotFound, id)
		}
		return nil, fmt.Errorf("opening GCS range reader: %w", err)
	}
	return resp.Body, nil
}

func (s *Storage) Delete(ctx context.Context, id string) error {
	if err := s.objects.Delete(s.bucket, id).Context(ctx).Do(); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("deleting object: %w", err)
	}
	return nil
}

func isNotFound(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusNotFound
}
