//go:build e2e

package e2e

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// TestReferenceDataPostgreSQL is the reference-data-postgresql phase.
func TestReferenceDataPostgreSQL(t *testing.T) {
	runReferenceDataPhase(t, phases.ReferenceDataPostgreSQL, "postgresql")
}

// TestReferenceDataMySQL is the reference-data-mysql phase.
func TestReferenceDataMySQL(t *testing.T) {
	runReferenceDataPhase(t, phases.ReferenceDataMySQL, "mysql")
}

// runReferenceDataPhase drives one engine from a database with no tables to a
// declared schema and declared rows the database agrees with. It runs after
// the migration path, in the namespace the data plane stood up, on a database
// of its own.
func runReferenceDataPhase(t *testing.T, phase phases.Of[phases.ReferenceDataInputs], engine string) {
	run, inputs := harness.Begin(t, phase)
	r := newReferenceRun(t, run, inputs, engine)
	for _, scenario := range []struct {
		name string
		body func()
	}{
		{"declared-row-values", r.declaredRowValues},
		{engine + "-reference-data", r.engineReferenceData},
	} {
		if !run.Scenario(scenario.name, r.scenario(scenario.body)) {
			return
		}
	}
	r.finishFixture()
	run.Logf("e2e reference data: PASS %s declared rows, with no row value in status, Events, or logs", r.engine.kind)
}

// Keep the proven source available for alerting's independent fixtures, but
// stop periodic reads from moving its plan pins during later acceptance.
func (r *referenceRun) finishFixture() {
	r.t.Helper()
	uid := r.status().UID
	r.check(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := r.status()
		if current.UID != uid {
			return fmt.Errorf("completed reference fixture was replaced")
		}
		before := current.DeepCopy()
		current.Spec.Suspend = true
		return r.cluster.Client.Patch(r.ctx, current,
			client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}), "suspend the completed reference fixture")
	r.check(harness.Wait(r.ctx, "the completed reference fixture to stop reconciling", waitTimeout, time.Second,
		func(context.Context) (bool, string, error) {
			current := r.status()
			if current.UID != uid {
				return false, "", fmt.Errorf("completed reference fixture was replaced")
			}
			return current.Spec.Suspend && current.Status.ObservedGeneration == current.Generation &&
				current.Status.Phase == ptahv1alpha1.PhaseSuspended && current.Status.ActiveOperation == nil &&
				current.Status.PendingLockRelease == nil, "waiting for the original reference fixture to suspend", nil
		}), "retire the completed reference fixture")
}

// referenceRun is what the reference-data proofs share. Each scenario runs as
// a subtest, and t is that subtest while it runs.
type referenceRun struct {
	t      *testing.T
	parent *testing.T
	ctx    context.Context
	in     phases.ReferenceDataInputs

	cluster *harness.Cluster
	engine  migrationEngine
	names   referenceNames
	// rerun is the suffix hack/e2e-rerun-phase.sh gave this run, and
	// repository where it publishes.
	rerun, repository string

	workDir     string
	kubectlPtah string
	// password is the data plane's application password for the engine, and
	// url the reference database's URL built with it. Both are protected.
	password, url string
	scanner       credentialScanner
	rows          referenceRowScanner

	// plan is the plan the last plan wait found.
	plan string
}

func newReferenceRun(t *testing.T, run *harness.Run, in phases.ReferenceDataInputs, engine string) *referenceRun {
	t.Helper()
	r := &referenceRun{t: t, parent: t, ctx: run.Context(), in: in}
	// Registered before anything is created, so a setup that fails part way
	// still reports and removes what it made.
	t.Cleanup(r.cleanup)
	for _, command := range []string{"kubectl", "go"} {
		if _, err := exec.LookPath(command); err != nil {
			r.fatalf("required command is not installed: %s", command)
		}
	}
	if info, err := os.Stat(in.Kubeconfig); err != nil || !info.Mode().IsRegular() {
		r.fatalf("E2E_KUBECONFIG does not name a file")
	}
	// The suite a phase runs in names one engine, and this phase runs that
	// one. The value is checked rather than defaulted: a phase that silently
	// ran one engine because the driver forgot to name it is coverage nobody
	// would notice was gone.
	if err := referenceInputsOK(in.Engine, engine, in.ExecutorImage, in.RunnerImage); err != nil {
		r.fatalf("%v", err)
	}
	var err error
	if r.engine, err = migrationEngineFor(engine); err != nil {
		r.fatalf("%v", err)
	}
	// E2E_PHASE_RERUN is not a driver input: hack/e2e-rerun-phase.sh sets it
	// when it puts this phase back on a lab an earlier run left behind.
	r.rerun = os.Getenv("E2E_PHASE_RERUN")
	if r.repository, err = referenceRepository(r.rerun); err != nil {
		r.fatalf("%v", err)
	}
	registryHost := in.RegistryService + "." + in.TestNamespace + ".svc.cluster.local:5000"
	r.names = referenceNamesFor(engine, registryHost, r.repository)
	if info, err := os.Stat(r.policyFile()); err != nil || !info.Mode().IsRegular() {
		r.fatalf("verification policy fixture is missing")
	}
	if r.cluster, err = harness.Connect(in.Kubeconfig); err != nil {
		r.fatalf("%v", err)
	}
	r.workDir, err = os.MkdirTemp("", "ptah-operator-reference-data-e2e.")
	r.check(err, "create the work directory")
	r.check(os.Chmod(r.workDir, 0o700), "make the work directory private")
	// The reference-data view is proved from the same build a user installs,
	// against the live resource, rather than from a golden file.
	r.kubectlPtah = filepath.Join(r.workDir, "kubectl-ptah")
	build := exec.CommandContext(r.ctx, "go", "build", "-trimpath", "-o", r.kubectlPtah, "./cmd/kubectl-ptah") //nolint:gosec // Arguments, not a shell.
	build.Dir = repositoryRoot
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	r.check(build.Run(), "build kubectl-ptah")

	// The password is read back from the Secret the data plane created rather
	// than derived a second time here. A second derivation is a second
	// definition, and the two would part company the moment either moved.
	secret := &corev1.Secret{}
	if err := r.get(r.engine.sourceSecret, secret); err != nil {
		r.fatalf("the data plane %s Secret %s could not be read", engine, r.engine.sourceSecret)
	}
	r.password = string(secret.Data["password"])
	if r.password == "" {
		r.fatalf("the data plane %s Secret carries no password", engine)
	}
	r.url = r.engine.databaseURL(in.TestNamespace, migrationDatabaseUser, r.password, referenceDatabase)
	r.scanner, err = newCredentialScanner(r.password, r.url)
	r.check(err, "build the credential scanner")
	return r
}

