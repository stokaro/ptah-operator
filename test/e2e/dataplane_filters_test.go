package e2e

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// These are the samples the shell gates held the filters to, each built the
// way the static gate and the filter self-test built it and changed the way
// they changed it: as a JSON document, before the typed decoding the phase
// reads the API through. A filter is shown to accept the reading it exists
// for and to refuse every mistake that was made.

// fixture is a JSON document a test edits in place, as the shell edited its
// samples with jq.
type fixture struct {
	t   *testing.T
	doc any
}

// newFixture decodes a document, or anything that encodes as one, into plain
// JSON values the test can edit without touching the value it came from.
func newFixture(t *testing.T, document any) fixture {
	t.Helper()
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return fixture{t: t, doc: decoded}
}

func parseFixture(t *testing.T, literal string) fixture {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(literal), &decoded); err != nil {
		t.Fatalf("decode fixture literal: %v", err)
	}
	return fixture{t: t, doc: decoded}
}

func (f fixture) clone() fixture {
	f.t.Helper()
	return newFixture(f.t, f.doc)
}

// step walks one key of an object or one index of a list; a negative index
// counts from the end.
func (f fixture) step(value any, step any) any {
	f.t.Helper()
	switch key := step.(type) {
	case string:
		object, ok := value.(map[string]any)
		if !ok {
			f.t.Fatalf("fixture step %q reads a %T", key, value)
		}
		return object[key]
	case int:
		list, ok := value.([]any)
		if !ok {
			f.t.Fatalf("fixture step %d reads a %T", key, value)
		}
		if key < 0 {
			key += len(list)
		}
		if key < 0 || key >= len(list) {
			f.t.Fatalf("fixture step %d is outside a list of %d", key, len(list))
		}
		return list[key]
	default:
		f.t.Fatalf("fixture step %v is neither a key nor an index", step)
		return nil
	}
}

func (f fixture) at(steps ...any) any {
	f.t.Helper()
	value := f.doc
	for _, step := range steps {
		value = f.step(value, step)
	}
	return value
}

// set writes value at the path, making objects on the way as jq's assignment
// does.
func (f fixture) set(value any, steps ...any) {
	f.t.Helper()
	parent := f.doc
	for index, step := range steps[:len(steps)-1] {
		next := f.step(parent, step)
		if next == nil {
			if _, isKey := steps[index+1].(string); !isKey {
				f.t.Fatalf("fixture path %v has no list to index", steps)
			}
			next = map[string]any{}
			f.assign(parent, step, next)
		}
		parent = next
	}
	f.assign(parent, steps[len(steps)-1], value)
}

func (f fixture) assign(parent, step, value any) {
	f.t.Helper()
	switch key := step.(type) {
	case string:
		parent.(map[string]any)[key] = value
	case int:
		list := parent.([]any)
		if key < 0 {
			key += len(list)
		}
		list[key] = value
	}
}

// del removes a key, or an element of a list.
func (f fixture) del(steps ...any) {
	f.t.Helper()
	parent := f.at(steps[:len(steps)-1]...)
	switch key := steps[len(steps)-1].(type) {
	case string:
		delete(parent.(map[string]any), key)
	case int:
		list := parent.([]any)
		if key < 0 {
			key += len(list)
		}
		f.set(append(append([]any{}, list[:key]...), list[key+1:]...), steps[:len(steps)-1]...)
	}
}

// push appends to the list at the path, making it when it is absent.
func (f fixture) push(value any, steps ...any) {
	f.t.Helper()
	list, _ := f.at(steps...).([]any)
	f.set(append(append([]any{}, list...), newFixture(f.t, value).doc), steps...)
}

// keep keeps the elements of the list at the path that match.
func (f fixture) keep(match func(map[string]any) bool, steps ...any) {
	f.t.Helper()
	var kept []any
	for _, element := range f.at(steps...).([]any) {
		if match(element.(map[string]any)) {
			kept = append(kept, element)
		}
	}
	if kept == nil {
		kept = []any{}
	}
	f.set(kept, steps...)
}

// named is the index of the element of the list at the path whose name is
// name.
func (f fixture) named(name string, steps ...any) int {
	f.t.Helper()
	for index, element := range f.at(steps...).([]any) {
		if object, ok := element.(map[string]any); ok && object["name"] == name {
			return index
		}
	}
	f.t.Fatalf("fixture list %v has no element named %s", steps, name)
	return -1
}

func notNamed(name string) func(map[string]any) bool {
	return func(element map[string]any) bool { return element["name"] != name }
}

