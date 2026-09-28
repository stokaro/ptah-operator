package e2e

import (
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The predicates below were the data plane's filters that earned a file of
// their own under testdata/e2e when the phase was a shell script. Each is the
// same predicate over the typed objects the API returns, and each is held by
// a unit test to the readings it must accept and the mistakes it must refuse.
// Only the publisher's filter still has a jq copy, because the migration
// phase, still a script, reads it.
//
// jq tells an absent field from an empty one, and a typed object cannot: an
// EnvVar's value, a reference's name, a Job's annotation all decode to "" when
// the API left them out. The API server leaves out exactly those empty values
// (every such field is omitempty in its JSON form), so "" here stands for the
// null jq compared with. Where a filter compared a whole object with a literal
// -- a security context, a volume, a mount -- the object is encoded back to
// JSON and compared as a document, which keeps jq's rule that an extra field,
// even one set to false, is a different object.

// renderedDocument is value encoded and decoded back into plain JSON values, the
// form jq compared.
func renderedDocument(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		return nil
	}
	return document
}

// literalDocument is a JSON literal the predicates compare with.
func literalDocument(literal string) any {
	var document any
	if err := json.Unmarshal([]byte(literal), &document); err != nil {
		panic("dataplane filters: malformed literal " + literal)
	}
	return document
}

// renderedEqual compares value, as the API would render it, with a JSON document.
func renderedEqual(value, want any) bool {
	return reflect.DeepEqual(renderedDocument(value), want)
}

