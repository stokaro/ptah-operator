//go:build e2e

package e2e

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

// stateVersion is the controller-state version as the API stores it.
func (d *dataPlane) stateVersion() int32 {
	d.t.Helper()
	version, err := strconv.ParseInt(d.in.ControllerStateVersion, 10, 32)
	d.check(err, "E2E_CONTROLLER_STATE_VERSION must be a positive integer")
	return int32(version)
}

// registryReference is where a schema's artifacts are published in the
// registry the test namespace routes to.
func (d *dataPlane) registryReference(repository string) string {
	return "oci://" + d.registryHost + "/schemas/" + repository + ":stable"
}

// publishSchema pushes one schema revision to the registry from a Job that
// holds the registry credential and no database credential, and returns the
// digest the push printed. The file defaults to the revision's fixture.
func (d *dataPlane) publishSchema(engine, revision, dialect, reference, file string) string {
	d.t.Helper()
	if file == "" {
		file = filepath.Join(repositoryRoot, "testdata", "e2e", engine+"-"+revision+".sql")
	}
	content, err := os.ReadFile(file)
	if err != nil {
		d.fatalf("schema fixture is missing: %s", file)
	}
	configMap, job := "e2e-"+engine+"-"+revision, "e2e-push-"+engine+"-"+revision
	d.logf("publishing %s %s", engine, revision)
	d.mustCreate(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"namespace": d.in.TestNamespace, "name": configMap},
		"data":     map[string]any{"schema.sql": string(content)},
	})
	registryEnv := func(name, key string) map[string]any {
		return secretEnv(name, registryAuthSecret, key)
	}
	labels := map[string]any{"app.kubernetes.io/component": "e2e-schema-publisher"}
	d.mustCreate(map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"namespace": d.in.TestNamespace, "name": job, "labels": labels},
		"spec": map[string]any{
			"backoffLimit": int64(0), "activeDeadlineSeconds": int64(300),
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"restartPolicy": "Never", "automountServiceAccountToken": false,
					"imagePullSecrets": []any{map[string]any{"name": registryPullSecret}},
					"securityContext": map[string]any{
						"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532), "fsGroup": int64(65532),
						"seccompProfile": map[string]any{"type": "RuntimeDefault"},
					},
					"containers": []any{map[string]any{
						"name": "publisher", "image": d.in.ExecutorImage, "imagePullPolicy": "IfNotPresent",
						"command": []any{"/usr/local/bin/ptah"},
						"args": []any{
							"schema", "push", reference, "--schema-file", "/schema/schema.sql",
							"--dialect", dialect, "--version", revision, "--plain-http",
						},
						"env": []any{
							map[string]any{"name": "HOME", "value": "/work"},
							map[string]any{"name": "TMPDIR", "value": "/work"},
							registryEnv("PTAH_OCI_USERNAME", "username"),
							registryEnv("PTAH_OCI_PASSWORD", "password"),
							registryEnv("PTAH_OCI_REGISTRY", "registry"),
						},
						"securityContext": map[string]any{
							"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
							"capabilities": map[string]any{"drop": []any{"ALL"}},
						},
						"volumeMounts": []any{
							map[string]any{"name": "schema", "mountPath": "/schema", "readOnly": true},
							map[string]any{"name": "work", "mountPath": "/work"},
						},
					}},
					"volumes": []any{
						map[string]any{"name": "schema", "configMap": map[string]any{"name": configMap}},
						map[string]any{"name": "work", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
					},
				},
			},
		},
	})
	published := d.waitForJob(job)
	if !publisherJobIsolation(published, d.in.ExecutorImage, registryAuthSecret) {
		d.fatalf("publisher Job did not preserve the no-database-credential boundary")
	}
	logs := d.jobLogs(published, "publisher")
	digest := ""
	for line := range strings.SplitSeq(string(logs), "\n") {
		if match := publishedDigest.FindStringSubmatch(line); match != nil {
			digest = match[1]
		}
	}
	if !sha256Pattern.MatchString(digest) {
		d.fatalf("could not read the published schema digest from Job %s", job)
	}
	return digest
}

// jobLogs reads a container's log from the Job's Pod, as kubectl logs job/<name>
// does.
func (d *dataPlane) jobLogs(job *batchv1.Job, container string) []byte {
	d.t.Helper()
	pods := &corev1.PodList{}
	d.mustList(pods, client.MatchingLabels{"job-name": job.Name})
	owned := ownedPods(pods.Items, job.UID)
	if len(owned) == 0 {
		d.fatalf("Job %s has no Pod to read logs from", job.Name)
	}
	logs, err := d.cluster.ContainerLog(d.ctx, d.in.TestNamespace, owned[0].Name, container)
	d.check(err, "read the logs of Job %s", job.Name)
	return logs
}