// decode reads the document into the typed object the phase would have read
// from the API.
func (f fixture) decode(target any) error {
	encoded, err := json.Marshal(f.doc)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

func (f fixture) jobs() []batchv1.Job {
	f.t.Helper()
	list := &batchv1.JobList{}
	if err := f.decode(list); err != nil {
		f.t.Fatalf("decode Job list: %v", err)
	}
	return list.Items
}

func (f fixture) pods() []corev1.Pod {
	f.t.Helper()
	list := &corev1.PodList{}
	if err := f.decode(list); err != nil {
		f.t.Fatalf("decode Pod list: %v", err)
	}
	return list.Items
}

func (f fixture) endpointSlices() []discoveryv1.EndpointSlice {
	f.t.Helper()
	list := &discoveryv1.EndpointSliceList{}
	if err := f.decode(list); err != nil {
		f.t.Fatalf("decode EndpointSlice list: %v", err)
	}
	return list.Items
}

func (f fixture) job() *batchv1.Job {
	f.t.Helper()
	job := &batchv1.Job{}
	if err := f.decode(job); err != nil {
		f.t.Fatalf("decode Job: %v", err)
	}
	return job
}

func (f fixture) schema() *ptahv1alpha1.PtahSchema {
	f.t.Helper()
	schema := &ptahv1alpha1.PtahSchema{}
	if err := f.decode(schema); err != nil {
		f.t.Fatalf("decode PtahSchema: %v", err)
	}
	return schema
}

func literalEnvFixture(name, value string) map[string]any {
	return map[string]any{"name": name, "value": value}
}

func secretEnvFixture(name, secret, key string) map[string]any {
	return map[string]any{"name": name, "valueFrom": map[string]any{"secretKeyRef": map[string]any{
		"name": secret, "key": key,
	}}}
}

func optionalSecretEnvFixture(name, secret, key string) map[string]any {
	return map[string]any{"name": name, "valueFrom": map[string]any{"secretKeyRef": map[string]any{
		"name": secret, "key": key, "optional": true,
	}}}
}

func concatFixtures(lists ...[]any) []any {
	var joined []any
	for _, list := range lists {
		joined = append(joined, list...)
	}
	if joined == nil {
		joined = []any{}
	}
	return joined
}

// controllerJobFixture is the Job list hack/e2e-static.sh held
// controller-job-isolation.jq to: one Job per operation, each with the
// containers and the environment its operation runs with.
func controllerJobFixture(t *testing.T) fixture {
	t.Helper()
	registryCredentialEnv := []any{
		optionalSecretEnvFixture("PTAH_OCI_USERNAME", "registry-auth", "username"),
		optionalSecretEnvFixture("PTAH_OCI_PASSWORD", "registry-auth", "password"),
		optionalSecretEnvFixture("PTAH_OCI_TOKEN", "registry-auth", "token"),
		literalEnvFixture("PTAH_OCI_REGISTRY", "registry.example"),
		literalEnvFixture("PTAH_PLAIN_HTTP", "false"),
		literalEnvFixture("PTAH_OCI_CA_FILE", "/credentials/ca-snapshot/ca.pem"),
	}
	authorityGuardEnv := []any{
		literalEnvFixture("PTAH_OPERATOR_OCI_AUTH_MODE", "Environment"),
		secretEnvFixture("PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT", "registry-auth", "registry"),
		secretEnvFixture("PTAH_OPERATOR_OCI_CA_SHA256_GRANT", "registry-auth", "caSHA256"),
		literalEnvFixture("PTAH_OPERATOR_OCI_HAS_CA", "true"),
		literalEnvFixture("PTAH_OPERATOR_OCI_CA_SOURCE_FILE", "/credentials/ca-source/ca.pem"),
		literalEnvFixture("PTAH_OCI_REGISTRY", "registry.example"),
		literalEnvFixture("PTAH_PLAIN_HTTP", "false"),
	}
	registryEnv := concatFixtures(registryCredentialEnv[:5], authorityGuardEnv)
	databaseEnv := []any{secretEnvFixture("PTAH_DB_URL", "database-url", "url")}
	observeEnv := concatFixtures(databaseEnv, []any{literalEnvFixture("PTAH_EXPECTED_DATABASE_ENGINE", "PostgreSQL")})
	job := func(operation string, mainEnv, fetchEnv []any) map[string]any {
		mainMounts := []any{}
		if operation == "resolve" || operation == "verify" {
			mainMounts = []any{map[string]any{"name": "registry-ca", "mountPath": "/credentials/ca-source", "readOnly": true}}
		}
		initContainers := []any{map[string]any{"name": "install-runner", "env": []any{}}}
		if fetchEnv != nil {
			initContainers = append(initContainers,
				map[string]any{"name": "validate-source-authority", "env": authorityGuardEnv, "volumeMounts": []any{
					map[string]any{"name": "runner", "mountPath": "/runner", "readOnly": true},
					map[string]any{"name": "registry-ca", "mountPath": "/credentials/ca-source", "readOnly": true},
					map[string]any{"name": "registry-ca-snapshot", "mountPath": "/credentials/ca-snapshot"},
				}},
				map[string]any{"name": "fetch-schema", "env": fetchEnv, "volumeMounts": []any{
					map[string]any{"name": "registry-ca-snapshot", "mountPath": "/credentials/ca-snapshot", "readOnly": true},
				}},
			)
		}
		return map[string]any{
			"metadata": map[string]any{
				"uid":    "uid-" + operation,
				"labels": map[string]any{"operator.ptah.run/operation": operation},
			},
			"spec": map[string]any{
				"backoffLimit":         0,
				"podReplacementPolicy": "Failed",
				"template": map[string]any{"spec": map[string]any{
					"restartPolicy": "Never",
					"containers": []any{map[string]any{
						"name": "ptah", "env": mainEnv, "volumeMounts": mainMounts,
					}},
					"initContainers": initContainers,
				}},
			},
		}
	}
	return newFixture(t, map[string]any{"items": []any{
		job("resolve", registryEnv, nil),
		job("verify", registryEnv, nil),
		job("observe", observeEnv, registryCredentialEnv),
		job("plan", observeEnv, registryCredentialEnv),
		job("apply", databaseEnv, nil),
	}})
}

const fixtureResolvedReference = "oci://registry.example/team/schema@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// customCAPodFixture is the Pod list hack/e2e-static.sh held
// custom-ca-pod-isolation.jq to: the Observe and Plan Pods the controller
// fixture's Jobs would run, admitted and completed.
func customCAPodFixture(t *testing.T) fixture {
	t.Helper()
	hardenedContainer := map[string]any{
		"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "runAsNonRoot": true,
		"runAsUser": 65532, "runAsGroup": 65532,
		"capabilities": map[string]any{"drop": []any{"ALL"}}, "seccompProfile": map[string]any{"type": "RuntimeDefault"},
	}
	jobs := controllerJobFixture(t)
	var items []any
	for index, element := range jobs.at("items").([]any) {
		operation := jobs.at("items", index, "metadata", "labels", "operator.ptah.run/operation")
		if operation != "observe" && operation != "plan" {
			continue
		}
		job := newFixture(t, element)
		spec := newFixture(t, job.at("spec", "template", "spec"))
		spec.set(false, "automountServiceAccountToken")
		spec.set(false, "enableServiceLinks")
		spec.set(map[string]any{
			"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532,
			"fsGroupChangePolicy": "OnRootMismatch", "seccompProfile": map[string]any{"type": "RuntimeDefault"},
		}, "securityContext")
		spec.set(hardenedContainer, "containers", 0, "securityContext")
		spec.set([]any{}, "containers", 0, "envFrom")
		spec.set([]any{
			map[string]any{"name": "runner", "mountPath": "/runner", "readOnly": true},
			map[string]any{"name": "work", "mountPath": "/work"},
			map[string]any{"name": "schema-source", "mountPath": "/source", "readOnly": true},
		}, "containers", 0, "volumeMounts")
		install := spec.named("install-runner", "initContainers")
		spec.set([]any{"/ptah-runner"}, "initContainers", install, "command")
		spec.set([]any{"--install-to", "/runner/ptah-runner"}, "initContainers", install, "args")
		spec.set([]any{}, "initContainers", install, "envFrom")
		spec.set(hardenedContainer, "initContainers", install, "securityContext")
		spec.set([]any{map[string]any{"name": "runner", "mountPath": "/runner"}}, "initContainers", install, "volumeMounts")
		guard := spec.named("validate-source-authority", "initContainers")
		spec.set([]any{"/runner/ptah-runner"}, "initContainers", guard, "command")
		spec.set([]any{"--validate-oci-source", fixtureResolvedReference, "--snapshot-oci-ca-to", "/credentials/ca-snapshot/ca.pem"},
			"initContainers", guard, "args")
		spec.set([]any{}, "initContainers", guard, "envFrom")
		spec.set(hardenedContainer, "initContainers", guard, "securityContext")
		fetch := spec.named("fetch-schema", "initContainers")
		spec.set([]any{"/usr/local/bin/ptah"}, "initContainers", fetch, "command")
		spec.set([]any{"schema", "pull", fixtureResolvedReference, "--out", "/source/schema.hcl"},
			"initContainers", fetch, "args")
		spec.set([]any{}, "initContainers", fetch, "envFrom")
		spec.set(hardenedContainer, "initContainers", fetch, "securityContext")
		spec.set([]any{
			map[string]any{"name": "schema-source", "mountPath": "/source"},
			map[string]any{"name": "fetch-work", "mountPath": "/fetch-work"},
			map[string]any{"name": "registry-ca-snapshot", "mountPath": "/credentials/ca-snapshot", "readOnly": true},
		}, "initContainers", fetch, "volumeMounts")
		spec.set([]any{
			map[string]any{"name": "runner", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "64Mi"}},
			map[string]any{"name": "work", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "128Mi"}},
			map[string]any{"name": "fetch-work", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "64Mi"}},
			map[string]any{"name": "registry-ca", "configMap": map[string]any{
				"name": "registry-ca", "optional": false, "defaultMode": 420,
				"items": []any{map[string]any{"key": "ca.pem", "path": "ca.pem", "mode": 288}},
			}},
			map[string]any{"name": "registry-ca-snapshot", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "2Mi"}},
			map[string]any{"name": "schema-source", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "64Mi"}},
		}, "volumes")
		labels := newFixture(t, job.at("metadata", "labels"))
		labels.set("custom-ca", "operator.ptah.run/schema")
		terminated := func(name string) map[string]any {
			return map[string]any{"name": name, "restartCount": 0, "state": map[string]any{"terminated": map[string]any{"exitCode": 0}}}
		}
		items = append(items, map[string]any{
			"metadata": map[string]any{
				"name":   "pod-" + operation.(string),
				"uid":    "pod-uid-" + operation.(string),
				"labels": labels.doc,
				"ownerReferences": []any{map[string]any{
					"apiVersion": "batch/v1", "kind": "Job", "name": "job", "uid": job.at("metadata", "uid"),
					"controller": true,
				}},
			},
			"spec": spec.doc,
			"status": map[string]any{
				"phase": "Succeeded",
				"initContainerStatuses": []any{
					terminated("install-runner"), terminated("validate-source-authority"), terminated("fetch-schema"),
				},
				"containerStatuses": []any{terminated("ptah")},
			},
		})
	}
	return newFixture(t, map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
}

