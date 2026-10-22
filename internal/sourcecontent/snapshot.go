package sourcecontent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// A snapshot is the set of files a ConfigMap source selected, stored as
// ConfigMap keys: the manifest under ManifestKey, and the file on manifest
// line i under SnapshotFileKey(i). Keys cannot hold "/", so the path lives in
// the manifest and the key only says which line a file belongs to.
const (
	// ManifestKey is the key the manifest is stored under.
	ManifestKey = "manifest"
	// MaxSnapshotBytes is the most a snapshot's files and manifest may take
	// together: the limit the API server puts on a ConfigMap's values.
	MaxSnapshotBytes = 1 << 20
)

// ErrContentMismatch is the refusal of a snapshot whose manifest or files no
// longer give the content digest the operation was bound to.
var ErrContentMismatch = errors.New("snapshot does not hold the bound content")

// SnapshotFileKey is the key the file on manifest line index is stored under.
func SnapshotFileKey(index int) string { return fmt.Sprintf("f%04d", index) }

// Canonical returns files sorted by path, with their manifest and digest,
// refusing a set the rules do not admit. The sorted order is the order the
// manifest lists them in, so file i is stored under SnapshotFileKey(i).
func Canonical(files []File) ([]File, []byte, string, error) {
	manifest, err := Manifest(files)
	if err != nil {
		return nil, nil, "", err
	}
	sorted := make([]File, len(files))
	copy(sorted, files)
	slices.SortFunc(sorted, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return sorted, manifest, DigestOf(manifest), nil
}

// DigestOf returns the content digest a manifest names.
func DigestOf(manifest []byte) string {
	sum := sha256.Sum256(manifest)
	return prefix + hex.EncodeToString(sum[:])
}

// ValidatePath reports whether path is admitted as a source path.
func ValidatePath(path string) error { return validPath(path) }

type manifestLine struct {
	sum  string
	size int64
	path string
}

// parseManifest reads a canonical manifest. It refuses anything Manifest
// would not have written: a malformed line, a size with a leading zero, an
// invalid path, or paths out of order.
func parseManifest(manifest []byte) ([]manifestLine, error) {
	if len(manifest) == 0 || manifest[len(manifest)-1] != '\n' {
		return nil, errors.New("manifest does not end with a newline")
	}
	raw := bytes.Split(manifest[:len(manifest)-1], []byte{'\n'})
	if len(raw) > MaxFiles {
		return nil, fmt.Errorf("manifest lists %d files, at most %d", len(raw), MaxFiles)
	}
	lines := make([]manifestLine, 0, len(raw))
	for number, line := range raw {
		fields := strings.SplitN(string(line), " ", 3)
		if len(fields) != 3 || len(fields[0]) != sha256.Size*2 || strings.ToLower(fields[0]) != fields[0] {
			return nil, fmt.Errorf("manifest line %d is malformed", number+1)
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return nil, fmt.Errorf("manifest line %d is malformed", number+1)
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || size < 0 || strconv.FormatInt(size, 10) != fields[1] {
			return nil, fmt.Errorf("manifest line %d has an invalid size", number+1)
		}
		if err := validPath(fields[2]); err != nil {
			return nil, fmt.Errorf("manifest line %d: %w", number+1, err)
		}
		if number > 0 && lines[number-1].path >= fields[2] {
			return nil, fmt.Errorf("manifest line %d is out of order", number+1)
		}
		lines = append(lines, manifestLine{sum: fields[0], size: size, path: fields[2]})
	}
	return lines, nil
}

// Materialize copies the snapshot mounted at snapshotDir into destination,
// which must be an empty directory or not exist yet, and refuses unless the
// snapshot holds exactly the content contentDigest names: the manifest gives
// the digest, every file has the size and SHA-256 its line names, the manifest
// is the one Manifest computes for those files, and the snapshot holds no
// other key.
//
// It reads every byte before it writes any, so a refused snapshot leaves
// destination as it found it.
func Materialize(snapshotDir, destination, contentDigest string) error {
	manifest, err := readBounded(filepath.Join(snapshotDir, ManifestKey), MaxSnapshotBytes)
	if err != nil {
		return fmt.Errorf("read the snapshot manifest: %w", err)
	}
	if DigestOf(manifest) != contentDigest {
		return fmt.Errorf("%w: the manifest gives %s, not %s", ErrContentMismatch, DigestOf(manifest), contentDigest)
	}
	lines, err := parseManifest(manifest)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrContentMismatch, err)
	}
	if err := onlyKeys(snapshotDir, len(lines)); err != nil {
		return err
	}
	files, err := Files(manifest, func(index int, size int64) ([]byte, error) {
		return readBounded(filepath.Join(snapshotDir, SnapshotFileKey(index)), size)
	})
	if err != nil {
		return err
	}
	return write(destination, files)
}

