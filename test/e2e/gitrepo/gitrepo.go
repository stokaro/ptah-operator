// Package gitrepo builds the Git repository the acceptance phase reads
// schemas and migrations from: a linear history described by a Spec, written
// into an in-memory object store with fixed authors and times, so the same
// Spec always gives the same commit ids. The fixture server serves it, and
// the phase computes the ids it expects from the same Spec.
package gitrepo

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Spec is the repository the server serves: a linear history, each commit a
// whole tree of files, and the refs that name commits by their index.
// Commits are written with a fixed author and a clock that starts at a fixed
// instant, so the same Spec always gives the same commit ids.
type Spec struct {
	Commits  []CommitSpec   `json:"commits"`
	Branches map[string]int `json:"branches"`
	Tags     map[string]int `json:"tags"`
}

// CommitSpec is one commit: its message, every file of its tree, and the
// submodules in it, each a path and the commit id it links to. A submodule's
// commit is not in this repository, as it would not be in a real one.
type CommitSpec struct {
	Message    string            `json:"message"`
	Files      map[string]string `json:"files"`
	Submodules map[string]string `json:"submodules,omitempty"`
}

// Repository is a built Spec.
type Repository struct {
	storage *memory.Storage
	refs    map[plumbing.ReferenceName]plumbing.Hash
	// commits are the ids, in the Spec's order.
	commits []plumbing.Hash
}

// Storage is the repository's object store.
func (r *Repository) Storage() *memory.Storage { return r.storage }

// Refs is every branch and tag, by its full name. The caller may change the
// map it gets: it is a copy.
func (r *Repository) Refs() map[plumbing.ReferenceName]plumbing.Hash {
	refs := make(map[plumbing.ReferenceName]plumbing.Hash, len(r.refs))
	for name, hash := range r.refs {
		refs[name] = hash
	}
	return refs
}

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// ParseSpec reads a Spec and refuses one whose refs name no commit.
func ParseSpec(data []byte) (Spec, error) {
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("decode the repository spec: %w", err)
	}
	if len(spec.Commits) == 0 {
		return Spec{}, errors.New("the repository spec has no commit")
	}
	for kind, refs := range map[string]map[string]int{"branch": spec.Branches, "tag": spec.Tags} {
		for name, index := range refs {
			if index < 0 || index >= len(spec.Commits) {
				return Spec{}, fmt.Errorf("%s %s names commit %d of %d", kind, name, index, len(spec.Commits))
			}
		}
	}
	if _, ok := spec.Branches["main"]; !ok {
		return Spec{}, errors.New("the repository spec has no main branch")
	}
	return spec, nil
}

// Build writes every commit of a Spec into a fresh object store.
func Build(spec Spec) (*Repository, error) {
	repository := &Repository{storage: memory.NewStorage(), refs: map[plumbing.ReferenceName]plumbing.Hash{}}
	var parent plumbing.Hash
	for index, commit := range spec.Commits {
		tree, err := repository.writeTree(commit.Files, commit.Submodules)
		if err != nil {
			return nil, fmt.Errorf("commit %d: %w", index, err)
		}
		signature := object.Signature{Name: "Ptah E2E", Email: "e2e@ptah.run", When: epoch.Add(time.Duration(index) * time.Minute)}
		written := &object.Commit{Author: signature, Committer: signature, Message: commit.Message + "\n", TreeHash: tree}
		if !parent.IsZero() {
			written.ParentHashes = []plumbing.Hash{parent}
		}
		hash, err := repository.write(written)
		if err != nil {
			return nil, err
		}
		repository.commits = append(repository.commits, hash)
		parent = hash
	}
	for name, index := range spec.Branches {
		repository.refs[plumbing.NewBranchReferenceName(name)] = repository.commits[index]
	}
	for name, index := range spec.Tags {
		repository.refs[plumbing.NewTagReferenceName(name)] = repository.commits[index]
	}
	return repository, nil
}

type encodable interface {
	Encode(plumbing.EncodedObject) error
}

func (r *Repository) write(value encodable) (plumbing.Hash, error) {
	encoded := r.storage.NewEncodedObject()
	if err := value.Encode(encoded); err != nil {
		return plumbing.ZeroHash, err
	}
	return r.storage.SetEncodedObject(encoded)
}

// writeTree writes the trees of a set of files and submodules and returns
// the root's id.
func (r *Repository) writeTree(files, submodules map[string]string) (plumbing.Hash, error) {
	type directory struct {
		files       map[string]plumbing.Hash
		links       map[string]plumbing.Hash
		directories map[string]*directory
	}
	newDirectory := func() *directory {
		return &directory{files: map[string]plumbing.Hash{}, links: map[string]plumbing.Hash{}, directories: map[string]*directory{}}
	}
	root := newDirectory()
	clean := func(name string) error {
		if name == "" || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("the file name %q is not a clean relative path", name)
		}
		return nil
	}
	parent := func(name string) (*directory, string) {
		current := root
		elements := strings.Split(name, "/")
		for _, element := range elements[:len(elements)-1] {
			next, ok := current.directories[element]
			if !ok {
				next = newDirectory()
				current.directories[element] = next
			}
			current = next
		}
		return current, elements[len(elements)-1]
	}
	for name, commit := range submodules {
		if err := clean(name); err != nil {
			return plumbing.ZeroHash, err
		}
		if _, isFile := files[name]; isFile {
			return plumbing.ZeroHash, fmt.Errorf("%s is both a file and a submodule", name)
		}
		if !plumbing.IsHash(commit) {
			return plumbing.ZeroHash, fmt.Errorf("the submodule %s links to %q, which is not a commit id", name, commit)
		}
		directory, base := parent(name)
		directory.links[base] = plumbing.NewHash(commit)
	}
	for name, content := range files {
		if err := clean(name); err != nil {
			return plumbing.ZeroHash, err
		}
		blob := r.storage.NewEncodedObject()
		blob.SetType(plumbing.BlobObject)
		writer, err := blob.Writer()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			return plumbing.ZeroHash, err
		}
		if err := writer.Close(); err != nil {
			return plumbing.ZeroHash, err
		}
		hash, err := r.storage.SetEncodedObject(blob)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		current, base := parent(name)
		current.files[base] = hash
	}
	var write func(*directory) (plumbing.Hash, error)
	write = func(current *directory) (plumbing.Hash, error) {
		tree := &object.Tree{}
		for name, hash := range current.files {
			tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: hash})
		}
		for name, hash := range current.links {
			tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: filemode.Submodule, Hash: hash})
		}
		for name, child := range current.directories {
			hash, err := write(child)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: hash})
		}
		// Git orders a tree's entries by name, a directory's name as if it
		// ended in "/".
		sortKey := func(entry object.TreeEntry) string {
			if entry.Mode == filemode.Dir {
				return entry.Name + "/"
			}
			return entry.Name
		}
		sort.Slice(tree.Entries, func(i, j int) bool { return sortKey(tree.Entries[i]) < sortKey(tree.Entries[j]) })
		return r.write(tree)
	}
	return write(root)
}

// Commit returns the id of the commit at index.
func (r *Repository) Commit(index int) plumbing.Hash { return r.commits[index] }
