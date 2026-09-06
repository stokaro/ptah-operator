package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	batchv1 "k8s.io/api/batch/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const maxRenderBytes = int64(16 << 20)

type renderedObjectHeader struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"metadata"`
}

func loadRenderedJob(path, namespace, name string) (*batchv1.Job, error) {
	contents, err := readRegularFile(path, maxRenderBytes)
	if err != nil {
		return nil, err
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(contents), 4096)
	var match *batchv1.Job
	for document := 1; ; document++ {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode render document %d: %w", document, err)
		}
		if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		var header renderedObjectHeader
		if err := json.Unmarshal(raw, &header); err != nil {
			return nil, fmt.Errorf("decode render document %d identity: %w", document, err)
		}
		if header.APIVersion != "batch/v1" || header.Kind != "Job" || header.Metadata.Namespace != namespace || header.Metadata.Name != name {
			continue
		}
		if match != nil {
			return nil, errors.New("candidate render contains more than one exact hook Job")
		}
		var job batchv1.Job
		if err := json.Unmarshal(raw, &job); err != nil {
			return nil, fmt.Errorf("decode exact rendered hook Job: %w", err)
		}
		match = job.DeepCopy()
	}
	if match == nil {
		return nil, errors.New("candidate render does not contain the exact hook Job")
	}
	return match, nil
}

func readRegularFile(path string, maximum int64) ([]byte, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve render path: %w", err)
	}
	absolute = filepath.Clean(absolute)
	before, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, errors.New("candidate render must be a regular non-symbolic-link file")
	}
	if err := validatePrivatePermissions(absolute, before); err != nil {
		return nil, err
	}
	if before.Size() > maximum {
		return nil, errors.New("candidate render exceeds the size limit")
	}
	file, err := os.OpenFile(absolute, os.O_RDONLY|secureOpenFlags, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	current, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(opened, current) {
		return nil, errors.New("candidate render changed while it was opened")
	}
	if err := validatePrivatePermissions(absolute, opened); err != nil {
		return nil, err
	}
	if err := validatePrivatePermissions(absolute, current); err != nil {
		return nil, err
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > maximum {
		return nil, errors.New("candidate render exceeds the size limit")
	}
	return contents, nil
}
