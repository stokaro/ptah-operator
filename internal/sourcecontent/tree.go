package sourcecontent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrNotRegular refuses a tree that holds anything but directories and
// regular files.
var ErrNotRegular = errors.New("source tree holds something other than a regular file or a directory")

// ReadTree reads every file under root, with its path relative to root. It
// refuses a symbolic link, a device or any other entry that is not a regular
// file or a directory, more than MaxFiles files, and more than maxBytes of
// file content in total. A tree Ptah wrote is read back this way before its
// digest is compared, so the bytes an operation reads are the ones that gave
// the digest.
func ReadTree(root string, maxBytes int64) ([]File, error) {
	var files []File
	var total int64
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%w: %q", ErrNotRegular, relative)
		}
		if len(files) == MaxFiles {
			return fmt.Errorf("%w: more than %d", ErrTooManyFiles, MaxFiles)
		}
		data, err := readBounded(current, maxBytes-total)
		if err != nil {
			return fmt.Errorf("read %q: %w", relative, err)
		}
		total += int64(len(data))
		files = append(files, File{Path: relative, Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, ErrEmpty
	}
	return files, nil
}

// ReadFile reads one regular file as the source file name, refusing a
// symbolic link and more than maxBytes.
func ReadFile(file, name string, maxBytes int64) ([]File, error) {
	info, err := os.Lstat(file)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %q", ErrNotRegular, name)
	}
	data, err := readBounded(file, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", name, err)
	}
	return []File{{Path: name, Data: data}}, nil
}

// Write creates every file under destination, which has to be empty or
// absent, and refuses to replace one.
func Write(destination string, files []File) error { return write(destination, files) }