// sourceJobFixture is the Resolve and Verify Job list hack/e2e-static.sh held
// source-job-isolation.jq to, for one registry authentication mode.
func sourceJobFixture(t *testing.T, authMode string) fixture {
	t.Helper()
	hardenedContainer := map[string]any{
		"capabilities": map[string]any{"drop": []any{"ALL"}},
		"runAsUser":    65532, "runAsGroup": 65532, "runAsNonRoot": true,
		"readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false,
		"seccompProfile": map[string]any{"type": "RuntimeDefault"},
	}
	hardenedPod := map[string]any{
		"runAsUser": 65532, "runAsGroup": 65532, "runAsNonRoot": true, "fsGroup": 65532,
		"fsGroupChangePolicy": "OnRootMismatch", "seccompProfile": map[string]any{"type": "RuntimeDefault"},
	}
	secretEnv := func(name, key string, optional bool) map[string]any {
		if optional {
			return optionalSecretEnvFixture(name, "registry-auth", key)
		}
		return secretEnvFixture(name, "registry-auth", key)
	}
	operationID := func(operation string) string {
		digit := "2"
		if operation == "resolve" {
			digit = "1"
		}
		return "sha256:" + strings.Repeat(digit, 64)
	}
	fixedGrants := []any{
		literalEnvFixture("PTAH_OPERATOR_OCI_AUTH_MODE", authMode),
		secretEnv("PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT", "registry", false),
		secretEnv("PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP", "allowPlainHTTP", false),
		literalEnvFixture("PTAH_OCI_REGISTRY", "registry.example:5000"),
		literalEnvFixture("PTAH_PLAIN_HTTP", "true"),
	}
	environmentCredentials := []any{
		secretEnv("PTAH_OCI_USERNAME", "username", true),
		secretEnv("PTAH_OCI_PASSWORD", "password", true),
		secretEnv("PTAH_OCI_TOKEN", "token", true),
	}
	operationEnvironment := func(operation string) []any {
		env := []any{
			literalEnvFixture("HOME", "/work"),
			literalEnvFixture("TMPDIR", "/work"),
			literalEnvFixture("PTAH_OPERATION_ID", operationID(operation)),
			literalEnvFixture("PTAH_REQUESTED_REFERENCE", "oci://registry.example:5000/acme/schema:latest"),
			literalEnvFixture("PTAH_RUNNER_PROTOCOL_VERSION", "5"),
		}
		if operation == "verify" {
			env = append(env,
				literalEnvFixture("PTAH_RESOLVED_REFERENCE", "oci://registry.example:5000/acme/schema@sha256:"+strings.Repeat("a", 64)),
				literalEnvFixture("PTAH_VERIFICATION_POLICY", "/verification/policy.yaml"),
				literalEnvFixture("PTAH_EXPECTED_ARTIFACT_TYPE", "application/vnd.stokaro.ptah.schema.v1"),
			)
		}
		return env
	}
	dockerVolume := map[string]any{
		"name": "registry-docker-config",
		"secret": map[string]any{
			"secretName":  "registry-auth",
			"items":       []any{map[string]any{"key": ".dockerconfigjson", "path": "config.json", "mode": 288}},
			"defaultMode": 420,
		},
	}
	job := func(operation string) map[string]any {
		credentials := environmentCredentials
		if authMode != "Environment" {
			credentials = []any{literalEnvFixture("DOCKER_CONFIG", "/credentials/docker")}
		}
		env := concatFixtures(operationEnvironment(operation), fixedGrants, credentials)
		sortFixturesByName(env)
		mounts := []any{
			map[string]any{"name": "runner", "mountPath": "/runner", "readOnly": true},
			map[string]any{"name": "work", "mountPath": "/work"},
		}
		volumes := []any{
			map[string]any{"name": "runner", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "64Mi"}},
			map[string]any{"name": "work", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "128Mi"}},
		}
		if authMode == "DockerConfigJSON" {
			mounts = append(mounts, map[string]any{"name": "registry-docker-config", "mountPath": "/credentials/docker", "readOnly": true})
			volumes = append(volumes, dockerVolume)
		}
		if operation == "verify" {
			mounts = append(mounts, map[string]any{"name": "verification-policy", "mountPath": "/verification", "readOnly": true})
			volumes = append(volumes, map[string]any{"name": "verification-policy", "configMap": map[string]any{
				"name":        "verification-policy",
				"items":       []any{map[string]any{"key": "policy.yaml", "path": "policy.yaml", "mode": 288}},
				"defaultMode": 420,
			}})
		}
		return map[string]any{
			"metadata": map[string]any{
				"labels":      map[string]any{"operator.ptah.run/operation": operation},
				"annotations": map[string]any{"operator.ptah.run/operation-id": operationID(operation)},
			},
			"spec": map[string]any{
				"backoffLimit":         0,
				"podReplacementPolicy": "Failed",
				"template": map[string]any{"spec": map[string]any{
					"restartPolicy":                "Never",
					"serviceAccountName":           "default",
					"automountServiceAccountToken": false,
					"enableServiceLinks":           false,
					"dnsPolicy":                    "ClusterFirst",
					"imagePullSecrets":             []any{},
					"securityContext":              hardenedPod,
					"initContainers": []any{map[string]any{
						"name":                     "install-runner",
						"image":                    "example.invalid/operator@sha256:" + strings.Repeat("e", 64),
						"imagePullPolicy":          "IfNotPresent",
						"command":                  []any{"/ptah-runner"},
						"args":                     []any{"--install-to", "/runner/ptah-runner"},
						"env":                      []any{},
						"terminationMessagePath":   "/dev/termination-log",
						"terminationMessagePolicy": "File",
						"securityContext":          hardenedContainer,
						"volumeMounts":             []any{map[string]any{"name": "runner", "mountPath": "/runner"}},
					}},
					"containers": []any{map[string]any{
						"name":            "ptah",
						"image":           "example.invalid/ptah@sha256:" + strings.Repeat("d", 64),
						"imagePullPolicy": "IfNotPresent",
						"command":         []any{"/runner/ptah-runner"},
						"args": []any{
							"--ptah-binary", "/usr/local/bin/ptah",
							"--max-result-bytes", "8388608",
							"--max-plan-bytes", "8388608",
							"--operation", operation,
						},
						"workingDir":               "/work",
						"terminationMessagePath":   "/dev/termination-log",
						"terminationMessagePolicy": "File",
						"securityContext":          hardenedContainer,
						"env":                      env,
						"volumeMounts":             mounts,
					}},
					"volumes": volumes,
				}},
			},
		}
	}
	return newFixture(t, map[string]any{"items": []any{job("resolve"), job("verify")}})
}

// sortFixturesByName is jq's sort_by(.name).
func sortFixturesByName(list []any) {
	name := func(element any) string { return element.(map[string]any)["name"].(string) }
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && name(list[j]) < name(list[j-1]); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

func sourceIsolationInputs(authMode string) sourceJobIsolationInputs {
	return sourceJobIsolationInputs{
		databaseSecret:     "database-url",
		registrySecret:     "registry-auth",
		registryAuthority:  "registry.example:5000",
		authMode:           authMode,
		executorImage:      "example.invalid/ptah@sha256:" + strings.Repeat("d", 64),
		runnerImage:        "example.invalid/operator@sha256:" + strings.Repeat("e", 64),
		verificationPolicy: "verification-policy",
		serviceAccountName: "default",
		imagePullSecrets:   []corev1.LocalObjectReference{},
		requestedReference: "oci://registry.example:5000/acme/schema:latest",
		resolvedReference:  "oci://registry.example:5000/acme/schema@sha256:" + strings.Repeat("a", 64),
	}
}

// mutation is one change to a valid sample that its filter must refuse.
type mutation struct {
	name   string
	mutate func(fixture)
}

// refusesEvery applies each mutation to a fresh copy of the sample, checks the
// mutation changed it, and requires the filter to refuse the result.
func refusesEvery(t *testing.T, sample func(*testing.T) fixture, accepts func(fixture) bool, mutations []mutation) {
	t.Helper()
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			baseline := sample(t)
			mutated := baseline.clone()
			test.mutate(mutated)
			if reflect.DeepEqual(mutated.doc, baseline.doc) {
				t.Fatal("the mutation did not change its valid baseline")
			}
			if accepts(mutated) {
				t.Fatal("the filter accepted a sample it has to refuse")
			}
		})
	}
}

// itemWith is the index of the list item whose operation label is operation.
func (f fixture) itemWith(operation string) int {
	f.t.Helper()
	for index := range f.at("items").([]any) {
		if f.at("items", index, "metadata", "labels", "operator.ptah.run/operation") == operation {
			return index
		}
	}
	f.t.Fatalf("no item runs %s", operation)
	return -1
}

func acceptsControllerJobs(f fixture) bool {
	return controllerJobIsolation(f.jobs(), "database-url", "registry-auth", true)
}

func TestControllerJobIsolationFilter(t *testing.T) {
	t.Parallel()
	if !acceptsControllerJobs(controllerJobFixture(t)) {
		t.Fatal("controller Job isolation refused the valid controller Job fixture")
	}
	withoutContainer := func(operation, container string) func(fixture) {
		return func(f fixture) {
			item := f.itemWith(operation)
			f.keep(notNamed(container), "items", item, "spec", "template", "spec", "containers")
			f.keep(notNamed(container), "items", item, "spec", "template", "spec", "initContainers")
		}
	}
	guardEnv := func(f fixture) []any {
		item := f.itemWith("observe")
		guard := f.named("validate-source-authority", "items", item, "spec", "template", "spec", "initContainers")
		return []any{"items", item, "spec", "template", "spec", "initContainers", guard, "env"}
	}
	withoutEngine := func(operation string) func(fixture) {
		return func(f fixture) {
			f.keep(notNamed("PTAH_EXPECTED_DATABASE_ENGINE"),
				"items", f.itemWith(operation), "spec", "template", "spec", "containers", 0, "env")
		}
	}
	refusesEvery(t, controllerJobFixture, acceptsControllerJobs, []mutation{
		{"resolve without ptah", withoutContainer("resolve", "ptah")},
		{"observe without validate-source-authority", withoutContainer("observe", "validate-source-authority")},
		{"observe without fetch-schema", withoutContainer("observe", "fetch-schema")},
		{"CA boundary missing-grant", func(f fixture) {
			f.keep(notNamed("PTAH_OPERATOR_OCI_CA_SHA256_GRANT"), guardEnv(f)...)
		}},
		{"CA boundary selectable-key", func(f fixture) {
			env := guardEnv(f)
			grant := f.named("PTAH_OPERATOR_OCI_CA_SHA256_GRANT", env...)
			f.set("schema-selected-key", append(env, grant, "valueFrom", "secretKeyRef", "key")...)
		}},
		{"CA boundary fetch-source-mount", func(f fixture) {
			item := f.itemWith("observe")
			fetch := f.named("fetch-schema", "items", item, "spec", "template", "spec", "initContainers")
			f.push(map[string]any{"name": "registry-ca", "mountPath": "/credentials/ca-source", "readOnly": true},
				"items", item, "spec", "template", "spec", "initContainers", fetch, "volumeMounts")
		}},
		{"Observe without the expected database engine", withoutEngine("observe")},
		{"Plan without the expected database engine", withoutEngine("plan")},
		{"missing pod replacement policy", func(f fixture) { f.del("items", 0, "spec", "podReplacementPolicy") }},
		{"terminating-or-failed pod replacement policy", func(f fixture) {
			f.set("TerminatingOrFailed", "items", 0, "spec", "podReplacementPolicy")
		}},
	})
}

