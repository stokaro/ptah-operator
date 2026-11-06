package runnerharness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// CapturesDirectory holds every committed capture, relative to the repository
// root: documents at the paths the decoders' tests read them from, and the
// transcript that says which row printed each one.
const CapturesDirectory = "internal/dataplane/testdata"

const (
	transcriptFile = "runner-harness/transcript.json"
	provenanceFile = "runner-harness/provenance.json"
	documentsDir   = "runner-harness/documents"
	// directoryRows keep the layout their consumers read: args, stdout, stderr
	// and exit-code in one directory per row, and nothing in the transcript.
	directoryRows = "git-pull"
)

// Child is what one Ptah command did, in the form it is committed: arguments,
// standard error and status in the transcript, with the volatile values masked,
// and standard output in a file of its own, as printed.
type Child struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Exit    int      `json:"exit"`
	Stderr  string   `json:"stderr,omitempty"`
	// Stdout is the file holding what it printed, under CapturesDirectory. A
	// child that printed nothing has none.
	Stdout string `json:"stdout,omitempty"`
}

// Row is one operation's run, or one command a row ran on its own.
type Row struct {
	Name      string `json:"name"`
	Operation string `json:"operation,omitempty"`
	Source    Source `json:"source,omitempty"`
	// Engine is the engine the row's target runs, as a plan's decoder is told it.
	Engine   string  `json:"engine,omitempty"`
	Children []Child `json:"children"`
	// Refusal is the error a fetch or a guard returned.
	Refusal string `json:"refusal,omitempty"`
	// Result is the frame's payload as the manager's parser read it.
	Result any `json:"result,omitempty"`
	// SavedPlan is the file holding the plan a Plan operation saved.
	SavedPlan string `json:"savedPlan,omitempty"`
}

// Captures is a complete set: the rows, every document they name by its path
// under CapturesDirectory, and what they were captured from.
type Captures struct {
	Rows       []Row
	Documents  map[string][]byte
	Provenance Provenance
}

// Provenance is the Ptah build a set of captures was taken from.
type Provenance struct {
	PtahRelease string `json:"ptahRelease"`
	PtahCommit  string `json:"ptahCommit"`
	PtahImage   string `json:"ptahImage"`
}

// DocumentPath is where a row's document is stored when the row names no other
// path: under the row's own name, numbered by the child that printed it.
func DocumentPath(row string, index int, command string, document []byte) string {
	extension := ".txt"
	if json.Valid(document) {
		extension = ".json"
	}
	name := fmt.Sprintf("%d-%s%s", index, strings.ReplaceAll(strings.TrimPrefix(command, "--"), " ", "-"), extension)
	return path.Join(documentsDir, row, name)
}

// directoryRow says whether a row is stored in the per-directory layout.
func directoryRow(name string) bool {
	return strings.HasPrefix(name, directoryRows+"/")
}

// LoadCaptures reads every committed capture.
func LoadCaptures(root string) (Captures, error) {
	captures := Captures{Documents: map[string][]byte{}}
	transcript, err := os.ReadFile(filepath.Join(root, transcriptFile)) // #nosec G304 -- the repository's own captures
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return Captures{}, err
	default:
		if err := json.Unmarshal(transcript, &captures.Rows); err != nil {
			return Captures{}, fmt.Errorf("read %s: %w", transcriptFile, err)
		}
	}
	provenance, err := os.ReadFile(filepath.Join(root, provenanceFile)) // #nosec G304 -- the repository's own captures
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return Captures{}, err
	default:
		if err := json.Unmarshal(provenance, &captures.Provenance); err != nil {
			return Captures{}, fmt.Errorf("read %s: %w", provenanceFile, err)
		}
	}
	directories, err := os.ReadDir(filepath.Join(root, directoryRows))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Captures{}, err
	}
	for _, directory := range directories {
		row, stdout, err := readDirectoryRow(root, directoryRows+"/"+directory.Name())
		if err != nil {
			return Captures{}, err
		}
		captures.Rows = append(captures.Rows, row)
		if len(stdout) != 0 {
			captures.Documents[row.Children[0].Stdout] = stdout
		}
	}
	slices.SortFunc(captures.Rows, func(a, b Row) int { return strings.Compare(a.Name, b.Name) })
	for _, row := range captures.Rows {
		for _, name := range row.documents() {
			if _, loaded := captures.Documents[name]; loaded {
				continue
			}
			document, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name))) // #nosec G304 -- the repository's own captures
			if err != nil {
				return Captures{}, fmt.Errorf("row %s: %w", row.Name, err)
			}
			captures.Documents[name] = document
		}
	}
	return captures, nil
}

