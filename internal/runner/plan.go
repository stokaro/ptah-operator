package runner

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah-operator/internal/plancontract"
)

const DefaultMaxPlanBytes int64 = plancontract.MaxExecutableBytes

func reconstructPlan(planDir, expectedDigest string, maxBytes int64) ([]byte, string, error) {
	if planDir == "" {
		return nil, "", errors.New("plan directory is empty")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxPlanBytes
	}
	// bytes.Buffer is indexed by int, and the extra byte below is used to
	// detect a file that grows after it is inspected.
	maximumBufferBytes := int64(^uint(0) >> 1)
	if maxBytes >= maximumBufferBytes {
		maxBytes = maximumBufferBytes - 1
	}
	expected, err := parseSHA256Digest(expectedDigest)
	if err != nil {
		return nil, "", fmt.Errorf("expected plan content digest: %w", err)
	}

	root, err := filepath.Abs(planDir)
	if err != nil {
		return nil, "", errors.New("resolve plan directory")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, "", errors.New("resolve plan directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, "", errors.New("read plan directory")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var plan bytes.Buffer
	chunkCount := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		chunkPath := filepath.Join(root, entry.Name())
		resolvedChunk, err := filepath.EvalSymlinks(chunkPath)
		if err != nil {
			return nil, "", fmt.Errorf("resolve plan chunk %q", entry.Name())
		}
		relative, err := filepath.Rel(resolvedRoot, resolvedChunk)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, "", fmt.Errorf("plan chunk %q resolves outside the plan directory", entry.Name())
		}
		chunkFile, err := os.Open(resolvedChunk)
		if err != nil {
			return nil, "", fmt.Errorf("open plan chunk %q", entry.Name())
		}
		info, err := chunkFile.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = chunkFile.Close()
			return nil, "", fmt.Errorf("plan chunk %q is not a regular file", entry.Name())
		}
		remaining := maxBytes - int64(plan.Len())
		if info.Size() > remaining {
			_ = chunkFile.Close()
			return nil, "", fmt.Errorf("reconstructed plan exceeds %d bytes", maxBytes)
		}
		chunk, err := io.ReadAll(io.LimitReader(chunkFile, remaining+1))
		if err != nil {
			_ = chunkFile.Close()
			return nil, "", fmt.Errorf("read plan chunk %q", entry.Name())
		}
		if err := chunkFile.Close(); err != nil {
			return nil, "", fmt.Errorf("close plan chunk %q", entry.Name())
		}
		if int64(len(chunk)) > remaining {
			return nil, "", fmt.Errorf("reconstructed plan exceeds %d bytes", maxBytes)
		}
		_, _ = plan.Write(chunk)
		chunkCount++
	}
	if chunkCount == 0 {
		return nil, "", errors.New("plan directory contains no chunks")
	}
	content := plan.Bytes()
	if !utf8.Valid(content) {
		return nil, "", errors.New("reconstructed plan is not valid UTF-8")
	}
	actual := sha256.Sum256(content)
	if subtle.ConstantTimeCompare(expected, actual[:]) != 1 {
		return nil, sha256Digest(content), errors.New("reconstructed plan content digest does not match the expected digest")
	}
	return append([]byte(nil), content...), sha256Digest(content), nil
}

func parseSHA256Digest(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if len(value) >= len("sha256:") && strings.EqualFold(value[:len("sha256:")], "sha256:") {
		value = value[len("sha256:"):]
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return nil, errors.New("must be a SHA-256 digest")
	}
	return decoded, nil
}
