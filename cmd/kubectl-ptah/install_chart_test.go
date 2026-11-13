package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/stokaro/ptah-operator/hack/chartstamp"
	"github.com/stokaro/ptah-operator/hack/chartstamp/chartstamptest"
)

// The rows above stand in for helm. These hand the published chart, as a
// release would stamp and push it, to the helm on PATH through the plugin's own
// runner, from a registry the test serves. They show what reaches the chart:
// the stamped images with no value given, the approvers as a binding, and the
// chart's own refusal of an executor without its version, which a values file
// handed to install cannot get past.

const (
	stampedManager  = "ghcr.io/stokaro/ptah-operator@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	stampedExecutor = "ghcr.io/stokaro/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	stampedPtah     = "v0.23.0"
	otherExecutor   = "registry.example/ptah@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	// approverName is the approver ClusterRole and its binding, for the
	// release name install uses.
	approverName = "ptah-operator-ptah-operator-approver"
)

// packagedChart is the chart in this tree, packaged the way a release packages it.
var packagedChart = sync.OnceValues(func() ([]byte, error) {
	directory, err := os.MkdirTemp("", "kubectl-ptah-install-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	command := exec.Command("go", "run", "../../hack/chartpackage",
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

// helmForTest is the helm on PATH, in an environment of the test's own, so no
// registry login or repository a developer configured reaches it.
func helmForTest(t *testing.T) helmRunner {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("Helm is required in CI: these rows are what prove install reaches the published chart")
		}
		t.Skip("Helm is required to render the published chart")
	}
	home := t.TempDir()
	return helmOnPath(append(os.Environ(),
		"HELM_REGISTRY_CONFIG="+filepath.Join(home, "registry.json"),
		"HELM_REPOSITORY_CONFIG="+filepath.Join(home, "repositories.yaml"),
		"HELM_CACHE_HOME="+filepath.Join(home, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(home, "config"),
		"HELM_DATA_HOME="+filepath.Join(home, "data"),
	))
}

// publish stamps the chart and pushes it to registry under its version, as a
// published release leaves it.
func publish(t *testing.T, registry *chartstamptest.Registry) chartstamp.Artifact {
	t.Helper()
	asset, err := packagedChart()
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := chartstamp.Build(asset, chartstamp.Release{
		Image: stampedManager, Executor: stampedExecutor, PtahVersion: stampedPtah, Revision: strings.Repeat("1", 40),
	})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := remote.NewRepository(registry.Host() + "/charts/ptah-operator")
	if err != nil {
		t.Fatal(err)
	}
	repository.PlainHTTP = true
	ctx := context.Background()
	if err := chartstamp.Stage(ctx, repository, artifact); err != nil {
		t.Fatal(err)
	}
	if err := chartstamp.Tag(ctx, repository, artifact.Digest, artifact.Version); err != nil {
		t.Fatal(err)
	}
	return artifact
}

func TestInstallRendersThePublishedChart(t *testing.T) {
	t.Parallel()
	helm := helmForTest(t)
	registry := chartstamptest.NewRegistry(t)
	artifact := publish(t, registry)
	install := installer{
		repository: registry.Host() + "/charts/ptah-operator", plainHTTP: true,
		pluginVersion: "v" + artifact.Version, helm: helm,
	}
	render := func(t *testing.T, arguments ...string) (string, string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		status := install.run(context.Background(), append([]string{"--yaml"}, arguments...), &stdout, &stderr)
		return stdout.String(), stderr.String(), status
	}

	t.Run("with no value, the images the release stamped", func(t *testing.T) {
		t.Parallel()
		output, stderr, status := render(t)
		if status != exitOK {
			t.Fatalf("install --yaml = %d:\n%s", status, tail(stderr))
		}
		requireSelection(t, output, stampedManager, stampedExecutor, stampedPtah)
		objects := decodeObjects(t, output)
		if bindings := renderedNamed(objects, "ClusterRoleBinding", approverName); len(bindings) != 0 {
			t.Fatalf("with no --approver the render binds the approver role: %v", bindings[0].Object)
		}
		if roles := renderedNamed(objects, "ClusterRole", approverName); len(roles) != 1 {
			t.Fatalf("the render holds %d approver ClusterRoles, want one", len(roles))
		}
	})

	t.Run("each approver named in one binding to the approver role", func(t *testing.T) {
		t.Parallel()
		approvers := []string{"alice@example.com", `o'brien, "ops"`, "system:serviceaccount:ci:deployer"}
		arguments := []string{}
		for _, approver := range approvers {
			arguments = append(arguments, "--approver", approver)
		}
		output, stderr, status := render(t, arguments...)
		if status != exitOK {
			t.Fatalf("install --yaml %v = %d:\n%s", arguments, status, tail(stderr))
		}
		objects := decodeObjects(t, output)
		roles := renderedNamed(objects, "ClusterRole", approverName)
		bindings := renderedNamed(objects, "ClusterRoleBinding", approverName)
		if len(roles) != 1 || len(bindings) != 1 {
			t.Fatalf("the render holds %d approver roles and %d bindings, want one of each", len(roles), len(bindings))
		}
		binding := bindings[0].Object
		if ref := binding["roleRef"]; fmt.Sprint(ref) != fmt.Sprint(map[string]any{
			"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": approverName,
		}) {
			t.Fatalf("the binding refers to %v", ref)
		}
		var got []string
		subjects, _ := binding["subjects"].([]any)
		for _, subject := range subjects {
			fields, _ := subject.(map[string]any)
			if fields["kind"] != "User" || fields["apiGroup"] != "rbac.authorization.k8s.io" || fields["namespace"] != nil {
				t.Errorf("the binding names %v, want a user", fields)
			}
			got = append(got, fmt.Sprint(fields["name"]))
		}
		if !slices.Equal(got, approvers) {
			t.Fatalf("the binding names %q, want %q", got, approvers)
		}
	})

	t.Run("an executor without its version is refused through install", func(t *testing.T) {
		t.Parallel()
		values := writeValues(t, "execution:\n  executorImage: "+otherExecutor+"\n")
		output, stderr, status := render(t, "--values", values)
		if status != exitFailed || !strings.Contains(stderr, "set execution.executorImage and execution.ptahVersion together") {
			t.Fatalf("install --yaml --values (executor only) = %d, printed %d bytes:\n%s", status, len(output), tail(stderr))
		}
	})

	// The refusal above means something only if the values file reaches the
	// chart: with the version beside it, the executor is the one that runs.
	t.Run("an executor with its version is taken through install", func(t *testing.T) {
		t.Parallel()
		values := writeValues(t, "execution:\n  executorImage: "+otherExecutor+"\n  ptahVersion: v0.24.0\n")
		output, stderr, status := render(t, "--values", values)
		if status != exitOK {
			t.Fatalf("install --yaml --values (executor and version) = %d:\n%s", status, tail(stderr))
		}
		requireSelection(t, output, stampedManager, otherExecutor, "v0.24.0")
	})

	t.Run("the out-dir holds the objects standard output does", func(t *testing.T) {
		t.Parallel()
		directory := t.TempDir()
		printed, stderr, status := render(t, "--approver", "alice@example.com")
		if status != exitOK {
			t.Fatalf("install --yaml = %d:\n%s", status, tail(stderr))
		}
		if _, stderr, status := render(t, "--approver", "alice@example.com", "--out-dir", directory); status != exitOK {
			t.Fatalf("install --yaml --out-dir = %d:\n%s", status, tail(stderr))
		}
		var written strings.Builder
		files := 0
		err := filepath.WalkDir(filepath.Join(directory, releaseChartName), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			files++
			content, err := os.ReadFile(path) //nolint:gosec // A file the test's own render wrote.
			written.WriteString("\n---\n")
			written.Write(content)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		want, got := identities(decodeObjects(t, printed)), identities(decodeObjects(t, written.String()))
		if files == 0 || len(want) < 50 || !slices.Equal(got, want) {
			t.Fatalf("%d files under %s hold\n%v\nand standard output\n%v", files, directory, got, want)
		}
	})

	t.Run("a version the registry does not hold", func(t *testing.T) {
		t.Parallel()
		output, stderr, status := render(t, "--version", "0.99.0")
		if status != exitFailed || output != "" || !strings.Contains(stderr, "0.99.0") {
			t.Fatalf("install --yaml --version 0.99.0 = %d, printed %q:\n%s", status, output, tail(stderr))
		}
	})
}

func writeValues(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var (
	renderedImage = regexp.MustCompile(`(?m)^\s+image: (\S+)$`)
	renderedArg   = regexp.MustCompile(`"--(controller-image|manager-image|runner-image|executor-image|ptah-version)=([^"]+)"`)
)

// requireSelection reads every image a workload runs and every argument that
// names one, and requires each to be there at least once, so a render that
// dropped one fails rather than passes.
func requireSelection(t *testing.T, render, manager, executor, ptahVersion string) {
	t.Helper()
	images := renderedImage.FindAllStringSubmatch(render, -1)
	if len(images) == 0 {
		t.Fatal("the render names no image")
	}
	for _, image := range images {
		if image[1] != manager {
			t.Errorf("a workload runs %s, want %s", image[1], manager)
		}
	}
	want := map[string]string{
		"controller-image": manager, "manager-image": manager, "runner-image": manager,
		"executor-image": executor, "ptah-version": ptahVersion,
	}
	seen := map[string]bool{}
	for _, argument := range renderedArg.FindAllStringSubmatch(render, -1) {
		seen[argument[1]] = true
		if argument[2] != want[argument[1]] {
			t.Errorf("--%s is %s, want %s", argument[1], argument[2], want[argument[1]])
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("the render passes no --%s", name)
		}
	}
}

func decodeObjects(t *testing.T, stream string) []*unstructured.Unstructured {
	t.Helper()
	reader := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(stream)))
	var objects []*unstructured.Unstructured
	for {
		document, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return objects
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(bytes.TrimSpace(document)) == 0 {
			continue
		}
		object := &unstructured.Unstructured{}
		if err := utilyaml.Unmarshal(document, &object.Object); err != nil {
			t.Fatalf("the render holds a document that does not parse: %v\n%s", err, document)
		}
		if len(object.Object) > 0 {
			objects = append(objects, object)
		}
	}
}

func renderedNamed(objects []*unstructured.Unstructured, kind, name string) []*unstructured.Unstructured {
	var found []*unstructured.Unstructured
	for _, object := range objects {
		if object.GetKind() == kind && object.GetName() == name {
			found = append(found, object)
		}
	}
	return found
}

func identities(objects []*unstructured.Unstructured) []string {
	var ids []string
	for _, object := range objects {
		ids = append(ids, object.GetKind()+" "+object.GetNamespace()+"/"+object.GetName())
	}
	sort.Strings(ids)
	return ids
}

func tail(output string) string {
	if len(output) > 2000 {
		return output[len(output)-2000:]
	}
	return output
}