func (r *referenceRun) scenario(body func()) func(*testing.T) {
	return func(t *testing.T) {
		r.t = t
		defer func() { r.t = r.parent }()
		body()
	}
}

func (r *referenceRun) fatalf(format string, arguments ...any) {
	r.t.Helper()
	r.t.Fatalf("e2e reference data: "+format, arguments...)
}

func (r *referenceRun) logf(format string, arguments ...any) {
	r.t.Helper()
	r.t.Logf("e2e reference data: "+format, arguments...)
}

func (r *referenceRun) check(err error, format string, arguments ...any) {
	r.t.Helper()
	if err != nil {
		r.fatalf("%s: %v", fmt.Sprintf(format, arguments...), err)
	}
}

// sleep pauses between two readings, and ends the scenario when the phase's
// own bound ends first.
func (r *referenceRun) sleep(duration time.Duration) {
	r.t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-r.ctx.Done():
		r.fatalf("the phase's bound ended while it waited: %v", r.ctx.Err())
	case <-timer.C:
	}
}

// referenceWithin is the bound every wait names when it gives up, in the seconds the
// script counted.
func referenceWithin() string {
	return fmt.Sprintf("%.0fs", waitTimeout.Seconds())
}

func (r *referenceRun) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: r.in.TestNamespace, Name: name}
}

func (r *referenceRun) get(name string, object client.Object) error {
	return r.cluster.Client.Get(r.ctx, r.key(name), object)
}

func (r *referenceRun) list(list client.ObjectList, options ...client.ListOption) error {
	return r.cluster.Client.List(r.ctx, list, append([]client.ListOption{client.InNamespace(r.in.TestNamespace)}, options...)...)
}

// create sends one document as kubectl create -f did, with strict field
// validation, and returns what the API server said.
func (r *referenceRun) create(document map[string]any) error {
	return r.cluster.Client.Create(r.ctx, &unstructured.Unstructured{Object: document},
		client.FieldOwner(harness.FieldOwner), client.FieldValidation("Strict"))
}

// apply sends one document as kubectl apply -f did: created when it is new,
// brought to what it says when it is not.
func (r *referenceRun) apply(document map[string]any) error {
	return r.cluster.Client.Patch(r.ctx, &unstructured.Unstructured{Object: document}, client.Apply, //nolint:staticcheck // The typed Apply needs generated apply configurations the API does not ship.
		client.FieldOwner(harness.FieldOwner), client.ForceOwnership, client.FieldValidation("Strict"))
}

// patchSchema merge-patches the reference schema, as kubectl patch
// --type=merge did.
func (r *referenceRun) patchSchema(patch map[string]any) error {
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Namespace, schema.Name = r.in.TestNamespace, r.names.schema
	return r.cluster.Client.Patch(r.ctx, schema, client.RawPatch(types.MergePatchType, body),
		client.FieldOwner(harness.FieldOwner))
}

// deleteAndWait deletes an object and waits until it is gone, as kubectl
// delete --wait=true did, with kubectl's background propagation.
func (r *referenceRun) deleteAndWait(object client.Object, description string) {
	r.t.Helper()
	if err := r.cluster.Client.Delete(r.ctx, object, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil &&
		!apierrors.IsNotFound(err) {
		r.fatalf("%s was not removed: %v", description, err)
	}
	key := client.ObjectKeyFromObject(object)
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		current := object.DeepCopyObject().(client.Object)
		err := r.cluster.Client.Get(r.ctx, key, current)
		if apierrors.IsNotFound(err) || (err == nil && object.GetUID() != "" && current.GetUID() != object.GetUID()) {
			return
		}
		r.sleep(time.Second)
	}
	r.fatalf("%s was not removed", description)
}

func (r *referenceRun) policyFile() string {
	return filepath.Join(repositoryRoot, "testdata", "e2e", "verification-policy.yaml")
}

// scan refuses evidence that carries a value only the Pod should ever hold.
// Every assertion reads through it.
func (r *referenceRun) scan(content []byte, description string) {
	r.t.Helper()
	if !r.scanner.ready() {
		r.fatalf("credential scanner has no non-empty protected patterns")
	}
	if r.scanner.leaks(content) {
		r.fatalf("%s carries a database credential", description)
	}
}

// scanRows is the refusal this phase exists to measure: "table rows never
// reach status, Events, or ordinary logs". A matching line is printed, at most
// three of them, unless it also carries a credential.
func (r *referenceRun) scanRows(content []byte, description string) {
	r.t.Helper()
	if !r.rows.ready() {
		r.fatalf("row scanner has no declared values to look for")
	}
	matches := r.rows.matches(content)
	if len(matches) == 0 {
		return
	}
	for index, line := range matches {
		if index == 3 {
			break
		}
		if r.scanner.ready() && r.scanner.leaks([]byte(line)) {
			_, _ = fmt.Fprintln(os.Stderr, "e2e reference data: a matching line is withheld: it carries a database credential")
			continue
		}
		_, _ = fmt.Fprintln(os.Stderr, line)
	}
	r.fatalf("%s carries a declared row value", description)
}

// scanBoth runs the credential scan and then the row scan, in the order the
// script ran them.
func (r *referenceRun) scanBoth(content []byte, description string) {
	r.t.Helper()
	r.scan(content, description)
	r.scanRows(content, description)
}

func (r *referenceRun) scanObject(object any, description string) {
	r.t.Helper()
	content, err := json.MarshalIndent(object, "", "    ")
	r.check(err, "encode %T", object)
	r.scanBoth(content, description)
}

