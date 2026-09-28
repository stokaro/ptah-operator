//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// faultRun is the restart and fault injection: five scenarios of the data
// plane that share one set of resourceVersion watches, a heartbeat that keeps
// every watch moving, the database and scheduling barriers the proofs hold
// Pods behind, and a credential audit of their own. What one scenario leaves
// for a later one to hold -- the Apply identities, the idle Leases, the
// checkpoints -- lives in state.
type faultRun struct {
	*dataPlane

	watcher client.WithWatch
	scanner credentialScanner

	principalRole, principalPassword string

	lastAudit time.Time
	// The fault audit's own ledgers: the Jobs and Pods whose credential audit
	// it completed, broadly, and the Pods it audited completely. Only the
	// complete ledgers let a later pass skip an object.
	auditedJobs, auditedPods, fullyAuditedPods map[string]bool

	follower *logFollower

	jobs      *watchRecorder[*batchv1.Job]
	pods      *watchRecorder[*corev1.Pod]
	schemas   *watchRecorder[*ptahv1alpha1.PtahSchema]
	approvals *watchRecorder[*ptahv1alpha1.PtahSchemaApproval]
	leases    *watchRecorder[*coordinationv1.Lease]
	recorders []recorder

	heartbeat        *heartbeat
	heartbeatStopped bool
	barrierSequence  int

	pgBarriers    map[string]*backgroundCommand
	mysqlBarrier  *mysqlBarrier
	readBarrierOn bool

	pgReference, mysqlReference string

	state faultState
}

// newFaultRun stands the fault injection up inside the data plane. Its
// cleanup is the parent test's, so it runs once the phase ends, whichever
// scenario the phase ended in, and before the data plane's own.
func newFaultRun(d *dataPlane) *faultRun {
	d.t.Helper()
	watcher, err := client.NewWithWatch(d.cluster.Config, client.Options{Scheme: d.cluster.Scheme})
	d.check(err, "build a watching client")
	f := &faultRun{
		dataPlane: d, watcher: watcher,
		auditedJobs: map[string]bool{}, auditedPods: map[string]bool{}, fullyAuditedPods: map[string]bool{},
		pgBarriers: map[string]*backgroundCommand{},
	}
	f.principalRole, f.principalPassword = principalCredentials(d.in.TestNamespace)
	d.parent.Cleanup(f.cleanup)
	return f
}

func (f *faultRun) fatalf(format string, arguments ...any) {
	f.t.Helper()
	f.t.Fatalf("e2e faults: "+format, arguments...)
}

func (f *faultRun) logf(format string, arguments ...any) {
	f.t.Helper()
	f.t.Logf("e2e faults: "+format, arguments...)
}

// check ends the scenario when err is not nil, saying what failed and why.
func (f *faultRun) check(err error, format string, arguments ...any) {
	f.t.Helper()
	if err != nil {
		f.fatalf("%s: %v", fmt.Sprintf(format, arguments...), err)
	}
}

// scan ends the scenario when content carries one of the credentials the
// fault injection protects.
func (f *faultRun) scan(content []byte, context string) {
	f.t.Helper()
	if !f.scanner.ready() {
		f.fatalf("fault credential scanner has no non-empty protected patterns")
	}
	if f.scanner.leaks(content) {
		f.fatalf("a task credential escaped into %s", context)
	}
}

func (f *faultRun) scanObject(object any, context string) {
	f.t.Helper()
	f.scan(f.jsonBytes(object), context)
}

// operatorKey names an object in the release namespace.
func (f *faultRun) operatorKey(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: f.in.OperatorNamespace, Name: name}
}

// poll reads until ready holds, auditing on the fault cadence between
// readings, and ends the scenario with the description when the wait bound
// passes first.
func (f *faultRun) poll(description string, ready func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		f.maybeAudit()
		if ready() {
			return
		}
		f.sleep(time.Second)
	}
	f.fatalf("timed out waiting for %s", description)
}