// renderedKeys is the sorted keys value renders with, or nil when it does not
// render as an object.
func renderedKeys(value any) []string {
	object, ok := renderedDocument(value).(map[string]any)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// containerView is what the filters read of a container, whichever list it
// is declared in.
type containerView struct {
	name    string
	env     []corev1.EnvVar
	envFrom []corev1.EnvFromSource
	mounts  []corev1.VolumeMount
}

func viewContainers(containers []corev1.Container) []containerView {
	views := make([]containerView, 0, len(containers))
	for _, container := range containers {
		views = append(views, containerView{
			name: container.Name, env: container.Env, envFrom: container.EnvFrom, mounts: container.VolumeMounts,
		})
	}
	return views
}

func viewEphemeralContainers(containers []corev1.EphemeralContainer) []containerView {
	views := make([]containerView, 0, len(containers))
	for _, container := range containers {
		views = append(views, containerView{
			name: container.Name, env: container.Env, envFrom: container.EnvFrom, mounts: container.VolumeMounts,
		})
	}
	return views
}

func (c containerView) envNames() []string {
	names := make([]string, 0, len(c.env))
	for _, variable := range c.env {
		names = append(names, variable.Name)
	}
	return names
}

func (c containerView) envNamed(name string) []corev1.EnvVar {
	var matches []corev1.EnvVar
	for _, variable := range c.env {
		if variable.Name == name {
			matches = append(matches, variable)
		}
	}
	return matches
}

func (c containerView) hasEnvName(name string) bool {
	return slices.Contains(c.envNames(), name)
}

// envSecretRef is the Secret key an environment variable reads, or nil.
func envSecretRef(variable corev1.EnvVar) *corev1.SecretKeySelector {
	if variable.ValueFrom == nil {
		return nil
	}
	return variable.ValueFrom.SecretKeyRef
}

// secretNames are the Secrets the container's environment names.
func (c containerView) secretNames() []string {
	var names []string
	for _, variable := range c.env {
		if reference := envSecretRef(variable); reference != nil && reference.Name != "" {
			names = append(names, reference.Name)
		}
	}
	return names
}

// secretEnvNames are the variables that read a Secret.
func (c containerView) secretEnvNames() []string {
	var names []string
	for _, variable := range c.env {
		if reference := envSecretRef(variable); reference != nil && reference.Name != "" {
			names = append(names, variable.Name)
		}
	}
	return names
}

// hasLiteralEnv is a variable with the literal value and no source.
func (c containerView) hasLiteralEnv(name, value string) bool {
	for _, variable := range c.env {
		if variable.Name == name && variable.Value == value && variable.ValueFrom == nil {
			return true
		}
	}
	return false
}

func optionalIs(reference *corev1.SecretKeySelector, optional bool) bool {
	if optional {
		return reference.Optional != nil && *reference.Optional
	}
	return reference.Optional == nil || !*reference.Optional
}

// hasSecretEnv is a variable with no literal value that reads the key of the
// Secret, required or optional as asked.
func (c containerView) hasSecretEnv(name, secret, key string, optional bool) bool {
	for _, variable := range c.env {
		reference := envSecretRef(variable)
		if variable.Name == name && variable.Value == "" && reference != nil && reference.Name == secret &&
			reference.Key == key && optionalIs(reference, optional) {
			return true
		}
	}
	return false
}

// hasSecretEnvAnyKey is a variable with no literal value that reads the Secret,
// whatever key.
func (c containerView) hasSecretEnvAnyKey(name, secret string) bool {
	for _, variable := range c.env {
		reference := envSecretRef(variable)
		if variable.Name == name && variable.Value == "" && reference != nil && reference.Name == secret {
			return true
		}
	}
	return false
}

func (c containerView) hasMount(name string, readOnly bool) bool {
	for _, mount := range c.mounts {
		if mount.Name == name && mount.ReadOnly == readOnly {
			return true
		}
	}
	return false
}

func (c containerView) mountsNamed(name string) []corev1.VolumeMount {
	var matches []corev1.VolumeMount
	for _, mount := range c.mounts {
		if mount.Name == name {
			matches = append(matches, mount)
		}
	}
	return matches
}

func (c containerView) envNameWith(match func(string) bool) bool {
	return slices.ContainsFunc(c.envNames(), match)
}

func registryEnvName(name string) bool {
	return strings.HasPrefix(name, "PTAH_OCI_") || strings.HasPrefix(name, "PTAH_OPERATOR_OCI_") ||
		name == "DOCKER_CONFIG" || name == "PTAH_PLAIN_HTTP"
}

func containersNamed(containers []containerView, name string) []containerView {
	var matches []containerView
	for _, container := range containers {
		if container.name == name {
			matches = append(matches, container)
		}
	}
	return matches
}

func containerNames(containers []containerView) []string {
	names := make([]string, 0, len(containers))
	for _, container := range containers {
		names = append(names, container.name)
	}
	return names
}

func jobsWithOperation(jobs []batchv1.Job, operation string) []*batchv1.Job {
	var matches []*batchv1.Job
	for index := range jobs {
		if jobs[index].Labels[labelOperation] == operation {
			matches = append(matches, &jobs[index])
		}
	}
	return matches
}

func int32PointerIs(value *int32, want int32) bool {
	return value != nil && *value == want
}

func int64PointerIs(value *int64, want int64) bool {
	return value != nil && *value == want
}

func boolPointerIs(value *bool, want bool) bool {
	return value != nil && *value == want
}

// falseOrAbsent is jq's `(.field // false) == false`.
func falseOrAbsent(value *bool) bool {
	return value == nil || !*value
}

// controllerIsolation holds the secrets the controller Job isolation reads.
type controllerIsolation struct {
	databaseSecret, registrySecret string
}

// jobContainers is a Job template's regular and init containers, the
// containers the controller isolation reads.
func jobContainers(job *batchv1.Job) []containerView {
	spec := job.Spec.Template.Spec
	return append(viewContainers(spec.Containers), viewContainers(spec.InitContainers)...)
}

func (s controllerIsolation) hasDatabaseRef(c containerView) bool {
	for _, variable := range c.env {
		reference := envSecretRef(variable)
		if variable.Name == "PTAH_DB_URL" && variable.Value == "" && reference != nil &&
			reference.Name == s.databaseSecret && reference.Key == "url" {
			return true
		}
	}
	return false
}

func hasExpectedDatabaseEngine(c containerView) bool {
	for _, variable := range c.env {
		if variable.Name == "PTAH_EXPECTED_DATABASE_ENGINE" && variable.ValueFrom == nil &&
			(variable.Value == "PostgreSQL" || variable.Value == "MySQL") {
			return true
		}
	}
	return false
}

// hasRegistryLiteral is PTAH_OCI_REGISTRY given as a literal.
func hasRegistryLiteral(c containerView) bool {
	for _, variable := range c.env {
		if variable.Name == "PTAH_OCI_REGISTRY" && variable.Value != "" && variable.ValueFrom == nil {
			return true
		}
	}
	return false
}

func (s controllerIsolation) hasRegistryCredentials(c containerView) bool {
	for _, name := range []string{"PTAH_OCI_USERNAME", "PTAH_OCI_PASSWORD", "PTAH_OCI_TOKEN"} {
		if !c.hasSecretEnvAnyKey(name, s.registrySecret) {
			return false
		}
	}
	return hasRegistryLiteral(c)
}

func (s controllerIsolation) hasCustomCAGuardInputs(c containerView) bool {
	return c.hasSecretEnv("PTAH_OPERATOR_OCI_CA_SHA256_GRANT", s.registrySecret, "caSHA256", false) &&
		c.hasLiteralEnv("PTAH_OPERATOR_OCI_HAS_CA", "true") &&
		c.hasLiteralEnv("PTAH_OPERATOR_OCI_CA_SOURCE_FILE", "/credentials/ca-source/ca.pem")
}

func (s controllerIsolation) hasAuthorityGuardInputs(c containerView) bool {
	return c.hasLiteralEnv("PTAH_OPERATOR_OCI_AUTH_MODE", "Environment") &&
		c.hasSecretEnv("PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT", s.registrySecret, "registry", false) &&
		hasRegistryLiteral(c) &&
		((c.hasSecretEnv("PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP", s.registrySecret, "allowPlainHTTP", false) &&
			c.hasLiteralEnv("PTAH_PLAIN_HTTP", "true")) ||
			(s.hasCustomCAGuardInputs(c) && c.hasLiteralEnv("PTAH_PLAIN_HTTP", "false")))
}

func noAuthorityGuardInputs(c containerView) bool {
	return !c.envNameWith(func(name string) bool { return strings.HasPrefix(name, "PTAH_OPERATOR_OCI_") })
}

func (s controllerIsolation) noDatabase(c containerView) bool {
	return !c.hasEnvName("PTAH_DB_URL") && !c.hasEnvName("PTAH_DEV_URL") &&
		!slices.Contains(c.secretNames(), s.databaseSecret)
}

func (s controllerIsolation) noRegistry(c containerView) bool {
	return !c.envNameWith(registryEnvName) && !slices.Contains(c.secretNames(), s.registrySecret)
}

func (s controllerIsolation) neutral(c containerView) bool {
	return s.noDatabase(c) && s.noRegistry(c)
}

func controllerSafeJobContract(job *batchv1.Job) bool {
	return int32PointerIs(job.Spec.BackoffLimit, 0) &&
		job.Spec.PodReplacementPolicy != nil && *job.Spec.PodReplacementPolicy == batchv1.Failed &&
		job.Spec.Template.Spec.RestartPolicy == corev1.RestartPolicyNever
}

func (s controllerIsolation) onlyNeutralExcept(job *batchv1.Job, allowed ...string) bool {
	for _, container := range jobContainers(job) {
		if !slices.Contains(allowed, container.name) && !s.neutral(container) {
			return false
		}
	}
	return true
}

func (s controllerIsolation) sourceCASnapshotBoundary(main containerView) bool {
	if !s.hasCustomCAGuardInputs(main) {
		return true
	}
	return !main.hasEnvName("PTAH_OCI_CA_FILE") && main.hasMount("registry-ca", true) &&
		!main.hasMount("registry-ca-snapshot", true) && !main.hasMount("registry-ca-snapshot", false)
}

func (s controllerIsolation) fetchCASnapshotBoundary(guard, fetch containerView) bool {
	if s.hasCustomCAGuardInputs(guard) {
		return guard.hasMount("registry-ca", true) && guard.hasMount("registry-ca-snapshot", false) &&
			!guard.hasMount("registry-ca-snapshot", true) &&
			fetch.hasLiteralEnv("PTAH_OCI_CA_FILE", "/credentials/ca-snapshot/ca.pem") &&
			fetch.hasMount("registry-ca-snapshot", true) &&
			!fetch.hasMount("registry-ca", true) && !fetch.hasMount("registry-ca", false)
	}
	return len(guard.mounts) == 1 && guard.mounts[0].Name == "runner" && !fetch.hasEnvName("PTAH_OCI_CA_FILE")
}

func (s controllerIsolation) sourceJobIsolated(job *batchv1.Job) bool {
	containers := jobContainers(job)
	main, fetch, guard := containersNamed(containers, "ptah"), containersNamed(containers, "fetch-schema"),
		containersNamed(containers, "validate-source-authority")
	return len(main) == 1 && len(fetch) == 0 && len(guard) == 0 &&
		s.noDatabase(main[0]) && s.hasRegistryCredentials(main[0]) && s.hasAuthorityGuardInputs(main[0]) &&
		s.sourceCASnapshotBoundary(main[0]) &&
		s.onlyNeutralExcept(job, "ptah")
}

func (s controllerIsolation) databaseSourceJobIsolated(job *batchv1.Job) bool {
	containers := jobContainers(job)
	main, fetch, guard := containersNamed(containers, "ptah"), containersNamed(containers, "fetch-schema"),
		containersNamed(containers, "validate-source-authority")
	if len(main) != 1 || len(fetch) != 1 || len(guard) != 1 {
		return false
	}
	initNames := containerNames(viewContainers(job.Spec.Template.Spec.InitContainers))
	return slices.Equal(initNames, []string{"install-runner", "validate-source-authority", "fetch-schema"}) &&
		s.hasDatabaseRef(main[0]) && s.noRegistry(main[0]) &&
		s.noDatabase(fetch[0]) && s.hasRegistryCredentials(fetch[0]) && noAuthorityGuardInputs(fetch[0]) &&
		s.noDatabase(guard[0]) && s.hasAuthorityGuardInputs(guard[0]) &&
		!guard[0].hasEnvName("PTAH_OCI_USERNAME") && !guard[0].hasEnvName("PTAH_OCI_PASSWORD") &&
		!guard[0].hasEnvName("PTAH_OCI_TOKEN") &&
		s.fetchCASnapshotBoundary(guard[0], fetch[0]) &&
		s.onlyNeutralExcept(job, "ptah", "validate-source-authority", "fetch-schema")
}

func (s controllerIsolation) applyJobIsolated(job *batchv1.Job) bool {
	containers := jobContainers(job)
	main, fetch := containersNamed(containers, "ptah"), containersNamed(containers, "fetch-schema")
	return len(main) == 1 && len(fetch) == 0 &&
		s.hasDatabaseRef(main[0]) && s.noRegistry(main[0]) &&
		s.onlyNeutralExcept(job, "ptah")
}

// controllerJobIsolation holds that the schema's operation Jobs keep the
// registry credential away from every
// container that holds the database URL, and the database URL away from every
// container that talks to the registry. requireApply demands an Apply Job
// among them.
func controllerJobIsolation(jobs []batchv1.Job, databaseSecret, registrySecret string, requireApply bool) bool {
	s := controllerIsolation{databaseSecret: databaseSecret, registrySecret: registrySecret}
	every := func(operation string, holds func(*batchv1.Job) bool) bool {
		matches := jobsWithOperation(jobs, operation)
		if len(matches) == 0 {
			return false
		}
		for _, job := range matches {
			if !controllerSafeJobContract(job) || !holds(job) {
				return false
			}
		}
		return true
	}
	databaseJob := func(job *batchv1.Job) bool {
		main := containersNamed(jobContainers(job), "ptah")
		return s.databaseSourceJobIsolated(job) && len(main) == 1 && hasExpectedDatabaseEngine(main[0])
	}
	return every("resolve", s.sourceJobIsolated) && every("verify", s.sourceJobIsolated) &&
		every("observe", databaseJob) && every("plan", databaseJob) &&
		(!requireApply || every("apply", s.applyJobIsolated))
}

// sourceJobIsolationInputs are what the source isolation reads besides the
// Jobs.
type sourceJobIsolationInputs struct {
	databaseSecret     string
	registrySecret     string
	registryAuthority  string
	authMode           string
	executorImage      string
	runnerImage        string
	verificationPolicy string
	serviceAccountName string
	imagePullSecrets   []corev1.LocalObjectReference
	requestedReference string
	resolvedReference  string
}

var runnerProtocolVersionPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// sourceJobIsolation holds that the Resolve and Verify Jobs of a schema read
// the registry and nothing else, and every field
// that could carry a credential somewhere else -- a volume, a mount, a hook, a
// probe, a host alias, a termination-message path -- is the one the manager
// writes.
func sourceJobIsolation(jobs []batchv1.Job, in sourceJobIsolationInputs) bool {
	operations := make([]string, 0, len(jobs))
	for index := range jobs {
		operations = append(operations, jobs[index].Labels[labelOperation])
	}
	sort.Strings(operations)
	if !slices.Equal(operations, []string{"resolve", "verify"}) {
		return false
	}
	for index := range jobs {
		if !in.safeJobContract(&jobs[index]) || !in.sourceJobIsolated(&jobs[index]) {
			return false
		}
	}
	return true
}

// exactLiteralEnv is exactly one variable of the name, with the literal value
// and no source.
func exactLiteralEnv(c containerView, name, value string) bool {
	matches := c.envNamed(name)
	return len(matches) == 1 && matches[0].Value == value && matches[0].ValueFrom == nil
}

// runnerProtocolEnv is the runner protocol the manager speaks: a literal
// version the runner compares with its own before it reads anything else.
func runnerProtocolEnv(c containerView) bool {
	matches := c.envNamed("PTAH_RUNNER_PROTOCOL_VERSION")
	return len(matches) == 1 && runnerProtocolVersionPattern.MatchString(matches[0].Value) &&
		matches[0].ValueFrom == nil
}

// exactSecretEnv is exactly one variable of the name that reads nothing but
// the key of the registry Secret, with optional set to true or left out.
func (in sourceJobIsolationInputs) exactSecretEnv(c containerView, name, key string, optional bool) bool {
	matches := c.envNamed(name)
	if len(matches) != 1 || matches[0].Value != "" || matches[0].ValueFrom == nil ||
		!slices.Equal(renderedKeys(matches[0].ValueFrom), []string{"secretKeyRef"}) {
		return false
	}
	reference := matches[0].ValueFrom.SecretKeyRef
	if reference.Name != in.registrySecret || reference.Key != key {
		return false
	}
	if optional {
		return slices.Equal(renderedKeys(reference), []string{"key", "name", "optional"}) &&
			reference.Optional != nil && *reference.Optional
	}
	return slices.Equal(renderedKeys(reference), []string{"key", "name"}) && reference.Optional == nil
}

func (in sourceJobIsolationInputs) exactSourceEnvNames(operation string) []string {
	names := []string{
		"HOME", "PTAH_OCI_REGISTRY", "PTAH_OPERATION_ID", "PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP",
		"PTAH_OPERATOR_OCI_AUTH_MODE", "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT", "PTAH_PLAIN_HTTP",
		"PTAH_REQUESTED_REFERENCE", "PTAH_RUNNER_PROTOCOL_VERSION", "TMPDIR",
	}
	switch in.authMode {
	case "Environment":
		names = append(names, "PTAH_OCI_PASSWORD", "PTAH_OCI_TOKEN", "PTAH_OCI_USERNAME")
	case "DockerConfigJSON":
		names = append(names, "DOCKER_CONFIG")
	}
	if operation == "verify" {
		names = append(names, "PTAH_EXPECTED_ARTIFACT_TYPE", "PTAH_RESOLVED_REFERENCE", "PTAH_VERIFICATION_POLICY")
	}
	sort.Strings(names)
	return names
}

func (in sourceJobIsolationInputs) noDatabase(c containerView) bool {
	return !c.hasEnvName("PTAH_DB_URL") && !c.hasEnvName("PTAH_DEV_URL") &&
		!slices.Contains(c.secretNames(), in.databaseSecret)
}

func noEnvFrom(c containerView) bool {
	return len(c.envFrom) == 0
}

func (in sourceJobIsolationInputs) noSourceAccess(c containerView) bool {
	return !c.envNameWith(registryEnvName) && len(c.mountsNamed("registry-docker-config")) == 0 &&
		!slices.Contains(c.secretNames(), in.registrySecret)
}

func (in sourceJobIsolationInputs) fixedGrants(c containerView) bool {
	var grants []string
	for _, name := range c.envNames() {
		if strings.HasPrefix(name, "PTAH_OPERATOR_OCI_") {
			grants = append(grants, name)
		}
	}
	sort.Strings(grants)
	return exactLiteralEnv(c, "PTAH_OPERATOR_OCI_AUTH_MODE", in.authMode) &&
		in.exactSecretEnv(c, "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT", "registry", false) &&
		in.exactSecretEnv(c, "PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP", "allowPlainHTTP", false) &&
		exactLiteralEnv(c, "PTAH_OCI_REGISTRY", in.registryAuthority) &&
		exactLiteralEnv(c, "PTAH_PLAIN_HTTP", "true") &&
		slices.Equal(grants, []string{
			"PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP", "PTAH_OPERATOR_OCI_AUTH_MODE", "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT",
		})
}

func noSecretVolumes(volumes []corev1.Volume) bool {
	for _, volume := range volumes {
		if volume.Secret != nil || projectsSecret(volume) {
			return false
		}
	}
	return true
}

func projectsSecret(volume corev1.Volume) bool {
	if volume.Projected == nil {
		return false
	}
	for _, source := range volume.Projected.Sources {
		if source.Secret != nil {
			return true
		}
	}
	return false
}

func volumesNamed(volumes []corev1.Volume, name string) []corev1.Volume {
	var matches []corev1.Volume
	for _, volume := range volumes {
		if volume.Name == name {
			matches = append(matches, volume)
		}
	}
	return matches
}

func (in sourceJobIsolationInputs) environmentCredentials(job *batchv1.Job, c containerView) bool {
	secretNames := c.secretEnvNames()
	sort.Strings(secretNames)
	volumes := job.Spec.Template.Spec.Volumes
	return in.exactSecretEnv(c, "PTAH_OCI_USERNAME", "username", true) &&
		in.exactSecretEnv(c, "PTAH_OCI_PASSWORD", "password", true) &&
		in.exactSecretEnv(c, "PTAH_OCI_TOKEN", "token", true) &&
		slices.Equal(secretNames, []string{
			"PTAH_OCI_PASSWORD", "PTAH_OCI_TOKEN", "PTAH_OCI_USERNAME",
			"PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP", "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT",
		}) &&
		!c.hasEnvName("DOCKER_CONFIG") &&
		len(volumesNamed(volumes, "registry-docker-config")) == 0 &&
		len(c.mountsNamed("registry-docker-config")) == 0 &&
		noSecretVolumes(volumes)
}

func (in sourceJobIsolationInputs) dockerConfigVolume() any {
	return map[string]any{
		"name": "registry-docker-config",
		"secret": map[string]any{
			"secretName":  in.registrySecret,
			"items":       []any{map[string]any{"key": ".dockerconfigjson", "path": "config.json", "mode": float64(288)}},
			"defaultMode": float64(420),
		},
	}
}

var dockerConfigMount = literalDocument(`{"name": "registry-docker-config", "mountPath": "/credentials/docker", "readOnly": true}`)

func (in sourceJobIsolationInputs) dockerConfigCredentials(job *batchv1.Job, c containerView) bool {
	secretNames := c.secretEnvNames()
	sort.Strings(secretNames)
	volumes := job.Spec.Template.Spec.Volumes
	mounts := c.mountsNamed("registry-docker-config")
	dockerVolumes := volumesNamed(volumes, "registry-docker-config")
	if c.envNameWith(func(name string) bool {
		return name == "PTAH_OCI_USERNAME" || name == "PTAH_OCI_PASSWORD" || name == "PTAH_OCI_TOKEN"
	}) || !slices.Equal(secretNames, []string{"PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP", "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT"}) ||
		!exactLiteralEnv(c, "DOCKER_CONFIG", "/credentials/docker") ||
		len(mounts) != 1 || !renderedEqual(mounts[0], dockerConfigMount) ||
		len(dockerVolumes) != 1 || !renderedEqual(dockerVolumes[0], in.dockerConfigVolume()) {
		return false
	}
	for _, volume := range volumes {
		if (volume.Secret != nil && volume.Name != "registry-docker-config") || projectsSecret(volume) {
			return false
		}
	}
	return true
}

func (in sourceJobIsolationInputs) expectedSourceVolumes(operation string) any {
	volumes := []any{
		map[string]any{"name": "runner", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "64Mi"}},
		map[string]any{"name": "work", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "128Mi"}},
	}
	if in.authMode == "DockerConfigJSON" {
		volumes = append(volumes, in.dockerConfigVolume())
	}
	if operation == "verify" {
		volumes = append(volumes, map[string]any{
			"name": "verification-policy",
			"configMap": map[string]any{
				"name":        in.verificationPolicy,
				"items":       []any{map[string]any{"key": "policy.yaml", "path": "policy.yaml", "mode": float64(288)}},
				"defaultMode": float64(420),
			},
		})
	}
	return volumes
}

