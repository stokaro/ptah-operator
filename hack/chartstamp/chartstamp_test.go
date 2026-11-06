package chartstamp_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/stokaro/ptah-operator/hack/chartstamp"
	"github.com/stokaro/ptah-operator/hack/chartstamp/chartstamptest"
)

const (
	managerDigest  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	executorDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	otherDigest    = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	managerImage   = "ghcr.io/stokaro/ptah-operator@" + managerDigest
	executorImage  = "ghcr.io/stokaro/ptah@" + executorDigest
	stampedVersion = "v0.18.0"
)

func release() chartstamp.Release {
	return chartstamp.Release{
		Image:       managerImage,
		Executor:    executorImage,
		PtahVersion: stampedVersion,
		Revision:    strings.Repeat("1", 40),
	}
}

// The chart this repository would release, from the packager the release runs.
var packaged = sync.OnceValues(func() ([]byte, error) {
	directory, err := os.MkdirTemp("", "chartstamp-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	command := exec.Command("go", "run", "../chartpackage",
		"-chart", "../../charts/ptah-operator", "-epoch", "1700000000", "-destination", directory)
	if output, err := command.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("package the chart: %w\n%s", err, output)
	}
	paths, err := filepath.Glob(filepath.Join(directory, "*.tgz"))
	if err != nil || len(paths) != 1 {
		return nil, fmt.Errorf("the packager wrote %v (%w)", paths, err)
	}
	return os.ReadFile(paths[0])
})

func asset(t *testing.T) []byte {
	t.Helper()
	body, err := packaged()
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func build(t *testing.T) chartstamp.Artifact {
	t.Helper()
	artifact, err := chartstamp.Build(asset(t), release())
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

type entry struct {
	header tar.Header
	body   []byte
}

func unpack(t *testing.T, chart []byte) (gzip.Header, []entry) {
	t.Helper()
	compressed, err := gzip.NewReader(bytes.NewReader(chart))
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(compressed)
	var entries []entry
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return compressed.Header, entries
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry{*header, body})
	}
}

// Anyone holding the asset and the manifest can compute the published chart, and
// the release relies on that: it records the digest before the push, and each
// later run computes it again and refuses a difference.
func TestBuildIsAFunctionOfTheAssetAndTheRelease(t *testing.T) {
	t.Parallel()
	first, second := build(t), build(t)
	if !bytes.Equal(first.Chart, second.Chart) || !bytes.Equal(first.Manifest, second.Manifest) || first.Digest != second.Digest {
		t.Fatalf("two builds differ: %s and %s", first.Digest, second.Digest)
	}
	for name, change := range map[string]func(*chartstamp.Release){
		"the image":    func(r *chartstamp.Release) { r.Image = "ghcr.io/stokaro/ptah-operator@" + otherDigest },
		"the executor": func(r *chartstamp.Release) { r.Executor = "ghcr.io/stokaro/ptah@" + otherDigest },
		"the version":  func(r *chartstamp.Release) { r.PtahVersion = "v0.19.0" },
		"the revision": func(r *chartstamp.Release) { r.Revision = strings.Repeat("2", 40) },
	} {
		changed := release()
		change(&changed)
		other, err := chartstamp.Build(asset(t), changed)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if other.Digest == first.Digest {
			t.Errorf("changing %s kept the digest %s, so the manifest would not bind it", name, first.Digest)
		}
	}
}

// The matrix installed the asset, so the stamp may not reach anything it ran:
// every entry but Chart.yaml keeps its bytes and its header, and Chart.yaml gains
// three lines and loses none.
func TestBuildChangesNothingButChartYaml(t *testing.T) {
	t.Parallel()
	artifact := build(t)
	sourceGzip, source := unpack(t, asset(t))
	stampedGzip, stamped := unpack(t, artifact.Chart)
	if sourceGzip.ModTime != stampedGzip.ModTime || sourceGzip.OS != stampedGzip.OS {
		t.Errorf("the gzip header moved: %+v to %+v", sourceGzip, stampedGzip)
	}
	if len(source) == 0 || len(source) != len(stamped) {
		t.Fatalf("the package holds %d entries, and the stamped one %d", len(source), len(stamped))
	}
	compared := 0
	for index := range source {
		from, to := source[index], stamped[index]
		if from.header.Name == "ptah-operator/Chart.yaml" {
			to.header.Size = from.header.Size
			want := strings.Replace(string(from.body), "\nannotations:\n", fmt.Sprintf("\nannotations:\n  %s: %q\n  %s: %q\n  %s: %q\n",
				chartstamp.AnnotationImage, managerImage,
				chartstamp.AnnotationExecutor, executorImage,
				chartstamp.AnnotationPtahVersion, stampedVersion), 1)
			if string(to.body) != want {
				t.Errorf("Chart.yaml reads:\n%s\nwant:\n%s", to.body, want)
			}
		} else if !bytes.Equal(from.body, to.body) {
			t.Errorf("%s changed", from.header.Name)
		}
		if fmt.Sprint(from.header) != fmt.Sprint(to.header) {
			t.Errorf("the header of %s moved:\n%+v\n%+v", from.header.Name, from.header, to.header)
		}
		compared++
	}
	if compared < 10 {
		t.Fatalf("compared %d entries, and the chart has more than that", compared)
	}
}