func readDirectoryRow(root, name string) (Row, []byte, error) {
	directory := filepath.Join(root, filepath.FromSlash(name))
	read := func(file string) ([]byte, error) {
		return os.ReadFile(filepath.Join(directory, file)) // #nosec G304 -- the repository's own captures
	}
	args, err := read("args")
	if err != nil {
		return Row{}, nil, err
	}
	stdout, err := read("stdout")
	if err != nil {
		return Row{}, nil, err
	}
	stderr, err := read("stderr")
	if err != nil {
		return Row{}, nil, err
	}
	exitText, err := read("exit-code")
	if err != nil {
		return Row{}, nil, err
	}
	exit, err := strconv.Atoi(strings.TrimSpace(string(exitText)))
	if err != nil {
		return Row{}, nil, fmt.Errorf("%s: %w", name, err)
	}
	child := Child{Args: strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"), Exit: exit, Stderr: string(stderr)}
	child.Command = CommandWords(child.Args)
	if len(stdout) != 0 {
		child.Stdout = name + "/stdout"
	}
	return Row{Name: name, Children: []Child{child}}, stdout, nil
}

// documents is every file the row names.
func (r Row) documents() []string {
	var names []string
	for _, child := range r.Children {
		if child.Stdout != "" {
			names = append(names, child.Stdout)
		}
	}
	if r.SavedPlan != "" {
		names = append(names, r.SavedPlan)
	}
	return names
}

// Change is one way a fresh run differs from the committed captures.
type Change struct {
	Row        string
	Difference Difference
}

func (c Change) String() string { return c.Row + ": " + c.Difference.String() }

// CompareCaptures reports every change of shape between two sets, and a row or
// a document in one and not the other. What it compares under each row is the
// whole record, so an exit status, an argument, a line of standard error and the
// frame's payload are held exactly as a document's fields are.
func CompareCaptures(masker *Masker, committed, fresh Captures) []Change {
	var changes []Change
	byName := func(rows []Row) map[string]Row {
		named := make(map[string]Row, len(rows))
		for _, row := range rows {
			named[row.Name] = row
		}
		return named
	}
	committedRows, freshRows := byName(committed.Rows), byName(fresh.Rows)
	var names []string
	for name := range committedRows {
		names = append(names, name)
	}
	for name := range freshRows {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range slices.Compact(names) {
		before, inCommitted := committedRows[name]
		after, inFresh := freshRows[name]
		switch {
		case !inCommitted:
			changes = append(changes, Change{name, Difference{Path: "row", Kind: "added", Fresh: "a row nothing committed"}})
			continue
		case !inFresh:
			changes = append(changes, Change{name, Difference{Path: "row", Kind: "removed", Committed: "a row nothing ran"}})
			continue
		}
		beforeValue, err := asValue(before)
		if err != nil {
			changes = append(changes, Change{name, Difference{Path: "row", Kind: "changed", Committed: err.Error()}})
			continue
		}
		afterValue, err := asValue(after)
		if err != nil {
			changes = append(changes, Change{name, Difference{Path: "row", Kind: "changed", Fresh: err.Error()}})
			continue
		}
		for _, difference := range CompareValues(masker, beforeValue, afterValue) {
			changes = append(changes, Change{name, difference})
		}
		for _, document := range after.documents() {
			committedDocument, present := committed.Documents[document]
			if !present {
				continue // the row's own record already reports the new path
			}
			for _, difference := range CompareDocuments(masker, committedDocument, fresh.Documents[document]) {
				difference.Path = document + " " + difference.Path
				changes = append(changes, Change{name, difference})
			}
		}
	}
	return changes
}

func asValue(row Row) (any, error) {
	encoded, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// WriteCaptures stores fresh as the committed captures under root.
//
// A document whose only differences from the committed one are volatile values
// stays as it is, so a repin's diff holds what the release changed and nothing
// else. A file under root that no row names is removed.
func WriteCaptures(root string, masker *Masker, committed, fresh Captures) error {
	transcript := make([]Row, 0, len(fresh.Rows))
	for _, row := range fresh.Rows {
		if !directoryRow(row.Name) {
			transcript = append(transcript, row)
			continue
		}
		if err := writeDirectoryRow(root, row); err != nil {
			return err
		}
	}
	encoded, err := encodeJSON(transcript)
	if err != nil {
		return err
	}
	if err := writeIfChanged(filepath.Join(root, transcriptFile), encoded); err != nil {
		return err
	}
	provenance, err := encodeJSON(fresh.Provenance)
	if err != nil {
		return err
	}
	if err := writeIfChanged(filepath.Join(root, provenanceFile), provenance); err != nil {
		return err
	}
	for name, document := range fresh.Documents {
		if before, present := committed.Documents[name]; present && len(CompareDocuments(masker, before, document)) == 0 {
			continue
		}
		if err := writeIfChanged(filepath.Join(root, filepath.FromSlash(name)), document); err != nil {
			return err
		}
	}
	return removeUnnamedFiles(root, fresh)
}

func writeDirectoryRow(root string, row Row) error {
	if len(row.Children) != 1 {
		return fmt.Errorf("%s: a directory row holds one command, not %d", row.Name, len(row.Children))
	}
	child := row.Children[0]
	directory := filepath.Join(root, filepath.FromSlash(row.Name))
	for file, content := range map[string][]byte{
		"args":      []byte(strings.Join(child.Args, "\n") + "\n"),
		"stderr":    []byte(child.Stderr),
		"exit-code": []byte(strconv.Itoa(child.Exit) + "\n"),
	} {
		if err := writeIfChanged(filepath.Join(directory, file), content); err != nil {
			return err
		}
	}
	if child.Stdout == "" {
		return writeIfChanged(filepath.Join(directory, "stdout"), nil)
	}
	return nil
}

func writeIfChanged(file string, content []byte) error {
	if existing, err := os.ReadFile(file); err == nil && bytes.Equal(existing, content) { // #nosec G304 -- the repository's own captures
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		return err
	}
	return os.WriteFile(file, content, 0o600)
}

func encodeJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// StrayFiles names the files under root that no row of the set names: a document
// the run no longer prints, which no test would ever read again.
func StrayFiles(root string, captures Captures) ([]string, error) {
	named := map[string]bool{transcriptFile: true, provenanceFile: true}
	for _, row := range captures.Rows {
		for _, document := range row.documents() {
			named[document] = true
		}
		if directoryRow(row.Name) {
			for _, file := range []string{"args", "stdout", "stderr", "exit-code"} {
				named[row.Name+"/"+file] = true
			}
		}
	}
	var stray []string
	err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		if name := filepath.ToSlash(relative); !named[name] {
			stray = append(stray, name)
		}
		return nil
	})
	return stray, err
}

func removeUnnamedFiles(root string, fresh Captures) error {
	stray, err := StrayFiles(root, fresh)
	if err != nil {
		return err
	}
	for _, name := range stray {
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			return err
		}
	}
	return nil
}