// query is sql_value against the reference database: what the client
// printed, with every whitespace character removed, since the two clients pad
// a value differently. A failed exec reads as whatever it printed, which a
// caller catches by comparing the value it did not get.
func (r *referenceRun) query(statement string) string {
	output, _ := databaseSQL(r.ctx, r.cluster, r.in.TestNamespace, r.engine, referenceDatabase, statement)
	return trimmedSQL(output)
}

// statement runs one statement for its status. A guard on query would read
// the trim and never the exec, so a statement the phase must see succeed runs
// here.
func (r *referenceRun) statement(statement string) error {
	_, err := databaseSQL(r.ctx, r.cluster, r.in.TestNamespace, r.engine, referenceDatabase, statement)
	return err
}

// resetAfterAnEarlierRun removes what an earlier run of this phase created,
// so a rerun starts where the first run did: no reference schema, and a
// database with no tables.
//
// Only this phase's objects go, by the names the run gives them. The data
// plane's PtahSchemas share the kind and the namespace, so nothing here is
// deleted by kind alone. The plans the reference schema published are owned
// by it and go with it; the approvals are not owned, so they are removed by
// the name every one of them starts with. The verification policy stays: it
// is applied rather than created.
func (r *referenceRun) resetAfterAnEarlierRun() {
	r.t.Helper()
	if r.rerun == "" {
		return
	}
	r.logf("rerun %s: removing what an earlier run of this phase left behind", r.rerun)
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Namespace, schema.Name = r.in.TestNamespace, r.names.schema
	r.deleteAndWait(schema, r.names.schema)
	secret := &corev1.Secret{}
	secret.Namespace, secret.Name = r.in.TestNamespace, r.names.secret
	if err := r.cluster.Client.Delete(r.ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		r.fatalf("%s was not removed: %v", r.names.secret, err)
	}
	approvals := &ptahv1alpha1.PtahSchemaApprovalList{}
	configMaps := &corev1.ConfigMapList{}
	jobs := &batchv1.JobList{}
	for _, list := range []client.ObjectList{approvals, configMaps, jobs} {
		if err := r.list(list); err != nil {
			r.fatalf("the objects an earlier run left behind could not be listed: %v", err)
		}
	}
	var leftovers []client.Object
	for index := range approvals.Items {
		if referenceLeftover(r.names, "PtahSchemaApproval", approvals.Items[index].Name) {
			leftovers = append(leftovers, &approvals.Items[index])
		}
	}
	for index := range configMaps.Items {
		if referenceLeftover(r.names, "ConfigMap", configMaps.Items[index].Name) {
			leftovers = append(leftovers, &configMaps.Items[index])
		}
	}
	for index := range jobs.Items {
		if referenceLeftover(r.names, "Job", jobs.Items[index].Name) {
			leftovers = append(leftovers, &jobs.Items[index])
		}
	}
	for _, object := range leftovers {
		r.deleteAndWait(object, fmt.Sprintf("%T %s", object, object.GetName()))
	}
	// FORCE ends the sessions an interrupted run left open.
	statement := "DROP DATABASE IF EXISTS " + referenceDatabase + " WITH (FORCE)"
	if r.engine.name == "mysql" {
		statement = "DROP DATABASE IF EXISTS " + referenceDatabase
	}
	if _, err := serverSQL(r.ctx, r.cluster, r.in.TestNamespace, r.engine, statement); err != nil {
		r.fatalf("database %s could not be dropped on %s: %v", referenceDatabase, r.engine.name, err)
	}
}

// declaredRowValues clears an earlier run's leftovers on a rerun, reads the
// values the row scanner looks for, and applies the verification policy the
// reference schema names.
func (r *referenceRun) declaredRowValues() {
	r.t.Helper()
	r.resetAfterAnEarlierRun()
	values, err := referenceDeclaredRowValues(repositoryRoot)
	if err != nil {
		r.fatalf("%v", err)
	}
	if r.rows, err = newReferenceRowScanner(values...); err != nil {
		r.fatalf("%v", err)
	}
	content, err := os.ReadFile(r.policyFile())
	r.check(err, "verification policy fixture is missing")
	r.check(r.apply(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "immutable": true,
		"metadata": map[string]any{"namespace": r.in.TestNamespace, "name": referencePolicy},
		"data":     map[string]any{referencePolicyKey: string(content)},
	}), "apply ConfigMap %s", referencePolicy)
	if !bytes.Contains(content, []byte(referenceSchemaArtifactType)) {
		r.fatalf("the verification policy does not pin the schema artifact type")
	}
}

// createReferenceDatabase creates the database this proof reconciles, on the
// server the data plane already runs, and one this suite has not touched
// before, then stores its URL in the schema's Secret.
func (r *referenceRun) createReferenceDatabase() {
	r.t.Helper()
	lookup := "SELECT count(*) FROM pg_database WHERE datname='" + referenceDatabase + "'"
	if r.engine.name == "mysql" {
		lookup = "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='" + referenceDatabase + "'"
	}
	existing, err := serverSQL(r.ctx, r.cluster, r.in.TestNamespace, r.engine, lookup)
	r.check(err, "database %s could not be looked up on %s", referenceDatabase, r.engine.name)
	if trimmedSQL(existing) != "0" {
		r.fatalf("database %s already exists on %s; the reference-data proof needs a database with no tables, so rerun this phase through hack/e2e-rerun-phase.sh, which clears it",
			referenceDatabase, r.engine.name)
	}
	create := "CREATE DATABASE " + referenceDatabase
	if r.engine.name == "mysql" {
		// The unprivileged user the operation Pod connects as owns nothing by
		// default, and MySQL grants are per schema: the grant is part of
		// creating the database rather than a separate setup step.
		create = "CREATE DATABASE " + referenceDatabase + "; GRANT ALL PRIVILEGES ON " + referenceDatabase +
			".* TO '" + migrationDatabaseUser + "'@'%'; FLUSH PRIVILEGES"
	}
	if _, err := serverSQL(r.ctx, r.cluster, r.in.TestNamespace, r.engine, create); err != nil {
		r.fatalf("database %s could not be created: %v", referenceDatabase, err)
	}
	r.check(r.apply(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "immutable": true, "type": "Opaque",
		"metadata": map[string]any{"namespace": r.in.TestNamespace, "name": r.names.secret},
		"data": secretData(map[string]string{
			"username": migrationDatabaseUser, "password": r.password, "database": referenceDatabase, "url": r.url,
		}),
	}), "apply Secret %s", r.names.secret)
}

