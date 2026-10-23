package dataplane

import (
	"fmt"
	"regexp"

	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

var gitCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// GitPullReport is the document `ptah schema pull` and `ptah migrations pull`
// print with --json for a git+https:// or git+ssh:// reference.
type GitPullReport struct {
	Source        string          `json:"source"`
	Reference     string          `json:"reference"`
	Commit        string          `json:"commit"`
	Path          string          `json:"path"`
	ContentDigest string          `json:"content_digest"`
	Files         []GitPulledFile `json:"files"`
	Written       []string        `json:"written"`
}

// GitPulledFile is one file a Git pull read.
type GitPulledFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// DecodeGitPull reads a Git pull's report and holds it to the pull it
// answers: the reference and the path asked for, a full commit id, and a
// content digest the listed files give. A report that names a digest its own
// file list does not give is refused, so the digest bound to a plan is the
// one the files on disk were written from.
func DecodeGitPull(data []byte, reference, selected string) (GitPullReport, error) {
	var report GitPullReport
	if err := decodeJSON(data, &report, false); err != nil {
		return GitPullReport{}, fmt.Errorf("decode Git pull report: %w", err)
	}
	switch {
	case report.Source != "git":
		return GitPullReport{}, fmt.Errorf("the pull report is not a Git one")
	case report.Reference != reference || report.Path != selected:
		return GitPullReport{}, fmt.Errorf("the pull report answers another reference or path")
	case !gitCommitPattern.MatchString(report.Commit):
		return GitPullReport{}, fmt.Errorf("the pull report names no full commit id")
	case !validDigest(report.ContentDigest):
		return GitPullReport{}, fmt.Errorf("the pull report names no content digest")
	}
	entries := make([]sourcecontent.Entry, 0, len(report.Files))
	for _, file := range report.Files {
		entries = append(entries, sourcecontent.Entry{Path: file.Path, Size: file.Size, SHA256: file.SHA256})
	}
	digest, err := sourcecontent.DigestOfEntries(entries)
	if err != nil {
		return GitPullReport{}, fmt.Errorf("the pull report lists files the content digest refuses: %w", err)
	}
	if digest != report.ContentDigest {
		return GitPullReport{}, fmt.Errorf("the pull report's files give %s, not the content digest it names", digest)
	}
	return report, nil
}