// Files checks every file a snapshot holds against its manifest and returns
// them in manifest order. read returns the bytes stored for manifest line
// index, reading no more than size and one byte; a file whose size or SHA-256
// is not the one its line names, a manifest that is not canonical, or one that
// is not the manifest Manifest computes for the files is refused with
// ErrContentMismatch. The caller compares the manifest's digest with the one
// it expects.
func Files(manifest []byte, read func(index int, size int64) ([]byte, error)) ([]File, error) {
	lines, err := parseManifest(manifest)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrContentMismatch, err)
	}
	files := make([]File, len(lines))
	total := int64(len(manifest))
	for index, line := range lines {
		total += line.size
		if total > MaxSnapshotBytes {
			return nil, fmt.Errorf("%w: the snapshot holds more than %d bytes", ErrContentMismatch, MaxSnapshotBytes)
		}
		data, err := read(index, line.size)
		if err != nil {
			return nil, fmt.Errorf("%w: read %s: %w", ErrContentMismatch, line.path, err)
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != line.size || hex.EncodeToString(sum[:]) != line.sum {
			return nil, fmt.Errorf("%w: %s does not match its manifest line", ErrContentMismatch, line.path)
		}
		files[index] = File{Path: line.path, Data: data}
	}
	recomputed, err := Manifest(files)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrContentMismatch, err)
	}
	if !bytes.Equal(recomputed, manifest) {
		return nil, fmt.Errorf("%w: the manifest is not the canonical one for its files", ErrContentMismatch)
	}
	return files, nil
}

// onlyKeys refuses a snapshot directory holding a key besides the manifest
// and the count files it lists. A ConfigMap volume also holds the kubelet's
// own entries, whose names start with "..".
func onlyKeys(snapshotDir string, count int) error {
	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		return fmt.Errorf("read the snapshot: %w", err)
	}
	seen := 0
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "..") {
			continue
		}
		if name != ManifestKey && !isFileKey(name, count) {
			return fmt.Errorf("%w: the snapshot holds the unexpected key %q", ErrContentMismatch, name)
		}
		seen++
	}
	if seen != count+1 {
		return fmt.Errorf("%w: the snapshot holds %d keys, not %d", ErrContentMismatch, seen, count+1)
	}
	return nil
}

func isFileKey(name string, count int) bool {
	if len(name) != 5 || name[0] != 'f' {
		return false
	}
	index, err := strconv.Atoi(name[1:])
	return err == nil && index >= 0 && index < count && SnapshotFileKey(index) == name
}

// readBounded reads at most limit bytes and refuses a file that holds more.
func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s holds more than %d bytes", filepath.Base(path), limit)
	}
	return data, nil
}

// write creates every file under destination and refuses to replace one.
func write(destination string, files []File) error {
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", destination, err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		return fmt.Errorf("read %s: %w", destination, err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("%s is not empty", destination)
	}
	for _, file := range files {
		target := filepath.Join(destination, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("create the directory of %s: %w", file.Path, err)
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o440)
		if err != nil {
			return fmt.Errorf("create %s: %w", file.Path, err)
		}
		_, writeErr := out.Write(file.Data)
		closeErr := out.Close()
		if writeErr != nil || closeErr != nil {
			return fmt.Errorf("write %s: %w", file.Path, errors.Join(writeErr, closeErr))
		}
	}
	return nil
}