// publish publishes one revision of the declared schema and rows, waits for
// the publisher to finish, and returns the digest it reported.
func (r *referenceRun) publish(revision string) string {
	return r.publishWithCheck(revision, nil)
}

// Keep collecting dependent workload results while their missing artifact is
// published. Blocking the collector here could lose them to the Job TTL.
func (r *referenceRun) publishWithCheck(revision string, check func()) string {
	r.t.Helper()
	directory := filepath.Join(repositoryRoot, "testdata", "e2e", "reference", revision)
	configMap := r.names.configMapPrefix + revision
	name := r.names.jobPrefix + revision
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		r.fatalf("reference-data fixtures are missing: %s", directory)
	}
	r.logf("publishing the %s declared schema and rows as %s", r.engine.kind, revision)
	data, mounts, err := referenceFixtureFiles(directory)
	r.check(err, "read the reference-data fixtures in %s", directory)
	r.check(r.create(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"namespace": r.in.TestNamespace, "name": configMap},
		"data":     data,
	}), "create ConfigMap %s", configMap)
	if len(mounts) == 0 {
		r.fatalf("no reference-data files to publish from %s", directory)
	}
	r.check(r.create(referencePublisherJob(r.in.TestNamespace, name, r.in.ExecutorImage, configMap,
		r.names.artifact, revision, mounts)), "create Job %s", name)
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if check != nil {
			check()
		}
		job := &batchv1.Job{}
		if r.get(name, job) == nil {
			switch referencePublisherState(job) {
			case "complete":
				return r.publishedDigest(job)
			case "failed":
				r.fatalf("publishing the %s reference-data artifact %s failed", r.engine.kind, revision)
			}
		}
		r.sleep(referencePoll)
	}
	r.fatalf("the %s reference-data artifact %s was not published within %s", r.engine.kind, revision, referenceWithin())
	return ""
}

// publishedDigest reads the digest the publisher reported from its Pod's log.
// The log carries the artifact's digest and nothing the declaration holds.
func (r *referenceRun) publishedDigest(job *batchv1.Job) string {
	r.t.Helper()
	pods := &corev1.PodList{}
	r.check(r.list(pods, client.MatchingLabels{"job-name": job.Name}), "list the Pods of Job %s", job.Name)
	owned := ownedPods(pods.Items, job.UID)
	if len(owned) == 0 {
		r.fatalf("Job %s has no Pod to read the published digest from", job.Name)
	}
	logs, err := r.cluster.ContainerLog(r.ctx, r.in.TestNamespace, owned[0].Name, "publisher")
	r.check(err, "read the logs of Job %s", job.Name)
	digests := publishedDigests(logs)
	if len(digests) == 0 || !sha256Pattern.MatchString(digests[len(digests)-1]) {
		r.fatalf("could not read the published digest from Job %s", job.Name)
	}
	return digests[len(digests)-1]
}

// createReferenceResource creates the reference schema.
func (r *referenceRun) createReferenceResource() {
	r.t.Helper()
	r.check(r.create(referenceSchemaDocument(r.in.TestNamespace, r.engine.kind, r.names)),
		"create PtahSchema %s", r.names.schema)
}

// status reads the schema and scans it for credentials and declared rows.
func (r *referenceRun) status() *ptahv1alpha1.PtahSchema {
	r.t.Helper()
	schema := &ptahv1alpha1.PtahSchema{}
	if err := r.get(r.names.schema, schema); err != nil {
		r.fatalf("%s could not be read", r.names.schema)
	}
	r.scanObject(schema, r.names.schema+" status")
	return schema
}

// phase is the schema's phase, or nothing when it cannot be read.
func (r *referenceRun) phase() ptahv1alpha1.ReconciliationPhase {
	schema := &ptahv1alpha1.PtahSchema{}
	if r.get(r.names.schema, schema) != nil {
		return ""
	}
	return schema.Status.Phase
}

// waitForPhase waits for the schema to reach a phase. A schema that fails is
// not waited out unless Failed is what the wait is for.
func (r *referenceRun) waitForPhase(want ptahv1alpha1.ReconciliationPhase) {
	r.t.Helper()
	var observed ptahv1alpha1.ReconciliationPhase
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		observed = r.phase()
		if observed == want {
			return
		}
		if observed == ptahv1alpha1.PhaseFailed && want != ptahv1alpha1.PhaseFailed {
			r.fatalf("%s failed while waiting for %s", r.names.schema, want)
		}
		r.sleep(referencePoll)
	}
	r.fatalf("%s did not reach %s within %s; it is in %s", r.names.schema, want, referenceWithin(), cmp.Or(string(observed), "<none>"))
}

// waitForRefusal waits for one reading that carries the whole protected-table
// refusal, and returns it, so the caller asserts against the reading that
// matched rather than re-reading and reopening the window.
func (r *referenceRun) waitForRefusal() *ptahv1alpha1.PtahSchema {
	r.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		schema := r.status()
		if referenceRefusalComplete(schema) {
			return schema
		}
		r.sleep(referencePoll)
	}
	r.fatalf("%s never reported a complete protected-table refusal within %s", r.names.schema, referenceWithin())
	return nil
}

// waitForCondition waits for one condition to hold with the reason that
// explains it, and returns the reading that satisfied it.
func (r *referenceRun) waitForCondition(kind string, status metav1.ConditionStatus, reason string) *ptahv1alpha1.PtahSchema {
	r.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		schema := r.status()
		if referenceExactlyOneCondition(schema.Status.Conditions, kind, status, reason) {
			return schema
		}
		r.sleep(referencePoll)
	}
	r.fatalf("%s did not report %s=%s (%s) within %s", r.names.schema, kind, status, reason, referenceWithin())
	return nil
}