func TestBuildRefusesWhatItCannotStamp(t *testing.T) {
	t.Parallel()
	sound := build(t)
	for _, row := range []struct {
		name    string
		asset   []byte
		release func(*chartstamp.Release)
		want    string
	}{
		{name: "an image named by a tag", release: func(r *chartstamp.Release) { r.Image = "ghcr.io/stokaro/ptah-operator:0.2.10" }, want: "not pinned"},
		{name: "no version", release: func(r *chartstamp.Release) { r.PtahVersion = "" }, want: "ptah version"},
		{name: "an abbreviated revision", release: func(r *chartstamp.Release) { r.Revision = "abc123" }, want: "revision"},
		{name: "a chart stamped already", asset: sound.Chart, want: "stamped before"},
		{name: "something that is not a package", asset: []byte("deterministic chart bytes"), want: "read the chart package"},
		{name: "a package with no Chart.yaml", asset: repack(t, asset(t), func(entries []entry) []entry {
			return slices.DeleteFunc(entries, func(e entry) bool { return e.header.Name == "ptah-operator/Chart.yaml" })
		}), want: "no Chart.yaml"},
		{name: "a Chart.yaml with no annotations key to add to", asset: repack(t, asset(t), func(entries []entry) []entry {
			for index, e := range entries {
				if e.header.Name == "ptah-operator/Chart.yaml" {
					entries[index].body = bytes.Replace(e.body, []byte("\nannotations:\n  artifacthub.io/license: Apache-2.0\n"), []byte("\n"), 1)
					entries[index].header.Size = int64(len(entries[index].body))
				}
			}
			return entries
		}), want: "annotations keys"},
		{name: "a package rooted at another name", asset: repack(t, asset(t), func(entries []entry) []entry {
			for index, e := range entries {
				entries[index].header.Name = "other/" + strings.TrimPrefix(e.header.Name, "ptah-operator/")
			}
			return entries
		}), want: "rooted at"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			input, identities := asset(t), release()
			if row.asset != nil {
				input = row.asset
			}
			if row.release != nil {
				row.release(&identities)
			}
			_, err := chartstamp.Build(input, identities)
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("Build() = %v, want a refusal naming %q", err, row.want)
			}
		})
	}
}

