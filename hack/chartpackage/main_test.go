package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testChartMetadata = "apiVersion: v2\nname: sample-chart\nversion: 1.2.3\n"

type archivedEntry struct {
	header  tar.Header
	content []byte
}

func TestPackageChartIsReproducibleAcrossWallClockSeconds(t *testing.T) {
	source := newTestChart(t)
	timestamp := time.Unix(1_700_000_000, 0).UTC()
	firstDestination := t.TempDir()
	secondDestination := t.TempDir()

	if err := packageChart(source, firstDestination, timestamp); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	changedModes := map[string]os.FileMode{
		"Chart.yaml":       0o777,
		"templates":        0o777,
		"templates/a.yaml": 0o755,
		"templates/z.yaml": 0o666,
	}
	for relativePath, mode := range changedModes {
		filePath := filepath.Join(source, relativePath)
		if err := os.Chmod(filePath, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filePath, time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := packageChart(source, secondDestination, timestamp); err != nil {
		t.Fatal(err)
	}

	first := mustReadFile(t, filepath.Join(firstDestination, "sample-chart-1.2.3.tgz"))
	second := mustReadFile(t, filepath.Join(secondDestination, "sample-chart-1.2.3.tgz"))
	if !bytes.Equal(first, second) {
		t.Fatal("chart packages created in different wall-clock seconds differ")
	}
}

func TestPackageChartArchiveLayoutMetadataAndContents(t *testing.T) {
	source := newTestChart(t)
	executable := filepath.Join(source, "templates", "executable.sh")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\ntrue\n"), 0o711); err != nil {
		t.Fatal(err)
	}
	timestamp := time.Unix(1_700_000_000, 0).UTC()
	destination := t.TempDir()
	if err := packageChart(source, destination, timestamp); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(destination, "sample-chart-1.2.3.tgz")
	archiveInfo, err := os.Stat(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if archiveInfo.Mode().Perm() != 0o644 {
		t.Fatalf("chart package mode = %o, want 644", archiveInfo.Mode().Perm())
	}
	gzipHeader, entries := readArchive(t, archivePath)
	if !gzipHeader.ModTime.Equal(timestamp) || gzipHeader.OS != 255 || gzipHeader.Name != "" || gzipHeader.Comment != "" || len(gzipHeader.Extra) != 0 {
		t.Fatalf("unexpected gzip header: %+v", gzipHeader)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.header.Name)
		if entry.header.Name != "sample-chart/" && !strings.HasPrefix(entry.header.Name, "sample-chart/") {
			t.Errorf("archive entry %q is outside the chart root", entry.header.Name)
		}
		cleanName := path.Clean(entry.header.Name)
		if path.IsAbs(entry.header.Name) || cleanName == ".." || strings.HasPrefix(cleanName, "../") {
			t.Errorf("archive entry %q permits path traversal", entry.header.Name)
		}
		if entry.header.Uid != 0 || entry.header.Gid != 0 || entry.header.Uname != "root" || entry.header.Gname != "root" {
			t.Errorf("entry %q has unexpected ownership: uid=%d gid=%d uname=%q gname=%q", entry.header.Name, entry.header.Uid, entry.header.Gid, entry.header.Uname, entry.header.Gname)
		}
		if !entry.header.ModTime.Equal(timestamp) || !entry.header.AccessTime.IsZero() || !entry.header.ChangeTime.IsZero() {
			t.Errorf("entry %q has unexpected timestamps: mtime=%s atime=%s ctime=%s", entry.header.Name, entry.header.ModTime, entry.header.AccessTime, entry.header.ChangeTime)
		}
		if entry.header.Format != tar.FormatUSTAR {
			t.Errorf("entry %q has archive format %v, want USTAR", entry.header.Name, entry.header.Format)
		}
		if entry.header.Size != int64(len(entry.content)) || entry.header.Linkname != "" || len(entry.header.PAXRecords) != 0 || entry.header.Devmajor != 0 || entry.header.Devminor != 0 {
			t.Errorf("entry %q has unexpected auxiliary header data: %+v", entry.header.Name, entry.header)
		}
	}
	wantNames := []string{
		"sample-chart/",
		"sample-chart/Chart.yaml",
		"sample-chart/templates/",
		"sample-chart/templates/a.yaml",
		"sample-chart/templates/executable.sh",
		"sample-chart/templates/z.yaml",
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("archive entries = %#v, want %#v", names, wantNames)
	}

	assertArchivedEntry(t, entries[0], tar.TypeDir, 0o755, nil)
	assertArchivedEntry(t, entries[1], tar.TypeReg, 0o644, []byte(testChartMetadata))
	assertArchivedEntry(t, entries[2], tar.TypeDir, 0o755, nil)
	assertArchivedEntry(t, entries[3], tar.TypeReg, 0o644, []byte("kind: ConfigMap\nmetadata:\n  name: a\n"))
	assertArchivedEntry(t, entries[4], tar.TypeReg, 0o644, []byte("#!/bin/sh\ntrue\n"))
	assertArchivedEntry(t, entries[5], tar.TypeReg, 0o644, []byte("kind: ConfigMap\nmetadata:\n  name: z\n"))
}

func TestPackageChartRejectsSymbolicLinks(t *testing.T) {
	source := newTestChart(t)
	if err := os.Symlink("outside", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	err := packageChart(source, t.TempDir(), time.Unix(1, 0))
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("packageChart() error = %v, want symbolic-link rejection", err)
	}
}

func TestPackageChartRejectsSpecialFiles(t *testing.T) {
	source := newTestChart(t)
	socketPath := filepath.Join(source, "unexpected.socket")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Skipf("Unix sockets are unavailable: %v", err)
	}
	defer listener.Close()

	err = packageChart(source, t.TempDir(), time.Unix(1, 0))
	if err == nil || !strings.Contains(err.Error(), "special file") {
		t.Fatalf("packageChart() error = %v, want special-file rejection", err)
	}
}

