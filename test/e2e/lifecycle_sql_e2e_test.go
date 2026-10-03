//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os/exec"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func (l *lifecycleRun) externalPostgresAudit() *databaseSQLAudit {
	l.t.Helper()
	l.check(predecessorApplyDockerTarget(l.in.dockerContext, l.in.externalPostgresContainerID),
		"identify the exact lifecycle SQL audit container")
	return &databaseSQLAudit{t: l.t, ctx: l.ctx, engine: "postgresql", serverExec: func(ctx context.Context, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "docker", append([]string{"--context", l.in.dockerContext, "exec", l.in.externalPostgresContainerID}, args...)...) //nolint:gosec // Exact validated context/container, no shell interpolation.
		// Output can contain raw SQL and credentials. Return it only to the
		// in-memory journal parser; never print server stdout or stderr.
		return command.Output()
	}}
}

func (l *lifecycleRun) quiesceCompletedApply() {
	l.t.Helper()
	l.mustKubectl("-n", l.in.proofNamespace, "patch", "ptahschema", predecessorApplySchema,
		"--type=merge", "-p", `{"spec":{"suspend":true}}`)
	var lastErr error
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); {
		// A stored result can clear the claim before kubelet reports Pod
		// termination. Suspension and terminal workloads form one barrier.
		if _, lastErr = l.quiescentApplyState(); lastErr == nil {
			return
		}
		l.sleep(time.Second)
	}
	l.fatalf("the original completed Apply did not become quiescent: %v", lastErr)
}

func (l *lifecycleRun) quiescentApplyState() ([]byte, error) {
	l.t.Helper()
	uid := types.UID(lifecycleRuntimeString(l.runningApply.stagedGap, "metadata", "uid"))
	schema := &ptahv1alpha1.PtahSchema{}
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.proofNamespace, Name: predecessorApplySchema}, schema),
		"read the quiescent lifecycle target")
	state, err := lifecycleQuiescentSchemaState(schema, uid)
	if err != nil {
		return nil, err
	}
	jobs, pods := &batchv1.JobList{}, &corev1.PodList{}
	l.check(l.cluster.Client.List(l.ctx, jobs, client.InNamespace(l.in.proofNamespace)), "list lifecycle target Jobs")
	l.check(l.cluster.Client.List(l.ctx, pods, client.InNamespace(l.in.proofNamespace)), "list lifecycle target Pods")
	return state, lifecycleQuiescentWorkloads(uid, types.UID(l.runningApply.jobUID), l.runningApply.podUID, jobs.Items, pods.Items)
}

func (l *lifecycleRun) auditQuiescentTransition(boundary string, transition func()) {
	l.t.Helper()
	beforeState, err := l.quiescentApplyState()
	l.check(err, "%s requires a quiescent target before transition", boundary)
	audit := l.externalPostgresAudit()
	beforeSQL := audit.snapshot()
	if len(l.runningApply.sqlJournal) == 0 || !bytes.HasPrefix(audit.pgPrefix, l.runningApply.sqlJournal) {
		l.fatalf("%s lost the original executed SQL control journal", boundary)
	}
	l.check(lifecycleSQLBackendControl(l.runningApply.sqlJournal, l.runningApply.sqlBackend),
		"retain the original lifecycle SQL control")
	transition()
	afterState, err := l.quiescentApplyState()
	l.check(err, "%s requires a quiescent target after transition", boundary)
	if !bytes.Equal(beforeState, afterState) {
		l.fatalf("%s changed the suspended target's spec, finalizers, execution binding or pending proof", boundary)
	}
	afterSQL := audit.snapshot()
	l.check(lifecycleSQLQuiescent(beforeSQL, afterSQL, l.runningApply.sqlBackend.Client), "%s received SQL", boundary)
	l.runningApply.sqlJournal = bytes.Clone(audit.pgPrefix)
	audit.close()
	l.logf("SQL audit: boundary=%q additionalRemoteRecords=0; original resource, finalizers, execution binding and pending proof retained", boundary)
}

func (l *lifecycleRun) runningApplySQLClient() lifecycleSQLBackend {
	l.t.Helper()
	pod, err := l.predecessorApplyPod(l.runningApply.podName)
	l.check(err, "read the original running Apply SQL client")
	if !predecessorApplyPodIn(pod, l.runningApply.podUID, corev1.PodRunning) ||
		!podControlledByJobUID(pod.OwnerReferences, types.UID(l.runningApply.jobUID)) {
		l.fatalf("the lifecycle SQL control lost its original running Job and Pod")
	}
	raw, err := l.predecessorApplySQL(l.ctx, lifecycleSQLBackendQuery())
	l.check(err, "identify the one SQL backend blocked on the lifecycle barrier")
	backend, err := lifecycleSQLBackendForPod([]byte(raw), pod, l.runningApply.barrierDatabase)
	l.check(err, "bind the blocked SQL backend to the original running workload")
	return backend
}

func (l *lifecycleRun) assertRunningApplySQLUnchanged(audit *databaseSQLAudit, before sqlAuditCounts, backend lifecycleSQLBackend, boundary string) {
	l.t.Helper()
	if l.runningApplySQLClient() != backend {
		l.fatalf("%s changed the running Apply SQL backend session", boundary)
	}
	after := audit.snapshot()
	l.check(lifecycleSQLQuiescent(before, after, backend.Client), "%s SQL audit", boundary)
	l.logf("SQL audit: boundary=%q jobUID=%s podUID=%s serverPID=%d sessionStart=%q client=%s additionalRemoteRecords=0",
		boundary, l.runningApply.jobUID, l.runningApply.podUID, backend.PID, backend.SessionStart, backend.Client)
}