// repack writes a package back with its entries changed, the way a tampered or
// hand-made one would be.
func repack(t *testing.T, chart []byte, change func([]entry) []entry) []byte {
	t.Helper()
	_, entries := unpack(t, chart)
	var output bytes.Buffer
	compressed := gzip.NewWriter(&output)
	archive := tar.NewWriter(compressed)
	for _, item := range change(entries) {
		header := item.header
		if err := archive.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(item.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

// The source tree's own Chart.yaml never carries the stamp; releaseverify refuses
// that. A chart without one has to keep behaving as it did, and these are the
// values the install page gives a person who selects their own images.
func TestAnUnstampedChartStillRefusesToRenderWithoutItsValues(t *testing.T) {
	t.Parallel()
	helm := helmBinary(t)
	chart := writeChart(t, asset(t))
	all := []string{
		"image.digest=" + managerDigest,
		"execution.runnerImage=ghcr.io/stokaro/ptah-operator@" + managerDigest,
		"execution.executorImage=" + executorImage,
		"execution.ptahVersion=" + stampedVersion,
	}
	if output, err := render(helm, chart, all...); err != nil {
		t.Fatalf("the chart refused every value it asks for: %v\n%s", err, output)
	}
	if _, err := render(helm, chart); err == nil {
		t.Error("the chart rendered with no values")
	}
	for omitted := range all {
		settings := slices.Delete(slices.Clone(all), omitted, omitted+1)
		for _, skip := range []bool{false, true} {
			arguments := settings
			if skip {
				arguments = append(slices.Clone(settings), "--skip-schema-validation")
			}
			if _, err := render(helm, chart, arguments...); err == nil {
				t.Errorf("the chart rendered without %s (skipping the schema: %t)", all[omitted], skip)
			}
		}
	}
}

// What a person gets from the published chart with no values is what they would
// get from the source chart by copying the four values out of the manifest, down
// to the byte. The bootstrap certificate is random, so both name a Secret instead.
func TestAStampedChartRendersWhatTheExplicitValuesRender(t *testing.T) {
	t.Parallel()
	helm := helmBinary(t)
	deterministic := []string{
		"webhook.existingSecret=webhook", "webhook.caBundle=ca",
	}
	stamped, err := render(helm, writeChart(t, build(t).Chart), append(deterministic, "--set=resultDelivery.enabled=false")...)
	if err != nil {
		t.Fatalf("the stamped chart refused no values: %v\n%s", err, stamped)
	}
	explicit, err := render(helm, writeChart(t, asset(t)), append(deterministic, "--set=resultDelivery.enabled=false",
		"image.digest="+managerDigest,
		"execution.runnerImage="+managerImage,
		"execution.executorImage="+executorImage,
		"execution.ptahVersion="+stampedVersion)...)
	if err != nil {
		t.Fatalf("the unstamped chart refused its values: %v\n%s", err, explicit)
	}
	if stamped != explicit {
		t.Fatal("the stamped chart with no values rendered something else than the unstamped chart with the same values")
	}
	assertSelection(t, stamped, selection{managerImage, managerImage, executorImage, stampedVersion})
}

type selection struct{ manager, runner, executor, ptahVersion string }

var (
	renderedImage = regexp.MustCompile(`(?m)^\s+image: (\S+)$`)
	renderedArg   = regexp.MustCompile(`"--(controller-image|manager-image|runner-image|executor-image|ptah-version)=([^"]+)"`)
)

// assertSelection reads every image a workload runs and every argument that
// names one. Each has to be there at least once, or this would pass on a render
// that dropped it.
func assertSelection(t *testing.T, render string, want selection) {
	t.Helper()
	images := renderedImage.FindAllStringSubmatch(render, -1)
	if len(images) == 0 {
		t.Fatal("the render names no image")
	}
	for _, image := range images {
		if image[1] != want.manager {
			t.Errorf("a workload runs %s, want %s", image[1], want.manager)
		}
	}
	wantArgs := map[string]string{
		"controller-image": want.manager,
		"manager-image":    want.manager,
		"runner-image":     want.runner,
		"executor-image":   want.executor,
		"ptah-version":     want.ptahVersion,
	}
	seen := map[string]bool{}
	for _, argument := range renderedArg.FindAllStringSubmatch(render, -1) {
		seen[argument[1]] = true
		if argument[2] != wantArgs[argument[1]] {
			t.Errorf("--%s is %s, want %s", argument[1], argument[2], wantArgs[argument[1]])
		}
	}
	for name := range wantArgs {
		if !seen[name] {
			t.Errorf("the render passes no --%s", name)
		}
	}
}

// Helm merges values before a template sees them, so the template tells a stamped
// default from an override only because the default is not a value. These are the
// halves a person could set, and each must be refused rather than paired with
// what the stamp left in place.
func TestAStampedChartTakesEachSelectionWholeOrNotAtAll(t *testing.T) {
	t.Parallel()
	helm := helmBinary(t)
	chart := writeChart(t, build(t).Chart)
	otherExecutor := "registry.example/ptah@" + otherDigest
	otherRunner := "ghcr.io/stokaro/ptah-operator@" + otherDigest
	mirroredRunner := "registry.example/ptah-operator@" + managerDigest
	for _, row := range []struct {
		name     string
		settings []string
		refused  string
		want     selection
	}{
		{name: "no values", want: selection{managerImage, managerImage, executorImage, stampedVersion}},
		{name: "empty values are no values", settings: []string{"image.digest=", "execution.runnerImage=", "execution.executorImage=", "execution.ptahVersion="},
			want: selection{managerImage, managerImage, executorImage, stampedVersion}},

		{name: "an executor without its version", settings: []string{"execution.executorImage=" + otherExecutor},
			refused: "set execution.executorImage and execution.ptahVersion together"},
		{name: "a version without its executor", settings: []string{"execution.ptahVersion=v0.19.0"},
			refused: "set execution.executorImage and execution.ptahVersion together"},
		{name: "an executor and its version", settings: []string{"execution.executorImage=" + otherExecutor, "execution.ptahVersion=v0.19.0"},
			want: selection{managerImage, managerImage, otherExecutor, "v0.19.0"}},
		{name: "another digest under the stamped version, said out loud", settings: []string{"execution.executorImage=" + otherExecutor, "execution.ptahVersion=" + stampedVersion},
			want: selection{managerImage, managerImage, otherExecutor, stampedVersion}},

		{name: "a manager without its runner", settings: []string{"image.digest=" + otherDigest},
			refused: "set image.digest and execution.runnerImage together"},
		{name: "a runner without its manager", settings: []string{"execution.runnerImage=" + otherRunner},
			refused: "set image.digest and execution.runnerImage together"},
		{name: "a manager and its runner", settings: []string{"image.digest=" + otherDigest, "execution.runnerImage=" + otherRunner},
			want: selection{"ghcr.io/stokaro/ptah-operator@" + otherDigest, otherRunner, executorImage, stampedVersion}},

		// A mirror serves the same digest, and the runner has to come from it too.
		{name: "a moved repository alone", settings: []string{"image.repository=registry.example/ptah-operator"},
			refused: "set image.digest and execution.runnerImage together, with image.repository if it moved"},
		{name: "a moved repository with both images", settings: []string{"image.repository=registry.example/ptah-operator", "image.digest=" + managerDigest, "execution.runnerImage=" + mirroredRunner},
			want: selection{mirroredRunner, mirroredRunner, executorImage, stampedVersion}},

		// The refusals a source-tree chart gives still hold on what was set.
		{name: "an executor that is not pinned", settings: []string{"execution.executorImage=registry.example/ptah:v1", "execution.ptahVersion=v1", "--skip-schema-validation"},
			refused: "execution.executorImage must be an image pinned"},
		{name: "a manager that is not pinned", settings: []string{"image.digest=latest", "execution.runnerImage=" + otherRunner, "--skip-schema-validation"},
			refused: "image.digest must pin the manager"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			output, err := render(helm, chart, row.settings...)
			if row.refused != "" {
				if err == nil || !strings.Contains(output, row.refused) {
					t.Fatalf("render err=%v, want a refusal naming %q:\n%s", err, row.refused, tail(output))
				}
				return
			}
			if err != nil {
				t.Fatalf("render: %v\n%s", err, tail(output))
			}
			assertSelection(t, output, row.want)
		})
	}
}

func TestAnIncompleteStampIsRefused(t *testing.T) {
	t.Parallel()
	helm := helmBinary(t)
	stamped := build(t).Chart
	for _, row := range []struct{ name, line string }{
		{"no image", chartstamp.AnnotationImage},
		{"no executor", chartstamp.AnnotationExecutor},
		{"no version", chartstamp.AnnotationPtahVersion},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			chart := writeChart(t, stripAnnotation(t, stamped, row.line))
			output, err := render(helm, chart)
			if err == nil || !strings.Contains(output, "incomplete release stamp") {
				t.Fatalf("render err=%v, want the incomplete stamp refused:\n%s", err, tail(output))
			}
		})
	}
}

