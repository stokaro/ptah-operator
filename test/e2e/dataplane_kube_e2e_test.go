//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// scan ends the scenario when content carries a protected credential. What
// the phase scans is what it is about to trust as credential-free, so a
// scanner with nothing to look for is a failure rather than a pass.
func (d *dataPlane) scan(content []byte, context string) {
	d.t.Helper()
	if !d.scanner.ready() {
		d.fatalf("credential scanner has no non-empty protected patterns")
	}
	if d.scanner.leaks(content) {
		d.fatalf("a task credential escaped into %s", context)
	}
}

// scanObject scans the JSON of an object the API returned.
func (d *dataPlane) scanObject(object any, context string) {
	d.t.Helper()
	d.scan(d.jsonBytes(object), context)
}

func (d *dataPlane) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: d.in.TestNamespace, Name: name}
}

// get reads one object from the test namespace.
func (d *dataPlane) get(name string, object client.Object) error {
	return d.cluster.Client.Get(d.ctx, d.key(name), object)
}

// mustGet reads one object from the test namespace and ends the scenario when
// it cannot.
func (d *dataPlane) mustGet(name string, object client.Object) {
	d.t.Helper()
	d.check(d.get(name, object), "read %T %s", object, name)
}

// list reads a list from the test namespace.
func (d *dataPlane) list(list client.ObjectList, options ...client.ListOption) error {
	return d.cluster.Client.List(d.ctx, list, append([]client.ListOption{client.InNamespace(d.in.TestNamespace)}, options...)...)
}

func (d *dataPlane) mustList(list client.ObjectList, options ...client.ListOption) {
	d.t.Helper()
	d.check(d.list(list, options...), "list %T in %s", list, d.in.TestNamespace)
}

func (d *dataPlane) schema(name string) *ptahv1alpha1.PtahSchema {
	d.t.Helper()
	schema := &ptahv1alpha1.PtahSchema{}
	d.mustGet(name, schema)
	return schema
}

func (d *dataPlane) schemaPlan(name string) *ptahv1alpha1.PtahSchemaPlan {
	d.t.Helper()
	plan := &ptahv1alpha1.PtahSchemaPlan{}
	d.mustGet(name, plan)
	return plan
}

func (d *dataPlane) job(name string) *batchv1.Job {
	d.t.Helper()
	job := &batchv1.Job{}
	d.mustGet(name, job)
	return job
}

// unstructuredSchema reads a schema as it is stored, for a claim about the
// stored representation rather than about what it means.
func (d *dataPlane) unstructuredSchema(name string) *unstructured.Unstructured {
	d.t.Helper()
	object := &unstructured.Unstructured{}
	object.SetAPIVersion(ptahSchemaAPIVersion)
	object.SetKind("PtahSchema")
	d.mustGet(name, object)
	return object
}

// create sends one document as kubectl create -f did: as the JSON the phase
// wrote, with strict field validation, so a field the API does not know is
// refused rather than pruned.
func (d *dataPlane) create(document map[string]any) error {
	object := &unstructured.Unstructured{Object: document}
	return d.cluster.Client.Create(d.ctx, object, client.FieldOwner(harness.FieldOwner), client.FieldValidation("Strict"))
}

// mustCreate creates every document in order and ends the scenario at the
// first the API server refuses.
func (d *dataPlane) mustCreate(documents ...map[string]any) {
	d.t.Helper()
	for _, document := range documents {
		d.check(d.create(document), "create %s %s", document["kind"], nameOf(document))
	}
}

// apply sends one document as kubectl apply -f did: created when it is new,
// brought to what the document says when it is not.
func (d *dataPlane) apply(document map[string]any) error {
	object := &unstructured.Unstructured{Object: document}
	return d.cluster.Client.Patch(d.ctx, object, client.Apply, client.FieldOwner(harness.FieldOwner),
		client.ForceOwnership, client.FieldValidation("Strict"))
}

func (d *dataPlane) mustApply(documents ...map[string]any) {
	d.t.Helper()
	for _, document := range documents {
		d.check(d.apply(document), "apply %s %s", document["kind"], nameOf(document))
	}
}