// waitForPlan waits for the schema to publish a plan, records its name, and
// returns the reading that named it.
func (r *referenceRun) waitForPlan() *ptahv1alpha1.PtahSchema {
	r.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		schema := &ptahv1alpha1.PtahSchema{}
		if r.get(r.names.schema, schema) == nil {
			if name := referencePlanName(schema); name != "" {
				r.plan = name
				return schema
			}
		}
		r.sleep(referencePoll)
	}
	r.fatalf("%s published no plan within %s", r.names.schema, referenceWithin())
	return nil
}

// approve writes the approval a plan needs, read off the plan itself, and
// returns what the API server said. The plan has to be readable.
func (r *referenceRun) approve(name, planName string) error {
	r.t.Helper()
	plan := &ptahv1alpha1.PtahSchemaPlan{}
	if err := r.get(planName, plan); err != nil {
		r.fatalf("plan %s could not be read", planName)
	}
	return r.create(referenceApprovalDocument(r.in.TestNamespace, name, r.names.schema, plan))
}

// assertDeclaredRows reads the managed tables back through the database
// rather than through the operator, which is the only reading that can say
// the rows were applied rather than reported.
func (r *referenceRun) assertDeclaredRows(regions, countries, czechia string) {
	r.t.Helper()
	observedRegions := r.query("SELECT count(*) FROM regions")
	observedCountries := r.query("SELECT count(*) FROM countries")
	observedCzechia := r.query("SELECT name FROM countries WHERE code = 'CZ'")
	if err := referenceRowsMismatch(observedRegions, observedCountries, observedCzechia, regions, countries, czechia); err != nil {
		r.fatalf("%v", err)
	}
	// The foreign key is part of the declaration, so a row set that satisfied
	// the counts while pointing at nothing would still be wrong.
	orphans := r.query("SELECT count(*) FROM countries c LEFT JOIN regions r ON c.region_code = r.code WHERE r.code IS NULL")
	if orphans != "0" {
		r.fatalf("%s countries reference a region that does not exist", orphans)
	}
}

// assertRepeatedReconciliationChangesNothing: a repeated reconciliation with
// no changes issues no DML. The evidence is the resource staying in its
// converged cycle and settling back into InSync, and the rows untouched.
func (r *referenceRun) assertRepeatedReconciliationChangesNothing() {
	r.t.Helper()
	before := r.query("SELECT count(*) FROM countries")
	r.check(r.patchSchema(map[string]any{"spec": map[string]any{"interval": referenceRepeatInterval}}),
		"patch the interval of %s", r.names.schema)
	for window := time.Now().Add(referenceRepeatWindow); time.Now().Before(window); {
		if !referenceRepeatAllowed(r.status().Status.Phase) {
			r.fatalf("%s left the converged cycle while nothing had changed", r.names.schema)
		}
		r.sleep(referenceRepeatPoll)
	}
	r.waitForPhase(ptahv1alpha1.PhaseInSync)
	if after := r.query("SELECT count(*) FROM countries"); before != after {
		r.fatalf("a reconciliation with no declared change rewrote the managed rows")
	}
	r.check(r.patchSchema(map[string]any{"spec": map[string]any{"interval": referenceInterval}}),
		"patch the interval of %s", r.names.schema)
}

// assertDataOnlyChangeReconciles: a change to reference rows alone triggers
// planning, because no DDL change is not a reason to skip data
// reconciliation.
//
// The second revision declares the child table's rows, which the first
// revision deliberately left undeclared: Ptah emits declared rows in an order
// that can put a child row before the parent it references
// (stokaro/ptah#3252), so the parent rows arrive first and the child rows
// meet a foreign key they satisfy. The tables themselves are unchanged, so
// this is still a plan with no DDL in it.
func (r *referenceRun) assertDataOnlyChangeReconciles() {
	r.t.Helper()
	r.publish("v2")
	r.waitForPhase(ptahv1alpha1.PhaseAwaitingApproval)
	planned := r.waitForPlan()
	// The reading that named the plan is the one the claim is about.
	r.scanObject(planned, r.names.schema+" status")
	if !referencePlanHasStatements(planned) {
		r.fatalf("a data-only change produced a plan with no statements")
	}
	if err := r.approve(r.names.approval+"-v2", r.plan); err != nil {
		r.fatalf("the data-only plan could not be approved: %v", err)
	}
	r.waitForPhase(ptahv1alpha1.PhaseInSync)
	r.assertDeclaredRows("2", "3", "Czech Republic")
}

// assertKubectlPtahSchemaLine holds the reference-data view to the drift the
// proof created by hand, and to the rule that a count is all it may say. The
// scanners carry the second claim: they look for the values the fixtures
// declare, so a view that started printing a row fails on the value it
// printed.
//
// It waits rather than reads once. The phase says the operator has observed
// and planned; the view is a second read, and a poll that lands between them
// would report the harness.
func (r *referenceRun) assertKubectlPtahSchemaLine(want string) {
	r.t.Helper()
	var view []byte
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		command := exec.CommandContext(r.ctx, r.kubectlPtah, "schema", r.names.schema, //nolint:gosec // The phase's own build.
			"--kubeconfig", r.in.Kubeconfig, "-n", r.in.TestNamespace)
		var stdout bytes.Buffer
		command.Stdout, command.Stderr = &stdout, os.Stderr
		if err := command.Run(); err != nil {
			r.fatalf("kubectl ptah schema could not read %s", r.names.schema)
		}
		view = stdout.Bytes()
		r.scanBoth(view, "the kubectl ptah schema view")
		if !referenceViewHasLine(view, referenceSchemaViewLine(r.in.TestNamespace, r.names.schema)) {
			r.fatalf("kubectl ptah schema does not name the resource it read")
		}
		if referenceViewHasLine(view, want) {
			return
		}
		r.sleep(referencePoll)
	}
	_, _ = fmt.Fprintln(os.Stderr, "e2e reference data: the view said:")
	for line := range strings.SplitSeq(strings.TrimRight(string(view), "\n"), "\n") {
		_, _ = fmt.Fprintf(os.Stderr, "e2e reference data:   %s\n", line)
	}
	r.fatalf("kubectl ptah schema never reported [%s] within %s", want, referenceWithin())
}

