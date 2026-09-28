package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
)

// The one policy the chart writes by hand, and so the one rendered policy the
// generator does not account for.
const applyPolicyGuardPrefix = "ptah-operator-apply-policy-guard-"

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve the test's path")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

// TestTheChartShipsWhatTheGeneratorWrites is the gate verify-source runs
// through git, here so a plain go test says the same thing: every generated
// template in the chart is byte for byte what the generator writes now.
func TestTheChartShipsWhatTheGeneratorWrites(t *testing.T) {
	t.Parallel()

	rendered, err := render()
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered) == 0 {
		t.Fatal("the generator writes nothing")
	}
	chart := filepath.Join(repositoryRoot(t), "charts", "ptah-operator")
	for path, want := range rendered {
		got, err := os.ReadFile(filepath.Join(chart, filepath.FromSlash(path))) //nolint:gosec // A path the generator itself names.
		if err != nil {
			t.Fatalf("%s: %v (run make chart-policies)", path, err)
		}
		if string(got) != want {
			t.Errorf("%s differs from what the generator writes; run make chart-policies", path)
		}
	}
}

// TestRenderedPoliciesAreTheGoDefinitions renders the chart and compares
// every policy and binding helm produced with the ones Go builds for the
// same release. The generated template is what the Helm expressions in
// policies.go compute; this is what shows they compute the same values the
// Go definitions were given, and that the chart ships no policy the
// generator does not know about, apart from the one it says it leaves to
// the chart.
func TestRenderedPoliciesAreTheGoDefinitions(t *testing.T) {
	t.Parallel()

	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for chart render tests")
	}
	const (
		release   = "ptah"
		namespace = "ptah-system"
		image     = "ghcr.io/stokaro/ptah-operator@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, helm,
		"template", release, filepath.Join(repositoryRoot(t), "charts", "ptah-operator"),
		"--namespace", namespace,
		"--set-string", "image.digest=sha256:"+strings.Repeat("2", 64),
		"--set-string", "execution.executorImage=example.invalid/ptah@sha256:"+strings.Repeat("3", 64),
		"--set-string", "execution.runnerImage=example.invalid/operator@sha256:"+strings.Repeat("4", 64),
		"--set-string", "execution.ptahVersion=v0.1.0",
		// The Secret guard renders only when a deleted Secret may be
		// recreated.
		"--set", "certificateRotation.recreateMissingSecret=true",
	)
	home := t.TempDir()
	command.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(home, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(home, "config"),
		"HELM_DATA_HOME="+filepath.Join(home, "data"),
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	rendered := decodeRender(t, output)

	v := values{
		ReleaseNamespace:         namespace,
		ReleaseName:              release,
		ControllerServiceAccount: rendered.serviceAccount(t, "controller"),
		ManagerImage:             image,
		ControllerStateVersion:   controllerstate.CurrentVersion,
		RotatorServiceAccount:    rendered.serviceAccount(t, "certificate-rotation"),
		WebhookSecret:            rendered.rotatorArgument(t, "--secret-name="),
	}
	generated := map[string]bool{}
	for _, tmpl := range templates(v) {
		for _, pair := range tmpl.pairs {
			name := pair.policy.Name
			generated[name] = true
			policy, ok := rendered.policies[name]
			if !ok {
				t.Errorf("%s: the chart renders no policy %s", tmpl.path, name)
				continue
			}
			binding, ok := rendered.bindings[name]
			if !ok {
				t.Errorf("%s: the chart renders no binding %s", tmpl.path, name)
				continue
			}
			if !reflect.DeepEqual(policy.Spec, pair.policy.Spec) {
				t.Errorf("%s: rendered policy %s differs from the Go definition\nrendered: %s\ngo:       %s",
					tmpl.path, name, mustJSON(t, policy.Spec), mustJSON(t, pair.policy.Spec))
			}
			if !reflect.DeepEqual(binding.Spec, pair.binding.Spec) {
				t.Errorf("%s: rendered binding %s differs from the Go definition\nrendered: %s\ngo:       %s",
					tmpl.path, name, mustJSON(t, binding.Spec), mustJSON(t, pair.binding.Spec))
			}
			for kind, meta := range map[string]metav1.ObjectMeta{"policy": policy.ObjectMeta, "binding": binding.ObjectMeta} {
				for key, want := range pair.policy.Labels {
					if meta.Labels[key] != want {
						t.Errorf("%s: rendered %s %s has label %s=%q, want %q", tmpl.path, kind, name, key, meta.Labels[key], want)
					}
				}
				// An ordinary release object: Helm installs, upgrades and
				// deletes it with the rest, and it is no hook.
				if meta.Labels["app.kubernetes.io/managed-by"] != "Helm" || meta.Annotations["helm.sh/hook"] != "" {
					t.Errorf("%s: rendered %s %s is not an ordinary release object: labels %v, annotations %v",
						tmpl.path, kind, name, meta.Labels, meta.Annotations)
				}
			}
		}
	}
	if len(generated) == 0 {
		t.Fatal("the generator defines no policy")
	}
	for name := range rendered.policies {
		if !generated[name] && !strings.HasPrefix(name, applyPolicyGuardPrefix) {
			t.Errorf("the chart renders policy %s, which is neither generated nor the apply-policy guard", name)
		}
	}
	for name := range rendered.bindings {
		if _, ok := rendered.policies[name]; !ok {
			t.Errorf("the chart renders binding %s with no policy of that name", name)
		}
	}
}