// schemaResource is one PtahSchema a row creates. The zero values are the
// ones most rows use.
type schemaResource struct {
	name, engine, secret, reference, coordinationKey string
	// policy is the verification policy ConfigMap, e2e-verification-policy
	// when empty.
	policy string
	// registryAuthSecret and registryAuthMode select the registry credential,
	// e2e-registry-auth read as Environment when empty.
	registryAuthSecret string
	registryAuthMode   string
	// failureRetry is the failure retry interval, 5s when empty, and interval
	// the reconcile interval, the approval interval when empty.
	failureRetry string
	interval     string
	// apply is the explicit apply policy, or empty to leave the default.
	apply string
	// podMetadata is spec.execution.podMetadata, or nil for none: what a mesh
	// or a policy engine asks the operation Pods to carry.
	podMetadata map[string]any
	// resources is an explicit task budget for operational boundary rows.
	resources       *corev1.ResourceRequirements
	transactionMode string
}

// createSchemaResource creates the schema through both admission webhooks, and
// holds the apply policy it stored to the safe default or the explicit value,
// with destructive changes refused either way.
func (d *dataPlane) createSchemaResource(resource schemaResource) {
	d.t.Helper()
	policy := cmp.Or(resource.policy, verificationPolicyName)
	authSecret := cmp.Or(resource.registryAuthSecret, registryAuthSecret)
	authMode := cmp.Or(resource.registryAuthMode, "Environment")
	var registryAuth map[string]any
	switch authMode {
	case "Environment":
		registryAuth = map[string]any{
			"name": authSecret, "mode": "Environment", "usernameKey": "username", "passwordKey": "password",
		}
	case "DockerConfigJSON":
		registryAuth = map[string]any{"name": authSecret, "mode": "DockerConfigJSON", "dockerConfigJSONKey": ".dockerconfigjson"}
	default:
		d.fatalf("unsupported E2E registry authentication mode %s", authMode)
	}
	switch resource.apply {
	case "", "OnApproval", "Always", "Never":
	default:
		d.fatalf("unsupported explicit E2E apply policy %s", resource.apply)
	}
	schemaPolicy := map[string]any{"driftSeverity": "all", "lockTimeout": "30s", "transactionMode": cmp.Or(resource.transactionMode, "file")}
	if resource.apply != "" {
		schemaPolicy["apply"] = resource.apply
	}
	execution := map[string]any{
		"activeDeadlineSeconds": int64(300), "failureRetryInterval": cmp.Or(resource.failureRetry, "5s"),
		"connectTimeout": "30s", "runtimeClassName": admissionRuntimeClass,
	}
	if resource.podMetadata != nil {
		execution["podMetadata"] = resource.podMetadata
	}
	if resource.resources != nil {
		execution["resources"] = resource.resources
	}
	d.mustCreate(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": d.in.TestNamespace, "name": resource.name},
		"spec": map[string]any{
			"target": map[string]any{
				"engine": resource.engine, "coordinationKey": resource.coordinationKey,
				"urlFrom": map[string]any{"name": resource.secret, "key": "url"},
			},
			"desired": map[string]any{
				"ociRef":                 resource.reference,
				"registryAuthFrom":       registryAuth,
				"verificationPolicyFrom": map[string]any{"name": policy, "key": verificationPolicyKey},
				"transport":              map[string]any{"plainHTTP": true},
			},
			"policy":    schemaPolicy,
			"interval":  cmp.Or(resource.interval, approvalInterval),
			"execution": execution,
		},
	})
	stored := d.unstructuredSchema(resource.name)
	apply, _, _ := unstructured.NestedString(stored.Object, "spec", "policy", "apply")
	allowDestructive, found, _ := unstructured.NestedBool(stored.Object, "spec", "policy", "allowDestructive")
	want := cmp.Or(resource.apply, "OnApproval")
	if apply != want || !found || allowDestructive {
		if resource.apply == "" {
			d.fatalf("%s did not persist the safe apply-policy defaults", resource.name)
		}
		d.fatalf("%s did not persist explicit safe apply policy %s", resource.name, resource.apply)
	}
}

// schemaInterval is the interval as the schema stores it, the string the phase
// wrote rather than the duration it means.
func (d *dataPlane) schemaInterval(name string) string {
	d.t.Helper()
	interval, _, _ := unstructured.NestedString(d.unstructuredSchema(name).Object, "spec", "interval")
	return interval
}

