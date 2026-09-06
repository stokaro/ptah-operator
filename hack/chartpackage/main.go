package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const maximumGzipTimestamp = int64(1<<32 - 1)

var safeArchiveComponent = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

type chartMetadata struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

type chartEntry struct {
	sourcePath    string
	relativePath  string
	archivePath   string
	info          os.FileInfo
	contentDigest [sha256.Size]byte
}

func main() {
	chart := flag.String("chart", "charts/ptah-operator", "chart directory")
	destination := flag.String("destination", "dist", "package output directory")
	epoch := flag.Int64("epoch", 0, "source commit timestamp in Unix seconds")
	flag.Parse()

	if *epoch <= 0 {
		fatal(errors.New("epoch must be a positive Unix timestamp"))
	}
	if err := packageChart(*chart, *destination, time.Unix(*epoch, 0).UTC()); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func packageChart(source, destination string, timestamp time.Time) error {
	timestamp = timestamp.UTC().Truncate(time.Second)
	if timestamp.Unix() <= 0 || timestamp.Unix() > maximumGzipTimestamp {
		return errors.New("epoch must fit in the positive 32-bit gzip timestamp range")
	}

	source = filepath.Clean(source)
	entries, err := collectChartEntries(source)
	if err != nil {
		return err
	}
	metadata, err := loadChartMetadata(entries)
	if err != nil {
		return err
	}
	if err := prepareArchivePaths(entries, metadata.Name); err != nil {
		return err
	}

	if err := os.MkdirAll(destination, 0o755); err != nil {
		return fmt.Errorf("create chart package destination: %w", err)
	}
	archiveName := metadata.Name + "-" + metadata.Version + ".tgz"
	return writeChartArchive(filepath.Join(destination, archiveName), entries, timestamp)
}

func collectChartEntries(source string) ([]chartEntry, error) {
	rootInfo, err := os.Lstat(source)
	if err != nil {
		return nil, fmt.Errorf("stat chart: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("chart source must not be a symbolic link")
	}
	if !rootInfo.IsDir() {
		return nil, errors.New("chart source is not a directory")
	}

	var entries []chartEntry
	err = filepath.WalkDir(source, func(filePath string, directoryEntry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relativePath, err := filepath.Rel(source, filePath)
		if err != nil {
			return fmt.Errorf("resolve chart path %q: %w", filePath, err)
		}
		if err := validateRelativeArchivePath(relativePath); err != nil {
			return err
		}

		info, err := directoryEntry.Info()
		if err != nil {
			return fmt.Errorf("stat chart entry %q: %w", relativePath, err)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("chart contains unsupported symbolic link %q", relativePath)
		case info.IsDir(), info.Mode().IsRegular():
			entry := chartEntry{
				sourcePath:   filePath,
				relativePath: relativePath,
				info:         info,
			}
			if info.Mode().IsRegular() {
				contents, err := readRegularFile(entry)
				if err != nil {
					return err
				}
				entry.contentDigest = sha256.Sum256(contents)
			}
			entries = append(entries, entry)
		default:
			return fmt.Errorf("chart contains unsupported special file %q", relativePath)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func validateRelativeArchivePath(relativePath string) error {
	if relativePath == "." {
		return nil
	}
	if filepath.IsAbs(relativePath) {
		return fmt.Errorf("chart path %q is absolute", relativePath)
	}
	slashPath := filepath.ToSlash(relativePath)
	if strings.Contains(slashPath, `\`) {
		return fmt.Errorf("chart path %q contains a non-portable path separator", relativePath)
	}
	if filepath.Clean(relativePath) != relativePath {
		return fmt.Errorf("chart path %q is not clean", relativePath)
	}
	for _, component := range strings.Split(slashPath, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("chart path %q contains an unsafe component", relativePath)
		}
		for _, character := range component {
			if character < 0x20 || character == 0x7f {
				return fmt.Errorf("chart path %q contains a control character", relativePath)
			}
		}
	}
	return nil
}

func loadChartMetadata(entries []chartEntry) (chartMetadata, error) {
	var chartFile *chartEntry
	for index := range entries {
		if entries[index].relativePath == "Chart.yaml" {
			chartFile = &entries[index]
			break
		}
	}
	if chartFile == nil || !chartFile.info.Mode().IsRegular() {
		return chartMetadata{}, errors.New("chart must contain a regular Chart.yaml file")
	}

	contents, err := readBoundRegularFile(*chartFile)
	if err != nil {
		return chartMetadata{}, err
	}
	var metadata chartMetadata
	if err := yaml.Unmarshal(contents, &metadata); err != nil {
		return chartMetadata{}, fmt.Errorf("parse Chart.yaml: %w", err)
	}
	if err := validateMetadataComponent("name", metadata.Name); err != nil {
		return chartMetadata{}, err
	}
	if err := validateMetadataComponent("version", metadata.Version); err != nil {
		return chartMetadata{}, err
	}
	return metadata, nil
}

func validateMetadataComponent(field, value string) error {
	if !safeArchiveComponent.MatchString(value) {
		return fmt.Errorf("Chart.yaml %s %q is unsafe for an archive path", field, value)
	}
	return nil
}

func prepareArchivePaths(entries []chartEntry, chartName string) error {
	for index := range entries {
		if entries[index].relativePath == "." {
			entries[index].archivePath = chartName + "/"
			continue
		}
		archivePath := chartName + "/" + filepath.ToSlash(entries[index].relativePath)
		if entries[index].info.IsDir() {
			archivePath += "/"
		}
		if !strings.HasPrefix(archivePath, chartName+"/") {
			return fmt.Errorf("chart entry %q escapes archive root", entries[index].relativePath)
		}
		entries[index].archivePath = archivePath
	}
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].archivePath < entries[right].archivePath
	})
	return nil
}

func writeChartArchive(destination string, entries []chartEntry, timestamp time.Time) error {
	outputDirectory := filepath.Dir(destination)
	output, err := os.CreateTemp(outputDirectory, ".chart-package-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary chart package: %w", err)
	}
	temporaryPath := output.Name()
	committed := false
	defer func() {
		if !committed {
			_ = output.Close()
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := output.Chmod(0o644); err != nil {
		return fmt.Errorf("set chart package permissions: %w", err)
	}

	gzipWriter, err := gzip.NewWriterLevel(output, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("create gzip writer: %w", err)
	}
	gzipWriter.Header = gzip.Header{
		ModTime: timestamp,
		OS:      255,
	}
	tarWriter := tar.NewWriter(gzipWriter)

	for _, entry := range entries {
		if err := writeChartEntry(tarWriter, entry, timestamp); err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return err
		}
	}
	if err := tarWriter.Close(); err != nil {
		_ = gzipWriter.Close()
		return fmt.Errorf("finish chart tar archive: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return fmt.Errorf("finish chart gzip archive: %w", err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close chart package: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return fmt.Errorf("install chart package: %w", err)
	}
	committed = true
	return nil
}

func writeChartEntry(writer *tar.Writer, entry chartEntry, timestamp time.Time) error {
	header := &tar.Header{
		Name:     entry.archivePath,
		Mode:     canonicalArchiveMode(entry.info),
		Uid:      0,
		Gid:      0,
		Uname:    "root",
		Gname:    "root",
		ModTime:  timestamp,
		Typeflag: tar.TypeDir,
		Format:   tar.FormatUSTAR,
	}
	if entry.info.IsDir() {
		if err := writer.WriteHeader(header); err != nil {
			return fmt.Errorf("write chart directory %q: %w", entry.relativePath, err)
		}
		return nil
	}

	contents, err := readBoundRegularFile(entry)
	if err != nil {
		return err
	}
	header.Typeflag = tar.TypeReg
	header.Size = int64(len(contents))
	if err := writer.WriteHeader(header); err != nil {
		return fmt.Errorf("write chart file header %q: %w", entry.relativePath, err)
	}
	if _, err := writer.Write(contents); err != nil {
		return fmt.Errorf("write chart file %q: %w", entry.relativePath, err)
	}
	return nil
}

func canonicalArchiveMode(info os.FileInfo) int64 {
	if info.IsDir() {
		return 0o755
	}
	return 0o644
}

func readRegularFile(entry chartEntry) ([]byte, error) {
	file, err := os.Open(entry.sourcePath)
	if err != nil {
		return nil, fmt.Errorf("open chart file %q: %w", entry.relativePath, err)
	}
	defer file.Close()

	beforeRead, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat open chart file %q: %w", entry.relativePath, err)
	}
	if !sameRegularFileSnapshot(entry.info, beforeRead) {
		return nil, fmt.Errorf("chart file %q changed while packaging", entry.relativePath)
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read chart file %q: %w", entry.relativePath, err)
	}
	afterRead, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat read chart file %q: %w", entry.relativePath, err)
	}
	if !sameRegularFileSnapshot(entry.info, afterRead) || int64(len(contents)) != entry.info.Size() {
		return nil, fmt.Errorf("chart file %q changed while packaging", entry.relativePath)
	}
	return contents, nil
}

func readBoundRegularFile(entry chartEntry) ([]byte, error) {
	contents, err := readRegularFile(entry)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(contents) != entry.contentDigest {
		return nil, fmt.Errorf("chart file %q contents changed while packaging", entry.relativePath)
	}
	return contents, nil
}

func sameRegularFileSnapshot(expected, actual os.FileInfo) bool {
	return actual.Mode().IsRegular() &&
		os.SameFile(expected, actual) &&
		expected.Size() == actual.Size() &&
		expected.ModTime().Equal(actual.ModTime())
}