func (in sourceJobIsolationInputs) expectedMainMounts(operation string) any {
	mounts := []any{
		map[string]any{"name": "runner", "mountPath": "/runner", "readOnly": true},
		map[string]any{"name": "work", "mountPath": "/work"},
	}
	if in.authMode == "DockerConfigJSON" {
		mounts = append(mounts, dockerConfigMount)
	}
	if operation == "verify" {
		mounts = append(mounts, map[string]any{"name": "verification-policy", "mountPath": "/verification", "readOnly": true})
	}
	return mounts
}

var (
	hardenedSourceContainer = literalDocument(`{
		"capabilities": {"drop": ["ALL"]},
		"runAsUser": 65532, "runAsGroup": 65532, "runAsNonRoot": true,
		"readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false,
		"seccompProfile": {"type": "RuntimeDefault"}
	}`)
	hardenedSourcePod = literalDocument(`{
		"runAsUser": 65532, "runAsGroup": 65532, "runAsNonRoot": true,
		"fsGroup": 65532, "fsGroupChangePolicy": "OnRootMismatch",
		"seccompProfile": {"type": "RuntimeDefault"}
	}`)
	installRunnerMounts = literalDocument(`[{"name": "runner", "mountPath": "/runner"}]`)
)

// renderedListEqual is jq's `(.list // []) == [...]`: an absent list is an empty
// one.
func renderedListEqual[T any](list []T, want any) bool {
	if list == nil {
		list = []T{}
	}
	return renderedEqual(list, want)
}