// assertExternalEditRefusesAStaleApproval: a change made after approval is
// never silently overwritten by a stale plan. The row is edited in the
// database between the plan and the approval, so the approval names a plan
// whose observed state no longer holds.
func (r *referenceRun) assertExternalEditRefusesAStaleApproval() {
	r.t.Helper()
	if err := r.statement("UPDATE countries SET name = 'Edited outside the operator' WHERE code = 'US'"); err != nil {
		r.fatalf("the external edit could not be made: %v", err)
	}
	r.waitForPhase(ptahv1alpha1.PhaseAwaitingApproval)
	r.waitForPlan()
	// One managed row differs, and this proof is what made it differ, so the
	// counts are known rather than read back from the thing under test.
	r.assertKubectlPtahSchemaLine(referenceExternalEditLine)
	stale := r.plan
	if err := r.statement("UPDATE countries SET name = 'Edited again outside the operator' WHERE code = 'US'"); err != nil {
		r.fatalf("the second external edit could not be made: %v", err)
	}
	// The operator observes the second edit and publishes a plan for it. The
	// approval below still names the first one, which is the stale decision.
	for deadline := time.Now().Add(waitTimeout); time.Now().Before(deadline); {
		r.waitForPlan()
		if r.plan != stale {
			break
		}
		r.sleep(referencePoll)
	}
	if r.plan == stale {
		r.fatalf("the second external edit produced no new plan")
	}
	err := r.approve(r.names.approval+"-stale", stale)
	if err == nil {
		r.fatalf("an approval naming the replaced plan was accepted")
	}
	// A name an earlier approval already holds is a failure of the harness,
	// not the refusal this row is about, and its message names an approval
	// as well as any refusal would.
	if apierrors.IsAlreadyExists(err) {
		r.fatalf("the approval naming the replaced plan was never judged: %v", err)
	}
	refusal := []byte(err.Error())
	if !referenceRefusalNamesWhatItRefused(err.Error()) {
		r.fatalf("the refusal of a replaced-plan approval did not say what it refused")
	}
	r.scanBoth(refusal, "the stale approval refusal")
	// The current plan is approvable, and applying it puts the declared value
	// back: an external edit is drift, not a new declaration.
	if err := r.approve(r.names.approval+"-recovered", r.plan); err != nil {
		r.fatalf("the current plan could not be approved: %v", err)
	}
	r.waitForPhase(ptahv1alpha1.PhaseInSync)
	if restored := r.query("SELECT name FROM countries WHERE code = 'US'"); restored != trimmedSQL("United States") {
		r.fatalf("the declared value was not restored after the external edit: %s", restored)
	}
}

// assertRemovedDeclarationKeepsRows: a removed declaration ends management;
// it does not delete rows.
//
// The schema is still InSync on the revision before when the publish ends, so
// a phase alone would settle the wait on its first reading and the row would
// pass without v3 ever being reconciled. The wait is for the resource to
// settle on the digest v3 was published as, and the approval decision is made
// on the reading that matched.
func (r *referenceRun) assertRemovedDeclarationKeepsRows() {
	r.t.Helper()
	digest := r.publish("v3")
	var settled, last *ptahv1alpha1.PtahSchema
	for deadline := time.Now().Add(waitTimeout); time.Now().Before(deadline); {
		schema := &ptahv1alpha1.PtahSchema{}
		if r.get(r.names.schema, schema) == nil {
			last = schema
			if referenceRemovalReconciled(schema, digest) {
				settled = schema
				break
			}
			// A failure on the withdrawn declaration is its verdict; one on the
			// revision before it is not.
			if schema.Status.Phase == ptahv1alpha1.PhaseFailed && schema.Status.Source.Digest == digest {
				r.fatalf("%s failed while waiting for InSync", r.names.schema)
			}
		}
		r.sleep(referencePoll)
	}
	if settled == nil {
		if last != nil {
			r.logf("the last reading was phase %s on digest %s", cmpOrNone(string(last.Status.Phase)),
				cmpOrNone(last.Status.Source.Digest))
		}
		r.fatalf("%s did not settle on the withdrawn declaration %s within %s", r.names.schema, digest, referenceWithin())
	}
	r.scanObject(settled, r.names.schema+" status")
	if settled.Status.Phase == ptahv1alpha1.PhaseAwaitingApproval {
		r.waitForPlan()
		if err := r.approve(r.names.approval+"-v3", r.plan); err != nil {
			r.fatalf("the plan after the removed declaration could not be approved: %v", err)
		}
	}
	r.waitForPhase(ptahv1alpha1.PhaseInSync)
	if remaining := r.query("SELECT count(*) FROM countries"); remaining != "3" {
		r.fatalf("removing the declaration changed the managed rows: countries holds %s, want 3", remaining)
	}
}

// assertDeclaredEmptySetClearsTheRows: withdrawing a declaration and declaring
// an empty set are different statements about the same table. The revision
// before this one withdrew the declaration and the three rows stayed. This one
// declares the table again and says it holds nothing.
//
// Against the pinned executor on PostgreSQL 17 this fixture plans three
// DELETEs, each classified destructive, so the policy holds the plan at the
// approval gate. A run that found no plan would mean the empty set was read as
// no declaration at all, which is the confusion this row is about.
func (r *referenceRun) assertDeclaredEmptySetClearsTheRows() {
	r.t.Helper()
	if before := r.query("SELECT count(*) FROM countries"); before != "3" {
		r.fatalf("the empty-set proof starts from %s countries, and the rows the withdrawn declaration left are three", before)
	}
	r.publish("v4")
	r.waitForPhase(ptahv1alpha1.PhaseAwaitingApproval)
	planned := r.waitForPlan()
	r.scanObject(planned, r.names.schema+" status")
	if !referencePlanDestructive(planned) {
		r.fatalf("the plan for an explicitly empty declared set is not marked destructive")
	}
	if err := r.approve(r.names.approval+"-v4", r.plan); err != nil {
		r.fatalf("the empty-set plan could not be approved: %v", err)
	}
	r.waitForPhase(ptahv1alpha1.PhaseInSync)
	if after := r.query("SELECT count(*) FROM countries"); after != "0" {
		r.fatalf("an explicitly empty declared set left %s rows in countries", after)
	}
	// The other declared table is untouched: an empty set clears the table
	// that declares it and says nothing about any other.
	if r.query("SELECT count(*) FROM regions") != "2" {
		r.fatalf("the empty countries declaration changed the regions rows")
	}
}