func TestPackageChartRejectsArchivePathTraversal(t *testing.T) {
	tests := []struct {
		name     string
		metadata string
	}{
		{name: "chart name", metadata: "apiVersion: v2\nname: ../escape\nversion: 1.2.3\n"},
		{name: "chart version", metadata: "apiVersion: v2\nname: sample-chart\nversion: ../../escape\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "Chart.yaml"), []byte(test.metadata), 0o644); err != nil {
				t.Fatal(err)
			}
			parent := t.TempDir()
			destination := filepath.Join(parent, "packages")
			err := packageChart(source, destination, time.Unix(1, 0))
			if err == nil || !strings.Contains(err.Error(), "unsafe for an archive path") {
				t.Fatalf("packageChart() error = %v, want unsafe-path rejection", err)
			}
			if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
				t.Fatalf("unsafe metadata created the package directory: %v", statErr)
			}
			if _, statErr := os.Stat(filepath.Join(parent, "escape-1.2.3.tgz")); !os.IsNotExist(statErr) {
				t.Fatalf("path traversal created an outside archive: %v", statErr)
			}
		})
	}
}

func TestValidateRelativeArchivePathRejectsNonPortableNames(t *testing.T) {
	for _, relativePath := range []string{
		"../escape",
		"templates/../escape",
		"templates\\escape.yaml",
		"templates/line\nbreak.yaml",
		"templates/control\x7f.yaml",
	} {
		t.Run(relativePath, func(t *testing.T) {
			if err := validateRelativeArchivePath(relativePath); err == nil {
				t.Fatalf("validateRelativeArchivePath(%q) accepted an unsafe path", relativePath)
			}
		})
	}
	for _, relativePath := range []string{".", "Chart.yaml", "templates/_helpers.tpl"} {
		t.Run("valid "+relativePath, func(t *testing.T) {
			if err := validateRelativeArchivePath(relativePath); err != nil {
				t.Fatalf("validateRelativeArchivePath(%q) error = %v", relativePath, err)
			}
		})
	}
}