// captureCurrentPlan reads the plan the schema's status names now.
func (d *dataPlane) captureCurrentPlan(schema string) currentPlan {
	d.t.Helper()
	status := d.schema(schema).Status
	if status.Plan == nil || status.Plan.Name == "" {
		d.fatalf("%s has no current plan", schema)
	}
	plan := d.schemaPlan(status.Plan.Name)
	if !sha256Pattern.MatchString(plan.Spec.Fingerprint) {
		d.fatalf("%s does not have a SHA-256 plan fingerprint", plan.Name)
	}
	d.plan = currentPlan{name: plan.Name, uid: string(plan.UID), fingerprint: plan.Spec.Fingerprint}
	return d.plan
}

// assertPlan waits for the schema to verify the digest and publish a plan,
// and holds the plan to what published it: the source, target and execution
// evidence on the schema, a committed content-addressed plan whose storage
// cannot be edited and was projected nowhere, and the Observe and Plan results
// of the one Job each that produced it, bound to that evidence. withAfter
// pauses the status writes as soon as the plan is published and returns the
// checkpoint taken there, which bounds the result Jobs the proof reads.
func (d *dataPlane) assertPlan(schema, reference, digest, dialect string, destructive bool,
	observeCheckpoint, planCheckpoint checkpoint, withAfter bool,
) *checkpoint {
	d.t.Helper()
	d.waitForSchema(schema, "verified digest "+digest+" and a published plan", func(s *ptahv1alpha1.PtahSchema) bool {
		status := s.Status
		return status.Source.Digest == digest && status.Plan != nil && status.NextReconciliationTime != nil &&
			(status.Phase == ptahv1alpha1.PhaseAwaitingApproval || status.Phase == ptahv1alpha1.PhaseBlocked)
	})
	var after *checkpoint
	if withAfter {
		d.pauseStatusWrites()
		taken := d.checkpointJobs(schema, "")
		after = &taken
	}
	if err := planStatusBound(d.schema(schema), reference, digest, d.controller, d.stateVersion()); err != nil {
		d.fatalf("%s did not retain resolve, verification, and observation evidence: %v", schema, err)
	}
	if !sha256Pattern.MatchString(d.schema(schema).Status.Target.DriftReportDigest) {
		d.fatalf("%s did not retain a SHA-256 drift-report digest", schema)
	}
	current := d.captureCurrentPlan(schema)
	if err := committedPlan(d.schemaPlan(current.name), schema, digest, dialect, destructive, d.controller, d.stateVersion()); err != nil {
		d.fatalf("%s is not a committed content-addressed native plan: %v", current.name, err)
	}
	d.assertPlanStorageImmutable(schema, current.name, current.uid)
	d.assertPlanNotProjected(current.name)

	observe := d.captureOneNewJobResult(schema, "observe", observeCheckpoint, after)
	planned := d.captureOneNewJobResult(schema, "plan", planCheckpoint, after)
	status := d.schema(schema).Status
	if err := observedDriftBound(observe, status.Target, dialect); err != nil {
		d.fatalf("%s Observe result is not bound to its persisted drift evidence: %v", schema, err)
	}
	if err := changedPlanBound(planned, status.Plan); err != nil {
		d.fatalf("%s Plan result is not bound to its published immutable plan: %v", schema, err)
	}
	// The result's stdout is the plan sealed to the manager's key, so the
	// document the content digest covers is read back from the plan's own
	// immutable chunks, and the same digest has to name it in the result, on
	// the schema and on the plan.
	plan := d.schemaPlan(current.name)
	document, chunks := d.rebuildPlanDocument(plan)
	d.scan(document, schema+" native plan document")
	digestOfDocument := sha256Digest(document)
	if digestOfDocument != planned.PlanContentDigest {
		d.fatalf("%s Plan result content digest does not cover the plan document its chunks hold", schema)
	}
	if status.Plan == nil || digestOfDocument != status.Plan.ContentDigest {
		d.fatalf("%s status.plan.contentDigest does not cover the plan document its chunks hold", schema)
	}
	parsed, err := parsePlanDocument(document)
	d.check(err, "%s plan document", schema)
	if err := planBoundToDocument(plan, parsed, digestOfDocument); err != nil {
		d.fatalf("%s is not bound to the exact native Plan result: %v", current.name, err)
	}
	if err := sealedPayloadLeak(planned.Stdout, document); err != nil {
		d.fatalf("%s %v", schema, err)
	}
	d.planDocument = document
	d.logf("%s Plan result is sealed, and its content digest covers the %d-chunk plan document", schema, chunks)
	return after
}

// rebuildPlanDocument reads a plan document back from the chunks the plan
// names, as rebuiltPlanDocument says, and returns it with the number of chunks
// it read.
func (d *dataPlane) rebuildPlanDocument(plan *ptahv1alpha1.PtahSchemaPlan) ([]byte, int) {
	d.t.Helper()
	document, err := rebuiltPlanDocument(plan, d.planChunk)
	if err != nil {
		d.fatalf("%v", err)
	}
	return document, len(plan.Spec.Chunks)
}

