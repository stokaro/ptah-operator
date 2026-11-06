// chartpublish is the release workflow's program for the chart it publishes to an
// OCI registry, one subcommand per workflow step. Each reads the release manifest,
// so a step is this command and nothing around it.
//
//	record  adds the stamped chart's reference to a manifest being written
//	stage   pushes the stamped chart by digest, under no tag
//	verify  reads it without signing in, and checks its signature and attestation
//	tag     gives it its version tag, once the release is published
//	stamp   computes it offline, for anyone holding the asset and the manifest
//
// A write authenticates as GITHUB_ACTOR with the token in GH_TOKEN, read here so it
// reaches no argument, file or log. A read never authenticates: it stands where a
// reader stands.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/stokaro/ptah-operator/hack/chartstamp"
)

// GitHub's attestation, signature and release reads can trail the write that made
// them, so each is tried on the schedule the release workflow's own loops use.
const (
	readAttempts   = 8
	firstReadDelay = 2 * time.Second
	lastReadDelay  = 30 * time.Second
)

type program struct {
	stdout io.Writer
	getenv func(string) string
	// run executes a child program and returns its standard output.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// wait is the pause between attempts.
	wait func(time.Duration)
}

type options struct {
	chart, manifest, output, repository string
	plainHTTP                           bool
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	err := program{stdout: os.Stdout, getenv: os.Getenv, run: runChild, wait: time.Sleep}.dispatch(ctx, os.Args[1:])
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "chartpublish:", err)
		os.Exit(1)
	}
}

