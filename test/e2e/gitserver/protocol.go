package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/stokaro/ptah-operator/test/e2e/gitrepo"
)

// The server speaks upload-pack, protocol version 0, to a client that sends
// its whole request at once: what go-git sends, over smart HTTP and over SSH
// alike. It answers a want with that commit and as many ancestors as the
// request's depth allows, never a delta against something the client has.

// served is a repository as the server answers from it: its objects and a
// copy of its refs, which a test may change.
type served struct {
	repository *gitrepo.Repository
	storage    *memory.Storage
	refs       map[plumbing.ReferenceName]plumbing.Hash
}

func newServed(repository *gitrepo.Repository) *served {
	return &served{repository: repository, storage: repository.Storage(), refs: repository.Refs()}
}

// Commit is the id of the commit at index of the Spec.
func (s *served) Commit(index int) plumbing.Hash { return s.repository.Commit(index) }

// advertise writes the refs and the capabilities, with the smart HTTP
// service header when service is set.
func advertise(w io.Writer, repository *served, byID bool, service bool) error {
	advertised := packp.NewAdvRefs()
	for _, set := range []struct {
		capability capability.Capability
		values     []string
	}{
		{capability.Agent, []string{"ptah-e2e-git-server"}},
		{capability.OFSDelta, nil},
		{capability.Shallow, nil},
		{capability.Sideband64k, nil},
		{capability.NoProgress, nil},
		{capability.SymRef, []string{"HEAD:refs/heads/main"}},
	} {
		if err := advertised.Capabilities.Set(set.capability, set.values...); err != nil {
			return err
		}
	}
	if byID {
		if err := advertised.Capabilities.Set(capability.AllowReachableSHA1InWant); err != nil {
			return err
		}
	}
	head := repository.refs[plumbing.NewBranchReferenceName("main")]
	advertised.Head = &head
	for name, hash := range repository.refs {
		advertised.References[name.String()] = hash
	}
	if service {
		advertised.Prefix = [][]byte{[]byte("# service=git-upload-pack"), pktline.Flush}
	}
	return advertised.Encode(w)
}

// uploadPack reads one request and writes its response: the shallow
// boundary when the request asked for a depth, a NAK, and the pack, on the
// side band when the request asked for one.
func uploadPack(r io.Reader, w io.Writer, repository *served, byID bool) error {
	buffered := bufio.NewReader(r)
	request := packp.NewUploadRequest()
	if err := request.Decode(buffered); err != nil {
		return fmt.Errorf("decode the upload request: %w", err)
	}
	// The haves and "done" follow; a client that has nothing sends none.
	_, _ = io.Copy(io.Discard, buffered)
	if len(request.Wants) == 0 {
		return errors.New("the request wants nothing")
	}
	if len(request.Shallows) > 0 {
		return errors.New("the request names commits it already has shallowly")
	}
	advertisedTips := map[plumbing.Hash]bool{}
	for _, hash := range repository.refs {
		advertisedTips[hash] = true
	}
	for _, want := range request.Wants {
		if !advertisedTips[want] && !byID {
			return fmt.Errorf("%s is not a ref tip, and fetching by id is off", want)
		}
		if _, err := repository.storage.EncodedObject(plumbing.CommitObject, want); err != nil {
			return fmt.Errorf("want %s: %w", want, err)
		}
	}
	depth := 0
	if commits, ok := request.Depth.(packp.DepthCommits); ok {
		depth = int(commits)
	}
	commits, boundary, err := history(repository, request.Wants, depth)
	if err != nil {
		return err
	}
	if depth > 0 {
		update := &packp.ShallowUpdate{Shallows: boundary}
		if err := update.Encode(w); err != nil {
			return err
		}
	}
	if err := pktline.NewEncoder(w).Encodef("NAK\n"); err != nil {
		return err
	}
	objects, err := reachable(repository, commits)
	if err != nil {
		return err
	}
	if !request.Capabilities.Supports(capability.Sideband64k) {
		_, err := packfile.NewEncoder(w, repository.storage, false).Encode(objects, 0)
		return err
	}
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, repository.storage, false).Encode(objects, 0); err != nil {
		return err
	}
	if _, err := sideband.NewMuxer(sideband.Sideband64k, w).Write(pack.Bytes()); err != nil {
		return err
	}
	return pktline.NewEncoder(w).Flush()
}

// history walks back from the wants, depth commits deep, or to the root when
// depth is zero, and returns the commits and the ones at the edge whose
// parents were left out.
func history(repository *served, wants []plumbing.Hash, depth int) ([]plumbing.Hash, []plumbing.Hash, error) {
	seen := map[plumbing.Hash]bool{}
	var commits, boundary []plumbing.Hash
	level := append([]plumbing.Hash(nil), wants...)
	for distance := 1; len(level) > 0; distance++ {
		var next []plumbing.Hash
		for _, hash := range level {
			if seen[hash] {
				continue
			}
			seen[hash] = true
			commits = append(commits, hash)
			commit, err := object.GetCommit(repository.storage, hash)
			if err != nil {
				return nil, nil, err
			}
			if depth > 0 && distance == depth {
				if commit.NumParents() > 0 {
					boundary = append(boundary, hash)
				}
				continue
			}
			next = append(next, commit.ParentHashes...)
		}
		level = next
	}
	sort.Slice(boundary, func(i, j int) bool { return boundary[i].String() < boundary[j].String() })
	return commits, boundary, nil
}

// reachable is every object the commits need: themselves, their trees and
// their blobs.
func reachable(repository *served, commits []plumbing.Hash) ([]plumbing.Hash, error) {
	seen := map[plumbing.Hash]bool{}
	var objects []plumbing.Hash
	add := func(hash plumbing.Hash) bool {
		if seen[hash] {
			return false
		}
		seen[hash] = true
		objects = append(objects, hash)
		return true
	}
	var walk func(plumbing.Hash) error
	walk = func(hash plumbing.Hash) error {
		if !add(hash) {
			return nil
		}
		tree, err := object.GetTree(repository.storage, hash)
		if err != nil {
			return err
		}
		for _, entry := range tree.Entries {
			if entry.Mode == filemode.Submodule {
				// A submodule links to a commit of another repository, and
				// Git sends the link, not what it links to.
				continue
			}
			if entry.Mode.IsFile() {
				add(entry.Hash)
				continue
			}
			if err := walk(entry.Hash); err != nil {
				return err
			}
		}
		return nil
	}
	for _, hash := range commits {
		commit, err := object.GetCommit(repository.storage, hash)
		if err != nil {
			return nil, err
		}
		add(hash)
		if err := walk(commit.TreeHash); err != nil {
			return nil, err
		}
	}
	return objects, nil
}
