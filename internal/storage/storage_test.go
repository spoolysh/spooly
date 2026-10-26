package storage_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/boxysh/boxy/internal/storage"
	"github.com/boxysh/boxy/internal/storage/local"
	"github.com/boxysh/boxy/internal/storage/memory"
)

// helloCRC32C is the CRC32C of "hello" as GCS and S3 report it.
const helloCRC32C = "mnG7TA=="

func TestEncodeCRC32C(t *testing.T) {
	h := storage.NewCRC32C()
	h.Write([]byte("hello"))
	if got := storage.EncodeCRC32C(h.Sum32()); got != helloCRC32C {
		t.Fatalf("EncodeCRC32C = %q, want %q", got, helloCRC32C)
	}
}

func TestPutReportsChecksum(t *testing.T) {
	disk, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	for name, store := range map[string]storage.Storage{"memory": memory.New(), "local": disk} {
		stored, err := store.Put(t.Context(), "01m5w32mczebq4dpgdjy8p8twa", "text/plain", strings.NewReader("hello"))
		if err != nil {
			t.Fatalf("%s: Put: %v", name, err)
		}
		if stored.Size != 5 || stored.Checksum != helloCRC32C {
			t.Errorf("%s: Put = %+v, want size 5 and checksum %s", name, stored, helloCRC32C)
		}
	}
}

func TestMissingFileIsErrNotFound(t *testing.T) {
	disk, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	for name, store := range map[string]storage.Storage{"memory": memory.New(), "local": disk} {
		if _, err := store.Get(t.Context(), "01m5w32mczebq4dpgdjy8p8twb"); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("%s: Get = %v, want ErrNotFound", name, err)
		}
		if _, err := store.GetRange(t.Context(), "01m5w32mczebq4dpgdjy8p8twb", 0, 1); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("%s: GetRange = %v, want ErrNotFound", name, err)
		}
	}
}

func TestGetRange(t *testing.T) {
	disk, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	for name, store := range map[string]storage.Storage{"memory": memory.New(), "local": disk} {
		if _, err := store.Put(t.Context(), "01m5w32mczebq4dpgdjy8p8twa", "", strings.NewReader("hello world")); err != nil {
			t.Fatalf("%s: Put: %v", name, err)
		}
		rc, err := store.GetRange(t.Context(), "01m5w32mczebq4dpgdjy8p8twa", 6, 3)
		if err != nil {
			t.Fatalf("%s: GetRange: %v", name, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if string(b) != "wor" {
			t.Errorf("%s: GetRange = %q, want %q", name, b, "wor")
		}
	}
}