func stripAnnotation(t *testing.T, chart []byte, annotation string) []byte {
	t.Helper()
	return repack(t, chart, func(entries []entry) []entry {
		for index, item := range entries {
			if item.header.Name != "ptah-operator/Chart.yaml" {
				continue
			}
			kept := regexp.MustCompile(`(?m)^  `+regexp.QuoteMeta(annotation)+`: .*\n`).ReplaceAll(item.body, nil)
			if len(kept) == len(item.body) {
				t.Fatalf("Chart.yaml has no %s to strip", annotation)
			}
			entries[index].body = kept
			entries[index].header.Size = int64(len(kept))
		}
		return entries
	})
}

// The artifact has to be one Helm reads, so Helm reads it: staged by digest, tagged,
// pulled by digest and by version, and rendered straight from the registry.
func TestHelmPullsWhatStageAndTagWrite(t *testing.T) {
	t.Parallel()
	helm := helmBinary(t)
	ctx := context.Background()
	registry := chartstamptest.NewRegistry(t)
	repository := repositoryOn(t, registry)
	artifact := build(t)
	reference := "oci://" + registry.Host() + "/charts/ptah-operator"

	for attempt := range 2 { // A rerun of the transaction resumes it.
		if err := chartstamp.Stage(ctx, repository, artifact); err != nil {
			t.Fatalf("staging, attempt %d: %v", attempt+1, err)
		}
	}
	if tags := registry.Tags("charts/ptah-operator"); len(tags) != 0 {
		t.Fatalf("staging wrote the tags %v, and a staged chart has no name to install by", tags)
	}
	if err := chartstamp.ReadBack(ctx, repository, artifact.Digest, artifact.Digest); err != nil {
		t.Fatalf("read back by digest: %v", err)
	}
	if output, err := helmRun(helm, "show", "chart", reference, "--version", artifact.Version, "--plain-http"); err == nil {
		t.Fatalf("helm read a version before it was tagged:\n%s", output)
	}
	if output, err := helmRun(helm, "show", "chart", reference+"@"+artifact.Digest, "--plain-http"); err != nil {
		t.Fatalf("helm read the staged chart by digest: %v\n%s", err, output)
	}

	if err := chartstamp.Tag(ctx, repository, artifact.Digest, artifact.Version); err != nil {
		t.Fatal(err)
	}
	if err := chartstamp.ReadBack(ctx, repository, artifact.Version, artifact.Digest); err != nil {
		t.Fatalf("read back by version: %v", err)
	}
	directory := t.TempDir()
	output, err := helmRun(helm, "pull", reference, "--version", artifact.Version, "--plain-http", "--destination", directory)
	if err != nil {
		t.Fatalf("helm pull: %v\n%s", err, output)
	}
	if !strings.Contains(output, "Digest: "+artifact.Digest) {
		t.Errorf("helm pull reported:\n%s\nwant digest %s", output, artifact.Digest)
	}
	pulled, err := os.ReadFile(filepath.Join(directory, artifact.Name+"-"+artifact.Version+".tgz"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pulled, artifact.Chart) {
		t.Error("helm pulled other bytes than the artifact holds")
	}
	rendered, err := helmRun(helm, "template", "ptah-operator", reference, "--version", artifact.Version,
		"--plain-http", "--namespace", "ptah-system")
	if err != nil {
		t.Fatalf("helm template from the registry: %v\n%s", err, tail(rendered))
	}
	assertSelection(t, rendered, selection{managerImage, managerImage, executorImage, stampedVersion})
}

func TestTagNeverMovesAVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	registry := chartstamptest.NewRegistry(t)
	repository := repositoryOn(t, registry)
	first := build(t)
	changed := release()
	changed.Executor = "ghcr.io/stokaro/ptah@" + otherDigest
	second, err := chartstamp.Build(asset(t), changed)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []chartstamp.Artifact{first, second} {
		if err := chartstamp.Stage(ctx, repository, artifact); err != nil {
			t.Fatal(err)
		}
	}
	// The same digest twice is the done state, which is what a rerun finds.
	for range 2 {
		if err := chartstamp.Tag(ctx, repository, first.Digest, first.Version); err != nil {
			t.Fatal(err)
		}
	}
	if err := chartstamp.Tag(ctx, repository, second.Digest, first.Version); err == nil || !strings.Contains(err.Error(), "refusing to move it") {
		t.Fatalf("Tag() over another digest = %v, want it refused", err)
	}
	if err := chartstamp.ReadBack(ctx, repository, first.Version, first.Digest); err != nil {
		t.Fatalf("after the refusal: %v", err)
	}
	if err := chartstamp.ReadBack(ctx, repository, first.Version, second.Digest); err == nil {
		t.Fatal("ReadBack() accepted a version tag that holds another digest")
	}
}