type renderedChart struct {
	policies    map[string]*admissionregistrationv1.ValidatingAdmissionPolicy
	bindings    map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding
	deployments []*appsv1.Deployment
}

func decodeRender(t *testing.T, stream []byte) *renderedChart {
	t.Helper()
	r := &renderedChart{
		policies: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{},
		bindings: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{},
	}
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(stream))
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var typeMeta metav1.TypeMeta
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			t.Fatal(err)
		}
		switch typeMeta.Kind {
		case "ValidatingAdmissionPolicy":
			object := &admissionregistrationv1.ValidatingAdmissionPolicy{}
			if err := json.Unmarshal(raw, object); err != nil {
				t.Fatal(err)
			}
			if _, duplicate := r.policies[object.Name]; duplicate {
				t.Fatalf("the chart renders policy %s twice", object.Name)
			}
			r.policies[object.Name] = object
		case "ValidatingAdmissionPolicyBinding":
			object := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
			if err := json.Unmarshal(raw, object); err != nil {
				t.Fatal(err)
			}
			if _, duplicate := r.bindings[object.Name]; duplicate {
				t.Fatalf("the chart renders binding %s twice", object.Name)
			}
			r.bindings[object.Name] = object
		case "Deployment":
			object := &appsv1.Deployment{}
			if err := json.Unmarshal(raw, object); err != nil {
				t.Fatal(err)
			}
			r.deployments = append(r.deployments, object)
		}
	}
	return r
}

// serviceAccount is the ServiceAccount the Deployment labeled component runs
// as, read out of the render rather than written down here so a renamed
// identity moves this test with the chart.
func (r *renderedChart) serviceAccount(t *testing.T, component string) string {
	t.Helper()
	name := ""
	for _, deployment := range r.deployments {
		if deployment.Labels["app.kubernetes.io/component"] != component {
			continue
		}
		if name != "" {
			t.Fatalf("the chart renders two Deployments with component %s", component)
		}
		name = deployment.Spec.Template.Spec.ServiceAccountName
	}
	if name == "" {
		t.Fatalf("the chart renders no Deployment with component %s and a ServiceAccount", component)
	}
	return name
}

// rotatorArgument reads one flag's value off the certificate rotator's
// container.
func (r *renderedChart) rotatorArgument(t *testing.T, prefix string) string {
	t.Helper()
	for _, deployment := range r.deployments {
		if deployment.Labels["app.kubernetes.io/component"] != "certificate-rotation" {
			continue
		}
		for _, container := range deployment.Spec.Template.Spec.Containers {
			for _, argument := range container.Args {
				if value, ok := strings.CutPrefix(argument, prefix); ok && value != "" {
					return value
				}
			}
		}
	}
	t.Fatalf("the certificate rotator Deployment carries no %s argument", prefix)
	return ""
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
