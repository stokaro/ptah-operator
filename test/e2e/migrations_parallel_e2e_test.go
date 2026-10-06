//go:build e2e

package e2e

import (
	"maps"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// independentApplyFaults overlaps waits whose resources, databases, Jobs and
// coordination keys are distinct. Cluster-wide faults run after both lanes
// finish. MySQL needs a second server: its migration advisory lock is shared
// across databases, so two sleeping Applies on one server are not independent.
func (m *migrationRun) independentApplyFaults() {
	m.t.Helper()
	second := m.independentFaultEngine()
	m.runProofLanes([2]migrationProofLane{
		{name: "deletion-log-loss-and-retry", engine: m.engine, proofs: []func(*migrationRun){
			(*migrationRun).deletionDuringApplyProof,
			(*migrationRun).lostLogProof,
			(*migrationRun).retryIntervalProof,
		}},
		{name: "deadline-and-suspension", engine: second, proofs: []func(*migrationRun){
			(*migrationRun).stoppedApplyProof,
			(*migrationRun).suspensionDuringApplyProof,
		}},
	})
	// These rows retain their evidence. Stop their periodic reads before
	// the later egress row isolates the original database server.
	m.finishFixture("e2e-lost-log-" + m.engine.name)
	m.finishFixture("e2e-stopped-" + m.engine.name)
}

type migrationProofLane struct {
	name   string
	engine migrationEngine
	proofs []func(*migrationRun)
}

// runProofLanes bounds concurrency to two inside the existing CI job. Each
// lane owns its test handle, credential patterns and captured Job map. The
// parent waits for both, and a failure stops the enclosing migration phase.
func (m *migrationRun) runProofLanes(lanes [2]migrationProofLane) {
	m.t.Helper()
	var completed [2]*migrationRun
	var cases [2]harness.ParallelCase
	for i, lane := range lanes {
		cases[i] = harness.ParallelCase{Name: lane.name, Run: func(t *testing.T) {
			local := *m
			local.t, local.parent, local.engine = t, t, lane.engine
			local.patterns = slices.Clone(m.patterns)
			local.jobs = maps.Clone(m.jobs)
			for _, proof := range lane.proofs {
				proof(&local)
			}
			completed[i] = &local
		}}
	}
	passed := harness.ParallelPair(m.t, "independent-apply-faults", cases)
	if !passed {
		m.t.FailNow()
	}
	for i, local := range completed {
		if local == nil {
			m.fatalf("independent Apply fault lane %s did not finish every proof", lanes[i].name)
		}
		// Later diagnostics still scan every credential introduced by a lane.
		m.protect(local.patterns...)
	}
}

func (m *migrationRun) independentFaultEngine() migrationEngine {
	m.t.Helper()
	engine := m.engine
	if engine.name != "mysql" {
		return engine
	}
	original := &appsv1.Deployment{}
	m.check(m.get(engine.service, original), "read the MySQL fixture image")
	image := ""
	for _, container := range original.Spec.Template.Spec.Containers {
		if container.Name == "mysql" {
			image = container.Image
		}
	}
	if image == "" {
		m.fatalf("the MySQL fixture has no mysql container image")
	}
	engine.service += "-apply-faults"
	if m.rerun != "" {
		engine.service += "-" + m.rerun
	}
	// This server has its own emptyDir and the existing fixture credentials.
	// Keep it until suite teardown, like the original server, so retained
	// resources and failure diagnostics can still reach their databases.
	document := databaseDeployment(m.in.TestNamespace, engine.service, "mysql", image,
		"mysql", 3306, 5, "/var/lib/mysql", []any{
			secretEnv("MYSQL_USER", engine.sourceSecret, "username"),
			secretEnv("MYSQL_PASSWORD", engine.sourceSecret, "password"),
			secretEnv("MYSQL_ROOT_PASSWORD", engine.sourceSecret, "rootPassword"),
			secretEnv("MYSQL_DATABASE", engine.sourceSecret, "database"),
		})
	// Admission fixtures are already installed here. Their tiny defaults
	// target operation Pods and cannot initialize a second MySQL server.
	containers := document["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	containers[0].(map[string]any)["resources"] = map[string]any{
		"requests": map[string]any{"cpu": "100m", "memory": "256Mi"},
		"limits":   map[string]any{"cpu": "1", "memory": "1Gi"},
	}
	m.mustCreate(document)
	m.mustCreate(databaseService(m.in.TestNamespace, engine.service, "mysql", 3306))
	m.check(m.cluster.WaitForRollout(m.ctx, m.in.TestNamespace, engine.service, waitTimeout),
		"wait for the independent MySQL fault server")
	return engine
}