func (d *dataPlane) planChunk(name string) (*ptahv1alpha1.PtahSchemaPlanChunk, error) {
	chunk := &ptahv1alpha1.PtahSchemaPlanChunk{}
	return chunk, d.get(name, chunk)
}

func (d *dataPlane) unstructuredObject(kind, name string) *unstructured.Unstructured {
	d.t.Helper()
	object := &unstructured.Unstructured{}
	object.SetAPIVersion(ptahSchemaAPIVersion)
	object.SetKind(kind)
	d.mustGet(name, object)
	return object
}

// refusedMutation sends a merge patch the API server must refuse as a change
// to something immutable, and holds the object to what it was: the same UID,
// the same resourceVersion and the same spec.
func (d *dataPlane) refusedMutation(kind, name string, patch map[string]any, what string) {
	d.t.Helper()
	before := d.unstructuredObject(kind, name)
	target := &unstructured.Unstructured{}
	target.SetAPIVersion(ptahSchemaAPIVersion)
	target.SetKind(kind)
	target.SetNamespace(d.in.TestNamespace)
	target.SetName(name)
	err := d.mergePatch(target, patch)
	if err == nil {
		d.fatalf("%s accepted a mutation to %s", name, what)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "immutable") {
		d.fatalf("%s mutation of %s was refused for an unexpected reason: %v", name, what, err)
	}
	after := d.unstructuredObject(kind, name)
	switch {
	case after.GetUID() != before.GetUID():
		d.fatalf("%s identity changed after a refused mutation", name)
	case after.GetResourceVersion() != before.GetResourceVersion():
		d.fatalf("%s resourceVersion changed after a refused mutation", name)
	case !reflect.DeepEqual(after.Object["spec"], before.Object["spec"]):
		d.fatalf("%s spec changed after a refused mutation", name)
	}
}

// assertPlanStorageImmutable holds a committed plan and every chunk it names to
// what was committed: a change to the plan's spec or to a chunk's bytes is
// refused, and nothing about either moved.
func (d *dataPlane) assertPlanStorageImmutable(schema, plan, planUID string) {
	d.t.Helper()
	object := d.unstructuredObject("PtahSchemaPlan", plan)
	destructive, found, err := unstructured.NestedBool(object.Object, "spec", "destructive")
	if err != nil || !found {
		d.fatalf("plan spec.destructive must be a boolean")
	}
	d.refusedMutation("PtahSchemaPlan", plan, map[string]any{"spec": map[string]any{"destructive": !destructive}},
		"its committed spec")
	chunks := d.schemaPlan(plan).Spec.Chunks
	if len(chunks) == 0 {
		d.fatalf("%s has no plan chunks to protect", plan)
	}
	for _, reference := range chunks {
		if reference.Name == "" {
			d.fatalf("%s contains an incomplete chunk reference", plan)
		}
		chunk := d.unstructuredObject("PtahSchemaPlanChunk", reference.Name)
		data, _, _ := unstructured.NestedString(chunk.Object, "spec", "data")
		owners := chunk.GetOwnerReferences()
		if len(owners) != 1 || !ownedExactlyOnce(owners, ptahSchemaAPIVersion, "PtahSchemaPlan", plan, types.UID(planUID)) || data == "" {
			d.fatalf("%s is not a plan chunk owned by %s", reference.Name, plan)
		}
		replacement := "eA=="
		if data == replacement {
			replacement = "eQ=="
		}
		d.refusedMutation("PtahSchemaPlanChunk", reference.Name, map[string]any{"spec": map[string]any{"data": replacement}},
			"committed plan bytes")
	}
	d.logf("immutable plan storage enforced for %s (%d chunks)", schema, len(chunks))
}

// assertPlanNotProjected proves a published plan reached no ConfigMap. Its
// chunks are its only store until an Apply of it is dispatched, which is what
// keeps the SQL of a plan waiting for a person from whoever may read the
// namespace's ConfigMaps. The label is the one the store writes on a
// projection, and assertPlanProjected is where the same selector is shown to
// find one.
func (d *dataPlane) assertPlanNotProjected(plan string) {
	d.t.Helper()
	projections := &corev1.ConfigMapList{}
	d.check(d.list(projections, client.MatchingLabels{"operator.ptah.run/plan": plan}),
		"the ConfigMaps labeled for %s could not be listed", plan)
	if count := len(projections.Items); count != 0 {
		d.fatalf("%s was projected into %d ConfigMaps before any Apply of it", plan, count)
	}
	d.logf("%s is stored in its chunks alone until an Apply", plan)
}

