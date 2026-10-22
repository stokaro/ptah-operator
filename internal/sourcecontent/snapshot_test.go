package sourcecontent_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

// writeSnapshot lays out files the way a mounted snapshot ConfigMap does, with
// the kubelet's own entries beside the keys, and returns the directory and the
// content digest.
func writeSnapshot(t *testing.T, files []sourcecontent.File) (string, string) {
	t.Helper()
	sorted, manifest, digest, err := sourcecontent.Canonical(files)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(sourcecontent.ManifestKey, manifest)
	for index, file := range sorted {
		write(sourcecontent.SnapshotFileKey(index), file.Data)
	}
	if err := os.Mkdir(filepath.Join(dir, "..2026_10_22_09_00_00.000000000"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..2026_10_22_09_00_00.000000000", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	return dir, digest
}

var migrationFiles = []sourcecontent.File{
	{Path: "0002_orders.up.sql", Data: []byte("CREATE TABLE orders (id int);\n")},
	{Path: "0001_init.up.sql", Data: []byte("CREATE TABLE customers (id int);\r\n")},
	{Path: "nested/0003_seed.up.sql", Data: []byte{0xff, 0x00, 'x'}},
}

func TestMaterializeCopiesExactlyTheBoundFiles(t *testing.T) {
	t.Parallel()
	snapshot, digest := writeSnapshot(t, migrationFiles)
	destination := filepath.Join(t.TempDir(), "migrations")
	if err := sourcecontent.Materialize(snapshot, destination, digest); err != nil {
		t.Fatalf("Materialize refused the snapshot it was bound to: %v", err)
	}
	var written int
	err := filepath.WalkDir(destination, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		written++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if written != len(migrationFiles) {
		t.Fatalf("wrote %d files, want %d", written, len(migrationFiles))
	}
	for _, file := range migrationFiles {
		data, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(file.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != string(file.Data) {
			t.Fatalf("%s holds %q, want %q", file.Path, data, file.Data)
		}
	}
}

func TestMaterializeRefusesASnapshotThatDoesNotGiveTheDigest(t *testing.T) {
	t.Parallel()
	for name, tamper := range map[string]func(t *testing.T, dir string) string{
		"another digest": func(t *testing.T, dir string) string {
			_, _, other, err := sourcecontent.Canonical([]sourcecontent.File{{Path: "x.sql", Data: []byte("x")}})
			if err != nil {
				t.Fatal(err)
			}
			return other
		},
		"a changed byte": func(t *testing.T, dir string) string {
			replace(t, dir, sourcecontent.SnapshotFileKey(0), []byte("CREATE TABLE customers (id int);\n\n"))
			return ""
		},
		"same size other bytes": func(t *testing.T, dir string) string {
			replace(t, dir, sourcecontent.SnapshotFileKey(2), []byte{0xff, 0x00, 'y'})
			return ""
		},
		"two files swapped": func(t *testing.T, dir string) string {
			first := read(t, dir, sourcecontent.SnapshotFileKey(0))
			second := read(t, dir, sourcecontent.SnapshotFileKey(1))
			replace(t, dir, sourcecontent.SnapshotFileKey(0), second)
			replace(t, dir, sourcecontent.SnapshotFileKey(1), first)
			return ""
		},
		"a missing file": func(t *testing.T, dir string) string {
			if err := os.Remove(filepath.Join(dir, sourcecontent.SnapshotFileKey(2))); err != nil {
				t.Fatal(err)
			}
			return ""
		},
		"an extra key": func(t *testing.T, dir string) string {
			replace(t, dir, sourcecontent.SnapshotFileKey(3), []byte("DROP TABLE orders;\n"))
			return ""
		},
		"an unexpected name": func(t *testing.T, dir string) string {
			replace(t, dir, "schema.sql", []byte("DROP TABLE orders;\n"))
			return ""
		},
		"no manifest": func(t *testing.T, dir string) string {
			if err := os.Remove(filepath.Join(dir, sourcecontent.ManifestKey)); err != nil {
				t.Fatal(err)
			}
			return ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			snapshot, digest := writeSnapshot(t, migrationFiles)
			if other := tamper(t, snapshot); other != "" {
				digest = other
			}
			destination := filepath.Join(t.TempDir(), "migrations")
			err := sourcecontent.Materialize(snapshot, destination, digest)
			if err == nil {
				t.Fatal("Materialize copied a snapshot that does not give the bound digest")
			}
			if name != "no manifest" && !errors.Is(err, sourcecontent.ErrContentMismatch) {
				t.Fatalf("Materialize() = %v, want ErrContentMismatch", err)
			}
			if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
				t.Fatalf("a refused snapshot left %s behind: %v", destination, statErr)
			}
		})
	}
}

// A manifest whose digest matches is still refused when it is not the one
// Manifest writes: lines out of order, a padded size, an invalid path.
func TestMaterializeRefusesANonCanonicalManifest(t *testing.T) {
	t.Parallel()
	sum := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for name, manifest := range map[string]string{
		"out of order":    sum + " 0 b.sql\n" + sum + " 0 a.sql\n",
		"duplicate":       sum + " 0 a.sql\n" + sum + " 0 a.sql\n",
		"padded size":     sum + " 00 a.sql\n",
		"escaping path":   sum + " 0 ../a.sql\n",
		"upper-case hash": "E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855 0 a.sql\n",
		"no newline":      sum + " 0 a.sql",
		"empty":           "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			replace(t, dir, sourcecontent.ManifestKey, []byte(manifest))
			replace(t, dir, sourcecontent.SnapshotFileKey(0), nil)
			replace(t, dir, sourcecontent.SnapshotFileKey(1), nil)
			err := sourcecontent.Materialize(dir, filepath.Join(t.TempDir(), "out"), sourcecontent.DigestOf([]byte(manifest)))
			if !errors.Is(err, sourcecontent.ErrContentMismatch) {
				t.Fatalf("Materialize() = %v, want ErrContentMismatch", err)
			}
		})
	}
}

func TestMaterializeRefusesADestinationThatHoldsAFile(t *testing.T) {
	t.Parallel()
	snapshot, digest := writeSnapshot(t, migrationFiles)
	destination := t.TempDir()
	replace(t, destination, "0001_init.up.sql", []byte("DROP TABLE customers;\n"))
	if err := sourcecontent.Materialize(snapshot, destination, digest); err == nil {
		t.Fatal("Materialize wrote into a directory that already held a file")
	}
	if got := string(read(t, destination, "0001_init.up.sql")); got != "DROP TABLE customers;\n" {
		t.Fatalf("Materialize replaced a file it found: %q", got)
	}
}

func replace(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