func acceptsCustomCAPods(f fixture) bool {
	return customCAPodIsolation(f.pods(), customCAPodIsolationInputs{
		databaseSecret:    "database-url",
		registrySecret:    "registry-auth",
		registryAuthority: "registry.example",
		caConfigMap:       "registry-ca",
		resolvedReference: fixtureResolvedReference,
	})
}

func TestCustomCAPodIsolationFilter(t *testing.T) {
	t.Parallel()
	if !acceptsCustomCAPods(customCAPodFixture(t)) {
		t.Fatal("custom-CA Pod isolation refused the valid Pod fixture")
	}
	initContainer := func(f fixture, name string, rest ...any) []any {
		index := f.named(name, "items", 0, "spec", "initContainers")
		return append([]any{"items", 0, "spec", "initContainers", index}, rest...)
	}
	refusesEvery(t, customCAPodFixture, acceptsCustomCAPods, []mutation{
		{"guard-credentials", func(f fixture) {
			f.push(optionalSecretEnvFixture("PTAH_OCI_PASSWORD", "registry-auth", "password"),
				initContainer(f, "validate-source-authority", "env")...)
		}},
		{"fetch-source-ca", func(f fixture) {
			f.push(map[string]any{"name": "registry-ca", "mountPath": "/credentials/ca-source", "readOnly": true},
				initContainer(f, "fetch-schema", "volumeMounts")...)
		}},
		{"main-registry", func(f fixture) {
			f.push(literalEnvFixture("PTAH_OCI_REGISTRY", "registry.example"), "items", 0, "spec", "containers", 0, "env")
		}},
		{"init-order", func(f fixture) {
			initContainers := f.at("items", 0, "spec", "initContainers").([]any)
			f.set([]any{initContainers[1], initContainers[0], initContainers[2]}, "items", 0, "spec", "initContainers")
		}},
		{"guard-failed", func(f fixture) {
			index := f.named("validate-source-authority", "items", 0, "status", "initContainerStatuses")
			f.set(1, "items", 0, "status", "initContainerStatuses", index, "state", "terminated", "exitCode")
		}},
		{"pod-root", func(f fixture) { f.set(0, "items", 0, "spec", "securityContext", "runAsUser") }},
		{"missing-container-security", func(f fixture) {
			f.del("items", 0, "spec", "initContainers", 0, "securityContext")
		}},
		{"automount-enabled", func(f fixture) { f.set(true, "items", 0, "spec", "automountServiceAccountToken") }},
		{"guard-envfrom-registry", func(f fixture) {
			f.set([]any{map[string]any{"secretRef": map[string]any{"name": "registry-auth"}}},
				initContainer(f, "validate-source-authority", "envFrom")...)
		}},
		{"main-envfrom-database", func(f fixture) {
			f.set([]any{map[string]any{"secretRef": map[string]any{"name": "database-url"}}},
				"items", 0, "spec", "containers", 0, "envFrom")
		}},
		{"secret-volume", func(f fixture) {
			f.push(map[string]any{"name": "registry-secret", "secret": map[string]any{"secretName": "registry-auth"}},
				"items", 0, "spec", "volumes")
		}},
		{"projected-secret-volume", func(f fixture) {
			f.push(map[string]any{"name": "database-projection", "projected": map[string]any{"sources": []any{
				map[string]any{"secret": map[string]any{"name": "database-url"}},
			}}}, "items", 0, "spec", "volumes")
		}},
	})
}

const publisherImage = "e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// publisherJobFixture is the publisher Job hack/e2e-static.sh held
// publisher-job-isolation.jq to.
func publisherJobFixture(t *testing.T) fixture {
	t.Helper()
	return newFixture(t, map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"containers": []any{map[string]any{
			"name":  "publisher",
			"image": publisherImage,
			"env": []any{
				literalEnvFixture("HOME", "/work"),
				literalEnvFixture("TMPDIR", "/work"),
				secretEnvFixture("PTAH_OCI_USERNAME", "registry-auth", "username"),
				secretEnvFixture("PTAH_OCI_PASSWORD", "registry-auth", "password"),
				secretEnvFixture("PTAH_OCI_REGISTRY", "registry-auth", "registry"),
			},
		}},
	}}}})
}

func acceptsPublisherJob(f fixture) bool {
	return publisherJobIsolation(f.job(), publisherImage, "registry-auth")
}

func TestPublisherJobIsolationFilter(t *testing.T) {
	t.Parallel()
	if !acceptsPublisherJob(publisherJobFixture(t)) {
		t.Fatal("publisher isolation refused the valid publisher Job")
	}
	refusesEvery(t, publisherJobFixture, acceptsPublisherJob, []mutation{
		{"a development database credential", func(f fixture) {
			f.push(secretEnvFixture("PTAH_DEV_URL", "database-url", "url"),
				"spec", "template", "spec", "containers", 0, "env")
		}},
	})
}

const (
	endpointNamespace = "ptah-endpoint-test"
	endpointPodName   = "e2e-registry-tls-pod"
	endpointPodUID    = "11111111-2222-3333-4444-555555555555"
	endpointPodIP     = "10.0.0.8"
)

// endpointFixture is the EndpointSlice list hack/e2e-static.sh held
// tls-proxy-service-endpoints.jq to: the optional ready flag and API version
// left out, as the endpoint controller may.
func endpointFixture(t *testing.T) fixture {
	t.Helper()
	return newFixture(t, map[string]any{"items": []any{map[string]any{
		"addressType": "IPv4",
		"ports":       []any{map[string]any{"name": "tls", "protocol": "TCP", "port": 5443}},
		"endpoints": []any{map[string]any{
			"addresses":  []any{endpointPodIP},
			"conditions": map[string]any{},
			"targetRef": map[string]any{
				"kind": "Pod", "namespace": endpointNamespace, "name": endpointPodName, "uid": endpointPodUID,
			},
		}},
	}}})
}

func endpointsMatch(f fixture, podIP string) bool {
	return tlsProxyServiceEndpoints(f.endpointSlices(), endpointNamespace, endpointPodName, endpointPodUID, podIP)
}

