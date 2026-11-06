// Package volumes writes and unpacks what the harness's stand-in for the
// kubelet puts on a Pod's volumes: the directories an emptyDir starts as, and
// the files a Secret or a ConfigMap projects, with their owners and modes.
package volumes

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Entry is one directory or one file. Path is relative to the volume's root,
// and "." is the root itself.
type Entry struct {
	Volume string
	Path   string
	// Directory says this entry is a directory; otherwise it is a file with
	// Data.
	Directory bool
	Data      []byte
	Mode      int64
	UID, GID  int
}

// Pack is the entries as the tar stream Unpack reads.
func Pack(entries []Entry) ([]byte, error) {
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	for _, entry := range entries {
		header := &tar.Header{
			Name: path.Join(entry.Volume, entry.Path), Mode: entry.Mode, Uid: entry.UID, Gid: entry.GID,
			Size: int64(len(entry.Data)), Typeflag: tar.TypeReg,
		}
		if entry.Directory {
			header.Typeflag, header.Size, header.Name = tar.TypeDir, 0, header.Name+"/"
		}
		if err := archive.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("write %s: %w", header.Name, err)
		}
		if _, err := archive.Write(entry.Data); err != nil {
			return nil, fmt.Errorf("write %s: %w", header.Name, err)
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// Unpack writes a Pack stream under root, setting owners and modes as named.
// It refuses a name that leaves root and anything but a directory or a file.
func Unpack(stream io.Reader, root string) error {
	archive := tar.NewReader(stream)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the volumes: %w", err)
		}
		name := filepath.Clean(header.Name)
		if name == ".." || filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("the name %q leaves the volumes", header.Name)
		}
		target := filepath.Join(root, name)
		mode := os.FileMode(header.Mode & 0o7777) // #nosec G115 -- masked to the permission bits
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- held under root above
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, archive) // #nosec G110 -- the harness's own stream
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				return errors.Join(copyErr, closeErr)
			}
		default:
			return fmt.Errorf("%s is neither a directory nor a file", header.Name)
		}
		if err := os.Chown(target, header.Uid, header.Gid); err != nil {
			return err
		}
		if err := os.Chmod(target, mode); err != nil {
			return err
		}
	}
}
