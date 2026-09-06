package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRenderedJobSelectsOneExactJobFromMultipleDocuments(t *testing.T) {
	t.Parallel()
	requirePrivateModeSemantics(t)

	exact := validRenderedJob()
	other := exact.DeepCopy()
	other.Name += "-other"
	documents := []string{
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"unrelated","namespace":"` + testNamespace + `"}}`,
		mustMarshalJSON(t, other),
		mustMarshalJSON(t, exact),
	}
	path := writePrivateRender(t, strings.Join(documents, "\n---\n"))

	loaded, err := loadRenderedJob(path, testNamespace, testJobName)
	if err != nil {
		t.Fatalf("loadRenderedJob: %v", err)
	}
	if loaded.Namespace != testNamespace || loaded.Name != testJobName || loaded.APIVersion != "batch/v1" || loaded.Kind != "Job" {
		t.Fatalf("loaded unexpected Job identity: %#v", loaded.TypeMeta)
	}
	if err := validateRenderedJob(loaded, testCaptureConfig()); err != nil {
		t.Fatalf("loaded Job does not satisfy the rendered contract: %v", err)
	}
}

func TestLoadRenderedJobRequiresExactlyOneMatch(t *testing.T) {
	t.Parallel()
	requirePrivateModeSemantics(t)

	exact := mustMarshalJSON(t, validRenderedJob())
	tests := map[string]struct {
		contents string
		want     string
	}{
		"missing": {
			contents: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"unrelated","namespace":"` + testNamespace + `"}}`,
			want:     "does not contain",
		},
		"duplicate": {
			contents: exact + "\n---\n" + exact,
			want:     "more than one",
		},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := loadRenderedJob(writePrivateRender(t, test.contents), testNamespace, testJobName)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadRenderedJob error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestReadRegularFileRejectsUnsafeAndOversizedInputs(t *testing.T) {
	t.Parallel()
	requirePrivateModeSemantics(t)

	t.Run("non-0600 mode", func(t *testing.T) {
		t.Parallel()
		path := writePrivateRender(t, "safe")
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("change render mode: %v", err)
		}
		if _, err := readRegularFile(path, maxRenderBytes); err == nil || !strings.Contains(err.Error(), "mode 0600") {
			t.Fatalf("readRegularFile error = %v, want private-mode rejection", err)
		}
	})

	t.Run("symbolic link", func(t *testing.T) {
		t.Parallel()
		target := writePrivateRender(t, "safe")
		link := filepath.Join(t.TempDir(), "render-link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("create render symbolic link: %v", err)
		}
		if _, err := readRegularFile(link, maxRenderBytes); err == nil || !strings.Contains(err.Error(), "non-symbolic-link") {
			t.Fatalf("readRegularFile error = %v, want symbolic-link rejection", err)
		}
	})

	t.Run("oversized", func(t *testing.T) {
		t.Parallel()
		path := writePrivateRender(t, strings.Repeat("x", 33))
		if _, err := readRegularFile(path, 32); err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("readRegularFile error = %v, want size rejection", err)
		}
	})
}

func writePrivateRender(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "render.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write private render: %v", err)
	}
	return path
}

func mustMarshalJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal render fixture: %v", err)
	}
	return string(encoded)
}
