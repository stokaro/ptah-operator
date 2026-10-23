// Package sourcecontent defines the content digest of a delivered source: the
// files a schema or a migration directory is read from, whatever carried them.
//
// The digest names the bytes, not the carrier. The same files give the same
// digest from a ConfigMap snapshot, a Git tree or a directory on disk, so a
// plan can bind the content it was computed from and every later operation can
// refuse anything else.
//
// The manifest has one line per file, sorted by the path's bytes:
//
//	<lowercase hex SHA-256 of the file> <size in bytes, decimal> <path>\n
//
// and the digest is "sha256:" followed by the lowercase hex SHA-256 of the
// manifest. File bytes are never normalized: line endings, encoding and the
// order inside a file stay as delivered. A path is slash-separated and
// relative; the rules below keep every line unambiguous and every path the
// same file on every operating system that reads it.
//
// testdata/sourcecontent holds the cases any other implementation, such as
// Ptah's Git pull, is held to.
package sourcecontent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// MaxFiles is the most files one source may select. It bounds the
	// manifest and the volume an operation mounts.
	MaxFiles = 1000
	// MaxPathElementBytes is the longest name one path element may have, the
	// limit ordinary file systems keep.
	MaxPathElementBytes = 255

	prefix = "sha256:"
)

// The refusals, matched with errors.Is. Each names the rule a source broke;
// the message says which path.
var (
	ErrEmpty        = errors.New("source selects no file")
	ErrTooManyFiles = errors.New("source selects too many files")
	ErrInvalidPath  = errors.New("invalid source path")
	ErrDuplicate    = errors.New("duplicate source path")
	ErrPathConflict = errors.New("source path is both a file and a directory")
	ErrInvalidEntry = errors.New("invalid manifest entry")
)

// File is one selected file: its path inside the source and its exact bytes.
type File struct {
	Path string
	Data []byte
}

// Manifest returns the canonical manifest of files, refusing a set the rules
// do not admit. The input order does not matter.
func Manifest(files []File) ([]byte, error) {
	if len(files) == 0 {
		return nil, ErrEmpty
	}
	if len(files) > MaxFiles {
		return nil, fmt.Errorf("%w: %d, at most %d", ErrTooManyFiles, len(files), MaxFiles)
	}
	sorted := make([]File, len(files))
	copy(sorted, files)
	slices.SortFunc(sorted, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	paths := make([]string, len(sorted))
	for i, file := range sorted {
		paths[i] = file.Path
	}
	if err := checkSortedPaths(paths); err != nil {
		return nil, err
	}
	var manifest strings.Builder
	for _, file := range sorted {
		sum := sha256.Sum256(file.Data)
		manifest.WriteString(hex.EncodeToString(sum[:]))
		manifest.WriteByte(' ')
		manifest.WriteString(strconv.Itoa(len(file.Data)))
		manifest.WriteByte(' ')
		manifest.WriteString(file.Path)
		manifest.WriteByte('\n')
	}
	return []byte(manifest.String()), nil
}

// checkSortedPaths refuses a sorted set of paths the rules do not admit: an
// invalid path, the same path twice, or a path that is both a file and a
// directory.
func checkSortedPaths(paths []string) error {
	for i, path := range paths {
		if err := validPath(path); err != nil {
			return err
		}
		if i > 0 && paths[i-1] == path {
			return fmt.Errorf("%w: %q", ErrDuplicate, path)
		}
	}
	// A file "a" and a file "a/b" cannot both exist. Every path that has "a"
	// as a directory sorts after "a/" and before "a0", so checking each path
	// against the paths that follow it until the prefix no longer matches is
	// enough.
	for i, path := range paths {
		directory := path + "/"
		for _, later := range paths[i+1:] {
			if !strings.HasPrefix(later, path) {
				break
			}
			if strings.HasPrefix(later, directory) {
				return fmt.Errorf("%w: %q", ErrPathConflict, path)
			}
		}
	}
	return nil
}

// Digest returns the content digest of files: "sha256:" and the hex SHA-256 of
// their manifest.
func Digest(files []File) (string, error) {
	manifest, err := Manifest(files)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(manifest)
	return prefix + hex.EncodeToString(sum[:]), nil
}

// validPath admits a relative slash-separated path of valid UTF-8 whose
// elements are neither empty, "." nor "..", hold no control character and no
// backslash, and fit MaxPathElementBytes. A control character could end a
// manifest line; a backslash is a separator on Windows, so the same path would
// name another file there.
func validPath(path string) error {
	if path == "" {
		return fmt.Errorf("%w: empty", ErrInvalidPath)
	}
	if !utf8.ValidString(path) {
		return fmt.Errorf("%w: %q is not valid UTF-8", ErrInvalidPath, path)
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return fmt.Errorf("%w: %q holds %U", ErrInvalidPath, path, r)
		}
	}
	for element := range strings.SplitSeq(path, "/") {
		switch {
		case element == "":
			return fmt.Errorf("%w: %q has an empty element", ErrInvalidPath, path)
		case element == "." || element == "..":
			return fmt.Errorf("%w: %q has the element %q", ErrInvalidPath, path, element)
		case len(element) > MaxPathElementBytes:
			return fmt.Errorf("%w: %q has an element of %d bytes, at most %d", ErrInvalidPath, path, len(element), MaxPathElementBytes)
		}
	}
	return nil
}

// Entry is one line of a manifest as a report states it: the path, the size
// and the SHA-256 of the file's bytes, without the bytes.
type Entry struct {
	Path   string
	Size   int64
	SHA256 string
}

// DigestOfEntries returns the content digest a set of manifest entries
// gives, refusing a set the rules do not admit. A report that lists its files
// can be held to the digest it names without the files themselves.
func DigestOfEntries(entries []Entry) (string, error) {
	if len(entries) == 0 {
		return "", ErrEmpty
	}
	if len(entries) > MaxFiles {
		return "", fmt.Errorf("%w: %d, at most %d", ErrTooManyFiles, len(entries), MaxFiles)
	}
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b Entry) int { return strings.Compare(a.Path, b.Path) })
	paths := make([]string, len(sorted))
	for i, entry := range sorted {
		paths[i] = entry.Path
	}
	if err := checkSortedPaths(paths); err != nil {
		return "", err
	}
	var manifest strings.Builder
	for _, entry := range sorted {
		if entry.Size < 0 || len(entry.SHA256) != sha256.Size*2 || strings.ToLower(entry.SHA256) != entry.SHA256 {
			return "", fmt.Errorf("%w: %q has no valid size and SHA-256", ErrInvalidEntry, entry.Path)
		}
		if _, err := hex.DecodeString(entry.SHA256); err != nil {
			return "", fmt.Errorf("%w: %q has no valid SHA-256", ErrInvalidEntry, entry.Path)
		}
		manifest.WriteString(entry.SHA256)
		manifest.WriteByte(' ')
		manifest.WriteString(strconv.FormatInt(entry.Size, 10))
		manifest.WriteByte(' ')
		manifest.WriteString(entry.Path)
		manifest.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(manifest.String()))
	return prefix + hex.EncodeToString(sum[:]), nil
}