// pollQuiet is poll without the audit, for the waits that must not read the
// manager's logs while its Pods are being replaced.
func (f *faultRun) pollQuiet(description string, ready func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		f.sleep(time.Second)
	}
	f.fatalf("timed out waiting for %s", description)
}

// waitForSchema reads the schema every second until match holds and returns
// the document that satisfied it. Unlike the data plane's wait it lets a
// schema be Failed: a refusal the fault injection proves is one.
func (f *faultRun) waitForSchema(name, description string, match func(*ptahv1alpha1.PtahSchema) bool) *ptahv1alpha1.PtahSchema {
	f.t.Helper()
	var matched *ptahv1alpha1.PtahSchema
	f.poll(name+": "+description, func() bool {
		schema := &ptahv1alpha1.PtahSchema{}
		if err := f.get(name, schema); err == nil && match(schema) {
			matched = schema
			return true
		}
		return false
	})
	return matched
}

// waitForAbsence waits until the object is gone from the test namespace.
func (f *faultRun) waitForAbsence(object client.Object, name string) {
	f.t.Helper()
	f.poll(fmt.Sprintf("%T %s to be deleted", object, name), func() bool {
		return apierrors.IsNotFound(f.get(name, object))
	})
}

// deleteSchema deletes a schema without waiting for its finalizer, as
// kubectl delete --wait=false did, and waits for it to be gone.
func (f *faultRun) deleteSchema(name string) {
	f.t.Helper()
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Namespace, schema.Name = f.in.TestNamespace, name
	f.check(f.cluster.Client.Delete(f.ctx, schema), "delete PtahSchema %s", name)
	f.waitForAbsence(&ptahv1alpha1.PtahSchema{}, name)
}

// unstructuredApproval reads an approval as it is stored.
func (f *faultRun) unstructuredApproval(name string) *unstructured.Unstructured {
	f.t.Helper()
	stored := &unstructured.Unstructured{}
	stored.SetAPIVersion(ptahSchemaAPIVersion)
	stored.SetKind("PtahSchemaApproval")
	f.check(f.get(name, stored), "read PtahSchemaApproval %s", name)
	return stored
}

// assertApprovalConsumed holds an approval, read once, to having dispatched
// the plan named and been retired with it.
func (f *faultRun) assertApprovalConsumed(approval, planUID string) {
	f.t.Helper()
	if err := faultApprovalConsumed(f.unstructuredApproval(approval), planUID); err != nil {
		f.fatalf("%s was not durably consumed by the exact dispatched plan: %v", approval, err)
	}
}

// liveJobName is the name of the one live Job with the UID given.
func (f *faultRun) liveJobName(uid, description string, options ...client.ListOption) string {
	f.t.Helper()
	jobs := &batchv1.JobList{}
	f.check(f.list(jobs, options...), "list Jobs")
	var names []string
	for index := range jobs.Items {
		if uidIs(&jobs.Items[index], uid) {
			names = append(names, jobs.Items[index].Name)
		}
	}
	if len(names) != 1 {
		f.fatalf("%s UID %s is not live exactly once", description, uid)
	}
	return names[0]
}

// operationJobs selects a schema's Jobs of one operation.
func operationJobs(schema, operation string) client.MatchingLabels {
	return client.MatchingLabels{labelSchema: schema, labelOperation: operation}
}

// backgroundCommand is a command the phase leaves running while it goes on:
// a database barrier's session or a log follower. Its output is kept in
// memory, and it is scanned before anything reads it.
type backgroundCommand struct {
	command *exec.Cmd
	cancel  context.CancelFunc
	output  lockedBuffer
	done    chan struct{}
	err     error
}

// lockedBuffer is a buffer the command writes while the phase may read it.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(content []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(content)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buffer.Bytes())
}