// A registry that stores something else, or serves it, is a failure rather than a
// reported success.
func TestStageAndReadBackRefuseOtherBytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	registry := chartstamptest.NewRegistry(t)
	repository := repositoryOn(t, registry)
	artifact := build(t)
	if err := chartstamp.Stage(ctx, substituting{repository}, artifact); err == nil || !strings.Contains(err.Error(), "the registry resolves") {
		t.Fatalf("Stage() = %v, want the substitution refused", err)
	}
	if err := chartstamp.ReadBack(ctx, substituting{repository}, artifact.Digest, artifact.Digest); err == nil {
		t.Fatal("ReadBack() accepted another digest")
	}
	if err := chartstamp.ReadBack(ctx, repository, "0.0.0", artifact.Digest); err == nil {
		t.Fatal("ReadBack() accepted a tag the registry does not have")
	}
}

type substituting struct{ oras.Target }

func (s substituting) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	stored, err := s.Target.Resolve(ctx, reference)
	if err != nil {
		return stored, err
	}
	stored.Digest = content.NewDescriptorFromBytes(stored.MediaType, []byte("other bytes")).Digest
	return stored, nil
}

// The manifest a release writes is the one the path after it reads.
func TestRecordInManifestAddsTheReferenceOnce(t *testing.T) {
	t.Parallel()
	identity := release()
	document := []byte("version=0.2.10\nsource-repository=stokaro/ptah-operator\nsource-ref=refs/tags/v0.2.10\n" +
		"source-sha=" + identity.Revision + "\nimage=" + identity.Image + "\nexecutor=" + identity.Executor + "\n" +
		"executor-ptah-version=" + identity.PtahVersion + "\nchart-asset-sha256=abc\nclient-assets=x\n")
	updated, artifact, err := chartstamp.RecordInManifest(document, asset(t))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(string(document), "chart-asset-sha256=abc\n", "chart-asset-sha256=abc\nchart-oci="+artifact.Reference()+"\n", 1)
	if string(updated) != want {
		t.Fatalf("the manifest reads:\n%s\nwant:\n%s", updated, want)
	}
	manifest, err := chartstamp.ParseManifest(updated)
	if err != nil {
		t.Fatal(err)
	}
	if digest, err := manifest.ChartDigest(); err != nil || digest != artifact.Digest {
		t.Fatalf("ChartDigest() = %q, %v, want %s", digest, err, artifact.Digest)
	}
	if manifest.Release != identity {
		t.Fatalf("the manifest reads as %+v", manifest.Release)
	}
	for name, row := range map[string]struct {
		document string
		want     string
	}{
		"recorded twice":      {string(updated), "already records"},
		"no record to follow": {strings.Replace(string(document), "chart-asset-sha256=abc\n", "", 1), "to follow"},
		"no image":            {strings.Replace(string(document), "image=", "imagine=", 1), "no image record"},
		"a mutable image":     {strings.Replace(string(document), identity.Image, "ghcr.io/stokaro/ptah-operator:latest", 1), "not pinned"},
	} {
		if _, _, err := chartstamp.RecordInManifest([]byte(row.document), asset(t)); err == nil || !strings.Contains(err.Error(), row.want) {
			t.Errorf("%s: RecordInManifest() = %v, want a refusal naming %q", name, err, row.want)
		}
	}
	for _, chart := range []string{"", chartstamp.Repository + ":0.2.10", "ghcr.io/stokaro/other@" + artifact.Digest, chartstamp.Repository + "@sha256:short"} {
		if _, err := (chartstamp.Manifest{Chart: chart}).ChartDigest(); err == nil {
			t.Errorf("ChartDigest() accepted chart-oci %q", chart)
		}
	}
}