// boundedContainer is a container with no hook, no probe, no port, no device,
// no terminal and no restart policy of its own, whose termination message is
// the default file.
func boundedContainer(container *corev1.Container) bool {
	return container.TerminationMessagePath == "/dev/termination-log" &&
		container.TerminationMessagePolicy == corev1.TerminationMessageReadFile &&
		container.Lifecycle == nil && container.LivenessProbe == nil && container.ReadinessProbe == nil &&
		container.StartupProbe == nil && len(container.Ports) == 0 && len(container.VolumeDevices) == 0 &&
		!container.Stdin && !container.StdinOnce && !container.TTY && container.RestartPolicy == nil
}

func (in sourceJobIsolationInputs) exactSourceLiterals(job *batchv1.Job, c containerView, operation string) bool {
	operationID := job.Annotations[annotationOperationID]
	if !sha256Pattern.MatchString(operationID) ||
		!exactLiteralEnv(c, "HOME", "/work") || !exactLiteralEnv(c, "TMPDIR", "/work") ||
		!exactLiteralEnv(c, "PTAH_OPERATION_ID", operationID) ||
		!exactLiteralEnv(c, "PTAH_REQUESTED_REFERENCE", in.requestedReference) ||
		!runnerProtocolEnv(c) {
		return false
	}
	if operation == "verify" {
		return exactLiteralEnv(c, "PTAH_RESOLVED_REFERENCE", in.resolvedReference) &&
			exactLiteralEnv(c, "PTAH_VERIFICATION_POLICY", "/verification/policy.yaml") &&
			exactLiteralEnv(c, "PTAH_EXPECTED_ARTIFACT_TYPE", schemaArtifactType)
	}
	return true
}

