package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
)

// lab is the environment a bootstrapped cluster left behind.
//
// It is read, never computed. `hack/e2e-kind.sh` with E2E_STOP_AFTER=bootstrap
// builds the registry, the external database, the chart install and the
// admission policies the end-to-end suite needs, and writes what it built to
// one file. The recorder reads that file so the demonstration runs against the
// environment the suite proves, rather than against a second one nobody checks.
type lab struct {
	values map[string]string
	path   string
}

// loadLab reads the environment file written by the bootstrap.
func loadLab(path string) (lab, error) {
	file, err := os.Open(path)
	if err != nil {
		return lab{}, fmt.Errorf(
			"read the lab environment: %w\n\nbring the lab up first: make demo-up", err)
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		name, value, found := strings.Cut(line, "=")
		if !found {
			return lab{}, fmt.Errorf("%s: %q is not a NAME=value line", path, line)
		}
		values[name] = value
	}
	if err := scanner.Err(); err != nil {
		return lab{}, fmt.Errorf("read %s: %w", path, err)
	}
	return lab{values: values, path: path}, nil
}

// required returns one value, and fails when the bootstrap did not write it.
//
// A missing value is a mismatch between the bootstrap and this program, not a
// condition to default through: defaulting would run the scenario against some
// other namespace and record whatever it found there.
func (l lab) required(name string) (string, error) {
	value, ok := l.values[name]
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf(
			"%s does not carry %s; the lab was bootstrapped by a different revision of "+
				"hack/e2e-kind.sh than this recorder expects", l.path, name)
	}
	return value, nil
}

// labVariables are the names a scenario step may read from the environment.
//
// The list is explicit because the shell in a scenario file is published. A
// step that read an arbitrary variable would publish whatever the recorder
// happened to be run with, and a reader copying the command would find nothing
// under that name.
var labVariables = []string{
	"E2E_KUBECONFIG",
	"E2E_OPERATOR_NAMESPACE",
	"E2E_TEST_NAMESPACE",
	"E2E_HELM_RELEASE",
	"E2E_CONTROLLER_NAME",
	"E2E_CONTROLLER_IMAGE",
	"E2E_CONTROLLER_REVISION",
	"E2E_CONTROLLER_STATE_VERSION",
	"E2E_EXECUTOR_IMAGE",
	"E2E_RUNNER_IMAGE",
	"E2E_PTAH_VERSION",
	"E2E_PTAH_REVISION",
	"E2E_REGISTRY_HOST",
	"E2E_REGISTRY_SERVICE",
	"E2E_REGISTRY_IP",
	"E2E_REGISTRY_USERNAME",
	"E2E_POSTGRES_IMAGE",
	"E2E_REGISTRY_CREDENTIALS_FILE",
	"E2E_EXTERNAL_POSTGRES_CONTAINER_ID",
	"E2E_EXTERNAL_POSTGRES_IP",
	"E2E_EXTERNAL_POSTGRES_SERVICE",
	"E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE",
	"E2E_KIND_CLUSTER_NAME",
	"E2E_DOCKER_CONTEXT",
	"E2E_DOCKER_CONFIG",
	"E2E_KUBERNETES_VERSION",
}

// environment is what a scenario step runs with.
//
// KUBECONFIG and NAMESPACE are the two a reader types themselves, so they are
// spelled the way a reader would have them. LAB_ROOT is the repository, which
// is what demo/bin/lab needs to find its own helpers.
func (l lab) environment(root string) ([]string, error) {
	namespace, err := l.required("E2E_TEST_NAMESPACE")
	if err != nil {
		return nil, err
	}
	kubeconfig, err := l.required("E2E_KUBECONFIG")
	if err != nil {
		return nil, err
	}

	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LAB_ROOT=" + root,
		"KUBECONFIG=" + kubeconfig,
		"NAMESPACE=" + namespace,
		// The two names a step needs that are not the demonstration's own: the
		// operator's release namespace and the Deployment the chart named.
		"OPERATOR_NAMESPACE=" + l.values["E2E_OPERATOR_NAMESPACE"],
		"CONTROLLER=" + l.values["E2E_CONTROLLER_NAME"],
	}
	for _, name := range labVariables {
		if value, ok := l.values[name]; ok {
			environment = append(environment, name+"="+value)
		}
	}
	return environment, nil
}

// describe returns the lab's identity, for the recording's metadata.
func (l lab) describe() map[string]string {
	described := map[string]string{}
	for _, name := range []string{
		"E2E_KUBERNETES_VERSION",
		"E2E_KIND_CLUSTER_NAME",
		"E2E_CONTROLLER_IMAGE",
		"E2E_CONTROLLER_REVISION",
		"E2E_EXECUTOR_IMAGE",
		"E2E_RUNNER_IMAGE",
		"E2E_PTAH_VERSION",
		"E2E_PTAH_REVISION",
	} {
		if value, ok := l.values[name]; ok && value != "" {
			described[strings.TrimPrefix(name, "E2E_")] = value
		}
	}
	return described
}

// names returns the variable names the lab carries, sorted, for diagnostics.
func (l lab) names() []string {
	names := make([]string, 0, len(l.values))
	for name := range l.values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
