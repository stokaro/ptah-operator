package chartstamp

import (
	"errors"
	"fmt"
	"strings"
)

// chartRecordAfter is the manifest record the chart's reference follows.
const chartRecordAfter = "chart-asset-sha256"

// Manifest is what the chart's path reads from a release manifest. releaseverify
// holds the whole document to its exact shape; this reads the records it names and
// refuses a document that lacks one.
type Manifest struct {
	Release
	Version, SourceRepository, SourceRef string
	// Chart is the chart-oci record, empty before the release records it.
	Chart string
}

// ParseManifest reads a release manifest's key=value records.
func ParseManifest(document []byte) (Manifest, error) {
	fields := make(map[string]string)
	for _, line := range strings.Split(string(document), "\n") {
		if key, value, found := strings.Cut(line, "="); found {
			fields[key] = value
		}
	}
	for _, key := range []string{"version", "source-repository", "source-ref", "source-sha", "image", "executor", "executor-ptah-version"} {
		if fields[key] == "" {
			return Manifest{}, fmt.Errorf("the release manifest has no %s record", key)
		}
	}
	return Manifest{
		Release: Release{
			Image:       fields["image"],
			Executor:    fields["executor"],
			PtahVersion: fields["executor-ptah-version"],
			Revision:    fields["source-sha"],
		},
		Version:          fields["version"],
		SourceRepository: fields["source-repository"],
		SourceRef:        fields["source-ref"],
		Chart:            fields["chart-oci"],
	}, nil
}

// ChartDigest is the digest the chart-oci record names, which has to be a pinned
// reference into Repository.
func (m Manifest) ChartDigest() (string, error) {
	digest, found := strings.CutPrefix(m.Chart, Repository+"@")
	if !found || !sha256Digest.MatchString(digest) {
		return "", fmt.Errorf("the release manifest's chart-oci is %q, not %s@sha256:<64 lowercase hex>", m.Chart, Repository)
	}
	return digest, nil
}

// RecordInManifest returns the document with the stamped chart's reference added,
// computed from the asset and the identities the document already names.
func RecordInManifest(document, asset []byte) ([]byte, Artifact, error) {
	manifest, err := ParseManifest(document)
	if err != nil {
		return nil, Artifact{}, err
	}
	if manifest.Chart != "" {
		return nil, Artifact{}, errors.New("the release manifest already records chart-oci")
	}
	artifact, err := Build(asset, manifest.Release)
	if err != nil {
		return nil, Artifact{}, err
	}
	lines := strings.SplitAfter(string(document), "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, chartRecordAfter+"=") {
			record := "chart-oci=" + artifact.Reference() + "\n"
			updated := strings.Join(lines[:index+1], "") + record + strings.Join(lines[index+1:], "")
			return []byte(updated), artifact, nil
		}
	}
	return nil, Artifact{}, fmt.Errorf("the release manifest has no %s record to follow", chartRecordAfter)
}