func (in sourceJobIsolationInputs) safeJobContract(job *batchv1.Job) bool {
	spec := job.Spec.Template.Spec
	pullSecrets := spec.ImagePullSecrets
	if pullSecrets == nil {
		pullSecrets = []corev1.LocalObjectReference{}
	}
	want := in.imagePullSecrets
	if want == nil {
		want = []corev1.LocalObjectReference{}
	}
	return int32PointerIs(job.Spec.BackoffLimit, 0) &&
		job.Spec.PodReplacementPolicy != nil && *job.Spec.PodReplacementPolicy == batchv1.Failed &&
		spec.RestartPolicy == corev1.RestartPolicyNever &&
		boolPointerIs(spec.AutomountServiceAccountToken, false) && boolPointerIs(spec.EnableServiceLinks, false) &&
		spec.DNSPolicy == corev1.DNSClusterFirst && spec.DNSConfig == nil && len(spec.HostAliases) == 0 &&
		!spec.HostNetwork && !spec.HostPID && !spec.HostIPC && falseOrAbsent(spec.ShareProcessNamespace) &&
		spec.ServiceAccountName == in.serviceAccountName &&
		reflect.DeepEqual(renderedDocument(pullSecrets), renderedDocument(want)) &&
		renderedEqual(spec.SecurityContext, hardenedSourcePod)
}

func (in sourceJobIsolationInputs) sourceJobIsolated(job *batchv1.Job) bool {
	operation := job.Labels[labelOperation]
	spec := job.Spec.Template.Spec
	if !slices.Equal(containerNames(viewContainers(spec.Containers)), []string{"ptah"}) ||
		!slices.Equal(containerNames(viewContainers(spec.InitContainers)), []string{"install-runner"}) ||
		len(spec.EphemeralContainers) != 0 {
		return false
	}
	main, install := &spec.Containers[0], &spec.InitContainers[0]
	mainView := viewContainers(spec.Containers)[0]
	envNames := mainView.envNames()
	sort.Strings(envNames)
	if !slices.Equal(envNames, in.exactSourceEnvNames(operation)) ||
		len(install.Env) != 0 || install.Image != in.runnerImage ||
		install.ImagePullPolicy != corev1.PullIfNotPresent ||
		!slices.Equal(install.Command, []string{"/ptah-runner"}) ||
		!slices.Equal(install.Args, []string{"--install-to", "/runner/ptah-runner"}) ||
		install.WorkingDir != "" || !boundedContainer(install) ||
		!renderedEqual(install.SecurityContext, hardenedSourceContainer) ||
		!renderedListEqual(install.VolumeMounts, installRunnerMounts) ||
		main.Image != in.executorImage || main.ImagePullPolicy != corev1.PullIfNotPresent ||
		!slices.Equal(main.Command, []string{"/runner/ptah-runner"}) ||
		!slices.Equal(main.Args, []string{
			"--ptah-binary", "/usr/local/bin/ptah", "--max-result-bytes", "8388608",
			"--max-plan-bytes", "8388608", "--operation", operation,
		}) ||
		main.WorkingDir != "/work" || !boundedContainer(main) ||
		!renderedEqual(main.SecurityContext, hardenedSourceContainer) ||
		!renderedListEqual(main.VolumeMounts, in.expectedMainMounts(operation)) ||
		!renderedListEqual(spec.Volumes, in.expectedSourceVolumes(operation)) ||
		!in.exactSourceLiterals(job, mainView, operation) ||
		!in.fixedGrants(mainView) {
		return false
	}
	switch in.authMode {
	case "Environment":
		if !in.environmentCredentials(job, mainView) {
			return false
		}
	case "DockerConfigJSON":
		if !in.dockerConfigCredentials(job, mainView) {
			return false
		}
	default:
		return false
	}
	all := append(append(viewContainers(spec.Containers), viewContainers(spec.InitContainers)...),
		viewEphemeralContainers(spec.EphemeralContainers)...)
	for _, container := range all {
		if !in.noDatabase(container) || !noEnvFrom(container) {
			return false
		}
	}
	for _, container := range viewContainers(spec.InitContainers) {
		if !in.noSourceAccess(container) {
			return false
		}
	}
	return true
}

