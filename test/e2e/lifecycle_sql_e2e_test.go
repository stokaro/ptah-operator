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
