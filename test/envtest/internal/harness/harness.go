// Package harness starts the envtest control plane the suites under
// test/envtest share: one kube-apiserver and one etcd per package, and nothing
// else. No controller runs there -- nothing reconciles, garbage-collects or
// schedules -- so what a suite proves against it is the API server's own
// verdict: schemas, CEL, admission policies and webhooks.
package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	// AssetsVariable names the directory holding kube-apiserver, etcd and
	// kubectl. `make test-envtest` fills it from the pinned setup-envtest.
	AssetsVariable = "KUBEBUILDER_ASSETS"
	// RequireVariable turns a missing control plane from a skip into a
	// failure. `make test-envtest` sets it, so the target CI runs can only pass
	// by running every suite.
	RequireVariable = "PTAH_REQUIRE_ENVTEST"
)

// ControlPlane is the API server a package's tests share.
type ControlPlane struct {
	Environment *envtest.Environment
	// Config authenticates as the envtest administrator, a member of
	// system:masters. RBAC does not constrain it; admission still does.
	Config *rest.Config

	unavailable string
	cleanups    []func()
}

// New wraps environment. The package's TestMain keeps the result and calls
// Main on it.
func New(environment *envtest.Environment) *ControlPlane {
	return &ControlPlane{Environment: environment}
}

// Cleanup registers f to run once the package's tests are done and before the
// control plane stops, last registered first: a webhook server has to stop
// while the API server that calls it is still there to be told.
func (plane *ControlPlane) Cleanup(f func()) {
	plane.cleanups = append(plane.cleanups, f)
}

// prepare pins what every control plane these suites start has in common.
func prepare(environment *envtest.Environment, assets string) {
	// USE_EXISTING_CLUSTER would point every write at whatever cluster the
	// caller's kubeconfig names. These suites create and delete objects
	// freely, so they run against their own control plane or not at all.
	existing := false
	environment.UseExistingCluster = &existing
	environment.BinaryAssetsDirectory = assets
	environment.ErrorIfCRDPathMissing = true
	if environment.ControlPlaneStartTimeout == 0 {
		environment.ControlPlaneStartTimeout = time.Minute
	}
	if environment.ControlPlaneStopTimeout == 0 {
		environment.ControlPlaneStopTimeout = time.Minute
	}
}

// Start starts another control plane, from the same binaries, for one test
// that has to leave an API server in a state no other test may see. It stops
// when the test ends.
func (plane *ControlPlane) Start(t testing.TB, environment *envtest.Environment) *rest.Config {
	t.Helper()
	plane.Require(t)
	prepare(environment, plane.Environment.BinaryAssetsDirectory)
	config, err := environment.Start()
	if err != nil {
		_ = environment.Stop()
		t.Fatalf("start a control plane of the test's own: %v", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop the test's control plane: %v", err)
		}
	})
	return config
}

// Main starts the environment, runs setup against it, runs the package's
// tests and stops the control plane again. It returns the exit code for
// os.Exit.
//
// Without binaries the tests skip, unless RequireVariable is set, in which
// case the package fails before any test runs: a suite nothing ran is not a
// suite that passed, and the target CI uses sets the variable.
func (plane *ControlPlane) Main(m *testing.M, setup func() error) int {
	environment := plane.Environment
	assets := os.Getenv(AssetsVariable)
	if assets == "" {
		reason := fmt.Sprintf("%s is not set; run `make test-envtest`, which fetches the pinned control plane", AssetsVariable)
		if os.Getenv(RequireVariable) == "1" {
			fmt.Fprintf(os.Stderr, "%s is set and %s\n", RequireVariable, reason)
			return 1
		}
		plane.unavailable = reason
		return m.Run()
	}

	prepare(environment, assets)
	config, err := environment.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start the envtest control plane from %s: %v\n", assets, err)
		_ = environment.Stop()
		return 1
	}
	plane.Config = config

	code := 1
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "set up the suite: %v\n", err)
	} else {
		code = m.Run()
	}
	for index := len(plane.cleanups) - 1; index >= 0; index-- {
		plane.cleanups[index]()
	}
	if err := environment.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop the envtest control plane: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// Require skips the calling test when the control plane was never started
