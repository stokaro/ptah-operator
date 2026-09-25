package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `go build ./hack/<tool>` from the repository root writes a binary named after
// the tool beside go.mod. #113 committed unknownlayerfixture that way and #275
// committed crdreference, twenty-two megabytes between them, and both stayed
// on master unnoticed: no review reads a binary diff, and no gate looked.
//
// The check reads the first bytes of every tracked file for the executable
// formats a Go build produces on the platforms this repository is built on.
// Nothing here is supposed to be one, so there is no allowance to keep.
var executableMagic = []struct {
	format string
	magic  []byte
}{
	{"ELF", []byte{0x7f, 'E', 'L', 'F'}},
	{"Mach-O 64-bit", []byte{0xcf, 0xfa, 0xed, 0xfe}},
	{"Mach-O 32-bit", []byte{0xce, 0xfa, 0xed, 0xfe}},
	{"Mach-O universal", []byte{0xca, 0xfe, 0xba, 0xbe}},
	{"PE", []byte{'M', 'Z'}},
}

func TestNoTrackedFileIsACompiledExecutable(t *testing.T) {
	t.Parallel()
	root := repositoryFile(t, ".")
	listing, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("list the tracked files: %v", err)
	}
	examined := 0
	var executables []string
	for _, name := range strings.Split(strings.TrimRight(string(listing), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		format, err := executableFormat(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		examined++
		if format != "" {
			executables = append(executables, name+" ("+format+")")
		}
	}
	// An empty listing would pass every file it did not read.
	if examined == 0 {
		t.Fatal("git listed no tracked file, so this check read nothing")
	}
	if len(executables) > 0 {
		t.Errorf("tracked files are compiled executables; build into bin/ and remove them: %v", executables)
	}
}

// executableFormat names the executable format a file starts with, or returns
// an empty string. A path that is not a regular file, such as a symlink to a
// directory, is not one.
func executableFormat(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Deleted in the worktree and not yet staged.
			return "", nil
		}
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", nil
	}
	file, err := os.Open(path) //nolint:gosec // A path git lists under the repository.
	if err != nil {
		return "", err
	}
	defer file.Close()
	head := make([]byte, 4)
	read, err := io.ReadFull(file, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	head = head[:read]
	for _, candidate := range executableMagic {
		if bytes.HasPrefix(head, candidate.magic) {
			return candidate.format, nil
		}
	}
	return "", nil
}

// The check has to refuse the formats it names and pass what is not one, or a
// wrong magic reads exactly like a clean tree.
func TestExecutableFormatTellsABinaryFromText(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	cases := map[string][]byte{
		"text":  []byte("package main\n"),
		"short": []byte("M"),
		"empty": nil,
	}
	for _, candidate := range executableMagic {
		cases[candidate.format] = append(append([]byte{}, candidate.magic...), 0, 0, 0, 0)
	}
	for name, content := range cases {
		path := filepath.Join(directory, strings.ReplaceAll(name, " ", "-"))
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := executableFormat(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := ""
		if name != "text" && name != "short" && name != "empty" {
			want = name
		}
		if got != want {
			t.Errorf("%s: format %q, want %q", name, got, want)
		}
	}
}