// assertAProtectedTableRefusesTheChange is the fence. spec.policy.
// protectedTables names a declared table, and a revision that would change
// its rows is refused rather than rated: no plan is published, no approval
// can be offered, and the table keeps what it holds.
//
// Then the entry goes, and the same revision converges. A fence that refused
// everything forever would pass a proof that only measured the refusal.
func (r *referenceRun) assertAProtectedTableRefusesTheChange() {
	r.t.Helper()
	if before := r.query("SELECT count(*) FROM countries"); before != "0" {
		r.fatalf("the fence proof starts from %s countries, and the emptied set left none", before)
	}
	r.logf("fencing the %s countries table off from the declarative path", r.engine.kind)
	if err := r.patchSchema(map[string]any{"spec": map[string]any{"policy": map[string]any{"protectedTables": []any{"countries"}}}}); err != nil {
		r.fatalf("the protected table could not be added to %s: %v", r.names.schema, err)
	}
	r.publish("v5")
	refused := r.waitForRefusal()
	// The reading that satisfied the wait: no plan, and the refusal named on
	// the conditions a reader looks at.
	if refused.Status.Plan != nil {
		r.fatalf("a plan was published for a change to a protected table")
	}
	// A refusal is not a fault, and the phase a reader sees says so.
	if refused.Status.Phase != ptahv1alpha1.PhaseBlocked {
		r.fatalf("a fenced change left the resource in phase %s", refused.Status.Phase)
	}
	if !referenceExactlyOneCondition(refused.Status.Conditions, "Ready", metav1.ConditionFalse, "ProtectedTable") {
		r.fatalf("the protected-table refusal is not readable on Ready")
	}
	content, err := json.MarshalIndent(refused, "", "    ")
	r.check(err, "encode %s", r.names.schema)
	r.scanRows(content, "the status of a refused fenced change")
	r.scan(content, "the status of a refused fenced change")
	if r.query("SELECT count(*) FROM countries") != "0" {
		r.fatalf("a refused fenced change wrote rows into countries")
	}
	if r.query("SELECT count(*) FROM regions") != "2" {
		r.fatalf("a refused fenced change touched the regions rows")
	}

	r.logf("removing the %s fence, so the same revision may converge", r.engine.kind)
	if err := r.patchSchema(map[string]any{"spec": map[string]any{"policy": map[string]any{"protectedTables": []any{}}}}); err != nil {
		r.fatalf("the protected table could not be removed from %s: %v", r.names.schema, err)
	}
	// The step above leaves the refusal's own verdict standing, and that
	// verdict belongs to the generation that carried the fence. A phase wait
	// that starts within a second of the patch reads it long before the
	// controller looks at the spec again: measured in run 35282131046 on all
	// three minors, the refusal is logged 0.2 to 0.5 seconds after the patch,
	// with the resource's own nextReconciliationTime still half a minute out.
	//
	// So the wait is for the claim this step makes, which is that the plan the
	// fence refused is published. The status patch that carries PlanReady True
	// carries the phase with it, so the phase wait below reads this
	// generation's outcome rather than the previous one's.
	r.waitForCondition("PlanReady", metav1.ConditionTrue, "Published")
	r.waitForPhase(ptahv1alpha1.PhaseAwaitingApproval)
	r.waitForPlan()
	if err := r.approve(r.names.approval+"-v5", r.plan); err != nil {
		r.fatalf("the plan that the fence had refused could not be approved: %v", err)
	}
	r.waitForPhase(ptahv1alpha1.PhaseInSync)
	r.assertDeclaredRows("2", "1", "Czechia")
}

// assertRowsNeverLeftTheDatabase is the refusal that has to hold through
// every step above: no declared row value reaches status, an Event, the
// controller's own log, or a Plan Pod's log.
//
// Log reads must succeed and select a real Pod. The controller has logged
// reconciliation activity; a Plan Pod may have empty diagnostics when its
// successful result was delivered through the durable receiver instead.
func (r *referenceRun) assertRowsNeverLeftTheDatabase() {
	r.t.Helper()
	r.status()
	events := &corev1.EventList{}
	if err := r.list(events); err != nil {
		r.fatalf("namespace Events could not be read: %v", err)
	}
	content, err := json.MarshalIndent(events, "", "    ")
	r.check(err, "encode the namespace Events")
	r.scanBoth(content, "namespace Events")
	log, err := r.controllerLog()
	if err != nil {
		r.fatalf("the controller log could not be read: %v", err)
	}
	if len(log) == 0 {
		r.fatalf("the controller log is empty, so the row scan would have measured nothing")
	}
	r.scanBoth(log, "the controller log")
	r.assertPlanPodLogCarriesNoPlanText()
}

// referenceControllerLogTail is how many lines of each manager Pod's log the
// row scan reads, as kubectl logs --tail read them.
const referenceControllerLogTail int64 = 2000

// controllerLog is what kubectl logs -l app.kubernetes.io/component=controller
// --tail=2000 printed: the last lines of each manager Pod's default container,
// which is the one the kubectl.kubernetes.io/default-container annotation
// names, or the first.
func (r *referenceRun) controllerLog() ([]byte, error) {
	pods := &corev1.PodList{}
	if err := r.cluster.Client.List(r.ctx, pods, client.InNamespace(r.in.OperatorNamespace),
		client.MatchingLabels{labelComponent: "controller"}); err != nil {
		return nil, err
	}
	var log []byte
	for _, pod := range pods.Items {
		container := pod.Annotations["kubectl.kubernetes.io/default-container"]
		if container == "" && len(pod.Spec.Containers) > 0 {
			container = pod.Spec.Containers[0].Name
		}
		tail := referenceControllerLogTail
		content, err := r.cluster.Clientset.CoreV1().Pods(r.in.OperatorNamespace).
			GetLogs(pod.Name, &corev1.PodLogOptions{Container: container, TailLines: &tail}).DoRaw(r.ctx)
		if err != nil {
			return nil, fmt.Errorf("pod %s: %w", pod.Name, err)
		}
		log = append(log, content...)
	}
	return log, nil
}

