package sourcecontent_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

func entriesOf(files []sourcecontent.File) []sourcecontent.Entry {
	entries := make([]sourcecontent.Entry, len(files))
	for i, file := range files {
		sum := sha256.Sum256(file.Data)
		entries[i] = sourcecontent.Entry{Path: file.Path, Size: int64(len(file.Data)), SHA256: hex.EncodeToString(sum[:])}
	}
	return entries
}

// A report that lists its files without their bytes gives the digest the
// files give, on every shared case.
func TestDigestOfEntriesMatchesEveryCase(t *testing.T) {
	t.Parallel()
	cases := readFixtures(t)
	for _, c := range append(append([]fixtureCase(nil), cases.Accepted...), cases.Generated...) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			digest, err := sourcecontent.DigestOfEntries(entriesOf(c.files(t)))
			if err != nil || digest != c.Digest {
				t.Fatalf("DigestOfEntries() = %q, %v; want %q", digest, err, c.Digest)
			}
		})
	}
	refusals := map[string]error{
		"empty":          sourcecontent.ErrEmpty,
		"too-many-files": sourcecontent.ErrTooManyFiles,
		"invalid-path":   sourcecontent.ErrInvalidPath,
		"duplicate":      sourcecontent.ErrDuplicate,
		"path-conflict":  sourcecontent.ErrPathConflict,
	}
	for _, c := range cases.Refused {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if digest, err := sourcecontent.DigestOfEntries(entriesOf(c.files(t))); !errors.Is(err, refusals[c.Refusal]) || digest != "" {
				t.Fatalf("DigestOfEntries() = %q, %v; want %v", digest, err, refusals[c.Refusal])
			}
		})
	}
}

func TestDigestOfEntriesRefusesAMalformedLine(t *testing.T) {
	t.Parallel()
	valid := strings.Repeat("a", 64)
	for name, entry := range map[string]sourcecontent.Entry{
		"negative size":     {Path: "a.sql", Size: -1, SHA256: valid},
		"uppercase digest":  {Path: "a.sql", Size: 1, SHA256: strings.Repeat("A", 64)},
		"short digest":      {Path: "a.sql", Size: 1, SHA256: valid[:63]},
		"non-hex digest":    {Path: "a.sql", Size: 1, SHA256: strings.Repeat("g", 64)},
		"digest with space": {Path: "a.sql", Size: 1, SHA256: valid[:63] + " "},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := sourcecontent.DigestOfEntries([]sourcecontent.Entry{entry}); !errors.Is(err, sourcecontent.ErrInvalidEntry) {
				t.Fatalf("DigestOfEntries() = %v, want %v", err, sourcecontent.ErrInvalidEntry)
			}
		})
	}
}

func TestReadTreeReadsWhatADigestNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	files := []sourcecontent.File{{Path: "0001.up.sql", Data: []byte("one\n")}, {Path: "nested/0002.up.sql", Data: []byte("two\r\n")}}
	if err := sourcecontent.Write(root, files); err != nil {
		t.Fatal(err)
	}
	read, err := sourcecontent.ReadTree(root, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := sourcecontent.Digest(files)
	if got, err := sourcecontent.Digest(read); err != nil || got != want {
		t.Fatalf("the tree gives %q, %v; want %q", got, err, want)
	}
	if _, err := sourcecontent.ReadTree(root, 8); err == nil {
		t.Fatal("ReadTree read more than its bound")
	}
	if err := os.Symlink("0001.up.sql", filepath.Join(root, "link.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err := sourcecontent.ReadTree(root, 1<<10); !errors.Is(err, sourcecontent.ErrNotRegular) {
		t.Fatalf("ReadTree() = %v, want the link refused", err)
	}
	if _, err := sourcecontent.ReadFile(filepath.Join(root, "link.sql"), "schema.hcl", 1<<10); !errors.Is(err, sourcecontent.ErrNotRegular) {
		t.Fatalf("ReadFile() = %v, want the link refused", err)
	}
	if _, err := sourcecontent.ReadTree(t.TempDir(), 1<<10); !errors.Is(err, sourcecontent.ErrEmpty) {
		t.Fatalf("ReadTree() of an empty directory = %v, want %v", err, sourcecontent.ErrEmpty)
	}
}
