package runnerharness

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// The variables a run reads.
const (
	// RequireVariable says the harness has to run: a missing prerequisite then
	// fails it rather than skipping it. The make targets set it, so the target
	// CI runs can only pass by running every row, as PTAH_REQUIRE_ENVTEST does
	// for envtest. Unset, a plain `go test ./...` skips the run and touches no
	// Docker daemon.
	RequireVariable = "PTAH_REQUIRE_RUNNER_HARNESS"
	// ContextVariable names the Docker context to use. It has no default: every
	// container goes to the daemon somebody named.
	ContextVariable = "DOCKER_CONTEXT"
	// WriteVariable regenerates the committed captures.
	WriteVariable = "RUNNER_HARNESS_WRITE"
	// ImageVariable runs another executor image than the pin, to see what it
	// breaks. It cannot write captures.
	ImageVariable = "RUNNER_HARNESS_IMAGE"
	// PrefixVariable starts the name of everything the run creates, and a random
	// part follows it. Unset, the names start with ptah-runner-harness.
	PrefixVariable = "RUNNER_HARNESS_PREFIX"
	// OutVariable keeps the fresh captures of a run in this directory.
	OutVariable = "RUNNER_HARNESS_OUT"
)

// Gate is what a run was asked to do.
type Gate struct {
	Required bool
	Context  string
	Write    bool
	Image    string
	Prefix   string
	Out      string
}

// ReadGate reads the variables. A required run with no Docker context is an
// error rather than a skip.
func ReadGate(lookup func(string) string) (Gate, error) {
	gate := Gate{
		Required: lookup(RequireVariable) == "1",
		Context:  lookup(ContextVariable),
		Write:    lookup(WriteVariable) == "1",
		Image:    lookup(ImageVariable),
		Prefix:   lookup(PrefixVariable),
		Out:      lookup(OutVariable),
	}
	if gate.Required && gate.Context == "" {
		return Gate{}, fmt.Errorf("%s=1 and no %s: name the Docker context to run on", RequireVariable, ContextVariable)
	}
	return gate, nil
}

// PinnedExecutor reads the image, commit and release support/ptah.json pins for
// edge. The catalog is what a repin edits, so nothing here names a version.
func PinnedExecutor(catalog []byte) (image, commit, release string, err error) {
	var document struct {
		Releases []struct {
			Operator string `json:"operator"`
			Verified []struct {
				PtahRelease string `json:"ptahRelease"`
				PtahCommit  string `json:"ptahCommit"`
				PtahImage   string `json:"ptahImage"`
			} `json:"verified"`
		} `json:"releases"`
	}
	if err := json.Unmarshal(catalog, &document); err != nil {
		return "", "", "", fmt.Errorf("read the Ptah catalog: %w", err)
	}
	for _, row := range document.Releases {
		if row.Operator != "edge" {
			continue
		}
		if len(row.Verified) != 1 {
			return "", "", "", fmt.Errorf("edge verifies %d Ptah builds, want one", len(row.Verified))
		}
		pinned := row.Verified[0]
		if !digestPinned.MatchString(pinned.PtahImage) {
			return "", "", "", fmt.Errorf("edge pins %q, not a digest", pinned.PtahImage)
		}
		return pinned.PtahImage, pinned.PtahCommit, pinned.PtahRelease, nil
	}
	return "", "", "", errors.New("the Ptah catalog has no edge row")
}

var digestPinned = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// DrivenImages reads the database and registry images the lifecycle driver pins.
func DrivenImages(driver []byte) (postgres, mysql, registry string, err error) {
	read := func(variable string) (string, error) {
		pattern := regexp.MustCompile(variable + `=\$\{` + variable + `:-([^}\s]+@sha256:[0-9a-f]{64})\}`)
		match := pattern.FindSubmatch(driver)
		if match == nil {
			return "", fmt.Errorf("the lifecycle driver pins no %s", variable)
		}
		return string(match[1]), nil
	}
	if postgres, err = read("E2E_POSTGRES_SOURCE_IMAGE"); err != nil {
		return "", "", "", err
	}
	if mysql, err = read("E2E_MYSQL_SOURCE_IMAGE"); err != nil {
		return "", "", "", err
	}
	if registry, err = read("E2E_REGISTRY_IMAGE"); err != nil {
		return "", "", "", err
	}
	return postgres, mysql, registry, nil
}