// assertPlanProjected proves the ConfigMaps an Apply mounted its plan
// through carry the plan's chunks and nothing else, as planProjectedExactly
// says.
func (d *dataPlane) assertPlanProjected(plan *ptahv1alpha1.PtahSchemaPlan) {
	d.t.Helper()
	projections := &corev1.ConfigMapList{}
	d.check(d.list(projections, client.MatchingLabels{"operator.ptah.run/plan": plan.Name}),
		"the ConfigMaps %s was projected into could not be listed", plan.Name)
	if err := planProjectedExactly(plan, projections.Items, d.planChunk); err != nil {
		d.fatalf("%v", err)
	}
	d.logf("%s was projected for its Apply into %d ConfigMaps that hold its chunks exactly", plan.Name, len(plan.Spec.Chunks))
}

// assertConvergenceResultPair reads the Observe and Plan results of the one
// Job each since the checkpoints, and before after when it is given, and holds
// them and the schema to a converged cycle: no drift, no changes, and nothing
// planned, pending or claimed.
func (d *dataPlane) assertConvergenceResultPair(schema string, observeCheckpoint, planCheckpoint checkpoint, after *checkpoint) {
	d.t.Helper()
	observe := d.captureOneNewJobResult(schema, "observe", observeCheckpoint, after)
	plan := d.captureOneNewJobResult(schema, "plan", planCheckpoint, after)
	if err := convergedResults(d.schema(schema), observe, plan); err != nil {
		d.fatalf("%s did not prove convergence with exact Observe and NoChanges Plan results: %v", schema, err)
	}
}

// createExactApproval approves the schema's current plan by its UID and
// fingerprint, after holding the schema and the plan to the manager and the
// realm the approval is about to trust, and holds the stored approval to the
// decision as written plus who made it and when -- and nothing that names the
// coordination key the plan's realm was derived from.
func (d *dataPlane) createExactApproval(schemaName, planName, approval, coordinationKey, coordinationDigestValue string) {
	d.t.Helper()
	schema, plan := d.schema(schemaName), d.schemaPlan(planName)
	if plan.Spec.Fingerprint == "" || plan.Spec.ExecutionBindingID == "" {
		d.fatalf("%s names no fingerprint or execution binding", planName)
	}
	binding, current := schema.Status.ExecutionBinding, schema.Status.Plan
	stateVersion := d.stateVersion()
	if binding == nil || !executionEpoch.MatchString(binding.Epoch) || binding.Epoch != plan.Spec.ExecutionBindingID ||
		binding.ControllerStateVersion != stateVersion || current == nil ||
		current.Name != planName || current.UID != plan.UID || current.Fingerprint != plan.Spec.Fingerprint ||
		current.ExecutionBindingID != plan.Spec.ExecutionBindingID || current.ControllerImage != d.controller.image ||
		current.ControllerRevision != d.controller.revision || current.ControllerStateVersion != stateVersion {
		d.fatalf("%s current plan is not bound to the exact controller identity", schemaName)
	}
	// The plan is where the bindings live. The approval names it by UID and
	// fingerprint, so what the plan says about its realm, its execution and
	// the manager that published it is checked here, on the plan.
	spec := plan.Spec
	if spec.ContractVersion != fingerprint.CurrentPlanContractVersion || spec.ControllerImage != d.controller.image ||
		spec.ControllerRevision != d.controller.revision || spec.ControllerStateVersion != stateVersion ||
		int64(spec.RunnerProtocolVersion) != d.runnerProtocol || spec.CoordinationDigest != coordinationDigestValue ||
		!sha256Pattern.MatchString(spec.ArtifactDigest) || !digestSuffix.MatchString(spec.ExecutorImage) {
		d.fatalf("%s is not a current-contract plan with the exact controller identity", planName)
	}
	d.mustCreate(approvalDocument(d.in.TestNamespace, approval, schemaName, string(schema.UID), planName, string(plan.UID),
		plan.Spec.Fingerprint))
	stored := &unstructured.Unstructured{}
	stored.SetAPIVersion(ptahSchemaAPIVersion)
	stored.SetKind("PtahSchemaApproval")
	d.mustGet(approval, stored)
	if err := approvalStampedExact(stored,
		ptahv1alpha1.ImmutableObjectReference{Name: schemaName, UID: schema.UID},
		ptahv1alpha1.ImmutableObjectReference{Name: planName, UID: plan.UID},
		plan.Spec.Fingerprint); err != nil || holdsString(stored.Object["spec"], coordinationKey) {
		d.fatalf("%s was not stamped and bound to the exact plan: %v", approval, err)
	}
}

