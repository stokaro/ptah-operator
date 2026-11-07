//go:build e2e

package e2e

import (
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// unreadableOCISchema delivers a schema artifact the executor cannot parse. No
// product command builds one, because `ptah schema push` parses the file first,
// so a newer Ptah than the executor is what produces it. The fetch step holds the
// registry credentials and writes the layer without parsing it, so the run goes on
// to the ptah container, which holds none: its credential-free check names the
// file and Ptah's reason. The row holds the condition and the Event to that
// reason, and the Pods to a fetch that ended at zero before the ptah container
// ran.
func (d *dataPlane) unreadableOCISchema() {
	d.t.Helper()
	const (
		schemaName = "e2e-unreadable-oci-postgresql"
		key        = "e2e/unreadable-oci-postgresql"
		repository = "unreadable-oci-postgresql"
	)
	d.logf("checking what a schema artifact the executor cannot parse says")
	d.publishUnparsableSchema(repository)
	d.createIsolatedPostgreSQLDatabase(unreadableOCIPGDatabase, unreadableOCIPGSecret, d.credentials.unreadableOCIPGURL)
	d.createSchemaResource(schemaResource{
		name: schemaName, engine: "PostgreSQL", secret: unreadableOCIPGSecret, reference: d.registryReference(repository),
		coordinationKey: key, failureRetry: "5s", interval: "1m",
	})
	var message string
	matched := d.waitForSchema(schemaName, "the schema to say why it could not read the artifact", func(s *ptahv1alpha1.PtahSchema) bool {
		failed := meta.FindStatusCondition(s.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed)
		if failed == nil || failed.Status != metav1.ConditionTrue || failed.Reason != string(ptahv1alpha1.ReasonOperationFailed) {
			return false
		}
		message = failed.Message
		return unreadableSourceSays(message, unreadableArtifactFile, unreadableArtifactWords) == nil
	})
	d.scan([]byte(message), "the ReconciliationFailed message of "+schemaName)
	if strings.Contains(message, unreadableOCIPGDatabase) {
		d.fatalf("the reason names the target: %s", message)
	}
	d.waitForUnreadableArtifactEvent(matched.UID)
	d.readUnreadableArtifactPods(schemaName)
	d.repairedTagIsRead(schemaName, repository)
	d.deleteAndWait(schemaName, 2*time.Minute)
	d.logf("PASS a schema artifact the executor cannot parse said why from the ptah container: %s", message)
}

// repairedTagIsRead pushes a schema the executor reads to the tag the failing
// read came from, and waits for the interval to find it. Only a read of the
// source can show a moved tag, so the interval is what ends the failed claim here,
// and nothing writes to the resource. The wait cannot stop on phase Failed as
// waitForSchema does: the attempts it waits through are the point. It audits as
// that wait does.
func (d *dataPlane) repairedTagIsRead(schemaName, repository string) {
	d.t.Helper()
	failing := &ptahv1alpha1.PtahSchema{}
	d.check(d.get(schemaName, failing), "read %s before its tag moved", schemaName)
	generation, failedSource := failing.Generation, schemaSourceIdentity(failing)
	repaired := filepath.Join(repositoryRoot, "testdata", "e2e", "postgresql-v1.sql")
	digest := d.publishSchema("postgresql", "unreadable-repair", "postgres", d.registryReference(repository), repaired)
	deadline := time.Now().Add(waitTimeout)
	var last error
	for time.Now().Before(deadline) {
		d.auditCompletedJobs()
		schema := &ptahv1alpha1.PtahSchema{}
		d.check(d.get(schemaName, schema), "read %s while its tag is read again", schemaName)
		if last = schemaReadTheRepair(schema, generation, failedSource); last == nil {
			if schema.Status.Source.Digest != digest {
				d.fatalf("%s planned from digest %s, not the pushed %s", schemaName, schema.Status.Source.Digest, digest)
			}
			return
		}
		d.sleep(2 * time.Second)
	}
	d.fatalf("%s never read the repaired tag: %v", schemaName, last)
}

// publishUnparsableSchema runs the fixture on the host, against the registry as
// the host reaches it, the way the unknown-layer row does. The publisher is handed
// the registry password, so what it prints is scanned before it is printed.
func (d *dataPlane) publishUnparsableSchema(repository string) {
	d.t.Helper()
	root, err := filepath.Abs(repositoryRoot)
	d.check(err, "resolve the repository root")
	reference := d.in.RegistryHostAddress + strings.TrimPrefix(d.registryReference(repository), "oci://"+d.registryHost)
	command := exec.CommandContext(d.ctx, "go", "-C", root, "run", "./hack/unparsableschemafixture", //nolint:gosec // Arguments, not a shell.
		"--reference", reference,
		"--username", d.registry.Username,
		"--password", d.registry.Password,
		"--plain-http")
	output, err := command.CombinedOutput()
	d.scan(output, "the unparsable schema publisher")
	if err != nil {
		d.fatalf("the unparsable schema artifact could not be published: %v: %s", err, output)
	}
}

// waitForUnreadableArtifactEvent waits for the Event that repeats the reason.
// Events are written after the status patch, so a reading of the condition does
// not yet promise one.
func (d *dataPlane) waitForUnreadableArtifactEvent(owner types.UID) {
	d.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	var last error
	for time.Now().Before(deadline) {
		events := &corev1.EventList{}
		d.mustList(events)
		if last = unreadableSourceEvent(events.Items, owner, "OperationFailed", unreadableArtifactFile, unreadableArtifactWords); last == nil {
			return
		}
		d.sleep(2 * time.Second)
	}
	d.fatalf("the OperationFailed Event never said why the artifact could not be read: %v", last)
}

// readUnreadableArtifactPods holds every Pod of the schema's finished Observe
// and Plan Jobs, the operations that fetch the layer, to a fetch that passed it
// on, and scans the logs of both steps that read the artifact. Only the failed
// reads have run when it is called, and a later attempt may not have finished,
// so only a terminal Job counts, and the row needs one.
func (d *dataPlane) readUnreadableArtifactPods(schema string) {
	d.t.Helper()
	jobs := &batchv1.JobList{}
	d.mustList(jobs, client.MatchingLabels{labelSchema: schema})
	examined := 0
	for index := range jobs.Items {
		operation := jobs.Items[index].Labels[labelOperation]
		if (operation != "observe" && operation != "plan") || !jobTerminal(&jobs.Items[index]) {
			continue
		}
		pods := &corev1.PodList{}
		d.mustList(pods, client.MatchingLabels{"job-name": jobs.Items[index].Name})
		for _, pod := range ownedPods(pods.Items, jobs.Items[index].UID) {
			d.check(fetchPassedTheLayerOn(&pod), "the Pod of Job %s", jobs.Items[index].Name)
			for _, container := range []string{schemaFetchStep, "ptah"} {
				log, err := d.cluster.ContainerLog(d.ctx, d.in.TestNamespace, pod.Name, container)
				d.check(err, "read the %s log of Pod %s", container, pod.Name)
				d.scan(log, "the "+container+" log of Pod "+pod.Name)
			}
			examined++
		}
	}
	if examined == 0 {
		d.fatalf("no finished Job of %s has a Pod to read, among %d Jobs", schema, len(jobs.Items))
	}
}