// publisherJobIsolation is testdata/e2e/publisher-job-isolation.jq: the Job
// that pushes a schema artifact holds the registry credential and no database
// credential.
func publisherJobIsolation(job *batchv1.Job, image, registrySecret string) bool {
	spec := job.Spec.Template.Spec
	if len(spec.Containers) == 0 {
		return false
	}
	container := viewContainers(spec.Containers)[0]
	if spec.Containers[0].Name != "publisher" || spec.Containers[0].Image != image ||
		len(spec.Containers) != 1 || len(spec.InitContainers) != 0 ||
		container.hasEnvName("PTAH_DB_URL") || container.hasEnvName("PTAH_DEV_URL") {
		return false
	}
	for _, name := range container.secretNames() {
		if name != registrySecret {
			return false
		}
	}
	for _, name := range []string{"PTAH_OCI_USERNAME", "PTAH_OCI_PASSWORD", "PTAH_OCI_REGISTRY"} {
		if !container.hasSecretEnvAnyKey(name, registrySecret) {
			return false
		}
	}
	return true
}

// customCAPodIsolationInputs are what the custom-CA Pod isolation reads
// besides the Pods.
type customCAPodIsolationInputs struct {
	databaseSecret    string
	registrySecret    string
	registryAuthority string
	caConfigMap       string
	resolvedReference string
}

// customCAPodIsolation holds that the completed Observe and Plan Pods of a schema that reads its artifact over
// authenticated HTTPS with a custom CA keep the guard, the CA snapshot, the
// source and the fetch isolated from the database credential.
func customCAPodIsolation(pods []corev1.Pod, in customCAPodIsolationInputs) bool {
	var selected []*corev1.Pod
	for index := range pods {
		switch pods[index].Labels[labelOperation] {
		case "observe", "plan":
			selected = append(selected, &pods[index])
		}
	}
	if len(selected) != 2 {
		return false
	}
	for _, pod := range selected {
		if !in.isolated(pod) {
			return false
		}
	}
	return true
}

func podContainers(pod *corev1.Pod) []corev1.Container {
	return append(slices.Clone(pod.Spec.Containers), pod.Spec.InitContainers...)
}

var runtimeDefaultSeccomp = literalDocument(`{"type": "RuntimeDefault"}`)

// customCAHardenedContainer holds a container to the restricted profile field
// by field: no escalation, a read-only root, the nonroot user and group, not
// privileged, no capability added and every one dropped, the runtime's
// default seccomp profile.
func customCAHardenedContainer(container *corev1.Container) bool {
	context := container.SecurityContext
	if context == nil {
		return false
	}
	var added, dropped []corev1.Capability
	if context.Capabilities != nil {
		added, dropped = context.Capabilities.Add, context.Capabilities.Drop
	}
	return boolPointerIs(context.AllowPrivilegeEscalation, false) && boolPointerIs(context.ReadOnlyRootFilesystem, true) &&
		boolPointerIs(context.RunAsNonRoot, true) && int64PointerIs(context.RunAsUser, 65532) &&
		int64PointerIs(context.RunAsGroup, 65532) && falseOrAbsent(context.Privileged) &&
		len(added) == 0 && context.Capabilities != nil && slices.Equal(dropped, []corev1.Capability{"ALL"}) &&
		context.SeccompProfile != nil && renderedEqual(context.SeccompProfile, runtimeDefaultSeccomp)
}

func customCAHardenedPod(pod *corev1.Pod) bool {
	spec := pod.Spec
	context := spec.SecurityContext
	if !boolPointerIs(spec.AutomountServiceAccountToken, false) || !boolPointerIs(spec.EnableServiceLinks, false) || context == nil ||
		!boolPointerIs(context.RunAsNonRoot, true) || !int64PointerIs(context.RunAsUser, 65532) ||
		!int64PointerIs(context.RunAsGroup, 65532) || !int64PointerIs(context.FSGroup, 65532) ||
		context.FSGroupChangePolicy == nil || *context.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch ||
		context.SeccompProfile == nil || !renderedEqual(context.SeccompProfile, runtimeDefaultSeccomp) ||
		len(context.SupplementalGroups) != 0 || len(context.Sysctls) != 0 || len(spec.EphemeralContainers) != 0 {
		return false
	}
	for _, container := range podContainers(pod) {
		if !customCAHardenedContainer(&container) || len(container.EnvFrom) != 0 {
			return false
		}
	}
	return true
}

// statusSucceeded is one status for the container, never restarted, that
// terminated with status 0.
func statusSucceeded(pod *corev1.Pod, name string, init bool) bool {
	statuses := pod.Status.ContainerStatuses
	if init {
		statuses = pod.Status.InitContainerStatuses
	}
	var matches []corev1.ContainerStatus
	for _, status := range statuses {
		if status.Name == name {
			matches = append(matches, status)
		}
	}
	return len(matches) == 1 && matches[0].RestartCount == 0 &&
		matches[0].State.Terminated != nil && matches[0].State.Terminated.ExitCode == 0
}

// exactMemoryVolume is exactly one volume of the name that is a memory-backed
// emptyDir of the size and nothing else.
func exactMemoryVolume(pod *corev1.Pod, name, size string) bool {
	count := 0
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == name && slices.Equal(renderedKeys(volume), []string{"emptyDir", "name"}) &&
			volume.EmptyDir.Medium == corev1.StorageMediumMemory &&
			volume.EmptyDir.SizeLimit != nil && volume.EmptyDir.SizeLimit.String() == size {
			count++
		}
	}
	return count == 1
}

var customCAItems = literalDocument(`[{"key": "ca.pem", "path": "ca.pem", "mode": 288}]`)

func (in customCAPodIsolationInputs) exactCAVolume(pod *corev1.Pod) bool {
	count := 0
	for _, volume := range pod.Spec.Volumes {
		source := volume.ConfigMap
		if volume.Name == "registry-ca" && slices.Equal(renderedKeys(volume), []string{"configMap", "name"}) &&
			source.Name == in.caConfigMap && falseOrAbsent(source.Optional) &&
			(source.DefaultMode == nil || *source.DefaultMode == 420) && renderedEqual(source.Items, customCAItems) {
			count++
		}
	}
	return count == 1
}

// expectedMount is one mount as exactMounts compares it.
type expectedMount struct {
	name, mountPath string
	readOnly        bool
}