func TestTLSProxyServiceEndpointsFilter(t *testing.T) {
	t.Parallel()
	if !endpointsMatch(endpointFixture(t), endpointPodIP) {
		t.Fatal("EndpointSlice predicate rejected omitted optional ready and API version fields")
	}
	explicit := endpointFixture(t)
	explicit.set(map[string]any{"ready": true, "serving": true, "terminating": false}, "items", 0, "endpoints", 0, "conditions")
	explicit.set("v1", "items", 0, "endpoints", 0, "targetRef", "apiVersion")
	if !endpointsMatch(explicit, endpointPodIP) {
		t.Fatal("EndpointSlice predicate rejected explicit valid endpoint fields")
	}
	ipv6 := endpointFixture(t)
	ipv6.set("IPv6", "items", 0, "addressType")
	ipv6.set([]any{"2001:db8::8"}, "items", 0, "endpoints", 0, "addresses")
	if !endpointsMatch(ipv6, "2001:db8::8") {
		t.Fatal("EndpointSlice predicate rejected an exact IPv6 route")
	}
	endpoint := []any{"items", 0, "endpoints", 0}
	at := func(rest ...any) []any { return append(append([]any{}, endpoint...), rest...) }
	refusesEvery(t, endpointFixture, func(f fixture) bool { return endpointsMatch(f, endpointPodIP) }, []mutation{
		{"extra unready endpoint", func(f fixture) {
			unready := f.clone()
			unready.set(false, at("conditions", "ready")...)
			f.push(unready.at(endpoint...), "items", 0, "endpoints")
		}},
		{"wrong address type", func(f fixture) { f.set("IPv6", "items", 0, "addressType") }},
		{"extra endpoint port", func(f fixture) {
			f.push(map[string]any{"name": "admin", "protocol": "TCP", "port": 8081}, "items", 0, "ports")
		}},
		{"wrong endpoint port name", func(f fixture) { f.set("admin", "items", 0, "ports", 0, "name") }},
		{"wrong endpoint port protocol", func(f fixture) { f.set("UDP", "items", 0, "ports", 0, "protocol") }},
		{"wrong endpoint port number", func(f fixture) { f.set(5444, "items", 0, "ports", 0, "port") }},
		{"not-ready endpoint", func(f fixture) { f.set(false, at("conditions", "ready")...) }},
		{"not-serving endpoint", func(f fixture) { f.set(false, at("conditions", "serving")...) }},
		{"terminating endpoint", func(f fixture) { f.set(true, at("conditions", "terminating")...) }},
		{"contradictory API version", func(f fixture) { f.set("apps/v1", at("targetRef", "apiVersion")...) }},
		{"wrong kind", func(f fixture) { f.set("Service", at("targetRef", "kind")...) }},
		{"missing namespace", func(f fixture) { f.del(at("targetRef", "namespace")...) }},
		{"wrong name", func(f fixture) { f.set("other-pod", at("targetRef", "name")...) }},
		{"wrong UID", func(f fixture) { f.set("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", at("targetRef", "uid")...) }},
		{"wrong Pod IP", func(f fixture) { f.set([]any{"10.0.0.9"}, at("addresses")...) }},
	})
}

// isoInstant is jq's todateiso8601: whole seconds in UTC.
func isoInstant(instant time.Time) string {
	return instant.UTC().Format("2006-01-02T15:04:05Z")
}

// approvalBoundaryFixture is the schema hack/e2e-static.sh held
// custom-ca-approval-boundary.jq to: a plan published and waiting for a
// person, with its refresh deadline an hour ahead.
func approvalBoundaryFixture(t *testing.T) fixture {
	t.Helper()
	return newFixture(t, map[string]any{
		"metadata": map[string]any{"generation": 7},
		"status": map[string]any{
			"observedGeneration":     7,
			"phase":                  "AwaitingApproval",
			"nextReconciliationTime": isoInstant(time.Now().Add(time.Hour)),
			"plan": map[string]any{
				"name":          "e2e-custom-ca-plan",
				"uid":           "11111111-2222-3333-4444-555555555555",
				"fingerprint":   "sha256:" + strings.Repeat("a", 64),
				"contentDigest": "sha256:" + strings.Repeat("b", 64),
			},
			"conditions": []any{
				map[string]any{"type": "PlanReady", "status": "True", "reason": "Published", "observedGeneration": 7},
				map[string]any{"type": "ApprovalRequired", "status": "True", "reason": "Waiting", "observedGeneration": 7},
			},
		},
	})
}

func acceptsApprovalBoundary(f fixture) bool {
	return customCAApprovalBoundary(f.schema(), time.Now())
}

func TestCustomCAApprovalBoundaryFilter(t *testing.T) {
	t.Parallel()
	if !acceptsApprovalBoundary(approvalBoundaryFixture(t)) {
		t.Fatal("custom-CA boundary rejected the durable Waiting state")
	}
	condition := func(f fixture, kind string, rest ...any) []any {
		for index, element := range f.at("status", "conditions").([]any) {
			if element.(map[string]any)["type"] == kind {
				return append([]any{"status", "conditions", index}, rest...)
			}
		}
		f.t.Fatalf("no %s condition", kind)
		return nil
	}
	refusesEvery(t, approvalBoundaryFixture, acceptsApprovalBoundary, []mutation{
		{"transient ApprovalRequired reason", func(f fixture) { f.set("PlanReady", condition(f, "ApprovalRequired", "reason")...) }},
		{"wrong phase", func(f fixture) { f.set("ReadyToApply", "status", "phase") }},
		{"stale observed generation", func(f fixture) { f.set(8, "metadata", "generation") }},
		{"missing metadata generation", func(f fixture) { f.del("metadata", "generation") }},
		{"active operation", func(f fixture) {
			f.set(map[string]any{"type": "Apply", "id": "unexpected"}, "status", "activeOperation")
		}},
		{"missing refresh deadline", func(f fixture) { f.del("status", "nextReconciliationTime") }},
		{"expired refresh deadline", func(f fixture) {
			f.set(isoInstant(time.Now().Add(-time.Minute)), "status", "nextReconciliationTime")
		}},
		{"missing current plan", func(f fixture) { f.del("status", "plan") }},
		{"empty plan name", func(f fixture) { f.set("", "status", "plan", "name") }},
		{"empty plan UID", func(f fixture) { f.set("", "status", "plan", "uid") }},
		{"malformed plan fingerprint", func(f fixture) { f.set("not-a-digest", "status", "plan", "fingerprint") }},
		{"malformed plan content digest", func(f fixture) { f.set("sha256:short", "status", "plan", "contentDigest") }},
		{"consumed approval", func(f fixture) {
			f.set(map[string]any{"name": "consumed", "uid": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}, "status", "plan", "approval")
		}},
		{"wrong ApprovalRequired reason", func(f fixture) { f.set("NotRequired", condition(f, "ApprovalRequired", "reason")...) }},
		{"wrong ApprovalRequired status", func(f fixture) { f.set("False", condition(f, "ApprovalRequired", "status")...) }},
		{"stale ApprovalRequired condition", func(f fixture) {
			f.set(6, condition(f, "ApprovalRequired", "observedGeneration")...)
		}},
		{"missing PlanReady condition", func(f fixture) {
			f.keep(func(element map[string]any) bool { return element["type"] != "PlanReady" }, "status", "conditions")
		}},
		{"wrong PlanReady status", func(f fixture) { f.set("False", condition(f, "PlanReady", "status")...) }},
		{"wrong PlanReady reason", func(f fixture) { f.set("Stale", condition(f, "PlanReady", "reason")...) }},
		{"stale PlanReady condition", func(f fixture) { f.set(6, condition(f, "PlanReady", "observedGeneration")...) }},
	})

	// jq refused a deadline it could not parse. A typed read cannot even hold
	// one: the API server stores the field as a timestamp, and the document is
	// refused where it is decoded, before the predicate sees it.
	malformed := approvalBoundaryFixture(t)
	malformed.set("not-a-timestamp", "status", "nextReconciliationTime")
	if err := malformed.decode(&ptahv1alpha1.PtahSchema{}); err == nil {
		t.Fatal("a malformed refresh deadline decoded into a schema")
	}
}

func acceptsSourceJobs(authMode string) func(fixture) bool {
	return func(f fixture) bool { return sourceJobIsolation(f.jobs(), sourceIsolationInputs(authMode)) }
}

// sourceSpec is the path to one Job's Pod template spec.
func sourceSpec(item int, rest ...any) []any {
	return append([]any{"items", item, "spec", "template", "spec"}, rest...)
}

// sourceEnv is the path to one variable of one Job's ptah container, and
// rest below it.
func (f fixture) sourceEnv(item int, name string, rest ...any) []any {
	f.t.Helper()
	env := sourceSpec(item, "containers", 0, "env")
	return append(append(env, f.named(name, env...)), rest...)
}

// sourceVolume and sourceMount are the paths to the Docker config Secret
// volume and its mount in the first Job.
func (f fixture) sourceVolume(rest ...any) []any {
	f.t.Helper()
	volumes := sourceSpec(0, "volumes")
	return append(append(volumes, f.named("registry-docker-config", volumes...)), rest...)
}

func (f fixture) sourceMount(rest ...any) []any {
	f.t.Helper()
	mounts := sourceSpec(0, "containers", 0, "volumeMounts")
	return append(append(mounts, f.named("registry-docker-config", mounts...)), rest...)
}