func runChild(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, args[0], err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

func (p program) dispatch(ctx context.Context, args []string) error {
	const usage = "usage: chartpublish record|stage|verify|tag|stamp -manifest FILE [-chart FILE] [-output DIR]"
	if len(args) == 0 {
		return errors.New(usage)
	}
	var opts options
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.StringVar(&opts.chart, "chart", "", "packaged chart")
	flags.StringVar(&opts.manifest, "manifest", "", "release manifest")
	flags.StringVar(&opts.output, "output", "", "directory stamp writes the chart to")
	flags.StringVar(&opts.repository, "repository", chartstamp.Repository, "registry repository, for a registry other than the release's")
	flags.BoolVar(&opts.plainHTTP, "plain-http", false, "reach that registry over plain HTTP")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if opts.manifest == "" {
		return errors.New(usage)
	}
	switch args[0] {
	case "record":
		return p.record(opts)
	case "stamp":
		return p.stamp(opts)
	case "stage":
		return p.stage(ctx, opts)
	case "verify":
		return p.verify(ctx, opts)
	case "tag":
		return p.tag(ctx, opts)
	default:
		return errors.New(usage)
	}
}

func (p program) record(opts options) error {
	document, err := os.ReadFile(opts.manifest)
	if err != nil {
		return fmt.Errorf("read the release manifest: %w", err)
	}
	asset, err := os.ReadFile(opts.chart)
	if err != nil {
		return fmt.Errorf("read the packaged chart: %w", err)
	}
	updated, artifact, err := chartstamp.RecordInManifest(document, asset)
	if err != nil {
		return err
	}
	if err := os.WriteFile(opts.manifest, updated, 0o644); err != nil { //nolint:gosec // A release asset every reader may read.
		return fmt.Errorf("write the release manifest: %w", err)
	}
	fmt.Fprintln(p.stdout, artifact.Reference())
	return nil
}

func (p program) stamp(opts options) error {
	_, artifact, err := load(opts)
	if err != nil {
		return err
	}
	if opts.output != "" {
		if err := os.MkdirAll(opts.output, 0o750); err != nil {
			return fmt.Errorf("create the output directory: %w", err)
		}
		path := filepath.Join(opts.output, artifact.Name+"-"+artifact.Version+".tgz")
		if err := os.WriteFile(path, artifact.Chart, 0o644); err != nil { //nolint:gosec // A chart every reader may read.
			return fmt.Errorf("write the stamped chart: %w", err)
		}
	}
	fmt.Fprintln(p.stdout, artifact.Reference())
	return nil
}

func (p program) stage(ctx context.Context, opts options) error {
	manifest, artifact, err := load(opts)
	if err != nil {
		return err
	}
	if manifest.Chart == "" {
		return errors.New("the release manifest records no chart-oci to stage")
	}
	target, err := p.writer(opts)
	if err != nil {
		return err
	}
	if err := chartstamp.Stage(ctx, target, artifact); err != nil {
		return err
	}
	reference := opts.repository + "@" + artifact.Digest
	fmt.Fprintln(p.stdout, reference)
	return p.writeOutputs(map[string]string{"repository": opts.repository, "digest": artifact.Digest, "reference": reference})
}

// verify is everything a reader can check before the release is published, and a
// failure here leaves the release a draft. It reads the registry first, since a
// first release stops there until a new package is made public.
func (p program) verify(ctx context.Context, opts options) error {
	manifest, digest, err := recorded(opts)
	if err != nil {
		return err
	}
	anonymous, err := reader(opts)
	if err != nil {
		return err
	}
	if err := chartstamp.ReadBack(ctx, anonymous, digest, digest); err != nil {
		return fmt.Errorf("read the chart without signing in, as a reader does: %w; a new package is private until it is made public, and a rerun resumes the transaction", err)
	}
	reference := opts.repository + "@" + digest
	workflow := manifest.SourceRepository + "/.github/workflows/release.yml"
	for _, command := range [][]string{
		{"cosign", "verify",
			"--certificate-identity", "https://github.com/" + workflow + "@" + manifest.SourceRef,
			"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
			reference},
		{"gh", "attestation", "verify", "oci://" + reference,
			"--repo", manifest.SourceRepository,
			"--source-ref", manifest.SourceRef,
			"--source-digest", manifest.Revision,
			"--signer-workflow", workflow},
	} {
		if _, err := p.withRetries(ctx, command[0], command[1:]...); err != nil {
			return err
		}
	}
	fmt.Fprintln(p.stdout, "verified", reference)
	return nil
}

// tag is the last step of a publication: it refuses a release that is not
// published and immutable, so the name a reader installs by exists for no other.
func (p program) tag(ctx context.Context, opts options) error {
	manifest, digest, err := recorded(opts)
	if err != nil {
		return err
	}
	if err := p.requirePublished(ctx, manifest); err != nil {
		return err
	}
	target, err := p.writer(opts)
	if err != nil {
		return err
	}
	if err := chartstamp.Tag(ctx, target, digest, manifest.Version); err != nil {
		return err
	}
	anonymous, err := reader(opts)
	if err != nil {
		return err
	}
	if err := chartstamp.ReadBack(ctx, anonymous, manifest.Version, digest); err != nil {
		return fmt.Errorf("read the version tag without signing in, as a reader does: %w", err)
	}
	fmt.Fprintf(p.stdout, "%s:%s is %s\n", opts.repository, manifest.Version, digest)
	return nil
}

func (p program) requirePublished(ctx context.Context, manifest chartstamp.Manifest) error {
	output, err := p.withRetries(ctx, "gh", "release", "view", "v"+manifest.Version,
		"--repo", manifest.SourceRepository, "--json", "isDraft,isImmutable")
	if err != nil {
		return err
	}
	var state struct {
		IsDraft     bool `json:"isDraft"`
		IsImmutable bool `json:"isImmutable"`
	}
	if err := json.Unmarshal(output, &state); err != nil {
		return fmt.Errorf("read the state of v%s: %w", manifest.Version, err)
	}
	if state.IsDraft || !state.IsImmutable {
		return fmt.Errorf("v%s is not a published immutable release (draft=%t immutable=%t), so it gets no version tag", manifest.Version, state.IsDraft, state.IsImmutable)
	}
	return nil
}

func (p program) withRetries(ctx context.Context, name string, args ...string) ([]byte, error) {
	delay := firstReadDelay
	for attempt := 1; ; attempt++ {
		output, err := p.run(ctx, name, args...)
		if err == nil {
			return output, nil
		}
		if attempt == readAttempts || ctx.Err() != nil {
			return nil, err
		}
		p.wait(delay)
		delay = min(delay*2, lastReadDelay)
	}
}

func (p program) writeOutputs(outputs map[string]string) error {
	path := p.getenv("GITHUB_OUTPUT")
	if path == "" {
		return nil
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600) //nolint:gosec // The runner names this file.
	if err != nil {
		return fmt.Errorf("open the step outputs: %w", err)
	}
	defer file.Close()
	for _, key := range []string{"repository", "digest", "reference"} {
		if _, err := fmt.Fprintf(file, "%s=%s\n", key, outputs[key]); err != nil {
			return fmt.Errorf("write the step outputs: %w", err)
		}
	}
	return nil
}

// load builds the stamped chart, and refuses one that does not stamp to the digest
// the manifest records, if it records one.
func load(opts options) (chartstamp.Manifest, chartstamp.Artifact, error) {
	manifest, err := readManifest(opts)
	if err != nil {
		return chartstamp.Manifest{}, chartstamp.Artifact{}, err
	}
	asset, err := os.ReadFile(opts.chart)
	if err != nil {
		return chartstamp.Manifest{}, chartstamp.Artifact{}, fmt.Errorf("read the packaged chart: %w", err)
	}
	artifact, err := chartstamp.Build(asset, manifest.Release)
	if err != nil {
		return chartstamp.Manifest{}, chartstamp.Artifact{}, err
	}
	if manifest.Chart != "" && manifest.Chart != artifact.Reference() {
		return chartstamp.Manifest{}, chartstamp.Artifact{}, fmt.Errorf("the chart stamps to %s, and the release manifest records %s", artifact.Reference(), manifest.Chart)
	}
	return manifest, artifact, nil
}

func recorded(opts options) (chartstamp.Manifest, string, error) {
	manifest, err := readManifest(opts)
	if err != nil {
		return chartstamp.Manifest{}, "", err
	}
	digest, err := manifest.ChartDigest()
	return manifest, digest, err
}

func readManifest(opts options) (chartstamp.Manifest, error) {
	document, err := os.ReadFile(opts.manifest)
	if err != nil {
		return chartstamp.Manifest{}, fmt.Errorf("read the release manifest: %w", err)
	}
	return chartstamp.ParseManifest(document)
}

// reader reaches the registry with no credential at all, whatever this process
// holds, so a chart it can read is one the public can read.
func reader(opts options) (*remote.Repository, error) {
	return repository(opts, nil)
}

func (p program) writer(opts options) (*remote.Repository, error) {
	token := p.getenv("GH_TOKEN")
	if token == "" {
		return repository(opts, nil)
	}
	actor := p.getenv("GITHUB_ACTOR")
	if actor == "" {
		return nil, errors.New("GH_TOKEN is set and GITHUB_ACTOR is not, so there is no registry user")
	}
	return repository(opts, &auth.Credential{Username: actor, Password: token})
}

// registryClient has a transport of its own. retry.DefaultClient rides on
// http.DefaultTransport, and anything else in the process can close that
// transport's idle connections under a request about to use one. Closing an
// httptest.Server does exactly that, which failed a request in a parallel test
// with "http: CloseIdleConnections called".
var registryClient = &http.Client{Transport: retry.NewTransport(http.DefaultTransport.(*http.Transport).Clone())}

func repository(opts options, credential *auth.Credential) (*remote.Repository, error) {
	target, err := remote.NewRepository(opts.repository)
	if err != nil {
		return nil, fmt.Errorf("repository %q: %w", opts.repository, err)
	}
	target.PlainHTTP = opts.plainHTTP
	client := &auth.Client{Client: registryClient, Cache: auth.NewCache()}
	if credential != nil {
		client.Credential = auth.StaticCredential(target.Reference.Registry, *credential)
	}
	target.Client = client
	return target, nil
}