// exactMounts holds a container's mounts, by name, to exactly the expected
// ones, none of them narrowed to a sub-path or propagating mounts.
func exactMounts(mounts []corev1.VolumeMount, expected []expectedMount) bool {
	actual := make([]expectedMount, 0, len(mounts))
	for _, mount := range mounts {
		if mount.SubPath != "" || mount.SubPathExpr != "" || mount.MountPropagation != nil ||
			(mount.RecursiveReadOnly != nil && *mount.RecursiveReadOnly != "") {
			return false
		}
		actual = append(actual, expectedMount{name: mount.Name, mountPath: mount.MountPath, readOnly: mount.ReadOnly})
	}
	want := slices.Clone(expected)
	byName := func(a, b expectedMount) int { return strings.Compare(a.name, b.name) }
	slices.SortStableFunc(actual, byName)
	slices.SortStableFunc(want, byName)
	return slices.Equal(actual, want)
}

func sameStringsSorted(values []string, want ...string) bool {
	sorted := slices.Clone(values)
	sort.Strings(sorted)
	sort.Strings(want)
	return slices.Equal(sorted, want)
}

func (in customCAPodIsolationInputs) isolated(pod *corev1.Pod) bool {
	all := viewContainers(podContainers(pod))
	main, install := containersNamed(all, "ptah"), containersNamed(all, "install-runner")
	guard, fetch := containersNamed(all, "validate-source-authority"), containersNamed(all, "fetch-schema")
	if len(main) != 1 || len(install) != 1 || len(guard) != 1 || len(fetch) != 1 ||
		pod.Status.Phase != corev1.PodSucceeded ||
		len(controllerOwners(pod.OwnerReferences, "batch/v1", "Job")) != 1 ||
		!slices.Equal(containerNames(viewContainers(pod.Spec.InitContainers)),
			[]string{"install-runner", "validate-source-authority", "fetch-schema"}) ||
		!slices.Equal(containerNames(viewContainers(pod.Spec.Containers)), []string{"ptah"}) ||
		!customCAHardenedPod(pod) ||
		!statusSucceeded(pod, "install-runner", true) || !statusSucceeded(pod, "validate-source-authority", true) ||
		!statusSucceeded(pod, "fetch-schema", true) || !statusSucceeded(pod, "ptah", false) {
		return false
	}
	var volumeNames []string
	for _, volume := range pod.Spec.Volumes {
		volumeNames = append(volumeNames, volume.Name)
	}
	if !sameStringsSorted(volumeNames, "fetch-work", "registry-ca", "registry-ca-snapshot", "runner", "schema-source", "work") ||
		!exactMemoryVolume(pod, "runner", "64Mi") || !exactMemoryVolume(pod, "work", "128Mi") ||
		!exactMemoryVolume(pod, "fetch-work", "64Mi") || !exactMemoryVolume(pod, "registry-ca-snapshot", "2Mi") ||
		!exactMemoryVolume(pod, "schema-source", "64Mi") || !in.exactCAVolume(pod) {
		return false
	}
	// The containers are found again by name among the declared ones, so the
	// command, arguments and mounts read are those of the one container each
	// name counted.
	container := func(name string) *corev1.Container {
		for _, candidate := range podContainers(pod) {
			if candidate.Name == name {
				return &candidate
			}
		}
		return nil
	}
	installer, guardian, fetcher, runner := container("install-runner"), container("validate-source-authority"),
		container("fetch-schema"), container("ptah")
	return slices.Equal(installer.Command, []string{"/ptah-runner"}) &&
		slices.Equal(installer.Args, []string{"--install-to", "/runner/ptah-runner"}) &&
		len(installer.Env) == 0 && len(install[0].secretNames()) == 0 &&
		exactMounts(installer.VolumeMounts, []expectedMount{{"runner", "/runner", false}}) &&
		in.guardIsolated(guardian, guard[0]) &&
		in.fetchIsolated(fetcher, fetch[0]) &&
		in.mainIsolated(runner, main[0])
}

func (in customCAPodIsolationInputs) guardIsolated(container *corev1.Container, view containerView) bool {
	return slices.Equal(container.Command, []string{"/runner/ptah-runner"}) &&
		slices.Equal(container.Args, []string{
			"--validate-oci-source", in.resolvedReference, "--snapshot-oci-ca-to", "/credentials/ca-snapshot/ca.pem",
		}) &&
		view.hasLiteralEnv("PTAH_OPERATOR_OCI_AUTH_MODE", "Environment") &&
		view.hasSecretEnv("PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT", in.registrySecret, "registry", false) &&
		view.hasSecretEnv("PTAH_OPERATOR_OCI_CA_SHA256_GRANT", in.registrySecret, "caSHA256", false) &&
		view.hasLiteralEnv("PTAH_OPERATOR_OCI_HAS_CA", "true") &&
		view.hasLiteralEnv("PTAH_OPERATOR_OCI_CA_SOURCE_FILE", "/credentials/ca-source/ca.pem") &&
		view.hasLiteralEnv("PTAH_OCI_REGISTRY", in.registryAuthority) &&
		view.hasLiteralEnv("PTAH_PLAIN_HTTP", "false") &&
		!view.hasEnvName("PTAH_OCI_USERNAME") && !view.hasEnvName("PTAH_OCI_PASSWORD") &&
		!view.hasEnvName("PTAH_OCI_TOKEN") &&
		sameStringsSorted(view.secretNames(), in.registrySecret, in.registrySecret) &&
		exactMounts(container.VolumeMounts, []expectedMount{
			{"runner", "/runner", true},
			{"registry-ca", "/credentials/ca-source", true},
			{"registry-ca-snapshot", "/credentials/ca-snapshot", false},
		})
}

func (in customCAPodIsolationInputs) fetchIsolated(container *corev1.Container, view containerView) bool {
	return slices.Equal(container.Command, []string{"/usr/local/bin/ptah"}) &&
		slices.Equal(container.Args, []string{"schema", "pull", in.resolvedReference, "--out", "/source/schema.hcl"}) &&
		view.hasSecretEnv("PTAH_OCI_USERNAME", in.registrySecret, "username", true) &&
		view.hasSecretEnv("PTAH_OCI_PASSWORD", in.registrySecret, "password", true) &&
		view.hasSecretEnv("PTAH_OCI_TOKEN", in.registrySecret, "token", true) &&
		view.hasLiteralEnv("PTAH_OCI_REGISTRY", in.registryAuthority) &&
		view.hasLiteralEnv("PTAH_OCI_CA_FILE", "/credentials/ca-snapshot/ca.pem") &&
		view.hasLiteralEnv("PTAH_PLAIN_HTTP", "false") &&
		noAuthorityGuardInputs(view) &&
		sameStringsSorted(view.secretNames(), in.registrySecret, in.registrySecret, in.registrySecret) &&
		exactMounts(container.VolumeMounts, []expectedMount{
			{"fetch-work", "/fetch-work", false},
			{"registry-ca-snapshot", "/credentials/ca-snapshot", true},
			{"schema-source", "/source", false},
		})
}