// approvalDocument is an approval of one plan of one schema, as a person
// writes it.
func approvalDocument(namespace, name, schema, schemaUID, plan, planUID, fingerprint string) map[string]any {
	return map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchemaApproval",
		"metadata": map[string]any{"namespace": namespace, "name": name},
		"spec": map[string]any{
			"schemaRef":       map[string]any{"name": schema, "uid": schemaUID},
			"planRef":         map[string]any{"name": plan, "uid": planUID},
			"planFingerprint": fingerprint,
		},
	}
}

// assertApprovalConsumed waits for an approval to keep the history of its
// dispatched Apply after a later observation retired its plan.
func (d *dataPlane) assertApprovalConsumed(approval, planUID string) {
	d.t.Helper()
	d.waitForApproval(approval, "the exact consumed approval to retain its later-observation history",
		func(a *ptahv1alpha1.PtahSchemaApproval) bool { return approvalConsumed(a, planUID) })
}

// assertJobIsolation holds the schema's Jobs, live or as kept evidence, to
// keeping the registry credential and the database URL in separate
// containers.
func (d *dataPlane) assertJobIsolation(schema, secret string, requireApply bool, archived []batchv1.Job) {
	d.t.Helper()
	jobs := archived
	if jobs == nil {
		live := &batchv1.JobList{}
		d.check(d.list(live, client.MatchingLabels{labelSchema: schema}),
			"%s live Jobs could not be read for isolation evidence", schema)
		jobs = live.Items
	}
	if !controllerJobIsolation(jobs, secret, registryAuthSecret, requireApply) {
		d.fatalf("%s Jobs did not isolate authenticated registry access from database credentials", schema)
	}
}

// assertSourceJobIsolation holds the schema's Resolve and Verify Jobs to
// reading the registry, with the credential and authority given, and nothing
// else.
func (d *dataPlane) assertSourceJobIsolation(schema, secret, registrySecret, authMode, policy, requested, resolved string) {
	d.t.Helper()
	jobs := &batchv1.JobList{}
	d.mustList(jobs, client.MatchingLabels{labelSchema: schema})
	if !sourceJobIsolation(jobs.Items, sourceJobIsolationInputs{
		databaseSecret: secret, registrySecret: cmp.Or(registrySecret, registryAuthSecret),
		registryAuthority: d.registryHost, authMode: cmp.Or(authMode, "Environment"),
		executorImage: d.in.ExecutorImage, runnerImage: d.in.RunnerImage, verificationPolicy: policy,
		serviceAccountName: "default", imagePullSecrets: []corev1.LocalObjectReference{},
		requestedReference: requested, resolvedReference: resolved,
	}) {
		d.fatalf("%s source Jobs did not preserve registry/database credential isolation", schema)
	}
}

// assertCoordinationBoundary holds a schema and its plan to the realm digest
// and to never carrying the coordination key it was derived from.
func (d *dataPlane) assertCoordinationBoundary(schemaName, key, digest string) {
	d.t.Helper()
	schema := d.schema(schemaName)
	status, err := asJSON(schema.Status)
	d.check(err, "encode %s status", schemaName)
	if schema.Spec.Target.CoordinationKey != key || schema.Status.Target.CoordinationDigest != digest ||
		schema.Status.Plan == nil || schema.Status.Plan.CoordinationDigest != digest || holdsString(status, key) {
		d.fatalf("%s did not preserve the key-free coordination boundary", schemaName)
	}
	if schema.Status.Plan.Name == "" {
		d.fatalf("%s has no plan for coordination checks", schemaName)
	}
	plan := d.schemaPlan(schema.Status.Plan.Name)
	document, err := asJSON(plan)
	d.check(err, "encode %s", plan.Name)
	if plan.Spec.CoordinationDigest != digest || holdsString(document, key) {
		d.fatalf("%s did not preserve the exact key-free coordination digest", plan.Name)
	}
}

func (d *dataPlane) coordinationLeases() []coordinationv1.Lease {
	d.t.Helper()
	leases := &coordinationv1.LeaseList{}
	d.check(d.cluster.Client.List(d.ctx, leases, client.InNamespace(d.in.OperatorNamespace), client.MatchingLabels{
		labelManagedBy: managedByOperator, "operator.ptah.run/coordination": "database-target",
	}), "list the coordination Leases")
	return leases.Items
}

// checkpointCoordinationLeases is the UIDs of the target Leases that exist now.
func (d *dataPlane) checkpointCoordinationLeases() checkpoint {
	d.t.Helper()
	uids := checkpoint{}
	for _, lease := range d.coordinationLeases() {
		uids = append(uids, string(lease.UID))
	}
	return sortedCheckpoint(uids)
}