func TestSourceJobIsolationFilter(t *testing.T) {
	t.Parallel()
	for _, authMode := range []string{"Environment", "DockerConfigJSON"} {
		if !acceptsSourceJobs(authMode)(sourceJobFixture(t, authMode)) {
			t.Fatalf("source isolation rejected the valid %s fixture", authMode)
		}
	}
	operationURL := "postgres://user:password@database.example/orders"
	environment := []mutation{
		{"missing Verify Job", func(f fixture) { f.del("items", 1) }},
		{"source Job that names no runner protocol", func(f fixture) {
			f.keep(notNamed("PTAH_RUNNER_PROTOCOL_VERSION"), sourceSpec(0, "containers", 0, "env")...)
		}},
		{"runner protocol read from a Secret", func(f fixture) {
			f.set(secretEnvFixture("PTAH_RUNNER_PROTOCOL_VERSION", "registry-auth", "protocol"),
				f.sourceEnv(0, "PTAH_RUNNER_PROTOCOL_VERSION")...)
		}},
		{"runner protocol that is not a version", func(f fixture) {
			f.set("05", f.sourceEnv(0, "PTAH_RUNNER_PROTOCOL_VERSION", "value")...)
		}},
		{"extra source Job", func(f fixture) { f.push(f.at("items", 1), "items") }},
		{"duplicate Resolve operation", func(f fixture) {
			f.set("resolve", "items", 1, "metadata", "labels", "operator.ptah.run/operation")
		}},
		{"unknown source operation", func(f fixture) {
			f.set("fetch", "items", 1, "metadata", "labels", "operator.ptah.run/operation")
		}},
		{"nonzero Job backoff", func(f fixture) { f.set(1, "items", 0, "spec", "backoffLimit") }},
		{"unsafe Pod replacement policy", func(f fixture) {
			f.set("TerminatingOrFailed", "items", 0, "spec", "podReplacementPolicy")
		}},
		{"restartable source Pod", func(f fixture) { f.set("OnFailure", sourceSpec(0, "restartPolicy")...) }},
		{"automounted service-account token", func(f fixture) {
			f.set(true, sourceSpec(0, "automountServiceAccountToken")...)
		}},
		{"enabled service links", func(f fixture) { f.set(true, sourceSpec(0, "enableServiceLinks")...) }},
		{"unexpected source service account", func(f fixture) {
			f.set("credential-bearing", sourceSpec(0, "serviceAccountName")...)
		}},
		{"source Job without a service account", func(f fixture) { f.del(sourceSpec(0, "serviceAccountName")...) }},
		{"database image-pull Secret", func(f fixture) {
			f.set([]any{map[string]any{"name": "database-url"}}, sourceSpec(0, "imagePullSecrets")...)
		}},
		{"unconfined source Pod", func(f fixture) {
			f.set("Unconfined", sourceSpec(0, "securityContext", "seccompProfile", "type")...)
		}},
		{"extra ptah main container", func(f fixture) {
			f.push(f.at(sourceSpec(0, "containers", 0)...), sourceSpec(0, "containers")...)
		}},
		{"ptah-named init container", func(f fixture) { f.set("ptah", sourceSpec(0, "initContainers", 0, "name")...) }},
		{"missing ptah main container", func(f fixture) { f.set([]any{}, sourceSpec(0, "containers")...) }},
		{"missing runner installer", func(f fixture) { f.set([]any{}, sourceSpec(0, "initContainers")...) }},
		{"extra neutral init container", func(f fixture) {
			f.push(map[string]any{"name": "neutral-init", "env": []any{}, "volumeMounts": []any{}},
				sourceSpec(0, "initContainers")...)
		}},
		{"neutral ephemeral container", func(f fixture) {
			f.set([]any{map[string]any{"name": "neutral-debug", "env": []any{}, "volumeMounts": []any{}}},
				sourceSpec(0, "ephemeralContainers")...)
		}},
		{"attacker executor image", func(f fixture) {
			f.set("attacker.invalid/tool:latest", sourceSpec(0, "containers", 0, "image")...)
		}},
		{"attacker executor command", func(f fixture) {
			f.set([]any{"/bin/sh", "-c"}, sourceSpec(0, "containers", 0, "command")...)
		}},
		{"privileged executor context", func(f fixture) {
			f.set(true, sourceSpec(0, "containers", 0, "securityContext", "allowPrivilegeEscalation")...)
		}},
		{"mismatched operation arguments", func(f fixture) {
			f.set("apply", sourceSpec(0, "containers", 0, "args", -1)...)
		}},
		{"attacker runner-installer image", func(f fixture) {
			f.set("attacker.invalid/tool:latest", sourceSpec(0, "initContainers", 0, "image")...)
		}},
		{"database Secret in init container", func(f fixture) {
			f.push(secretEnvFixture("PTAH_DB_URL", "database-url", "url"), sourceSpec(0, "initContainers", 0, "env")...)
		}},
		{"unrelated init-container environment", func(f fixture) {
			f.push(literalEnvFixture("UNEXPECTED", "present"), sourceSpec(0, "initContainers", 0, "env")...)
		}},
		{"database Secret volume", func(f fixture) {
			f.push(map[string]any{"name": "database", "secret": map[string]any{"secretName": "database-url"}},
				sourceSpec(0, "volumes")...)
		}},
		{"registry Secret volume in Environment mode", func(f fixture) {
			f.push(map[string]any{"name": "registry-copy", "secret": map[string]any{"secretName": "registry-auth"}},
				sourceSpec(0, "volumes")...)
		}},
		{"projected database Secret volume", func(f fixture) {
			f.push(map[string]any{"name": "projected-database", "projected": map[string]any{"sources": []any{
				map[string]any{"secret": map[string]any{"name": "database-url"}},
			}}}, sourceSpec(0, "volumes")...)
		}},
		{"projected service-account token", func(f fixture) {
			f.push(map[string]any{"name": "service-account-token", "projected": map[string]any{"sources": []any{
				map[string]any{"serviceAccountToken": map[string]any{"path": "token", "expirationSeconds": 3600}},
			}}}, sourceSpec(0, "volumes")...)
			f.push(map[string]any{"name": "service-account-token", "mountPath": "/var/run/secrets/tokens"},
				sourceSpec(0, "initContainers", 0, "volumeMounts")...)
		}},
		{"CSI database Secret volume", func(f fixture) {
			f.push(map[string]any{"name": "database-csi", "csi": map[string]any{
				"driver": "secrets.example.invalid", "nodePublishSecretRef": map[string]any{"name": "database-url"},
			}}, sourceSpec(0, "volumes")...)
			f.push(map[string]any{"name": "database-csi", "mountPath": "/credentials/database", "readOnly": true},
				sourceSpec(0, "containers", 0, "volumeMounts")...)
		}},
		{"unexpected source volume and mount", func(f fixture) {
			f.push(map[string]any{"name": "unexpected", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "1Mi"}},
				sourceSpec(0, "volumes")...)
			f.push(map[string]any{"name": "unexpected", "mountPath": "/unexpected"},
				sourceSpec(0, "containers", 0, "volumeMounts")...)
		}},
		{"container envFrom", func(f fixture) {
			f.set([]any{map[string]any{"secretRef": map[string]any{"name": "registry-auth"}}},
				sourceSpec(0, "initContainers", 0, "envFrom")...)
		}},
		{"wrong registry authority", func(f fixture) {
			f.set("other.example:5000", f.sourceEnv(0, "PTAH_OCI_REGISTRY", "value")...)
		}},
		{"database URL in operation ID", func(f fixture) {
			f.set(operationURL, f.sourceEnv(0, "PTAH_OPERATION_ID", "value")...)
		}},
		{"self-consistent database URL operation ID", func(f fixture) {
			f.set(operationURL, "items", 0, "metadata", "annotations", "operator.ptah.run/operation-id")
			f.set(operationURL, f.sourceEnv(0, "PTAH_OPERATION_ID", "value")...)
		}},
		{"missing operation-ID binding", func(f fixture) {
			f.del("items", 0, "metadata", "annotations", "operator.ptah.run/operation-id")
			f.del(f.sourceEnv(0, "PTAH_OPERATION_ID", "value")...)
		}},
		{"empty operation-ID binding", func(f fixture) {
			f.set("", "items", 0, "metadata", "annotations", "operator.ptah.run/operation-id")
			f.set("", f.sourceEnv(0, "PTAH_OPERATION_ID", "value")...)
		}},
		{"wrong requested reference", func(f fixture) {
			f.set("oci://attacker.example/schema:latest", f.sourceEnv(0, "PTAH_REQUESTED_REFERENCE", "value")...)
		}},
		{"wrong source working home", func(f fixture) { f.set("/credentials", f.sourceEnv(0, "HOME", "value")...) }},
		{"wrong authority grant key", func(f fixture) {
			f.set("authority", f.sourceEnv(0, "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT", "valueFrom", "secretKeyRef", "key")...)
		}},
		{"explicit optional false on authority grant", func(f fixture) {
			f.set(false, f.sourceEnv(0, "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT", "valueFrom", "secretKeyRef", "optional")...)
		}},
		{"wrong plain-HTTP grant", func(f fixture) {
			f.set("plainHTTP", f.sourceEnv(0, "PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP", "valueFrom", "secretKeyRef", "key")...)
		}},
		{"wrong plain-HTTP literal", func(f fixture) { f.set("false", f.sourceEnv(0, "PTAH_PLAIN_HTTP", "value")...) }},
		{"auth-mode mismatch", func(f fixture) {
			f.set("DockerConfigJSON", f.sourceEnv(0, "PTAH_OPERATOR_OCI_AUTH_MODE", "value")...)
		}},
		{"unexpected ConfigMap environment", func(f fixture) {
			f.push(map[string]any{"name": "UNEXPECTED_CONFIG", "valueFrom": map[string]any{
				"configMapKeyRef": map[string]any{"name": "unexpected", "key": "value"},
			}}, sourceSpec(0, "containers", 0, "env")...)
		}},
		{"required Environment username", func(f fixture) {
			f.set(false, f.sourceEnv(0, "PTAH_OCI_USERNAME", "valueFrom", "secretKeyRef", "optional")...)
		}},
		{"wrong Environment password Secret", func(f fixture) {
			f.set("other-registry-auth", f.sourceEnv(0, "PTAH_OCI_PASSWORD", "valueFrom", "secretKeyRef", "name")...)
		}},
		{"wrong Environment token key", func(f fixture) {
			f.set("accessToken", f.sourceEnv(0, "PTAH_OCI_TOKEN", "valueFrom", "secretKeyRef", "key")...)
		}},
		{"mixed Environment and Docker config auth", func(f fixture) {
			f.push(literalEnvFixture("DOCKER_CONFIG", "/credentials/docker"), sourceSpec(0, "containers", 0, "env")...)
		}},
	}
	docker := []mutation{
		{"registry host alias", func(f fixture) {
			f.set([]any{map[string]any{"ip": "203.0.113.10", "hostnames": []any{"registry.example"}}},
				sourceSpec(0, "hostAliases")...)
		}},
		{"custom registry DNS", func(f fixture) {
			f.set("None", sourceSpec(0, "dnsPolicy")...)
			f.set(map[string]any{"nameservers": []any{"203.0.113.53"}}, sourceSpec(0, "dnsConfig")...)
		}},
		{"host-networked source Pod", func(f fixture) { f.set(true, sourceSpec(0, "hostNetwork")...) }},
		{"credential termination-message path", func(f fixture) {
			f.set("/credentials/docker/config.json", sourceSpec(0, "containers", 0, "terminationMessagePath")...)
		}},
		{"executor lifecycle hook", func(f fixture) {
			f.set(map[string]any{"postStart": map[string]any{"exec": map[string]any{"command": []any{"/bin/sh", "-c", "exit 0"}}}},
				sourceSpec(0, "containers", 0, "lifecycle")...)
		}},
		{"executor probe command", func(f fixture) {
			f.set(map[string]any{"exec": map[string]any{"command": []any{"/bin/sh", "-c", "exit 0"}}},
				sourceSpec(0, "containers", 0, "livenessProbe")...)
		}},
		{"wrong resolved reference", func(f fixture) {
			f.set("oci://attacker.example/schema@sha256:"+strings.Repeat("b", 64),
				f.sourceEnv(1, "PTAH_RESOLVED_REFERENCE", "value")...)
		}},
		{"wrong verification-policy path", func(f fixture) {
			f.set("/unexpected/policy.yaml", f.sourceEnv(1, "PTAH_VERIFICATION_POLICY", "value")...)
		}},
		{"wrong expected artifact type", func(f fixture) {
			f.set("application/octet-stream", f.sourceEnv(1, "PTAH_EXPECTED_ARTIFACT_TYPE", "value")...)
		}},
		{"unexpected source literal", func(f fixture) {
			f.push(literalEnvFixture("PTAH_OCI_CA_FILE", "/unexpected"), sourceSpec(0, "containers", 0, "env")...)
		}},
		{"mixed Docker config and Environment auth", func(f fixture) {
			f.push(optionalSecretEnvFixture("PTAH_OCI_USERNAME", "registry-auth", "username"),
				sourceSpec(0, "containers", 0, "env")...)
		}},
		{"wrong DOCKER_CONFIG value", func(f fixture) {
			f.set("/var/lib/docker", f.sourceEnv(0, "DOCKER_CONFIG", "value")...)
		}},
		{"wrong Docker config Secret volume", func(f fixture) {
			f.set("other-registry-auth", f.sourceVolume("secret", "secretName")...)
		}},
		{"optional Docker config Secret volume", func(f fixture) { f.set(true, f.sourceVolume("secret", "optional")...) }},
		{"explicit required Docker config Secret volume", func(f fixture) {
			f.set(false, f.sourceVolume("secret", "optional")...)
		}},
		{"missing Docker config Secret volume", func(f fixture) {
			f.keep(notNamed("registry-docker-config"), sourceSpec(0, "volumes")...)
		}},
		{"wrong Docker config item", func(f fixture) {
			f.set("docker.json", f.sourceVolume("secret", "items", 0, "path")...)
		}},
		{"wrong Docker config item key", func(f fixture) {
			f.set("dockerconfigjson", f.sourceVolume("secret", "items", 0, "key")...)
		}},
		{"wrong Docker config item mode", func(f fixture) { f.set(256, f.sourceVolume("secret", "items", 0, "mode")...) }},
		{"wrong Docker config default mode", func(f fixture) { f.set(384, f.sourceVolume("secret", "defaultMode")...) }},
		{"wrong Docker config mount path", func(f fixture) { f.set("/var/lib/docker", f.sourceMount("mountPath")...) }},
		{"writable Docker config mount", func(f fixture) { f.set(false, f.sourceMount("readOnly")...) }},
		{"missing Docker config mount", func(f fixture) { f.set([]any{}, sourceSpec(0, "containers", 0, "volumeMounts")...) }},
		{"second Docker config mount", func(f fixture) {
			f.push(map[string]any{"name": "registry-docker-config", "mountPath": "/credentials/docker-copy", "readOnly": true},
				sourceSpec(0, "containers", 0, "volumeMounts")...)
		}},
		{"Docker config mounted by runner installer", func(f fixture) {
			f.push(map[string]any{"name": "registry-docker-config", "mountPath": "/credentials/docker", "readOnly": true},
				sourceSpec(0, "initContainers", 0, "volumeMounts")...)
		}},
		{"Docker config mount subPath", func(f fixture) { f.set("config.json", f.sourceMount("subPath")...) }},
		{"Docker config mount subPathExpr", func(f fixture) { f.set("$(POD_NAME)", f.sourceMount("subPathExpr")...) }},
		{"Docker config mount propagation", func(f fixture) {
			f.set("HostToContainer", f.sourceMount("mountPropagation")...)
		}},
		{"Verify policy mount missing", func(f fixture) {
			f.keep(notNamed("verification-policy"), sourceSpec(1, "containers", 0, "volumeMounts")...)
		}},
		{"Verify policy volume missing", func(f fixture) {
			f.keep(notNamed("verification-policy"), sourceSpec(1, "volumes")...)
		}},
		{"operation labels and env swapped", func(f fixture) {
			resolveEnv, verifyEnv := f.at(sourceSpec(0, "containers", 0, "env")...), f.at(sourceSpec(1, "containers", 0, "env")...)
			f.set("verify", "items", 0, "metadata", "labels", "operator.ptah.run/operation")
			f.set("resolve", "items", 1, "metadata", "labels", "operator.ptah.run/operation")
			f.set(verifyEnv, sourceSpec(0, "containers", 0, "env")...)
			f.set(resolveEnv, sourceSpec(1, "containers", 0, "env")...)
		}},
	}
	t.Run("Environment", func(t *testing.T) {
		t.Parallel()
		refusesEvery(t, func(t *testing.T) fixture { return sourceJobFixture(t, "Environment") },
			acceptsSourceJobs("Environment"), environment)
	})
	t.Run("DockerConfigJSON", func(t *testing.T) {
		t.Parallel()
		refusesEvery(t, func(t *testing.T) fixture { return sourceJobFixture(t, "DockerConfigJSON") },
			acceptsSourceJobs("DockerConfigJSON"), docker)
	})
}

