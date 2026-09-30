//go:build e2e

package e2e

import (
	"context"
	"os/exec"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
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

func (l *lifecycleRun) runningApplySQLClient() string {
	l.t.Helper()
	pod, err := l.predecessorApplyPod(l.runningApply.podName)
	l.check(err, "read the original running Apply SQL client")
	if !predecessorApplyPodIn(pod, l.runningApply.podUID, corev1.PodRunning) ||
		!podControlledByJobUID(pod.OwnerReferences, types.UID(l.runningApply.jobUID)) {
		l.fatalf("the lifecycle SQL control lost its original running Job and Pod")
	}
	return pod.Status.PodIP
}

func (l *lifecycleRun) assertRunningApplySQLUnchanged(audit *databaseSQLAudit, before sqlAuditCounts, host, boundary string) {
	l.t.Helper()
	if l.runningApplySQLClient() != host {
		l.fatalf("%s changed the running Apply SQL client address", boundary)
	}
	after := audit.snapshot()
	l.check(lifecycleSQLQuiescent(before, after, host), "%s SQL audit", boundary)
	l.logf("SQL audit: boundary=%q jobUID=%s podUID=%s client=%s controlRecords=%d additionalRemoteRecords=0",
		boundary, l.runningApply.jobUID, l.runningApply.podUID, host, before.clients[host])
}
