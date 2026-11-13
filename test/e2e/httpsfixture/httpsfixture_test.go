package httpsfixture_test

import (
	"archive/zip"
	"bytes"
	"io"
	"testing"

	"github.com/stokaro/ptah-operator/test/e2e/httpsfixture"
)

// The same definition gives the same bytes, so a capture's digest holds on the
// next run; the republished archive gives another digest for the same file.
func TestFilesAreTheSameBytesOnEveryBuild(t *testing.T) {
	t.Parallel()
	first, err := httpsfixture.Files()
	if err != nil {
		t.Fatal(err)
	}
	second, err := httpsfixture.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 6 {
		t.Fatalf("%d downloads, want 6", len(first))
	}
	for name, data := range first {
		if !bytes.Equal(data, second[name]) {
			t.Errorf("%s differs between two builds", name)
		}
	}
	if httpsfixture.Digest(first[httpsfixture.SchemaArchive]) == httpsfixture.Digest(first[httpsfixture.SchemaArchiveRepublished]) {
		t.Fatal("the republished archive has the digest of the first")
	}
	for _, name := range []string{httpsfixture.SchemaArchive, httpsfixture.SchemaArchiveRepublished} {
		if got := zipMember(t, first[name], httpsfixture.SchemaInArchive); got != httpsfixture.Schema {
			t.Errorf("%s holds %q at the selected path, want the schema", name, got)
		}
	}
}

func zipMember(t *testing.T, data []byte, name string) string {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range archive.File {
		if file.Name != name {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}
	return ""
}
