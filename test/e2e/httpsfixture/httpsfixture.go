// Package httpsfixture is the downloads a test serves through
// test/e2e/httpsserver: the runner harness from a container, and the
// https-sources acceptance phase on kind. One definition, so the documents
// the harness captures answer the files the phase reads.
//
// Every archive is built with fixed times, owners and modes, so the same
// definition gives the same bytes and the same download digest on every run.
package httpsfixture

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

// The names the server serves the downloads by, and what each one selects.
const (
	// SchemaFile is a schema served as one file.
	SchemaFile = "schema.sql"
	// MigrationArchive is a gzip-compressed tar holding the migration
	// directory under MigrationPath.
	MigrationArchive = "migrations.tar.gz"
	// SchemaArchive is a zip holding the schema at SchemaInArchive, beside a
	// file no source selects.
	SchemaArchive = "schema.zip"
	// SchemaArchiveRepublished holds the same selected file as SchemaArchive
	// in other bytes: the file beside it says something else.
	SchemaArchiveRepublished = "schema-republished.zip"
	// DotDotArchive holds an entry that names a path outside the archive.
	DotDotArchive = "dotdot.tar.gz"
	// SymlinkArchive holds a symbolic link inside the selected directory.
	SymlinkArchive = "symlink.tar.gz"

	MigrationPath   = "orders-1.0/db/migrations"
	SchemaInArchive = "app-1.0/db/schema.sql"
)

// archiveTime is every entry's time, so a rebuild gives the same bytes.
var archiveTime = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// Schema is the desired schema every schema download carries.
const Schema = "CREATE TABLE e2e_https_items (\n  id bigint NOT NULL PRIMARY KEY,\n  name text NOT NULL\n);\n"

// MigrationFiles is the migration directory the archive carries.
var MigrationFiles = map[string]string{
	"0000000001_create_https_parts.up.sql":   "CREATE TABLE e2e_https_parts (\n    id INTEGER PRIMARY KEY,\n    name TEXT NOT NULL\n);\n",
	"0000000001_create_https_parts.down.sql": "DROP TABLE e2e_https_parts;\n",
	"0000000002_add_https_sku.up.sql":        "ALTER TABLE e2e_https_parts ADD COLUMN sku TEXT;\n",
	"0000000002_add_https_sku.down.sql":      "ALTER TABLE e2e_https_parts DROP COLUMN sku;\n",
}

// entry is one member of an archive: a file, or a symbolic link to target.
type entry struct {
	name, data, target string
}

// Files is every download by the name the server serves it under.
func Files() (map[string][]byte, error) {
	var migrations []entry
	for _, name := range sortedNames(MigrationFiles) {
		migrations = append(migrations, entry{name: MigrationPath + "/" + name, data: MigrationFiles[name]})
	}
	files := map[string][]byte{SchemaFile: []byte(Schema)}
	var err error
	if files[MigrationArchive], err = tarGzip(migrations); err != nil {
		return nil, err
	}
	if files[SchemaArchive], err = zipArchive([]entry{
		{name: SchemaInArchive, data: Schema}, {name: "app-1.0/README", data: "first\n"},
	}); err != nil {
		return nil, err
	}
	if files[SchemaArchiveRepublished], err = zipArchive([]entry{
		{name: SchemaInArchive, data: Schema}, {name: "app-1.0/README", data: "published again\n"},
	}); err != nil {
		return nil, err
	}
	if files[DotDotArchive], err = tarGzip(append(slices.Clone(migrations), entry{name: "../escape.up.sql", data: "SELECT 1;\n"})); err != nil {
		return nil, err
	}
	if files[SymlinkArchive], err = tarGzip(append(slices.Clone(migrations),
		entry{name: MigrationPath + "/0000000003_linked.up.sql", target: "../../../etc/passwd"})); err != nil {
		return nil, err
	}
	return files, nil
}

// Digest is the download digest of bytes: what a spec pins.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// SchemaContent is the content digest of the schema as the operator binds
// it: one file, by its own name.
func SchemaContent(name string) (string, error) {
	return sourcecontent.Digest([]sourcecontent.File{{Path: name, Data: []byte(Schema)}})
}

// MigrationContent is the content digest of the migration directory.
func MigrationContent() (string, error) {
	files := make([]sourcecontent.File, 0, len(MigrationFiles))
	for _, name := range sortedNames(MigrationFiles) {
		files = append(files, sourcecontent.File{Path: name, Data: []byte(MigrationFiles[name])})
	}
	return sourcecontent.Digest(files)
}

func tarGzip(entries []entry) ([]byte, error) {
	var out bytes.Buffer
	compressed, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	compressed.ModTime = archiveTime
	archive := tar.NewWriter(compressed)
	for _, each := range entries {
		header := &tar.Header{Name: each.name, ModTime: archiveTime, Mode: 0o644, Format: tar.FormatPAX, Typeflag: tar.TypeReg, Size: int64(len(each.data))}
		if each.target != "" {
			header.Typeflag, header.Linkname, header.Size, header.Mode = tar.TypeSymlink, each.target, 0, 0o777
		}
		if err := archive.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("write %s: %w", each.name, err)
		}
		if _, err := archive.Write([]byte(each.data)); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	if err := compressed.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func zipArchive(entries []entry) ([]byte, error) {
	var out bytes.Buffer
	archive := zip.NewWriter(&out)
	for _, each := range entries {
		writer, err := archive.CreateHeader(&zip.FileHeader{Name: each.name, Method: zip.Deflate, Modified: archiveTime})
		if err != nil {
			return nil, err
		}
		if _, err := writer.Write([]byte(each.data)); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func sortedNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
