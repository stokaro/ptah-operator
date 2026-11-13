package dataplane

import (
	"fmt"
	"regexp"

	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

var gitCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// The kinds of source a pull report answers, as its source field names them.
const (
	PullSourceGit   = "git"
	PullSourceHTTPS = "https"
)

// SourcePullReport is the document `ptah schema pull` and `ptah migrations
// pull` print with --json for a Git or an HTTPS reference. Every kind names
// the reference, the selected path, the content digest of the selected files
// and the files themselves; the rest belongs to one kind.
type SourcePullReport struct {
	Source        string       `json:"source"`
	Reference     string       `json:"reference"`
	Path          string       `json:"path"`
	ContentDigest string       `json:"content_digest"`
	Files         []PulledFile `json:"files"`
	Written       []string     `json:"written"`

	// Commit is the commit a Git pull read.
	Commit string `json:"commit,omitempty"`

	// Format, DownloadDigest and DownloadSize are what an HTTPS pull read:
	// how the download carries the files, as the --format flag named it,
	// and the SHA-256 and the size of the bytes the URL served.
	Format         string `json:"format,omitempty"`
	DownloadDigest string `json:"download_digest,omitempty"`
	DownloadSize   int64  `json:"download_size,omitempty"`
}

// PulledFile is one file a pull read.
type PulledFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// PullRequest is what a pull was asked for: the kind of source, the reference
// and the path, and for HTTPS the format. A report answers it only if it
// names the same.
type PullRequest struct {
	Source    string
	Reference string
	Path      string
	Format    string
}

// DecodeSourcePull reads a pull's report and holds it to the pull it answers:
// the kind, the reference, the path and an HTTPS pull's format asked for,
// the evidence of that kind and no other's, and a content digest the listed
// files give. A report that names a digest its own file list does not give is
// refused, so the digest bound to a plan is the one the files on disk were
// written from.
func DecodeSourcePull(data []byte, asked PullRequest) (SourcePullReport, error) {
	var report SourcePullReport
	if err := decodeJSON(data, &report, false); err != nil {
		return SourcePullReport{}, fmt.Errorf("decode %s pull report: %w", pullKindName(asked.Source), err)
	}
	switch {
	case asked.Source != PullSourceGit && asked.Source != PullSourceHTTPS:
		return SourcePullReport{}, fmt.Errorf("no pull of a %q source is decoded", asked.Source)
	case report.Source != asked.Source:
		return SourcePullReport{}, fmt.Errorf("the pull report is not a %s one", pullKindName(asked.Source))
	case report.Reference != asked.Reference || report.Path != asked.Path:
		return SourcePullReport{}, fmt.Errorf("the pull report answers another reference or path")
	case !validDigest(report.ContentDigest):
		return SourcePullReport{}, fmt.Errorf("the pull report names no content digest")
	}
	if err := checkPullIdentity(report, asked); err != nil {
		return SourcePullReport{}, err
	}
	entries := make([]sourcecontent.Entry, 0, len(report.Files))
	for _, file := range report.Files {
		entries = append(entries, sourcecontent.Entry{Path: file.Path, Size: file.Size, SHA256: file.SHA256})
	}
	digest, err := sourcecontent.DigestOfEntries(entries)
	if err != nil {
		return SourcePullReport{}, fmt.Errorf("the pull report lists files the content digest refuses: %w", err)
	}
	if digest != report.ContentDigest {
		return SourcePullReport{}, fmt.Errorf("the pull report's files give %s, not the content digest it names", digest)
	}
	return report, nil
}

// checkPullIdentity holds a report to the evidence its kind gives: a Git
// pull's full commit, an HTTPS pull's format and download, and neither beside
// the other.
func checkPullIdentity(report SourcePullReport, asked PullRequest) error {
	https := report.Format != "" || report.DownloadDigest != "" || report.DownloadSize != 0
	switch asked.Source {
	case PullSourceGit:
		switch {
		case !gitCommitPattern.MatchString(report.Commit):
			return fmt.Errorf("the pull report names no full commit id")
		case https:
			return fmt.Errorf("the Git pull report names a download")
		}
	case PullSourceHTTPS:
		switch {
		case report.Commit != "":
			return fmt.Errorf("the HTTPS pull report names a commit")
		case report.Format != asked.Format:
			return fmt.Errorf("the pull report answers another format")
		case !validDigest(report.DownloadDigest):
			return fmt.Errorf("the pull report names no download digest")
		case report.DownloadSize <= 0:
			return fmt.Errorf("the pull report names no download size")
		}
	}
	return nil
}

func pullKindName(source string) string {
	switch source {
	case PullSourceGit:
		return "Git"
	case PullSourceHTTPS:
		return "HTTPS"
	}
	return "source"
}