// assertCoordinationLeaseBoundary holds the target Leases to exactly one new
// realm since the checkpoint, and to none of them carrying the coordination
// key.
func (d *dataPlane) assertCoordinationLeaseBoundary(key string, before checkpoint) {
	d.t.Helper()
	leases := d.coordinationLeases()
	created := 0
	for _, lease := range leases {
		document, err := asJSON(lease)
		d.check(err, "encode Lease %s", lease.Name)
		if holdsString(document, key) {
			d.fatalf("target Lease did not preserve one exact key-free coordination realm")
		}
		if !before.holds(string(lease.UID)) {
			created++
		}
	}
	if created != 1 {
		d.fatalf("target Lease did not preserve one exact key-free coordination realm")
	}
}

// waitForInSync waits for the schema to converge on the digest after an Apply
// and holds what it applied to this manager, and the cycle that proved it to a
// converged Observe and a NoChanges Plan.
func (d *dataPlane) waitForInSync(schema, digest string, observeCheckpoint, planCheckpoint checkpoint) {
	d.t.Helper()
	matched := d.waitForSchema(schema, "post-apply convergence for "+digest, func(s *ptahv1alpha1.PtahSchema) bool {
		return inSync(s, digest)
	})
	if err := appliedBound(matched, d.controller, d.stateVersion()); err != nil {
		d.fatalf("%s applied evidence is not bound to the exact controller identity: %v", schema, err)
	}
	d.assertConvergenceResultPair(schema, observeCheckpoint, planCheckpoint, nil)
}

// assertPeriodicNoop proves the persisted timer alone starts a read-only cycle
// on a converged schema: one Job of each read-only operation since the
// quiescent checkpoint, none that applies, and a converged result pair.
func (d *dataPlane) assertPeriodicNoop(schema string, quiescent checkpoint) {
	d.t.Helper()
	if d.rbac.paused {
		d.fatalf("periodic no-op proof requires active controller status writes")
	}
	if quiescent == nil {
		d.fatalf("%s periodic no-op proof lacks a quiescent checkpoint", schema)
	}
	for _, operation := range []string{"resolve", "verify", "observe", "plan"} {
		d.waitForOneNewJob(schema, operation, quiescent)
	}
	d.waitForSchema(schema, "a completed periodic no-op observation", func(s *ptahv1alpha1.PtahSchema) bool {
		return s.Status.Phase == ptahv1alpha1.PhaseInSync && s.Status.ActiveOperation == nil
	})
	d.pauseStatusWrites()
	after := d.checkpointJobs(schema, "")
	d.assertReadOnlyCycleBetween(schema, quiescent, after)
	d.assertConvergenceResultPair(schema, quiescent, quiescent, &after)
	d.auditRuntimeCredentials()
}

// assertReadOnlyCycleBetween holds the schema to one Job of each read-only
// operation between the checkpoints and no Apply.
func (d *dataPlane) assertReadOnlyCycleBetween(schema string, before, after checkpoint) {
	d.t.Helper()
	for _, operation := range []string{"resolve", "verify", "observe", "plan"} {
		d.assertOneJobBetween(schema, operation, before, after)
	}
	d.assertNoJobBetween(schema, "apply", before, after)
}

// setReconcileIntervalAndAssertNoop moves the schema to a new interval while
// its status writes are held, so the one cycle the generation change starts
// is the only one, and proves that cycle read-only and converged. Unless the
// writes are to stay held, it then takes the quiescent checkpoint the periodic
// no-op proof counts from and gives the writes back.
func (d *dataPlane) setReconcileIntervalAndAssertNoop(schema, digest, interval string, keepPaused bool) {
	d.t.Helper()
	if d.schemaInterval(schema) == interval {
		d.fatalf("%s interval transition requires a distinct value", schema)
	}
	if !d.rbac.paused {
		d.pauseStatusWrites()
	}
	before := d.checkpointJobs(schema, "")
	d.patchSchema(schema, map[string]any{"spec": map[string]any{"interval": interval}})
	d.mustResumeStatusWrites("could not restore controller status-write RBAC")
	for _, operation := range []string{"resolve", "verify", "observe", "plan"} {
		d.waitForOneNewJob(schema, operation, before)
	}
	d.waitForSchema(schema, "one generation-triggered no-op cycle after selecting interval "+interval,
		func(s *ptahv1alpha1.PtahSchema) bool {
			return s.Status.ObservedGeneration == s.Generation && s.Status.Phase == ptahv1alpha1.PhaseInSync &&
				s.Status.Source.Digest == digest && s.Status.ActiveOperation == nil
		})
	if d.schemaInterval(schema) != interval {
		d.fatalf("%s did not retain reconciliation interval %s", schema, interval)
	}
	d.pauseStatusWrites()
	after := d.checkpointJobs(schema, "")
	d.assertReadOnlyCycleBetween(schema, before, after)
	d.assertConvergenceResultPair(schema, before, before, &after)
	d.auditRuntimeCredentials()
	if !keepPaused {
		d.periodicNoop = d.checkpointJobs(schema, "")
		d.mustResumeStatusWrites("could not restore controller status-write RBAC")
	}
}