// because no binaries were available and nothing required them.
func (plane *ControlPlane) Require(t testing.TB) {
	t.Helper()
	if plane == nil {
		t.Fatal("the control plane was not set up; TestMain must call Main")
	}
	if plane.unavailable != "" {
		t.Skip(plane.unavailable)
	}
}

// Impersonate returns a copy of the administrator's configuration that acts
// as username. A ServiceAccount username with no groups gets the groups the
// API server derives for it -- system:serviceaccounts and the namespace's --
// and every non-anonymous identity gets system:authenticated, exactly as a
// token for that identity would.
func (plane *ControlPlane) Impersonate(username string, groups ...string) *rest.Config {
	config := rest.CopyConfig(plane.Config)
	config.Impersonate = rest.ImpersonationConfig{UserName: username, Groups: groups}
	return config
}

// RepositoryRoot is the checkout this package was compiled from.
func RepositoryRoot() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", "..", ".."))
}

// CRDDirectory holds the CRDs `make manifests` generates, byte for byte the
// ones the chart ships and the CRD manager installs.
func CRDDirectory() string {
	return filepath.Join(RepositoryRoot(), "config", "crd", "bases")
}

// Release is the chart release a suite renders.
type Release struct {
	Name      string
	Namespace string
	// Values are passed to helm as --set-string pairs, in order.
	Values [][2]string
}

// The release the suites render. The images are content-addressed and never
// pulled: nothing in envtest runs a container, and the admission rules read
// only the references.
var (
	ReleaseName      = "ptah"
	ReleaseNamespace = "ptah-system"
	ManagerDigest    = "sha256:" + strings.Repeat("2", 64)
	ManagerImage     = "ghcr.io/stokaro/ptah-operator@" + ManagerDigest
	ExecutorImage    = "example.invalid/ptah@sha256:" + strings.Repeat("3", 64)
	RunnerImage      = "example.invalid/operator@sha256:" + strings.Repeat("4", 64)
	PtahVersion      = "v0.1.0"
)

// DefaultRelease renders the chart with its own defaults and the four values
// it refuses to guess.
func DefaultRelease() Release {
	return Release{
		Name:      ReleaseName,
		Namespace: ReleaseNamespace,
		Values: [][2]string{
			{"image.digest", ManagerDigest},
			{"execution.executorImage", ExecutorImage},
			{"execution.runnerImage", RunnerImage},
			{"execution.ptahVersion", PtahVersion},
		},
	}
}

// ErrHelmMissing reports a machine with no helm on PATH.
var ErrHelmMissing = errors.New("helm is not on PATH; the admission suites render the chart they test")

// RenderChart runs `helm template` over charts/ptah-operator and decodes every
// document it prints. It renders offline, so every lookup the templates make
// sees an empty cluster, which is what a first install sees.
func RenderChart(ctx context.Context, release Release) ([]*unstructured.Unstructured, error) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		return nil, ErrHelmMissing
	}
	args := []string{
		"template", release.Name, filepath.Join(RepositoryRoot(), "charts", "ptah-operator"),
		"--namespace", release.Namespace,
	}
	for _, value := range release.Values {
		args = append(args, "--set-string", value[0]+"="+value[1])
	}
	home, err := os.MkdirTemp("", "ptah-envtest-helm-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(home) }()

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, helm, args...)
	command.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(home, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(home, "config"),
		"HELM_DATA_HOME="+filepath.Join(home, "data"),
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		// The rendered chart carries generated private keys, so only the
		// renderer's own complaint is repeated, never its output.
		return nil, fmt.Errorf("helm template: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return DecodeDocuments(output)
}

// DecodeDocuments splits a YAML stream into objects, dropping empty documents.
func DecodeDocuments(stream []byte) ([]*unstructured.Unstructured, error) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(stream), 4096)
	var objects []*unstructured.Unstructured
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(&object.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return objects, nil
			}
			return nil, fmt.Errorf("decode a rendered document: %w", err)
		}
		if len(object.Object) == 0 || object.GetKind() == "" {
			continue
		}
		objects = append(objects, object)
	}
}