func repositoryOn(t *testing.T, registry *chartstamptest.Registry) *remote.Repository {
	t.Helper()
	repository, err := remote.NewRepository(registry.Host() + "/charts/ptah-operator")
	if err != nil {
		t.Fatal(err)
	}
	repository.PlainHTTP = true
	return repository
}

func writeChart(t *testing.T, chart []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ptah-operator.tgz")
	if err := os.WriteFile(path, chart, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func helmBinary(t *testing.T) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("Helm is required in CI: these rows are what prove the chart's selection rules")
		}
		t.Skip("Helm is required to render the chart")
	}
	return helm
}

func render(helm, chart string, settings ...string) (string, error) {
	arguments := []string{"template", "ptah-operator", chart, "--namespace", "ptah-system"}
	for _, setting := range settings {
		if strings.HasPrefix(setting, "--") {
			arguments = append(arguments, setting)
		} else {
			arguments = append(arguments, "--set-string", setting)
		}
	}
	return helmRun(helm, arguments...)
}

func helmRun(helm string, arguments ...string) (string, error) {
	command := exec.Command(helm, arguments...)
	command.Env = append(os.Environ(), "HELM_REGISTRY_CONFIG="+filepath.Join(os.TempDir(), "chartstamp-no-registry-config.json"))
	output, err := command.CombinedOutput()
	return string(output), err
}

func tail(output string) string {
	if len(output) > 2000 {
		return output[len(output)-2000:]
	}
	return output
}
