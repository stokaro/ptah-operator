package runner

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const DefaultMaxPlanBytes int64 = 64 << 20

func reconstructPlan(planDir, expectedDigest string, maxBytes int64) ([]byte, string, error) {
	if planDir == "" {
		return nil, "", errors.New("plan directory is empty")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxPlanBytes
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
		info, err := os.Stat(resolvedChunk)
		if err != nil || !info.Mode().IsRegular() {
			return nil, "", fmt.Errorf("plan chunk %q is not a regular file", entry.Name())
		}
		if info.Size() > maxBytes-int64(plan.Len()) {
			return nil, "", fmt.Errorf("reconstructed plan exceeds %d bytes", maxBytes)
		}
		chunk, err := os.ReadFile(resolvedChunk)
		if err != nil {
			return nil, "", fmt.Errorf("read plan chunk %q", entry.Name())
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