func nameOf(document map[string]any) string {
	metadata, _ := document["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	return name
}

// mergePatch sends a JSON merge patch, as kubectl patch --type=merge did.
func (d *dataPlane) mergePatch(object client.Object, patch map[string]any) error {
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return d.cluster.Client.Patch(d.ctx, object, client.RawPatch(types.MergePatchType, body),
		client.FieldOwner(harness.FieldOwner))
}

// patchSchema merge-patches one schema's document.
func (d *dataPlane) patchSchema(name string, patch map[string]any) {
	d.t.Helper()
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Namespace, schema.Name = d.in.TestNamespace, name
	d.check(d.mergePatch(schema, patch), "patch PtahSchema %s", name)
}

// suspend sets spec.suspend on a schema.
func (d *dataPlane) suspend(name string, suspended bool) {
	d.t.Helper()
	d.patchSchema(name, map[string]any{"spec": map[string]any{"suspend": suspended}})
}

// execIn runs a command in the first container of a Deployment's Pod, as
// kubectl exec deployment/<name> does, and returns what it wrote on standard
// output.
func (d *dataPlane) execIn(deployment string, command ...string) (string, error) {
	stdout, stderr, err := d.cluster.Kubectl(d.ctx, append([]string{"-n", d.in.TestNamespace, "exec",
		"deployment/" + deployment, "--"}, command...)...)
	if err != nil {
		return string(stdout), fmt.Errorf("exec in deployment/%s: %w: %s", deployment, err, strings.TrimSpace(string(stderr)))
	}
	return string(stdout), nil
}

// psql runs one query against the lifecycle's PostgreSQL server, in database,
// as the server's own user, and returns its unaligned output.
func (d *dataPlane) psql(database, query string) (string, error) {
	return d.execIn(pgService, "sh", "-ec",
		`PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -v ON_ERROR_STOP=1 -Atqc "$2"`,
		"sh", database, query)
}

// psqlDefault is psql in the server's own database.
func (d *dataPlane) psqlDefault(query string) (string, error) {
	return d.execIn(pgService, "sh", "-ec",
		`PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -Atqc "$1"`,
		"sh", query)
}

// mysql runs one query against the lifecycle's MySQL server as the
// application user and returns its tab-separated output without a header.
func (d *dataPlane) mysql(query string) (string, error) {
	return d.execIn(mysqlService, "sh", "-ec",
		`MYSQL_PWD="$MYSQL_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -u"$MYSQL_USER" "$MYSQL_DATABASE" -Nse "$1"`,
		"sh", query)
}

// removeWhitespace is tr -d '[:space:]'.
func removeWhitespace(value string) string {
	return strings.Map(func(r rune) rune {
		if isSpace(r) {
			return -1
		}
		return r
	}, value)
}

// removeLineBreaks is tr -d '\r\n'.
func removeLineBreaks(value string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(value)
}

// waitForSchema reads the schema every two seconds until match holds, and
// returns the document that satisfied it: a claim about the moment the wait
// ended is made against that document, not a later read. Every reading audits
// the Jobs that finished since the last, and a schema that entered Failed for
// the spec it holds ends the wait at once.
func (d *dataPlane) waitForSchema(name, description string, match func(*ptahv1alpha1.PtahSchema) bool) *ptahv1alpha1.PtahSchema {
	d.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	var last *ptahv1alpha1.PtahSchema
	for time.Now().Before(deadline) {
		d.auditCompletedJobs()
		schema := &ptahv1alpha1.PtahSchema{}
		if err := d.get(name, schema); err == nil {
			last = schema
			if match(schema) {
				return schema
			}
			if failedForCurrentSpec(schema) {
				d.fatalf("%s entered Failed while waiting for %s", name, description)
			}
		}
		d.sleep(2 * time.Second)
	}
	d.reportSchemaWaitTimeout(name, last)
	d.fatalf("timed out waiting for %s: %s", name, description)
	return nil
}

// reportSchemaWaitTimeout says what the last document a wait read held when
// it gave up: the phase, the operation and plan it carried, and every
// condition with its message bounded. The timeout itself names only what never
// held, so the reason a schema stopped short -- DestructiveChangesDisabled,
// RealmConflict, PodAdmissionRefused -- had to be inferred from printer
// columns. The report is scanned like the cleanup projection, and withheld on
// a match.
func (d *dataPlane) reportSchemaWaitTimeout(name string, last *ptahv1alpha1.PtahSchema) {
	if last == nil || !d.scanner.ready() {
		return
	}
	report, err := json.Marshal(schemaWaitReport(last))
	if err != nil {
		return
	}
	if d.scanner.leaks(report) {
		_, _ = fmt.Fprintf(os.Stderr, "e2e data plane: the last %s document read is withheld: it matched a protected credential\n", name)
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "e2e data plane: the last %s document read before the wait gave up\n%s\n", name, report)
}

// waitForApproval reads the approval every two seconds until match holds.
func (d *dataPlane) waitForApproval(name, description string, match func(*ptahv1alpha1.PtahSchemaApproval) bool) *ptahv1alpha1.PtahSchemaApproval {
	d.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		d.auditCompletedJobs()
		approval := &ptahv1alpha1.PtahSchemaApproval{}
		if err := d.get(name, approval); err == nil && match(approval) {
			return approval
		}
		d.sleep(2 * time.Second)
	}
	d.fatalf("timed out waiting for %s: %s", name, description)
	return nil
}

// waitForJob waits for a Job the phase created to complete, auditing on the
// way, and ends the scenario when it failed.
func (d *dataPlane) waitForJob(name string) *batchv1.Job {
	d.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		d.auditCompletedJobs()
		job := &batchv1.Job{}
		if err := d.get(name, job); err == nil {
			if conditionTrue(job.Status.Conditions, batchv1.JobComplete) {
				d.auditCompletedJobs()
				return job
			}
			if conditionTrue(job.Status.Conditions, batchv1.JobFailed) {
				d.fatalf("Job %s failed", name)
			}
		}
		d.sleep(2 * time.Second)
	}
	d.fatalf("timed out waiting for Job %s", name)
	return nil
}