// assertPlanPodLogCarriesNoPlanText reads the most recent Plan Job's Pod
// directly, the way pods/log or a node log shipper would, and proves it holds
// neither a declared row value nor the plan document's own shape.
//
// The Pod is found by the labels the approver-facing docs and the diagnostic
// reader role describe. The latest one is still there: every scenario above
// that reached AwaitingApproval ran one, and a Job survives at least five
// minutes after it finishes.
func (r *referenceRun) assertPlanPodLogCarriesNoPlanText() {
	r.t.Helper()
	pods := &corev1.PodList{}
	r.check(r.list(pods, client.MatchingLabels{labelSchema: r.names.schema, labelOperation: "plan"}),
		"list the Plan Pods of %s", r.names.schema)
	pod, found := referenceLatestPod(pods.Items)
	if !found {
		r.fatalf("no Plan Pod remained for %s, so its diagnostic log could not be checked", r.names.schema)
	}
	owner := metav1.GetControllerOf(&pod)
	if owner == nil || owner.APIVersion != "batch/v1" || owner.Kind != "Job" {
		r.fatalf("the Plan Pod has no owning Job")
	}
	job := &batchv1.Job{}
	r.check(r.get(owner.Name, job), "read the Plan Pod's owning Job")
	log, err := r.cluster.ContainerLog(r.ctx, r.in.TestNamespace, pod.Name, "ptah")
	if err != nil {
		r.fatalf("the Plan Pod log could not be read: %v", err)
	}
	r.check(referencePlanLogEvidence(r.ctx, r.cluster.Client, r.status(), job, &pod, log),
		"validate the Plan result behind its diagnostic log")
	r.scanBoth(log, "the Plan Pod log")
}

// engineReferenceData drives the engine from a database with no tables to a
// declared schema and declared rows the database agrees with, and proves each
// step on the way.
func (r *referenceRun) engineReferenceData() {
	r.t.Helper()
	r.logf("starting the %s lifecycle on a database with no tables", r.engine.kind)
	r.createReferenceDatabase()
	r.publish("v1")
	r.createReferenceResource()

	r.waitForPhase(ptahv1alpha1.PhaseAwaitingApproval)
	r.waitForPlan()
	if err := r.approve(r.names.approval, r.plan); err != nil {
		r.fatalf("the first plan could not be approved: %v", err)
	}
	r.waitForPhase(ptahv1alpha1.PhaseInSync)
	r.assertDeclaredRows("2", "0", "")
	r.assertRepeatedReconciliationChangesNothing()
	r.assertDataOnlyChangeReconciles()
	r.assertExternalEditRefusesAStaleApproval()
	r.assertRemovedDeclarationKeepsRows()
	r.assertDeclaredEmptySetClearsTheRows()
	r.assertAProtectedTableRefusesTheChange()
	r.assertRowsNeverLeftTheDatabase()
	r.logf("PASS %s declared rows, data-only change, stale approval, ended management, an emptied set, and a protected table",
		r.engine.kind)
}

// cleanup prints, on a failure, what the controller decided and what the
// operation Jobs were doing, and removes the work directory. The status of a
// schema carries no rows and no SQL by contract, which is the refusal this
// phase exists to measure, so it is safe to print. Job logs are not printed:
// a credential-isolation failure would put a database URL in them, and a data
// statement would put a row in them.
func (r *referenceRun) cleanup() {
	t := r.parent
	r.t = t
	if r.cluster != nil && t.Failed() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		r.collectDiagnostics(ctx)
		cancel()
	}
	if r.workDir != "" {
		if !strings.HasPrefix(filepath.Base(r.workDir), "ptah-operator-reference-data-e2e.") {
			t.Errorf("e2e reference data: refusing to remove unexpected work directory %s", r.workDir)
			return
		}
		if err := os.RemoveAll(r.workDir); err != nil {
			t.Errorf("e2e reference data: remove the work directory: %v", err)
		}
	}
}

// collectDiagnostics prints every schema's name and status, and the schema
// operation Jobs' names, labels and status. Each is scanned for a credential
// and withheld on a match.
func (r *referenceRun) collectDiagnostics(ctx context.Context) {
	_, _ = fmt.Fprintln(os.Stderr, "e2e reference data: collecting failure diagnostics")
	emit := func(what string, value any) {
		content, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return
		}
		if r.scanner.ready() && r.scanner.leaks(content) {
			_, _ = fmt.Fprintf(os.Stderr, "e2e reference data: the %s are withheld: they matched a protected credential\n", what)
			return
		}
		_, _ = fmt.Fprintf(os.Stderr, "%s\n", content)
	}
	schemas := &ptahv1alpha1.PtahSchemaList{}
	if r.cluster.Client.List(ctx, schemas, client.InNamespace(r.in.TestNamespace)) == nil {
		for _, schema := range schemas.Items {
			emit("schema status", map[string]any{"name": schema.Name, "status": schema.Status})
		}
	}
	jobs := &batchv1.JobList{}
	if r.cluster.Client.List(ctx, jobs, client.InNamespace(r.in.TestNamespace),
		client.MatchingLabels{labelComponent: schemaOperationComponent}) == nil {
		projection := []any{}
		for _, job := range jobs.Items {
			projection = append(projection, map[string]any{"name": job.Name, "labels": job.Labels, "status": job.Status})
		}
		emit("schema operation Jobs", projection)
	}
	_, _ = fmt.Fprintln(os.Stderr, "e2e reference data: raw Job logs are suppressed to protect credential-isolation failures")
}