// gateSample is one reading the filter self-test judged
// an approval gate by, with whether the gate has to accept it.
type gateSample struct {
	name     string
	accepted bool
	document string
}

// The privileged gate's readings. The accepted one carries the message the
// controller writes, word for word. The refused ones are a controller that
// waits without saying why, one that applies without a person, a plan that
// names no kind or the wrong one, a message that quotes the statement, and
// the readings a gate could match mid-cycle or for the wrong artifact.
var privilegedGateSamples = []gateSample{
	{"held for a person, settled between cycles", true, `{"metadata":{"generation":4},"status":{"observedGeneration":4,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:08:00Z","plan":{"name":"ptah-plan-0","uid":"u-plan","destructive":false,"privilegeChanges":["SecurityDefiner"]},"conditions":[{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (SecurityDefiner), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"waiting for the generic reason", false, `{"metadata":{"generation":4},"status":{"observedGeneration":4,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:08:00Z","plan":{"name":"ptah-plan-0","uid":"u-plan","destructive":false,"privilegeChanges":["SecurityDefiner"]},"conditions":[{"type":"ApprovalRequired","status":"True","reason":"Waiting","message":"The plan changes privileges (SecurityDefiner), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"applied without a person", false, `{"metadata":{"generation":4},"status":{"observedGeneration":4,"phase":"ReadyToApply","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:08:00Z","plan":{"name":"ptah-plan-0","uid":"u-plan","destructive":false,"privilegeChanges":["SecurityDefiner"]},"conditions":[{"type":"ApprovalRequired","status":"False","reason":"NotRequired","message":"The plan changes privileges (SecurityDefiner), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"a plan that names no kind", false, `{"metadata":{"generation":4},"status":{"observedGeneration":4,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:08:00Z","plan":{"name":"ptah-plan-0","uid":"u-plan","destructive":false},"conditions":[{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (SecurityDefiner), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"a plan that names the wrong kind", false, `{"metadata":{"generation":4},"status":{"observedGeneration":4,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:08:00Z","plan":{"name":"ptah-plan-0","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"]},"conditions":[{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (SecurityDefiner), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"a message that does not name the kinds", false, `{"metadata":{"generation":4},"status":{"observedGeneration":4,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:08:00Z","plan":{"name":"ptah-plan-0","uid":"u-plan","destructive":false,"privilegeChanges":["SecurityDefiner"]},"conditions":[{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"Create an approval bound to the current plan fingerprint"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"a message that quotes the statement", false, `{"metadata":{"generation":4},"status":{"observedGeneration":4,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:08:00Z","plan":{"name":"ptah-plan-0","uid":"u-plan","destructive":false,"privilegeChanges":["SecurityDefiner"]},"conditions":[{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (SecurityDefiner), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan: CREATE FUNCTION \"e2e_widget_count\"() SECURITY DEFINER"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"another condition that names the table", false, `{"metadata":{"generation":4},"status":{"observedGeneration":4,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:08:00Z","plan":{"name":"ptah-plan-0","uid":"u-plan","destructive":false,"privilegeChanges":["SecurityDefiner"]},"conditions":[{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (SecurityDefiner), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan for e2e_widgets is waiting"}]}}`},
	{"an approval already recorded", false, `{"metadata":{"generation":4},"status":{"observedGeneration":4,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:08:00Z","plan":{"name":"ptah-plan-0","uid":"u-plan","destructive":false,"privilegeChanges":["SecurityDefiner"],"approval":{"name":"a","uid":"u-approval"}},"conditions":[{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (SecurityDefiner), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"an operation claimed", false, `{"metadata":{"generation":4},"status":{"observedGeneration":4,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:08:00Z","plan":{"name":"ptah-plan-0","uid":"u-plan","destructive":false,"privilegeChanges":["SecurityDefiner"]},"conditions":[{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (SecurityDefiner), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}],"activeOperation":{"type":"Apply","jobUID":"u-apply"}}}`},
	{"another digest", false, `{"metadata":{"generation":4},"status":{"observedGeneration":4,"phase":"AwaitingApproval","source":{"digest":"sha256:ee"},"nextReconciliationTime":"2026-09-26T10:08:00Z","plan":{"name":"ptah-plan-0","uid":"u-plan","destructive":false,"privilegeChanges":["SecurityDefiner"]},"conditions":[{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (SecurityDefiner), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"an older generation observed", false, `{"metadata":{"generation":4},"status":{"observedGeneration":3,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:08:00Z","plan":{"name":"ptah-plan-0","uid":"u-plan","destructive":false,"privilegeChanges":["SecurityDefiner"]},"conditions":[{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (SecurityDefiner), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"a destructive plan", false, `{"metadata":{"generation":4},"status":{"observedGeneration":4,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:08:00Z","plan":{"name":"ptah-plan-0","uid":"u-plan","destructive":true,"privilegeChanges":["SecurityDefiner"]},"conditions":[{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (SecurityDefiner), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"no refresh deadline persisted", false, `{"metadata":{"generation":4},"status":{"observedGeneration":4,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"plan":{"name":"ptah-plan-0","uid":"u-plan","destructive":false,"privilegeChanges":["SecurityDefiner"]},"conditions":[{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (SecurityDefiner), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
}

// The grant-only gate's readings. The accepted one carries the messages the
// controller writes, word for word, and an observation that found drift in no
// category. The refused ones are an observation that saw nothing or counted
// something, a scoped plan that found nothing, a plan that lost or widened its
// kind, a controller that waits without saying why or applies without a
// person, a status that quotes the statement, and the readings a gate could
// match mid-cycle or for the wrong artifact.
var grantGateSamples = []gateSample{
	{"observed as drift in no category, held for a person", true, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe"},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"]},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"an observation that found no drift", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z"},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"]},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"an observation that counted a finding", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe","driftFindingCount":1},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"]},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"an observation that listed a finding", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe","driftFindings":[{"category":"tables_added","count":1,"severity":"safe"}]},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"]},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"no observation recorded", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"]},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"a scoped plan that found nothing", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe"},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"]},"conditions":[{"type":"DriftDetected","status":"False","reason":"ScopedConverged","message":"The authoritative managed scope has no changes"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"a plan that names no kind", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe"},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"a plan that names another kind too", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe"},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant","Ownership"]},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"waiting for the generic reason", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe"},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"]},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PlanReady","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"applied without a person", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"ReadyToApply","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe"},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"]},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"False","reason":"NotRequired","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"a message that quotes the statement", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe"},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"]},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan: GRANT SELECT ON TABLE \"e2e_widgets\" TO PUBLIC"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"another condition that names the grantee", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe"},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"]},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan granting PUBLIC is waiting"}]}}`},
	{"an approval already recorded", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe"},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"],"approval":{"name":"a","uid":"u-approval"}},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"an operation claimed", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe"},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"]},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}],"activeOperation":{"type":"Apply","jobUID":"u-apply"}}}`},
	{"another digest", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ee"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe"},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"]},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"an older generation observed", false, `{"metadata":{"generation":5},"status":{"observedGeneration":4,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe"},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":false,"privilegeChanges":["Grant"]},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
	{"a destructive plan", false, `{"metadata":{"generation":5},"status":{"observedGeneration":5,"phase":"AwaitingApproval","source":{"digest":"sha256:ff"},"nextReconciliationTime":"2026-09-26T10:38:00Z","target":{"engine":"PostgreSQL","coordinationDigest":"sha256:cc","identityDigest":"sha256:dd","driftReportDigest":"sha256:ee","lastObservedAt":"2026-09-26T10:07:00Z","highestDriftSeverity":"safe"},"plan":{"name":"ptah-plan-1","uid":"u-plan","destructive":true,"privilegeChanges":["Grant"]},"conditions":[{"type":"DriftDetected","status":"True","reason":"ScopedChanges","message":"Managed scope requires 1 schema statements; highest severity safe"},{"type":"ApprovalRequired","status":"True","reason":"PrivilegeChanges","message":"The plan changes privileges (Grant), which apply policy Always applies only with an approval bound to this plan; read them with kubectl ptah plan"},{"type":"Ready","status":"False","reason":"AwaitingApproval","message":"Plan is waiting for approval"}]}}`},
}

// The digest every gate sample was judged against.
const gateSampleDigest = "sha256:ff"

func judgeGateSamples(t *testing.T, samples []gateSample, gate func(*ptahv1alpha1.PtahSchema, string) bool) {
	t.Helper()
	refused := 0
	for _, sample := range samples {
		if !sample.accepted {
			refused++
		}
		t.Run(sample.name, func(t *testing.T) {
			t.Parallel()
			if got := gate(parseFixture(t, sample.document).schema(), gateSampleDigest); got != sample.accepted {
				t.Fatalf("the gate returned %v for a reading it has to %s", got, map[bool]string{true: "accept", false: "refuse"}[sample.accepted])
			}
		})
	}
	if refused == 0 || refused == len(samples) {
		t.Fatalf("%d of %d readings are refusals; a gate needs one it accepts and one it refuses", refused, len(samples))
	}
}

func TestPrivilegedApprovalGateFilter(t *testing.T) {
	t.Parallel()
	judgeGateSamples(t, privilegedGateSamples, privilegedApprovalGate)
}

func TestGrantOnlyApprovalGateFilter(t *testing.T) {
	t.Parallel()
	judgeGateSamples(t, grantGateSamples, grantOnlyApprovalGate)
}