func TestReadRegularFileRejectsSameFileMutation(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(filePath, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filePath, []byte("mutated!\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changedTime := info.ModTime().Add(time.Second)
	if err := os.Chtimes(filePath, changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	_, err = readRegularFile(chartEntry{
		sourcePath:   filePath,
		relativePath: "values.yaml",
		info:         info,
	})
	if err == nil || !strings.Contains(err.Error(), "changed while packaging") {
		t.Fatalf("readRegularFile() error = %v, want same-file mutation rejection", err)
	}
}

func TestBoundChartFileRejectsSameSizeMutationWithRestoredTimestamp(t *testing.T) {
	source := newTestChart(t)
	entries, err := collectChartEntries(source)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := loadChartMetadata(entries)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Name != "sample-chart" {
		t.Fatalf("initial chart name = %q, want sample-chart", metadata.Name)
	}
	if err := prepareArchivePaths(entries, metadata.Name); err != nil {
		t.Fatal(err)
	}

	chartPath := filepath.Join(source, "Chart.yaml")
	info, err := os.Stat(chartPath)
	if err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(testChartMetadata, "sample-chart", "tamper-chart", 1)
	if len(mutated) != len(testChartMetadata) {
		t.Fatalf("regression fixture sizes differ: mutated=%d original=%d", len(mutated), len(testChartMetadata))
	}
	if err := os.WriteFile(chartPath, []byte(mutated), info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(chartPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	afterMutation, err := os.Stat(chartPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, afterMutation) || info.Size() != afterMutation.Size() || !info.ModTime().Equal(afterMutation.ModTime()) {
		t.Fatalf("regression fixture did not preserve inode, size, and mtime: before=%+v after=%+v", info, afterMutation)
	}

	_, err = loadChartMetadata(entries)
	if err == nil || !strings.Contains(err.Error(), "contents changed while packaging") {
		t.Fatalf("loadChartMetadata() error = %v, want digest-bound mutation rejection", err)
	}

	destination := t.TempDir()
	err = writeChartArchive(
		filepath.Join(destination, metadata.Name+"-"+metadata.Version+".tgz"),
		entries,
		time.Unix(1_700_000_000, 0).UTC(),
	)
	if err == nil || !strings.Contains(err.Error(), "contents changed while packaging") {
		t.Fatalf("writeChartArchive() error = %v, want digest-bound mutation rejection", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "sample-chart-1.2.3.tgz")); !os.IsNotExist(err) {
		t.Fatalf("digest mismatch produced an archive: %v", err)
	}
}

func TestWriteChartArchivePreservesExistingOutputOnFailure(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source.yaml")
	if err := os.WriteFile(sourcePath, []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(directory, "sample-chart-1.2.3.tgz")
	want := []byte("existing release artifact\n")
	if err := os.WriteFile(destination, want, 0o640); err != nil {
		t.Fatal(err)
	}
	err = writeChartArchive(destination, []chartEntry{{
		sourcePath:   sourcePath,
		relativePath: "values.yaml",
		archivePath:  "sample-chart/values.yaml",
		info:         info,
	}}, time.Unix(1_700_000_000, 0).UTC())
	if err == nil {
		t.Fatal("writeChartArchive() succeeded with a missing source file")
	}
	if got := mustReadFile(t, destination); !bytes.Equal(got, want) {
		t.Fatalf("failed packaging changed the existing output: got %q, want %q", got, want)
	}
	temporaryFiles, err := filepath.Glob(filepath.Join(directory, ".chart-package-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporaryFiles) != 0 {
		t.Fatalf("failed packaging left temporary files: %v", temporaryFiles)
	}
}

func newTestChart(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "Chart.yaml"), []byte(testChartMetadata), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "templates"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"z.yaml": "kind: ConfigMap\nmetadata:\n  name: z\n",
		"a.yaml": "kind: ConfigMap\nmetadata:\n  name: a\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(source, "templates", name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return source
}

func readArchive(t *testing.T, archivePath string) (gzip.Header, []archivedEntry) {
	t.Helper()
	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	header := gzipReader.Header

	var entries []archivedEntry
	tarReader := tar.NewReader(gzipReader)
	for {
		tarHeader, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		contents, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, archivedEntry{header: *tarHeader, content: contents})
	}
	return header, entries
}

func assertArchivedEntry(t *testing.T, entry archivedEntry, entryType byte, mode int64, contents []byte) {
	t.Helper()
	if entry.header.Typeflag != entryType || entry.header.Mode != mode || !bytes.Equal(entry.content, contents) {
		t.Errorf("entry %q = type %d mode %o content %q, want type %d mode %o content %q", entry.header.Name, entry.header.Typeflag, entry.header.Mode, entry.content, entryType, mode, contents)
	}
}

func mustReadFile(t *testing.T, filePath string) []byte {
	t.Helper()
	contents, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}