func (in customCAPodIsolationInputs) mainIsolated(container *corev1.Container, view containerView) bool {
	return view.hasSecretEnv("PTAH_DB_URL", in.databaseSecret, "url", false) &&
		view.hasLiteralEnv("PTAH_EXPECTED_DATABASE_ENGINE", "PostgreSQL") &&
		!view.envNameWith(registryEnvName) &&
		sameStringsSorted(view.secretNames(), in.databaseSecret) &&
		exactMounts(container.VolumeMounts, []expectedMount{
			{"runner", "/runner", true},
			{"schema-source", "/source", true},
			{"work", "/work", false},
		})
}

// customCAApprovalBoundary is a schema that holds a published plan for a
// person, at its observed generation, with a refresh deadline still ahead of
// now.
func customCAApprovalBoundary(schema *ptahv1alpha1.PtahSchema, now time.Time) bool {
	generation := schema.Generation
	status := schema.Status
	plan := status.Plan
	if generation <= 0 || status.ObservedGeneration != generation || status.Phase != ptahv1alpha1.PhaseAwaitingApproval ||
		status.ActiveOperation != nil || status.NextReconciliationTime == nil ||
		!status.NextReconciliationTime.After(now) ||
		plan == nil || plan.Name == "" || plan.UID == "" ||
		!sha256Pattern.MatchString(plan.Fingerprint) || !sha256Pattern.MatchString(plan.ContentDigest) ||
		plan.Approval != nil {
		return false
	}
	return hasCondition(status.Conditions, "ApprovalRequired", "True", "Waiting", generation) &&
		hasCondition(status.Conditions, "PlanReady", "True", "Published", generation)
}

// tlsProxyServiceEndpoints reports whether the proxy's Service routes to the
// one captured Pod and nowhere else.
func tlsProxyServiceEndpoints(slices []discoveryv1.EndpointSlice, namespace, name string, uid types.UID, podIP string) bool {
	addressType := discoveryv1.AddressTypeIPv4
	if strings.Contains(podIP, ":") {
		addressType = discoveryv1.AddressTypeIPv6
	}
	var routed []*discoveryv1.EndpointSlice
	for index := range slices {
		if len(slices[index].Endpoints) > 0 {
			routed = append(routed, &slices[index])
		}
	}
	if len(routed) != 1 {
		return false
	}
	slice := routed[0]
	if slice.AddressType != addressType || len(slice.Ports) != 1 {
		return false
	}
	port := slice.Ports[0]
	if port.Name == nil || *port.Name != "tls" || port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP ||
		!int32PointerIs(port.Port, 5443) || len(slice.Endpoints) != 1 {
		return false
	}
	endpoint := slice.Endpoints[0]
	conditions := endpoint.Conditions
	target := endpoint.TargetRef
	return (conditions.Ready == nil || *conditions.Ready) && (conditions.Serving == nil || *conditions.Serving) &&
		falseOrAbsent(conditions.Terminating) &&
		target != nil && (target.APIVersion == "" || target.APIVersion == "v1") && target.Kind == "Pod" &&
		target.Namespace == namespace && target.Name == name && target.UID == uid &&
		len(endpoint.Addresses) == 1 && endpoint.Addresses[0] == podIP
}

var (
	privilegedStatementText = regexp.MustCompile(`(?i)e2e_widget_count|SECURITY DEFINER|search_path|count\(|e2e_widgets`)
	grantStatementText      = regexp.MustCompile(`e2e_widgets|SELECT|PUBLIC`)
)

// privilegedApprovalGate is a plan
// that changes privileges, held for a person under apply: Always.
//
// The gate is the condition and the plan it is about, not the phase: a
// resource waiting for an approval still resolves, verifies, observes and
// plans at its interval, and is out of AwaitingApproval for part of every
// cycle while the requirement stands. Every condition message names kinds and
// nothing the statement says; the check reads the messages and nothing else in
// status, which carries the test namespace in image references.
func privilegedApprovalGate(schema *ptahv1alpha1.PtahSchema, digest string) bool {
	status := schema.Status
	plan := status.Plan
	if schema.Generation != status.ObservedGeneration || status.Source.Digest != digest ||
		status.ActiveOperation != nil || status.NextReconciliationTime == nil ||
		plan == nil || !slices.Equal(plan.PrivilegeChanges, []ptahv1alpha1.PrivilegeChange{"SecurityDefiner"}) ||
		plan.Destructive || plan.Approval != nil {
		return false
	}
	required := false
	for _, condition := range status.Conditions {
		if condition.Type == "ApprovalRequired" && condition.Status == "True" && condition.Reason == "PrivilegeChanges" &&
			strings.Contains(condition.Message, "(SecurityDefiner)") {
			required = true
		}
		if privilegedStatementText.MatchString(condition.Message) {
			return false
		}
	}
	return required
}

// grantOnlyApprovalGate is a change
// that touches only a grant, observed, planned, and held for a person under
// apply: Always.
//
// Ptah's drift report has no category for a grant, so the observation says
// drift with no finding: a safe highest severity, no count and no list. An
// empty severity would be an observation that found no drift, and a grant it
// did not see. Neither the observation nor a condition message may carry what
// the statement says.
func grantOnlyApprovalGate(schema *ptahv1alpha1.PtahSchema, digest string) bool {
	status := schema.Status
	plan := status.Plan
	target := status.Target
	if schema.Generation != status.ObservedGeneration || status.Source.Digest != digest ||
		status.ActiveOperation != nil ||
		target.HighestDriftSeverity != "safe" || target.DriftFindingCount != 0 || len(target.DriftFindings) != 0 ||
		plan == nil || !slices.Equal(plan.PrivilegeChanges, []ptahv1alpha1.PrivilegeChange{"Grant"}) ||
		plan.Destructive || plan.Approval != nil {
		return false
	}
	drift, required := false, false
	messages := make([]string, 0, len(status.Conditions))
	for _, condition := range status.Conditions {
		if condition.Type == "DriftDetected" && condition.Status == "True" && condition.Reason == "ScopedChanges" {
			drift = true
		}
		if condition.Type == "ApprovalRequired" && condition.Status == "True" && condition.Reason == "PrivilegeChanges" &&
			strings.Contains(condition.Message, "(Grant)") {
			required = true
		}
		messages = append(messages, condition.Message)
	}
	// jq read the observation and the messages as one JSON text; so does this.
	text, err := json.Marshal([]any{target, messages})
	if err != nil || grantStatementText.Match(text) {
		return false
	}
	return drift && required
}
