package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxVerificationPolicyBytes int64 = 1 << 20

// snapshotFile binds a child invocation to the same bytes that were hashed,
// even when the source is a projected volume whose symlink can rotate.
func snapshotFile(sourcePath, tempDir, prefix string, maxBytes int64) (string, string, func(), error) {
	if sourcePath == "" {
		return "", "", nil, errors.New("source path is empty")
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", "", nil, err
	}
	content, readErr := io.ReadAll(io.LimitReader(source, maxBytes+1))
	closeErr := source.Close()
	if readErr != nil {
		return "", "", nil, readErr
	}
	if closeErr != nil {
		return "", "", nil, closeErr
	}
	if int64(len(content)) > maxBytes {
		return "", "", nil, fmt.Errorf("source exceeds %d bytes", maxBytes)
	}

	extension := filepath.Ext(sourcePath)
	if extension != ".json" && extension != ".yaml" && extension != ".yml" {
		extension = ""
	}
	temporary, err := os.CreateTemp(tempDir, prefix+"-*"+extension)
	if err != nil {
		return "", "", nil, err
	}
	path := temporary.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		cleanup()
		return "", "", nil, err
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return "", "", nil, err
	}
	return path, sha256Digest(content), cleanup, nil
}