// startKubectl starts kubectl against the cluster and returns without
// waiting for it. The command outlives the scenario that started it, so it
// runs under a context of its own that only its stop or the cleanup cancels.
func (f *faultRun) startKubectl(arguments ...string) *backgroundCommand {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	background := &backgroundCommand{cancel: cancel, done: make(chan struct{})}
	background.command = exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", f.cluster.Kubeconfig}, arguments...)...) //nolint:gosec // Arguments, not a shell.
	background.command.Stdout, background.command.Stderr = &background.output, &background.output
	if err := background.command.Start(); err != nil {
		cancel()
		f.fatalf("could not start kubectl %s: %v", arguments[0], err)
	}
	go func() {
		background.err = background.command.Wait()
		close(background.done)
	}()
	return background
}

// exited reports whether the command has ended.
func (b *backgroundCommand) exited() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

// stop ends the command and waits for it.
func (b *backgroundCommand) stop() {
	b.cancel()
	<-b.done
}

// execInWithInput runs a command in a Deployment's Pod with input on its
// standard input, as kubectl exec -i did with a redirected file.
func (f *faultRun) execInWithInput(deployment string, input []byte, command ...string) error {
	arguments := append([]string{"--kubeconfig", f.cluster.Kubeconfig, "-n", f.in.TestNamespace, "exec", "-i",
		"deployment/" + deployment, "--"}, command...)
	execution := exec.CommandContext(f.ctx, "kubectl", arguments...) //nolint:gosec // Arguments, not a shell.
	execution.Stdin = bytes.NewReader(input)
	var output bytes.Buffer
	execution.Stdout, execution.Stderr = &output, &output
	if err := execution.Run(); err != nil {
		return fmt.Errorf("exec in deployment/%s: %w", deployment, err)
	}
	return nil
}

// cleanup puts back what the fault injection changed when the phase ends
// early: it stops the log follower, the heartbeat and the watches, ends the
// database barriers, gives the manager its status verbs back, and only then
// lifts the scheduling barrier, so no held Job runs while the manager cannot
// record it.
func (f *faultRun) cleanup() {
	t := f.parent
	if f.follower != nil {
		f.follower.command.stop()
	}
	if f.heartbeat != nil {
		f.heartbeat.requestStop()
		f.heartbeat.await(15 * time.Second)
	}
	for _, r := range f.recorders {
		r.requestStop()
	}
	for _, r := range f.recorders {
		if r.await(35*time.Second) != nil {
			r.abort()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for token, barrier := range f.pgBarriers {
		_, _ = f.psqlWith(ctx, "postgres", "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name='"+token+"'")
		barrier.stop()
	}
	if f.mysqlBarrier != nil {
		if id, err := f.mysqlRootWith(ctx, "mysql", "SELECT IS_USED_LOCK('"+f.mysqlBarrier.ready+"')"); err == nil {
			if id = removeWhitespace(id); decimalCount.MatchString(id) && id != "0" {
				_, _ = f.mysqlRootWith(ctx, "mysql", "KILL "+id)
			}
		}
		f.mysqlBarrier.session.stop()
	}
	if f.rbac.paused {
		// The phase's own context has ended when the phase ran out of time,
		// so the restore gets the cleanup's.
		f.dataPlane.ctx = ctx
		if err := f.resumeStatusWrites(); err != nil {
			t.Errorf("e2e faults: could not restore controller status-write RBAC: %v", err)
		}
	}
	if f.readBarrierOn {
		if err := f.setReadBarrier(ctx, false); err != nil {
			t.Errorf("e2e faults: could not remove the test scheduling barrier: %v", err)
		}
		f.readBarrierOn = false
	}
}

// annotate sets one annotation on an object with a merge patch, as kubectl
// annotate --overwrite did, and leaves the object as the API server returned
// it.
func (f *faultRun) annotate(ctx context.Context, object client.Object, key, value string) error {
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, key, value)
	return f.cluster.Client.Patch(ctx, object, client.RawPatch(types.MergePatchType, []byte(patch)),
		client.FieldOwner(harness.FieldOwner))
}
