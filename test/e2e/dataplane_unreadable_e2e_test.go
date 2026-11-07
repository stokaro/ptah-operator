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
// so a newer Ptah than the executor is what produces it. The run stops in the
// fetch step, whose container holds the registry credentials. The one sentence
// that leaves that container is this one: the row holds the condition and the
// Event to the code and the reason and to no word of the registry, and the step's
// log and termination message to holding the same sentence.
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
		coordinationKey: key, failureRetry: "5s", interval: reconcileInterval,
	})
	var message string
	matched := d.waitForSchema(schemaName, "the schema to say why the fetch stopped", func(s *ptahv1alpha1.PtahSchema) bool {
		failed := meta.FindStatusCondition(s.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed)
		if failed == nil || failed.Status != metav1.ConditionTrue || failed.Reason != string(ptahv1alpha1.ReasonOperationFailed) {
			return false
		}
		message = failed.Message
		return fetchReasonSays(message, d.registryHost) == nil
	})
	d.waitForFetchReasonEvent(matched.UID)
	d.readFetchStepLog(schemaName)
	d.deleteAndWait(schemaName, 2*time.Minute)
	d.logf("PASS a schema artifact the executor cannot parse said why in status and its Event: %s", message)
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

func (d *dataPlane) waitForFetchReasonEvent(owner types.UID) {
	d.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	var last error
	for time.Now().Before(deadline) {
		events := &corev1.EventList{}
		d.mustList(events)
		if last = fetchReasonEvent(events.Items, owner, "OperationFailed", d.registryHost); last == nil {
			return
		}
		d.sleep(2 * time.Second)
	}
	d.fatalf("the OperationFailed Event never said why the fetch stopped: %v", last)
}

// readFetchStepLog reads the log a person with pods/log reads, from the Pod whose
// fetch step exited nonzero. A later attempt may not have got that far yet, so only
// a step that ended counts, and the row needs one.
func (d *dataPlane) readFetchStepLog(schema string) {
	d.t.Helper()
	jobs := &batchv1.JobList{}
	d.mustList(jobs, client.MatchingLabels{labelSchema: schema})
	examined := 0
	for index := range jobs.Items {
		pods := &corev1.PodList{}
		d.mustList(pods, client.MatchingLabels{"job-name": jobs.Items[index].Name})
		for _, pod := range ownedPods(pods.Items, jobs.Items[index].UID) {
			for _, status := range pod.Status.InitContainerStatuses {
				if status.Name != unreadableFetchStep || status.State.Terminated == nil || status.State.Terminated.ExitCode == 0 {
					continue
				}
				log, err := d.cluster.ContainerLog(d.ctx, d.in.TestNamespace, pod.Name, unreadableFetchStep)
				d.check(err, "read the %s log of Pod %s", unreadableFetchStep, pod.Name)
				d.scan(log, "the "+unreadableFetchStep+" log")
				d.check(fetchStepLogSays(log), "the %s log of Pod %s", unreadableFetchStep, pod.Name)
				d.check(fetchStatusSays(status), "the %s status of Pod %s", unreadableFetchStep, pod.Name)
				examined++
			}
		}
	}
	if examined == 0 {
		d.fatalf("no Pod of %s has a %s step that ended nonzero, among %d Jobs", schema, unreadableFetchStep, len(jobs.Items))
	}
}