// suspendSchemaForTagMove suspends the schema at a new interval, gives the
// status writes back if they were held so the suspension is recorded, and
// waits for it to settle with nothing in flight.
func (d *dataPlane) suspendSchemaForTagMove(schema, interval string) {
	d.t.Helper()
	wasPaused := d.rbac.paused
	d.patchSchema(schema, map[string]any{"spec": map[string]any{"suspend": true, "interval": interval}})
	if wasPaused {
		d.mustResumeStatusWrites("could not restore controller status-write RBAC")
	}
	d.waitForSchema(schema, "a quiescent suspension before moving the mutable tag", quiescentlySuspended)
	if d.schemaInterval(schema) != interval {
		d.fatalf("%s did not retain tag-move interval %s", schema, interval)
	}
}

func (d *dataPlane) resumeSchemaAfterTagMove(schema string) {
	d.t.Helper()
	d.suspend(schema, false)
}

// databaseColumnCount counts the named column of e2e_widgets in the
// lifecycle's database of the engine.
func (d *dataPlane) databaseColumnCount(engine, column string) string {
	d.t.Helper()
	var result string
	var err error
	switch engine {
	case "postgresql":
		result, err = d.psqlDefault("SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='e2e_widgets' AND column_name='" + column + "'")
	case "mysql":
		result, err = d.mysql("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='e2e_widgets' AND column_name='" + column + "'")
	default:
		d.fatalf("unsupported database engine %s", engine)
	}
	d.check(err, "count %s column %s", engine, column)
	return removeWhitespace(result)
}

func (d *dataPlane) assertDatabaseColumn(engine, column string, expected int) {
	d.t.Helper()
	if actual := d.databaseColumnCount(engine, column); actual != strconv.Itoa(expected) {
		d.fatalf("%s column %s count is %s, expected %d", engine, column, actual, expected)
	}
}

// mysqlExactIndexQuery counts the exact single-column index on e2e_widgets.name
// of the uniqueness given.
func mysqlExactIndexQuery(index string, nonUnique int) string {
	return fmt.Sprintf("SELECT count(*) FROM (SELECT index_name FROM information_schema.statistics WHERE table_schema=DATABASE() "+
		"AND table_name='e2e_widgets' AND index_name='%s' GROUP BY index_name HAVING count(*)=1 AND sum(non_unique=%d "+
		"AND column_name='name' AND seq_in_index=1 AND expression IS NULL AND sub_part IS NULL)=1) AS exact_index", index, nonUnique)
}

func (d *dataPlane) assertMySQLUniqueIndex(expected int) {
	d.t.Helper()
	actual, err := d.mysql(mysqlExactIndexQuery("e2e_widgets_name_unique", 0))
	d.check(err, "count the MySQL unique index")
	if removeWhitespace(actual) != strconv.Itoa(expected) {
		d.fatalf("MySQL unique index count is %s, expected %d", removeWhitespace(actual), expected)
	}
}

func (d *dataPlane) assertMySQLPlainIndex(expected int) {
	d.t.Helper()
	actual, err := d.mysql(mysqlExactIndexQuery("e2e_widgets_name_idx", 1))
	d.check(err, "count the MySQL plain index")
	if removeWhitespace(actual) != strconv.Itoa(expected) {
		d.fatalf("MySQL plain index count is %s, expected %d", removeWhitespace(actual), expected)
	}
}

// assertMySQLDeclaredElementOrder reads the backing indexes of a table that
// declares a named foreign key before an unnamed index whose column collides
// with that constraint name. MySQL allocates both names in declaration order.
func (d *dataPlane) assertMySQLDeclaredElementOrder() {
	d.t.Helper()
	order, err := d.mysql("SELECT GROUP_CONCAT(CONCAT(index_name, ':', column_name) ORDER BY index_name, seq_in_index SEPARATOR ',') " +
		"FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='e2e_order_child'")
	d.check(err, "read the MySQL declaration-order indexes")
	if removeLineBreaks(order) != "b:a,b_2:b" {
		d.fatalf("MySQL declaration-order indexes are %s, expected b:a,b_2:b", removeLineBreaks(order))
	}
}
