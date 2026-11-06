// Package gitrefusal holds the words of Ptah's Git pull refusals that tests
// read. The git-sources acceptance rows match them in a condition message, and
// a unit test of the runner matches them in what it makes of the standard error
// captured from the pinned image, under internal/dataplane/testdata/git-pull.
// Sharing them means a Ptah release that rewords a refusal fails go test before
// it fails on kind.
//
// It has no build tag, so the e2e suite and the unit tests both import it. No
// program imports it: the operator does not depend on Ptah's wording.
package gitrefusal

const (
	// Submodule precedes the selected path.
	Submodule = "selected path is a submodule: "
	// LFSPointer precedes the selected path.
	LFSPointer = "selected path is a Git LFS pointer: "
	// HostKeyMismatch is a server whose host key the known hosts do not pin.
	HostKeyMismatch = "knownhosts: key mismatch"
	// DigestDiffers precedes the selected path, for content that does not give
	// the digest --expect-digest names.
	DigestDiffers = "content digest differs: "
	// CommitNotNamed precedes the commit id, for a commit asked for alone that
	// the server does not hold or does not give by its id.
	CommitNotNamed = "no branch or tag names commit "
	// CommitNotAvailable precedes the commit id, for a commit asked for with a
	// branch or tag whose history does not hold it.
	CommitNotAvailable = "commit not available: commit "
)
