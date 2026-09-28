//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// miUntil reads every interval until ready holds or waitTimeout passes, and
// says which, so a row can print its state and fail in its own words.
func (m *migrationRun) miUntil(interval time.Duration, ready func() bool) bool {
	m.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if ready() {
			return true
		}
		m.sleep(interval)
	}
	return false
}

// miReport prints what a row read when a check failed: fields chosen rather
// than objects dumped, since a Pod spec names Secrets, and all of it withheld
// when any of it matches a protected credential.
func (m *migrationRun) miReport(what string, state any) {
	content, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	if !m.scanner.ready() || m.scanner.leaks(content) {
		_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: %s: withheld, it matched a protected credential\n", what)
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: %s:\n%s\n", what, content)
}

// miConditions is each condition as the shell reports printed it, the message
// cut to the length given.
func miConditions(conditions []metav1.Condition, length int) []string {
	lines := make([]string, 0, len(conditions))
	for _, condition := range conditions {
		lines = append(lines, fmt.Sprintf("%s=%s reason=%s message=%s",
			condition.Type, condition.Status, condition.Reason, miClip(condition.Message, length)))
	}
	return lines
}

// miInstant is an optional instant as the API server writes it, or <none>.
func miInstant(instant *metav1.Time) string {
	if instant == nil {
		return "<none>"
	}
	return cmpOrNone(instantOf(*instant))
}

// isolatedNodeProof ports run_isolated_node_proof: an Apply on a node the API
// server loses, held by its resource until nothing could still be writing,
// and settled from a reading of the database with nothing run twice.
func (m *migrationRun) isolatedNodeProof() {
	m.t.Helper()
	r := &isolatedNodeRow{
		m: m, name: "e2e-isolated-node-" + m.engine.name, database: "ptah_e2e_isolated_node",
		secret: "e2e-" + m.engine.name + "-isolated-node-db", node: m.miIsolatedNode(),
	}
	r.requireWorker()
	m.isolatedDatabase(r.database, r.secret)
	r.createResource()
	r.waitForClaim()
	r.findLease()
	r.waitForPod()
	r.assertOnlyTheApplyOnNode()
	r.waitForCommit()
	r.isolate()
	r.hold()
	r.rejoin()
	r.assertSettles()
	migration := &ptahv1alpha1.PtahMigration{}
	migration.Namespace, migration.Name = m.in.TestNamespace, r.name
	m.deleteAndWait(migration, r.name)
	m.logf("PASS %s held an Apply on an isolated node until nothing could write, and replayed nothing", m.engine.kind)
}

// isolatedNodeRow is what the isolated-node row reads once and holds the
// cluster to afterwards. Instants are Unix seconds, as the shell compared
// them.
type isolatedNodeRow struct {
	m                             *migrationRun
	name, database, secret, node  string
	claim                         isolatedClaim
	leaseNamespace, lease, holder string
	termEnd, jobDeadlineAt        int64
	pod, podUID                   string
	toleration                    int64
	isolatedAt                    int64
	jobsAtIsolation               []string
	seen                          []string
	applied                       int
	lastLease                     *coordinationv1.Lease
	lastPods                      []corev1.Pod
}

// miIsolatedNode is the worker hack/e2e-kind.sh provisions for the row.
func (m *migrationRun) miIsolatedNode() string { return m.in.KindClusterName + "-worker2" }

// miNodeExec runs a command in the isolation worker's node container,
// through the Docker context the cluster's nodes run on.
func (m *migrationRun) miNodeExec(ctx context.Context, stderr io.Writer, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "docker", //nolint:gosec // Arguments, not a shell.
		append([]string{"--context", m.in.DockerContext, "exec", m.miIsolatedNode()}, arguments...)...)
	var stdout bytes.Buffer
	command.Stdout, command.Stderr = &stdout, stderr
	err := command.Run()
	return stdout.Bytes(), err
}

// miIsolationRules is how many rules marked as the row's the node carries, in
// any chain.
func (m *migrationRun) miIsolationRules(ctx context.Context) (int, error) {
	rules, err := m.miNodeExec(ctx, os.Stderr, "iptables", "-S")
	if err != nil {
		return 0, err
	}
	return isolationRuleCount(rules), nil
}

// isolationRuleSpecs is each rule the row inserts: the node's traffic to the
// API server, and the API server's to the kubelet.
var isolationRuleSpecs = [][]string{
	{"OUTPUT", "-p", "tcp", "--dport", "6443", "-m", "comment", "--comment", isolationRuleComment, "-j", "DROP"},
	{"INPUT", "-p", "tcp", "--dport", "10250", "-m", "comment", "--comment", isolationRuleComment, "-j", "DROP"},
}

// removeIsolationRules ports remove_isolation_rules: each rule is deleted
// until none is left, so a rule a failed run inserted twice goes too, and the
// node is then read back. The cleanup and the rerun reset call it however the
// phase ends: a node left cut off reads NotReady to every phase after this one.
func (m *migrationRun) removeIsolationRules(ctx context.Context) error {
	for _, rule := range isolationRuleSpecs {
		for {
			if _, err := m.miNodeExec(ctx, io.Discard, append([]string{"iptables", "-D"}, rule...)...); err != nil {
				break
			}
		}
	}
	count, err := m.miIsolationRules(ctx)
	if err != nil {
		return fmt.Errorf("read the rules on %s back: %w", m.miIsolatedNode(), err)
	}
	if count != 0 {
		return fmt.Errorf("%s still carries %d isolation rules", m.miIsolatedNode(), count)
	}
	return nil
}

// requireWorker holds the node to the one hack/e2e-kind.sh provisions. A suite
// that stopped declaring it would send the Apply to a node that does not
// exist, and the row would wait out a Pod the scheduler cannot place.
func (r *isolatedNodeRow) requireWorker() {
	m := r.m
	m.t.Helper()
	node := &corev1.Node{}
	if err := m.cluster.Client.Get(m.ctx, types.NamespacedName{Name: r.node}, node); err != nil {
		m.fatalf("the isolation worker %s is not in this cluster; the suite has to declare isolationWorker in support/e2e-suites.json", r.node)
	}
	if !isolationWorkerReady(node) {
		m.fatalf("%s is not the Ready, labeled and tainted isolation worker", r.node)
	}
	rules, err := m.miIsolationRules(m.ctx)
	if err != nil {
		m.fatalf("the rules on %s could not be read through Docker context %s", r.node, m.in.DockerContext)
	}
	if rules != 0 {
		m.fatalf("%s already carries %d isolation rules, so an earlier run left it cut off", r.node, rules)
	}
}

// createResource declares the row's resource on the uncertain row's artifact,
// for its slow third migration: the node is cut off inside it. The selector
// and the first toleration put every operation of the resource, and nothing
// else, on the isolation worker; the second replaces the cluster's
// five-minute default for the unreachable taint, so the Pod is evicted while
// its Job is alive. The deadline covers the read-only chain on a node that
// pulls its images for the first time and a third migration that sleeps, and
// the interval keeps the resource Blocked on the unread run for every poll.
func (r *isolatedNodeRow) createResource() {
	m := r.m
	m.t.Helper()
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: r.name, secret: r.secret, reference: m.reference("-uncertain"),
		coordinationKey: "e2e/isolated-node/" + m.engine.name, apply: "Always", interval: "2m",
		execution: map[string]any{
			"activeDeadlineSeconds": int64(180), "failureRetryInterval": "10s", "connectTimeout": "30s",
			"nodeSelector": map[string]any{isolationNodeKey: "true"},
			"tolerations": []any{
				map[string]any{"key": isolationNodeKey, "operator": "Equal", "value": "true", "effect": "NoSchedule"},
				map[string]any{
					"key": corev1.TaintNodeUnreachable, "operator": "Exists", "effect": "NoExecute",
					"tolerationSeconds": int64(30),
				},
			},
		},
	}))
}

// waitForClaim reads the claim the rest of the row holds the resource to from
// one document.
func (r *isolatedNodeRow) waitForClaim() {
	m := r.m
	m.t.Helper()
	if !m.miUntil(2*time.Second, func() bool {
		claim, ok := isolatedApplyClaim(m.migration(r.name))
		r.claim = claim
		return ok
	}) {
		r.report()
		m.fatalf("%s did not claim an Apply under a realm Lease within %s", r.name, waitTimeout)
	}
	r.seen = []string{r.claim.jobUID}
}

// findLease finds the Lease the claim took by the epoch it recorded, holds it
// to the duration it recorded, and reads when its term would end had nothing
// renewed it.
func (r *isolatedNodeRow) findLease() {
	m := r.m
	m.t.Helper()
	leases := &coordinationv1.LeaseList{}
	m.check(m.cluster.Client.List(m.ctx, leases), "the Leases could not be listed")
	lease, err := isolatedLease(leases.Items, r.claim.epoch, r.claim.duration)
	if err != nil {
		m.fatalf("no single realm Lease carries the epoch and duration %s claimed under: %v", r.name, err)
	}
	r.leaseNamespace, r.lease = lease.Namespace, lease.Name
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		m.fatalf("the realm Lease under %s's Apply names no holder", r.name)
	}
	r.holder = *lease.Spec.HolderIdentity
	termEnd, ok := leaseTermEnd(lease)
	if !ok {
		m.fatalf("the realm Lease under %s's Apply carries no acquisition to measure its term from", r.name)
	}
	r.termEnd = termEnd
}

// waitForPod waits for the Apply Pod to run on the isolation worker, and reads
// the two clocks it and its Job carry: how long the Pod tolerates an
// unreachable node, and when the Job's own deadline runs out.
func (r *isolatedNodeRow) waitForPod() {
	m := r.m
	m.t.Helper()
	if m.miUntil(2*time.Second, func() bool {
		pods := &corev1.PodList{}
		m.check(m.list(pods, client.MatchingLabels{"batch.kubernetes.io/controller-uid": r.claim.jobUID}),
			"the %s Apply Pods could not be listed", m.engine.name)
		if isolatedApplyPodRunning(pods.Items, r.node) {
			pod := &pods.Items[0]
			r.pod, r.podUID = pod.Name, string(pod.UID)
			seconds, ok := unreachableTolerationSeconds(pod)
			if !ok {
				m.fatalf("the %s Apply Pod carries no bounded toleration of an unreachable node to hold past", m.engine.name)
			}
			r.toleration = seconds
			job := &batchv1.Job{}
			m.check(m.get(r.claim.job, job), "the %s Apply Job could not be read", m.engine.name)
			deadline, err := jobDeadlineAt(job, r.claim.jobUID)
			if err != nil {
				m.fatalf("the %s Apply Job carries no deadline the hold can be measured against: %v", m.engine.name, err)
			}
			r.jobDeadlineAt = deadline
			return true
		}
		// A Pod placed anywhere else is not something waiting longer fixes.
		if podPlacedElsewhere(pods.Items, r.node) {
			r.report()
			m.fatalf("the %s Apply Pod was placed on a node other than %s", m.engine.name, r.node)
		}
		return false
	}) {
		return
	}
	r.report()
	m.fatalf("the %s Apply Pod did not run on %s within %s", m.engine.name, r.node, waitTimeout)
}

// assertOnlyTheApplyOnNode holds cutting the node off to taking away this
// Apply and nothing else.
func (r *isolatedNodeRow) assertOnlyTheApplyOnNode() {
	m := r.m
	m.t.Helper()
	pods := &corev1.PodList{}
	m.check(m.cluster.Client.List(m.ctx, pods, client.MatchingFields{"spec.nodeName": r.node}),
		"the Pods on %s could not be listed", r.node)
	if !onlyIsolatedApplyOnNode(pods.Items, m.in.TestNamespace, r.name, r.podUID) {
		r.report()
		m.fatalf("%s runs something besides the Apply this row isolates", r.node)
	}
}

// assertRanOnce takes one reading of the database and leaves the migrations
// recorded applied in applied.
func (r *isolatedNodeRow) assertRanOnce() {
	m := r.m
	m.t.Helper()
	value := m.query(isolatedReadingQuery(m.engine.name), r.database)
	m.scan([]byte(value), "the reading of "+r.database)
	reading, ok := parseIsolatedReading(value)
	if !ok {
		r.report()
		m.fatalf("the %s database of %s could not be read: [%s]", m.engine.name, r.name, value)
	}
	if !reading.ranOnce() {
		r.report()
		m.fatalf("the %s database of %s holds %d revision rows over %d versions and %d widgets, so a migration ran twice",
			m.engine.name, r.name, reading.rows, reading.versions, reading.widgets)
	}
	r.applied = reading.applied
}

// waitForCommit waits for the database to say the first two migrations
// committed: the part of the run that has to be behind the cut, with the
// third, which sleeps, in front of it.
func (r *isolatedNodeRow) waitForCommit() {
	m := r.m
	m.t.Helper()
	if !m.miUntil(2*time.Second, func() bool {
		return m.query("SELECT count(*) FROM schema_migrations WHERE version <= 2 AND state = 'applied'", r.database) == "2"
	}) {
		r.report()
		m.fatalf("the %s Apply on %s did not commit its first two migrations within %s", m.engine.name, r.node, waitTimeout)
	}
}

// isolate cuts the node off. Its own traffic to the API server leaves through
// OUTPUT, and the API server's to the kubelet arrives through INPUT; a Pod's
// traffic crosses the node through FORWARD, so the executor keeps its
// database, which the readings below check rather than assume.
func (r *isolatedNodeRow) isolate() {
	m := r.m
	m.t.Helper()
	m.logf("cutting %s off from the API server inside the %s Apply", r.node, m.engine.kind)
	// Armed before the first rule, so a failure between the two still has
	// cleanup take down whichever went in.
	m.isolationRulesApplied = true
	if _, err := m.miNodeExec(m.ctx, os.Stderr, append([]string{"iptables", "-I", "OUTPUT", "1"}, isolationRuleSpecs[0][1:]...)...); err != nil {
		m.fatalf("the rule cutting %s off from the API server could not be inserted", r.node)
	}
	if _, err := m.miNodeExec(m.ctx, os.Stderr, append([]string{"iptables", "-I", "INPUT", "1"}, isolationRuleSpecs[1][1:]...)...); err != nil {
		m.fatalf("the rule cutting the API server off from %s's kubelet could not be inserted", r.node)
	}
	r.isolatedAt = time.Now().Unix()
	r.jobsAtIsolation = r.jobUIDs()
	rules, err := m.miIsolationRules(m.ctx)
	if err != nil {
		m.fatalf("the rules on %s could not be read back", r.node)
	}
	if rules != 2 {
		m.fatalf("%s carries %d isolation rules, not the two this row inserted", r.node, rules)
	}
	// The run has to be going when the node is cut off. A run already over
	// would make the hold about nothing that could still write.
	r.assertRanOnce()
	if r.applied != 2 {
		r.report()
		m.fatalf("the %s Apply had %d migrations applied when its node was cut off, not the two before the one that sleeps",
			m.engine.name, r.applied)
	}
}

// jobUIDs is every Job of the resource, of any operation, by UID.
func (r *isolatedNodeRow) jobUIDs() []string {
	m := r.m
	m.t.Helper()
	jobs := &batchv1.JobList{}
	m.check(m.list(jobs, client.MatchingLabels{labelMigration: r.name}), "the Jobs of %s could not be listed", r.name)
	uids := make([]string, 0, len(jobs.Items))
	for _, job := range jobs.Items {
		uids = append(uids, string(job.UID))
	}
	slices.Sort(uids)
	return uids
}

// assertHeld is one poll of everything a replacement or a premature release
// would change. It leaves the Apply Pods and the Lease it read behind.
func (r *isolatedNodeRow) assertHeld() {
	m := r.m
	m.t.Helper()
	if !isolatedApplyHeld(m.migration(r.name), r.claim.jobUID, r.claim.epoch) {
		r.report()
		m.fatalf("%s let go of an Apply whose node is cut off, while its Pod may still be writing", r.name)
	}
	// One Apply Job and one Apply Pod, the ones dispatched before the node was
	// cut off, and no Job of any other operation either: while the claim
	// stands, the resource runs nothing else against this database.
	applies := m.applyJobUIDs(r.name)
	r.seen = append(r.seen, applies...)
	if !slices.Equal(applies, []string{r.claim.jobUID}) {
		r.report()
		m.fatalf("%s has Apply Jobs [%s] while the one it dispatched may still be writing", r.name, strings.Join(applies, " "))
	}
	if len(newUIDs(r.jobsAtIsolation, r.jobUIDs())) != 0 {
		r.report()
		m.fatalf("%s created a Job while its Apply's node was cut off", r.name)
	}
	pods := &corev1.PodList{}
	m.check(m.list(pods, client.MatchingLabels{labelMigration: r.name, labelOperation: "apply"}),
		"the %s Apply Pods could not be listed", m.engine.name)
	r.lastPods = pods.Items
	if !onlyPod(pods.Items, r.podUID) {
		r.report()
		m.fatalf("%s has Apply Pods other than the one on the node that was cut off", r.name)
	}
	// The realm stays with the claim: same holder, same epoch, still there.
	lease := &coordinationv1.Lease{}
	if err := m.cluster.Client.Get(m.ctx, types.NamespacedName{Namespace: r.leaseNamespace, Name: r.lease}, lease); err != nil {
		r.report()
		m.fatalf("the %s realm Lease is gone while the Apply that took it may still be writing", m.engine.name)
	}
	r.lastLease = lease
	if !heldAs(lease, r.holder, r.claim.epoch) {
		r.report()
		m.fatalf("the %s realm Lease left the claim while its Apply may still be writing", m.engine.name)
	}
	r.assertRanOnce()
}

// hold holds the claim until every clock that could tempt a replacement has
// run out, each read from what was persisted: the unreachable taint's
// timeAdded plus the Pod's own toleration, the Job's startTime plus its
// activeDeadlineSeconds, and the Lease's acquisition plus its duration. The
// hold ends on a poll that ran every assertion after the last of them, with a
// margin for the controller's requeue and the harness clock. Four things have
// to have happened inside it for it to have held anything: the node went
// Unknown, the API server could not read the Pod's log, the Pod was asked to
// stop, and the run committed its third migration while the node was cut off.
func (r *isolatedNodeRow) hold() {
	m := r.m
	m.t.Helper()
	const margin = 20
	bound := isolatedHoldBound(r.termEnd, r.jobDeadlineAt, int64(waitTimeout/time.Second))
	unreachable, logsRefused, committed := false, false, false
	evictionAt, stopRequestedAt := int64(0), int64(0)
	evicted, stopped := false, false
	for {
		if time.Now().Unix() >= bound {
			r.report()
			m.fatalf("the hold on %s did not see every clock run out by its bound: unreachable=%s eviction=%s stopRequested=%s logsRefused=%s committed=%s",
				r.node, miYesNo(unreachable), miSecondsOr(evicted, evictionAt, "unknown"),
				miSecondsOr(stopped, stopRequestedAt, "never"), miYesNo(logsRefused), miYesNo(committed))
		}
		r.assertHeld()
		if r.applied >= 3 {
			committed = true
		}
		// When the Pod was asked to stop: its deletion less its grace period.
		if at, ok := podStopRequestedAt(&r.lastPods[0]); ok && !stopped {
			stopRequestedAt, stopped = at, true
		}
		node := &corev1.Node{}
		if err := m.cluster.Client.Get(m.ctx, types.NamespacedName{Name: r.node}, node); err != nil {
			m.fatalf("%s could not be read", r.node)
		}
		if miNodeNotReady(node) {
			unreachable = true
		}
		if at, ok := unreachableTaintAddedAt(node); ok && !evicted {
			evictionAt, evicted = at+r.toleration, true
		}
		// The log the controller would read the result from, asked for through
		// the API server the way the controller asks. Nothing is printed: a read
		// that succeeded would carry the executor's output.
		if unreachable && !logsRefused {
			ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
			_, err := m.cluster.ContainerLog(ctx, m.in.TestNamespace, r.pod, "ptah")
			cancel()
			if err == nil {
				r.report()
				m.fatalf("the API server still read the %s Apply Pod's log from %s, so the node was not cut off", m.engine.name, r.node)
			}
			logsRefused = true
		}
		if unreachable && evicted && stopped && logsRefused && committed &&
			time.Now().Unix() > max(r.termEnd, r.jobDeadlineAt, evictionAt)+margin {
			break
		}
		m.sleep(5 * time.Second)
	}
	// The eviction came while the Job was alive, which is the state in which a
	// Job would start a second Pod if its replacement policy let it. Measured
	// from the Pod's and the Job's own timestamps rather than from the polls.
	if stopRequestedAt >= r.jobDeadlineAt {
		r.report()
		m.fatalf("the %s Apply Pod was first asked to stop at %d, not before its Job's deadline at %d, so the row never held an evicted Pod under a live Job",
			m.engine.name, stopRequestedAt, r.jobDeadlineAt)
	}
	// Past the term the Lease was taken for, it is still held, and renewed:
	// the controller did not leave it to lapse under a Pod it cannot see stop.
	if !leaseRenewedSince(r.lastLease, r.isolatedAt, time.Now().Unix()) {
		r.report()
		m.fatalf("the %s realm Lease was not renewed while its Apply's node was cut off", m.engine.name)
	}
	m.logf("%s held its Apply past the node going Unknown, the eviction, the Job deadline and the Lease term", r.name)
}

func miYesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func miSecondsOr(known bool, seconds int64, otherwise string) string {
	if !known {
		return otherwise
	}
	return strconv.FormatInt(seconds, 10)
}

// rejoin lets the node back to the API server and waits for it to be Ready.
func (r *isolatedNodeRow) rejoin() {
	m := r.m
	m.t.Helper()
	m.logf("letting %s back to the API server", r.node)
	if err := m.removeIsolationRules(m.ctx); err != nil {
		m.fatalf("the isolation rules could not be removed from %s: %v", r.node, err)
	}
	m.isolationRulesApplied = false
	if !m.miUntil(5*time.Second, func() bool {
		node := &corev1.Node{}
		return m.cluster.Client.Get(m.ctx, types.NamespacedName{Name: r.node}, node) == nil && miNodeReady(node)
	}) {
		r.report()
		m.fatalf("%s did not become Ready within %s of rejoining", r.node, waitTimeout)
	}
}

// assertSettles holds the end state the code defines, in the order it writes
// it: the run recorded Unknown against its own Job, the Lease handed back once
// no Pod of that Job is left, and the record settled by a reading of the same
// database. Across all of it, no second Apply and nothing run twice.
func (r *isolatedNodeRow) assertSettles() {
	m := r.m
	m.t.Helper()
	var recorded *ptahv1alpha1.PtahMigration
	if !m.miUntil(5*time.Second, func() bool {
		migration := m.migration(r.name)
		m.assertNoNewApplyJob(r.seen, "after its isolated Apply", r.name)
		r.seen = append(r.seen, m.applyJobUIDs(r.name)...)
		r.assertRanOnce()
		if isolatedRunRecorded(migration.Status, r.claim.jobUID) {
			recorded = migration
			return true
		}
		return false
	}) {
		r.report()
		m.fatalf("%s did not record its isolated Apply within %s of the node rejoining", r.name, waitTimeout)
	}
	if !isolatedRunUnknown(recorded.Status, r.claim.jobUID) {
		r.report()
		m.fatalf("%s did not record its isolated Apply as a run nobody accounted for", r.name)
	}

	// Released is an emptied holder. The controller never deletes the Lease,
	// so a read that fails is a read to repeat, not a release.
	if !m.miUntil(5*time.Second, func() bool {
		lease := &coordinationv1.Lease{}
		if m.cluster.Client.Get(m.ctx, types.NamespacedName{Namespace: r.leaseNamespace, Name: r.lease}, lease) != nil {
			return false
		}
		r.lastLease = lease
		return holderEmpty(lease)
	}) {
		r.report()
		m.fatalf("the %s realm Lease was not handed back once the isolated Apply was recorded", m.engine.name)
	}
	pods := &corev1.PodList{}
	m.check(m.list(pods, client.MatchingLabels{"batch.kubernetes.io/controller-uid": r.claim.jobUID}),
		"the %s Apply Pods could not be listed", m.engine.name)
	if len(pods.Items) != 0 {
		r.report()
		m.fatalf("the %s realm Lease was handed back while a Pod of the isolated Apply still exists", m.engine.name)
	}

	if !m.miUntil(5*time.Second, func() bool {
		migration := m.migration(r.name)
		m.assertNoNewApplyJob(r.seen, "after its isolated Apply", r.name)
		r.assertRanOnce()
		return isolatedRunSettled(migration.Status, r.claim.jobUID)
	}) {
		r.report()
		m.fatalf("%s did not settle its isolated Apply by reading the database within %s", r.name, waitTimeout)
	}

	// The database holds the three migrations, each once, and the rows the
	// first one inserted, once.
	if versions := m.query(appliedVersionsQuery(m.engine.name), r.database); versions != "1,2,3" {
		m.fatalf("the %s database of %s records [%s] applied, not migrations 1 to 3 once each", m.engine.name, r.name, versions)
	}
	r.assertRanOnce()
	if r.applied != 3 {
		m.fatalf("the %s database of %s records %d migrations applied, not three", m.engine.name, r.name, r.applied)
	}
	// Exactly one Apply Job ever existed for this resource. The record is
	// every listing taken from the claim to the settlement, because the
	// cleanup TTL removes a harvested Job before the row ends.
	if dispatched := miDistinct(r.seen); !slices.Equal(dispatched, []string{r.claim.jobUID}) {
		m.fatalf("%s dispatched Apply Jobs [%s], not the one it isolated", r.name, strings.Join(dispatched, " "))
	}
}

// report prints everything that decides whether a replacement could run, from
// each side: the claim, the Jobs and Pods the resource owns, the node the API
// server lost, the realm Lease, and the rules this row put in. Fields are
// chosen, not dumped: a Pod spec names Secrets.
func (r *isolatedNodeRow) report() {
	m := r.m
	state := map[string]any{}
	migration := &ptahv1alpha1.PtahMigration{}
	if err := m.get(r.name, migration); err != nil {
		state["migration"] = "the resource could not be read"
	} else {
		status := migration.Status
		entry := map[string]any{
			"phase": cmpOrNone(string(status.Phase)), "finalizers": migration.Finalizers,
			"conditions": miConditions(status.Conditions, 160),
		}
		if claim := status.ActiveOperation; claim != nil {
			entry["activeOperation"] = map[string]any{
				"type": claim.Type, "jobUID": claim.JobUID, "leaseEpoch": claim.LeaseEpoch,
				"leaseContinuityLost": claim.LeaseContinuityLost,
			}
		}
		if release := status.PendingLockRelease; release != nil {
			entry["pendingLockRelease"] = release.LeaseEpoch
		}
		if run := status.LastRun; run != nil {
			entry["lastRun"] = map[string]any{"outcome": run.Outcome, "jobUID": run.JobUID, "finishedAt": miInstant(run.FinishedAt)}
		}
		if run := status.UnresolvedRun; run != nil {
			entry["unresolvedRun"] = map[string]any{"outcome": run.Outcome, "jobUID": run.JobUID}
		}
		if history := status.History; history != nil {
			entry["history"] = map[string]any{
				"observedAt": instantOf(history.ObservedAt), "pending": history.PendingCount, "applied": history.AppliedCount,
			}
		}
		state["migration"] = entry
	}
	jobs := &batchv1.JobList{}
	if m.list(jobs, client.MatchingLabels{labelMigration: r.name}) == nil {
		var entries []any
		for _, job := range jobs.Items {
			var conditions []string
			for _, condition := range job.Status.Conditions {
				conditions = append(conditions, fmt.Sprintf("%s=%s(%s)", condition.Type, condition.Status, condition.Reason))
			}
			entries = append(entries, map[string]any{
				"name": job.Name, "uid": job.UID, "operation": job.Labels[labelOperation],
				"active": job.Status.Active, "terminating": job.Status.Terminating, "failed": job.Status.Failed,
				"succeeded": job.Status.Succeeded, "startTime": miInstant(job.Status.StartTime),
				"deadline": job.Spec.ActiveDeadlineSeconds, "conditions": conditions,
			})
		}
		state["jobs"] = entries
	}
	pods := &corev1.PodList{}
	if m.list(pods, client.MatchingLabels{labelMigration: r.name}) == nil {
		var entries []any
		for _, pod := range pods.Items {
			var conditions []string
			for _, condition := range pod.Status.Conditions {
				conditions = append(conditions, fmt.Sprintf("%s=%s(%s)", condition.Type, condition.Status, condition.Reason))
			}
			entries = append(entries, map[string]any{
				"name": pod.Name, "uid": pod.UID, "node": cmpOrDefault(pod.Spec.NodeName, "<unscheduled>"),
				"phase": pod.Status.Phase, "deletionTimestamp": miInstant(pod.DeletionTimestamp), "conditions": conditions,
			})
		}
		state["pods"] = entries
	}
	node := &corev1.Node{}
	if m.cluster.Client.Get(m.ctx, types.NamespacedName{Name: r.node}, node) == nil {
		var ready, taints []string
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				ready = append(ready, fmt.Sprintf("%s since %s", condition.Status, instantOf(condition.LastTransitionTime)))
			}
		}
		for _, taint := range node.Spec.Taints {
			added := "-"
			if taint.TimeAdded != nil {
				added = instantOf(*taint.TimeAdded)
			}
			taints = append(taints, fmt.Sprintf("%s:%s@%s", taint.Key, taint.Effect, added))
		}
		state["node"] = map[string]any{"name": node.Name, "ready": ready, "taints": taints}
	}
	if r.lease != "" {
		lease := &coordinationv1.Lease{}
		if m.cluster.Client.Get(m.ctx, types.NamespacedName{Namespace: r.leaseNamespace, Name: r.lease}, lease) == nil {
			entry := map[string]any{
				"name": lease.Name, "epoch": lease.Annotations[annotationLeaseEpoch],
				"duration": lease.Spec.LeaseDurationSeconds, "renewTime": "<none>",
			}
			if lease.Spec.HolderIdentity != nil {
				entry["holder"] = *lease.Spec.HolderIdentity
			}
			if lease.Spec.RenewTime != nil {
				entry["renewTime"] = lease.Spec.RenewTime.UTC().Format(time.RFC3339Nano)
			}
			state["lease"] = entry
		}
	}
	if rules, err := m.miIsolationRules(m.ctx); err == nil {
		state["isolationRules"] = rules
	} else {
		state["isolationRules"] = "unreadable"
	}
	m.miReport(r.name+" state when the check failed", state)
}

// unknownLayerProof ports run_unknown_layer_proof: an executor meets an
// artifact built by something newer than itself, and refuses the whole
// artifact on the layer descriptor before a database connection exists.
func (m *migrationRun) unknownLayerProof() {
	m.t.Helper()
	name := "e2e-unknown-layer-" + m.engine.name
	database, secret := "ptah_e2e_unknown_layer", "e2e-"+m.engine.name+"-unknown-layer-db"
	m.isolatedDatabase(database, secret)
	m.publishUnknownLayerArtifact()
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: m.reference("-unknown"),
		coordinationKey: "e2e/unknown-layer/" + m.engine.name, apply: "Always", interval: "30s",
	}))

	// First the refusal has to arrive, which takes as long as resolving and
	// verifying take: only the history read fetches artifact bytes, so that is
	// the operation whose fetch step fails. Then the window is held open to
	// say that nothing follows it. Holding it without waiting first would fail
	// on a poll that landed before the operator had got that far.
	if !m.miUntil(migrationPoll, func() bool {
		return refusedAtBoundary(m.migration(name).Status, "fetch-migrations")
	}) {
		m.fatalf("%s never named the step that refused the artifact within %s", name, waitTimeout)
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if !actedOnNothing(m.migration(name).Status) {
			m.fatalf("%s acted on an artifact carrying a layer its executor cannot read", name)
		}
		if len(m.applyJobUIDs(name)) != 0 {
			m.fatalf("%s dispatched a run for an artifact its executor refused", name)
		}
		m.sleep(10 * time.Second)
	}
	// Nothing connected. Ptah creates its revision table on the first history
	// read, so a database that has none was never opened, which the refusal's
	// own message cannot prove.
	if m.query(unknownLayerRevisionTablesQuery(m.engine.name, database), database) != "0" {
		m.fatalf("the %s database was opened for an artifact whose layers were refused", m.engine.name)
	}
	m.logf("PASS %s refused an artifact built newer than its executor, before opening the database", m.engine.kind)
}

// publishUnknownLayerArtifact builds the artifact no product command builds:
// the engine's migrations with a layer of the next version of the format. It
// runs on the host, against the registry as the host reaches it.
func (m *migrationRun) publishUnknownLayerArtifact() {
	m.t.Helper()
	if m.in.RegistryHostAddress == "" {
		m.fatalf("the harness published no registry address, so the unknown-layer artifact cannot be built")
	}
	if info, err := os.Stat(m.in.RegistryCredentialsFile); m.in.RegistryCredentialsFile == "" || err != nil || info.Size() == 0 {
		m.fatalf("the harness published no registry credentials file")
	}
	m.logf("publishing a %s artifact carrying a layer this executor cannot accept", m.engine.kind)
	content, err := os.ReadFile(m.in.RegistryCredentialsFile)
	m.check(err, "read the registry credentials file")
	credentials, err := parseRegistryCredentials(content)
	m.check(err, "read the registry credentials")
	root, err := filepath.Abs(repositoryRoot)
	m.check(err, "resolve the repository root")
	fixtures, err := filepath.Abs(m.fixtureDir(""))
	m.check(err, "resolve the migration fixtures")
	command := exec.CommandContext(m.ctx, "go", "-C", root, "run", "./hack/unknownlayerfixture", //nolint:gosec // Arguments, not a shell.
		"--reference", m.in.RegistryHostAddress+"/"+m.repository+"/"+m.engine.name+"-unknown:stable",
		"--dir", fixtures,
		"--unknown-media-type", unknownLayerMediaType,
		"--username", credentials.Username,
		"--password", credentials.Password,
		"--plain-http")
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err = command.Run()
	m.scan(output.Bytes(), "the unknown-layer publisher")
	if err != nil {
		// The publisher was handed the registry password, which the scanner
		// does not know, so its words are printed only without it.
		if bytes.Contains(output.Bytes(), []byte(credentials.Password)) {
			_, _ = fmt.Fprintln(os.Stderr, "e2e migrations:   the publisher's output is withheld: it carries the registry password")
		} else {
			for line := range strings.SplitSeq(strings.TrimRight(output.String(), "\n"), "\n") {
				_, _ = fmt.Fprintf(os.Stderr, "e2e migrations:   %s\n", line)
			}
		}
		m.fatalf("the unknown-layer artifact could not be published: %v", err)
	}
}

// egressPolicyProof ports run_egress_policy_proof: the egress example,
// applied in a cluster whose CNI enforces NetworkPolicy, and what each
// operation can actually reach under it.
//
// Only what the example tells a reader to replace is replaced. Each operation
// is represented by a probe Pod carrying the labels the builder gives that
// operation's Pods, except the manager label, which admission keeps for Pods
// of a Job the operator made; the probes carry egressProbeManager there, and a
// copy of the policies that differs in that value alone selects them. One real
// migration then runs to InSync under the same policies, which is the half a
// probe cannot show.
func (m *migrationRun) egressPolicyProof() {
	m.t.Helper()
	m.logf("applying the egress example to the %s operation Pods", m.engine.kind)
	r := &egressRow{m: m}
	r.readAddresses()
	r.render()
	r.assertEnforced()
	r.assertAMigrationConverges()
	r.removePolicies()
	m.check(m.cluster.Client.DeleteAllOf(m.ctx, &corev1.Pod{}, client.InNamespace(m.in.TestNamespace),
		client.MatchingLabels{egressProbeLabel: "egress"}), "the egress probes were not removed")
	m.poll("the egress probes to be removed", time.Second, func() bool {
		pods := &corev1.PodList{}
		return m.list(pods, client.MatchingLabels{egressProbeLabel: "egress"}) == nil && len(pods.Items) == 0
	})
	m.logf("PASS %s operation Pods reach what the egress example grants and nothing else", m.engine.kind)
}

// egressRow is what the egress row reads before it applies anything.
type egressRow struct {
	m                       *migrationRun
	target                  egressProbeTarget
	policies, probePolicies []map[string]any
}

// readAddresses reads each address a probe dials from the object that owns
// it, and holds each to exactly one.
func (r *egressRow) readAddresses() {
	m := r.m
	m.t.Helper()
	registry := &discoveryv1.EndpointSliceList{}
	m.check(m.list(registry, client.MatchingLabels{discoveryv1.LabelServiceName: m.in.RegistryService}),
		"the registry EndpointSlices could not be listed")
	registryIP, ok := soleEndpointAddress(registry.Items)
	if !ok {
		m.fatalf("the registry Service %s does not resolve to exactly one address", m.in.RegistryService)
	}
	databases := &corev1.PodList{}
	m.check(m.list(databases, client.MatchingLabels{"app.kubernetes.io/name": m.engine.service},
		client.MatchingFields{"status.phase": string(corev1.PodRunning)}), "the %s database Pods could not be listed", m.engine.kind)
	databaseIP, ok := soleRunningPodIP(databases.Items)
	if !ok {
		m.fatalf("the %s database is not exactly one running Pod labeled app.kubernetes.io/name=%s", m.engine.kind, m.engine.service)
	}
	// The probes run the PostgreSQL image on both engines: it is on every node
	// already, and its busybox has nc, timeout and nslookup, which the MySQL
	// image does not.
	postgres := &corev1.PodList{}
	m.check(m.list(postgres, client.MatchingLabels{"app.kubernetes.io/name": pgService}), "list the PostgreSQL Pods")
	image, ok := soleProbeImage(postgres.Items)
	if !ok {
		m.fatalf("no single PostgreSQL image is running in %s for the egress probes to use", m.in.TestNamespace)
	}
	api := &discoveryv1.EndpointSliceList{}
	m.check(m.cluster.Client.List(m.ctx, api, client.InNamespace(metav1.NamespaceDefault),
		client.MatchingLabels{discoveryv1.LabelServiceName: "kubernetes"}), "the API server endpoints could not be listed")
	apiIP, apiPort, ok := firstAPIEndpoint(api.Items)
	if !ok {
		m.fatalf("the API server has no endpoint for the probes to dial")
	}
	r.target = egressProbeTarget{
		image: image, registry: registryIP, database: databaseIP, databasePort: m.engine.port, api: apiIP, apiPort: apiPort,
	}
}

// render adapts the example, holds the rendering to the example's selectors,
// and makes the probe copy of it.
func (r *egressRow) render() {
	m := r.m
	m.t.Helper()
	content, err := os.ReadFile(filepath.Join(repositoryRoot, "examples", "networkpolicy-egress.yaml"))
	if err != nil {
		m.fatalf("the egress example could not be read")
	}
	example, err := egressExampleItems(content)
	if err != nil {
		m.fatalf("the egress example could not be read: %v", err)
	}
	if !egressExampleShaped(example) {
		m.fatalf("the egress example no longer has the default-deny, registry and database policies this row adapts")
	}
	port, err := strconv.ParseInt(m.engine.port, 10, 64)
	m.check(err, "read the %s port", m.engine.kind)
	r.policies, err = renderEgressPolicies(example, m.in.TestNamespace, r.target.registry, m.engine.service, port)
	m.check(err, "render the egress example")
	// What the row is about has to be the example's, byte for byte.
	if !egressSelectorsKept(example, r.policies) {
		m.fatalf("rendering the egress example changed a selector; the row would measure a policy the example does not contain")
	}
	r.probePolicies, err = egressProbeCopy(r.policies, egressProbeManager)
	if err != nil {
		m.fatalf("the probe copy of the egress policies could not be made: %v", err)
	}
	// Mapped back, the copy is the rendering it was made from.
	if !egressProbeCopyFaithful(r.policies, r.probePolicies) {
		m.fatalf("the probe copy of the egress policies differs from the example by more than the manager label")
	}
}

func (r *egressRow) applyPolicies() {
	m := r.m
	m.t.Helper()
	// Set before the first apply: an apply that fails after the API server
	// stored the object still leaves a policy for the cleanup to remove.
	m.egressApplied = true
	for _, policy := range r.policies {
		if err := m.apply(runtime.DeepCopyJSON(policy)); err != nil {
			m.fatalf("the egress policies could not be applied: %v", err)
		}
	}
	for _, policy := range r.probePolicies {
		if err := m.apply(runtime.DeepCopyJSON(policy)); err != nil {
			m.fatalf("the probe copy of the egress policies could not be applied: %v", err)
		}
	}
}

// removePolicies removes every policy the row applied, by the proof label, so
// the removal does not depend on a name list that could fall behind the
// example.
func (r *egressRow) removePolicies() {
	m := r.m
	m.t.Helper()
	m.check(m.cluster.Client.DeleteAllOf(m.ctx, &networkingv1.NetworkPolicy{}, client.InNamespace(m.in.TestNamespace),
		client.MatchingLabels{egressProofLabel: "egress"}), "the egress policies were not removed")
	m.poll("the egress policies to be removed", time.Second, func() bool {
		policies := &networkingv1.NetworkPolicyList{}
		return m.list(policies, client.MatchingLabels{egressProofLabel: "egress"}) == nil && len(policies.Items) == 0
	})
	m.egressApplied = false
}

// createProbe starts one probe Pod carrying the labels given; none is a Pod no
// policy selects.
func (r *egressRow) createProbe(name string, labels map[string]any) {
	m := r.m
	m.t.Helper()
	if err := m.create(egressProbePod(m.in.TestNamespace, name, labels, r.target)); err != nil {
		m.fatalf("egress probe %s could not be created", name)
	}
}

// reading is what a finished probe reached, as one line.
func (r *egressRow) reading(name string) string {
	m := r.m
	m.t.Helper()
	var reading string
	if !m.miUntil(2*time.Second, func() bool {
		pod := &corev1.Pod{}
		if m.get(name, pod) != nil {
			return false
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			log, err := m.cluster.ContainerLog(m.ctx, m.in.TestNamespace, name, "probe")
			m.check(err, "read what egress probe %s reached", name)
			m.scan(log, "egress probe "+name)
			reading = probeReading(log)
			return true
		case corev1.PodFailed:
			m.fatalf("egress probe %s failed instead of reporting what it reached", name)
		}
		return false
	}) {
		m.fatalf("egress probe %s did not finish within %s", name, waitTimeout)
	}
	return reading
}

// assertEnforced reads what every operation of both families reaches under
// the example, after showing that everything the table refuses was reachable
// before it, and that enforcement has begun.
func (r *egressRow) assertEnforced() {
	m := r.m
	m.t.Helper()
	// Unlabeled, before the policies: everything the table refuses has to be
	// reachable in the first place, or a refusal below says nothing.
	r.createProbe("egress-probe-baseline", nil)
	if baseline := r.reading("egress-probe-baseline"); baseline != egressOpenReading {
		m.fatalf("before any policy, an unlabeled probe reached only: %s", baseline)
	}

	r.applyPolicies()
	// Policies take effect some time after the API accepts them. The canary is
	// a schema Apply, which the example refuses the registry, and nothing below
	// is read until one has been refused: a table read earlier would report the
	// network before enforcement, which is every connection open.
	deadline := time.Now().Add(waitTimeout)
	for canary := 1; ; canary++ {
		name := "egress-probe-canary-" + strconv.Itoa(canary)
		r.createProbe(name, egressProbeLabels(egressProbeManager, schemaOperationComponent, "apply"))
		if strings.Contains(r.reading(name), "registry=closed") {
			break
		}
		if !time.Now().Before(deadline) {
			m.fatalf("the egress example was applied and a schema Apply Pod could still reach the registry after %s: the CNI is not enforcing it",
				waitTimeout)
		}
	}
	for _, expectation := range egressExpectations {
		r.createProbe("egress-probe-"+expectation.family+"-"+expectation.operation,
			egressProbeLabels(egressProbeManager, expectation.family+"-operation", expectation.operation))
	}
	r.createProbe("egress-probe-unselected", nil)

	checked := 0
	for _, expectation := range egressExpectations {
		got := r.reading("egress-probe-" + expectation.family + "-" + expectation.operation)
		if got != expectation.want() {
			m.fatalf("a %s %s Pod under the egress example reached {%s}, and the example grants {%s}",
				expectation.family, expectation.operation, got, expectation.want())
		}
		checked++
	}
	if checked != 9 {
		m.fatalf("the egress row checked %d operations, and both families have nine", checked)
	}
	// The policies isolate the Pods they select and no others.
	if unselected := r.reading("egress-probe-unselected"); unselected != egressOpenReading {
		m.fatalf("a Pod no egress policy selects reached only: %s", unselected)
	}
}

// assertAMigrationConverges runs a real migration, History and Apply
// included, to InSync with the example's policies in force: the half a probe
// cannot show, since the operation's own containers, the init container that
// fetches the artifact among them, have to get what they need.
func (r *egressRow) assertAMigrationConverges() {
	m := r.m
	m.t.Helper()
	name, secret := "e2e-egress-"+m.engine.name, "e2e-"+m.engine.name+"-egress-db"
	m.isolatedDatabase("ptah_e2e_egress", secret)
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: m.reference(""),
		coordinationKey: "e2e/egress/" + m.engine.name, apply: "Always",
	}))
	// The Jobs are recorded while the run goes on, because their TTL can
	// remove them before the end; their Pod templates carry the Pods' labels.
	var jobs []batchv1.Job
	var last *ptahv1alpha1.PtahMigration
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		listed := &batchv1.JobList{}
		if m.list(listed, client.MatchingLabels{labelMigration: name}) == nil {
			jobs = append(jobs, listed.Items...)
		}
		last = m.migration(name)
		if egressConverged(last.Status) {
			break
		}
		m.sleep(migrationPoll)
	}
	if last == nil || !egressConverged(last.Status) {
		phase := ""
		if last != nil {
			phase = string(last.Status.Phase)
		}
		m.fatalf("%s did not apply its sequence under the egress policies within %s; it is in %s", name, waitTimeout, cmpOrNone(phase))
	}
	// An Apply that converged because nothing selected it would pass
	// everything above.
	if !egressJobsSelected(jobs) {
		m.fatalf("the Jobs that ran %s are not all ones the egress example selects, or none of them was the Apply", name)
	}
}

// retargetBeforeDispatchProof ports run_retarget_before_dispatch_proof: the
// Secret a target's URL comes from, repointed between approval and dispatch.
// The apply gate holds the approved Apply's Pod off every node while the
// Secret is pointed at a second database on the same server; the runner
// compares the target it was handed with the one the plan names before it
// opens anything, so the refusal has to name that binding, and neither
// database may receive anything.
func (m *migrationRun) retargetBeforeDispatchProof() {
	m.t.Helper()
	r := &retargetRow{
		m: m, name: "e2e-retarget-" + m.engine.name, database: "ptah_e2e_retarget", other: "ptah_e2e_retarget_other",
		secret: "e2e-" + m.engine.name + "-retarget-db",
	}
	r.createDatabases()
	m.openApplyGate()
	// OnApproval so the approval chooses when the Apply is claimed, the gate in
	// the nodeSelector of every operation, and an hour's interval so no refresh
	// lands between the gate closing and the claim.
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: r.name, secret: r.secret, reference: m.reference(""),
		coordinationKey: "e2e/retarget/" + m.engine.name, apply: "OnApproval", interval: "1h",
		execution: map[string]any{
			"activeDeadlineSeconds": int64(300), "failureRetryInterval": "10s", "connectTimeout": "30s",
			"nodeSelector": map[string]any{applyGateLabel: "open"},
		},
	}))
	r.waitForPlan()
	m.logf("closing the gate before approving the %s plan", m.engine.kind)
	m.closeApplyGate()
	r.approve()
	r.waitForApply()
	r.waitForGatedPod()
	m.logf("pointing the %s Secret at another database while the Apply Pod waits", m.engine.kind)
	secret := &corev1.Secret{}
	secret.Namespace, secret.Name = m.in.TestNamespace, r.secret
	if err := m.mergePatch(secret, map[string]any{"stringData": map[string]any{"url": r.otherURL, "database": r.other}}); err != nil {
		m.fatalf("the %s retarget Secret could not be rewritten", m.engine.name)
	}
	m.openApplyGate()
	r.waitForRefusal()
	m.closeApplyGate()

	r.assertUntouched(r.database)
	r.assertUntouched(r.other)
	// A refused Apply is not replayed. The resource runs on an hour's interval,
	// so what could start another Apply inside this window is the refusal not
	// holding, which is the replay the unresolved record exists to prevent.
	recorded := []string{r.jobUID}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		m.assertNoNewApplyJob(recorded, "after its target was repointed", r.name)
		m.sleep(10 * time.Second)
	}
	r.assertUntouched(r.database)
	r.assertUntouched(r.other)
	m.logf("PASS %s refused an Apply whose target was repointed after approval", m.engine.kind)
}

// retargetRow is what the retarget row reads once.
type retargetRow struct {
	m                                *migrationRun
	name, database, other, secret    string
	url, otherURL, plan, job, jobUID string
}

// createDatabases creates both databases before the resource exists, so the
// second is a database the executor could have opened and not a connection
// that failed.
func (r *retargetRow) createDatabases() {
	m := r.m
	m.t.Helper()
	m.createDatabase(r.database)
	m.createDatabase(r.other)
	r.url, r.otherURL = m.databaseURL(r.database, ""), m.databaseURL(r.other, "")
	m.protect(r.url, r.otherURL)
	// Not immutable, unlike the other rows' Secrets: rewriting it is the row.
	m.check(m.apply(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"namespace": m.in.TestNamespace, "name": r.secret},
		"data": secretData(map[string]string{
			"username": migrationDatabaseUser, "password": m.password, "database": r.database, "url": r.url,
		}),
	}), "apply Secret %s", r.secret)
}

func (r *retargetRow) waitForPlan() {
	m := r.m
	m.t.Helper()
	if !m.miUntil(migrationPoll, func() bool {
		status := m.migration(r.name).Status
		if status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval && status.Plan != nil && status.Plan.Name != "" {
			r.plan = status.Plan.Name
			return true
		}
		return false
	}) {
		r.report()
		m.fatalf("%s did not publish a plan to approve within %s", r.name, waitTimeout)
	}
}

func (r *retargetRow) approve() {
	m := r.m
	m.t.Helper()
	migration, plan := &ptahv1alpha1.PtahMigration{}, &ptahv1alpha1.PtahMigrationPlan{}
	_ = m.get(r.name, migration)
	_ = m.get(r.plan, plan)
	if migration.UID == "" || plan.UID == "" || plan.Spec.Fingerprint == "" {
		m.fatalf("%s or its plan %s carries no identity to approve", r.name, r.plan)
	}
	if err := m.create(migrationApprovalDocument(m.in.TestNamespace, r.name+"-approval", r.name, string(migration.UID),
		r.plan, string(plan.UID), plan.Spec.Fingerprint)); err != nil {
		m.fatalf("the %s retarget approval could not be created", m.engine.name)
	}
}

func (r *retargetRow) waitForApply() {
	m := r.m
	m.t.Helper()
	if !m.miUntil(time.Second, func() bool {
		claim := m.migration(r.name).Status.ActiveOperation
		if claim == nil || claim.Type != ptahv1alpha1.MigrationOperationApply || claim.JobName == "" || claim.JobUID == "" {
			return false
		}
		r.job, r.jobUID = claim.JobName, string(claim.JobUID)
		return true
	}) {
		r.report()
		m.fatalf("%s did not claim an Apply within %s", r.name, waitTimeout)
	}
}

// waitForGatedPod waits for the Pod to exist with no node having taken it, so
// the value the container reads is the rewritten one and the approval really
// came first.
func (r *retargetRow) waitForGatedPod() {
	m := r.m
	m.t.Helper()
	if !m.miUntil(2*time.Second, func() bool {
		pods := &corev1.PodList{}
		m.check(m.list(pods, client.MatchingLabels{"job-name": r.job}),
			"the %s Apply Pods could not be read while the gate was closed", m.engine.name)
		if gatedApplyPods(pods.Items) {
			return true
		}
		if slices.ContainsFunc(pods.Items, func(pod corev1.Pod) bool { return pod.Spec.NodeName != "" }) {
			m.fatalf("the %s Apply Pod reached a node while the gate was closed, so the gate is not what held it", m.engine.name)
		}
		return false
	}) {
		r.report()
		m.fatalf("the %s Apply never produced a Pod held off every node", m.engine.name)
	}
}

func (r *retargetRow) waitForRefusal() {
	m := r.m
	m.t.Helper()
	if !m.miUntil(migrationPoll, func() bool {
		return retargetRefused(m.migration(r.name).Status, r.jobUID)
	}) {
		r.report()
		m.fatalf("%s never reported that its Apply was refused for a repointed target", r.name)
	}
}

// assertUntouched holds a database to no revision table with rows and no
// table from the artifact's first migration.
func (r *retargetRow) assertUntouched(database string) {
	m := r.m
	m.t.Helper()
	filter := m.engine.currentSchemaFilter()
	if m.query("SELECT count(*) FROM information_schema.tables WHERE "+filter+" AND table_name='schema_migrations'", database) != "0" &&
		m.query("SELECT count(*) FROM schema_migrations", database) != "0" {
		m.fatalf("the %s Apply whose target was repointed recorded migrations in %s", m.engine.name, database)
	}
	if m.query("SELECT count(*) FROM information_schema.tables WHERE "+filter+" AND table_name='e2e_migration_widgets'", database) != "0" {
		m.fatalf("the %s Apply whose target was repointed created its first migration's table in %s", m.engine.name, database)
	}
}

func (r *retargetRow) report() {
	m := r.m
	state := map[string]any{}
	migration := &ptahv1alpha1.PtahMigration{}
	if m.get(r.name, migration) == nil {
		status := migration.Status
		entry := map[string]any{"phase": cmpOrNone(string(status.Phase)), "conditions": miConditions(status.Conditions, 200)}
		if claim := status.ActiveOperation; claim != nil {
			entry["activeOperation"] = string(claim.Type) + "/" + cmpOrNone(claim.JobName)
		}
		if run := status.UnresolvedRun; run != nil {
			entry["unresolvedRun"] = cmpOrNone(run.JobName) + "/" + cmpOrNone(string(run.Outcome))
		}
		state["migration"] = entry
	}
	pods := &corev1.PodList{}
	if m.list(pods, client.MatchingLabels{labelMigration: r.name}) == nil {
		var entries []string
		for _, pod := range pods.Items {
			entries = append(entries, fmt.Sprintf("%s phase=%s node=%s", pod.Name, pod.Status.Phase,
				cmpOrDefault(pod.Spec.NodeName, "<unscheduled>")))
		}
		state["pods"] = entries
	}
	m.miReport(r.name+" state when the wait ended", state)
}

// rebuildDrill ports run_rebuild_drill: a cluster lost with an approval and a
// claimed Apply that had not run, rebuilt from that backup against a database
// that moved past it. The rebuilt resource reads the database, asks for a
// decision on what the release added meanwhile, and runs nothing its restored
// approval could appear to authorize.
func (m *migrationRun) rebuildDrill() {
	m.t.Helper()
	r := &drillRow{
		m: m, name: "e2e-drill-" + m.engine.name, database: "ptah_e2e_drill", secret: "e2e-" + m.engine.name + "-drill-db",
		olderReference: m.reference("-drill-older"), reference: m.reference("-drill"),
	}
	m.isolatedDatabase(r.database, r.secret)
	m.publish("drill-older", m.fixtureDir("-older"), r.olderReference)
	m.publish("drill", m.fixtureDir(""), r.reference)
	m.openApplyGate()
	// Always, on the two-migration artifact, so the database reaches version 2
	// with nothing to approve. The gate's selector is there from the start,
	// and the gate is open, because the selector reaches every operation Job.
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: r.name, secret: r.secret, reference: r.olderReference,
		coordinationKey: "e2e/drill/" + m.engine.name, apply: "Always", interval: "1h",
		execution: map[string]any{
			"activeDeadlineSeconds": int64(300), "failureRetryInterval": "10s", "connectTimeout": "30s",
			"nodeSelector": map[string]any{applyGateLabel: "open"},
		},
	}))
	r.waitForConvergence()
	if revisions := r.revisions(); revisions != "1,2" {
		m.fatalf("%s did not bring its database to version 2; it records [%s]", r.name, revisions)
	}

	// An approval for [3], its Apply claimed and held.
	moved := &ptahv1alpha1.PtahMigration{}
	moved.Namespace, moved.Name = m.in.TestNamespace, r.name
	if err := m.mergePatch(moved, map[string]any{"spec": map[string]any{
		"artifact": map[string]any{"ociRef": r.reference}, "policy": map[string]any{"apply": "OnApproval"},
	}}); err != nil {
		m.fatalf("%s could not be moved to the three-migration artifact", r.name)
	}
	matched, planName := r.waitForPlan()
	if versions := drillPlanVersions(m.planOf(planName)); versions != "3" {
		m.fatalf("the plan %s published approves [%s], and this row needs [3]", r.name, versions)
	}
	m.closeApplyGate()
	migrationUID := string(matched.UID)
	plan := m.planOf(planName)
	approval := r.name + "-approval"
	if err := m.create(migrationApprovalDocument(m.in.TestNamespace, approval, r.name, migrationUID,
		plan.Name, string(plan.UID), plan.Spec.Fingerprint)); err != nil {
		m.fatalf("the %s drill approval could not be created", m.engine.name)
	}
	// The backup, at the worst moment: an approval and a claimed Apply that
	// has not run.
	backup := r.waitForClaim()
	approvalBackup := &unstructured.Unstructured{}
	approvalBackup.SetGroupVersionKind(ptahv1alpha1.GroupVersion.WithKind("PtahMigrationApproval"))
	if err := m.get(approval, approvalBackup); err != nil {
		m.fatalf("the drill approval could not be backed up")
	}

	// The run goes ahead, so the database moves past the backup.
	m.openApplyGate()
	r.waitForConvergence()
	if revisions := r.revisions(); revisions != "1,2,3" {
		m.fatalf("the approved run did not bring the %s database to version 3; it records [%s]", m.engine.name, revisions)
	}

	// The loss. The resource's plans go with it through their owner
	// reference, and they are deleted by label as well so none outlives the
	// row.
	m.logf("losing %s, its plans and its approval", r.name)
	lostApproval := &ptahv1alpha1.PtahMigrationApproval{}
	lostApproval.Namespace, lostApproval.Name = m.in.TestNamespace, approval
	m.deleteAndWait(lostApproval, "the drill approval")
	lost := &ptahv1alpha1.PtahMigration{}
	lost.Namespace, lost.Name = m.in.TestNamespace, r.name
	m.deleteAndWait(lost, r.name)
	plans := &ptahv1alpha1.PtahMigrationPlanList{}
	m.check(m.list(plans, client.MatchingLabels{labelMigration: r.name}), "the %s plans could not be listed", r.name)
	for index := range plans.Items {
		m.deleteAndWait(&plans.Items[index], "the "+r.name+" plans")
	}

	// While the cluster is gone the release moves on, so the rebuilt resource
	// has a migration to run and a surviving approval would have something to
	// authorize.
	r.publishFourthMigration()

	// The rebuild: the specs reapplied from the backup.
	if err := m.create(strippedForRestore(backup.Object)); err != nil {
		m.fatalf("%s could not be restored from its backup", r.name)
	}
	rebuilt := &ptahv1alpha1.PtahMigration{}
	_ = m.get(r.name, rebuilt)
	rebuiltUID := string(rebuilt.UID)
	if rebuiltUID == "" || rebuiltUID == migrationUID {
		m.fatalf("the rebuilt %s kept the UID its backup had, so this is not the rebuild the row is about", r.name)
	}
	// The restored approval names a migration and a plan that no longer exist.
	// Admission may refuse it; if it is admitted it has to authorize nothing,
	// which the rest of the row checks either way.
	if err := m.create(strippedForRestore(approvalBackup.Object)); err == nil {
		m.logf("the restored approval was admitted; holding it to authorizing nothing")
	} else {
		refusal := err.Error()
		m.scan([]byte(refusal), "the restored approval's refusal")
		m.logf("the restored approval was refused: %s", refusal[:min(len(refusal), 300)])
	}

	// The rebuilt resource reads the database, which already holds [1 2 3],
	// and asks for a decision on [4]. The Apply Jobs it owns are recorded on
	// every poll rather than listed at the end, because a finished Job's TTL
	// can remove it before then.
	var applies []string
	var waiting *unstructured.Unstructured
	var rebuiltPlan string
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		jobs := &batchv1.JobList{}
		m.check(m.list(jobs, client.MatchingLabels{labelMigration: r.name, labelOperation: "apply"}),
			"the %s Apply Jobs could not be listed", r.name)
		applies = append(applies, drillOwnedApplyUIDs(jobs.Items, rebuiltUID)...)
		document, migration := r.read()
		if plan, ok := drillAwaitingDecision(migration.Status); ok && string(migration.UID) == rebuiltUID {
			waiting, rebuiltPlan = document, plan
			break
		}
		m.sleep(migrationPoll)
	}
	if waiting == nil {
		r.report()
		m.fatalf("the rebuilt %s never asked for a decision on the migration its database lacks", r.name)
	}
	// The document that matched is the one held to the rest of the claim.
	if versions := drillPlanVersions(m.planOf(rebuiltPlan)); versions != "4" {
		m.fatalf("the rebuilt %s asks to approve [%s], and the database lacks only [4]", r.name, versions)
	}
	if !drillRebuiltIdle(waiting.Object) {
		r.report()
		m.fatalf("the rebuilt %s recorded a run or claimed an Apply it had no approval for", r.name)
	}
	if len(applies) != 0 {
		m.fatalf("the rebuilt %s dispatched an Apply after the restore: %s", r.name, strings.Join(miDistinct(applies), " "))
	}
	if revisions := r.revisions(); revisions != "1,2,3" {
		m.fatalf("after the rebuild the %s database records [%s]; something ran without an approval", m.engine.name, revisions)
	}
	if m.query(drillMarkerQuery(m.engine.name), r.database) != "0" {
		m.fatalf("after the rebuild the %s database has the fourth migration's table; it ran without an approval", m.engine.name)
	}
	m.closeApplyGate()
	m.logf("PASS %s rebuilt against a database ahead of its backup and ran nothing unapproved", m.engine.kind)
}

// drillRow is what the rebuild drill reads.
type drillRow struct {
	m                         *migrationRun
	name, database, secret    string
	olderReference, reference string
	last                      *ptahv1alpha1.PtahMigration
}

// read reads the resource as the API server stores it, for the backup and
// for the claims about what is stored, and typed for the rest.
func (r *drillRow) read() (*unstructured.Unstructured, *ptahv1alpha1.PtahMigration) {
	m := r.m
	m.t.Helper()
	document := &unstructured.Unstructured{}
	document.SetGroupVersionKind(ptahv1alpha1.GroupVersion.WithKind("PtahMigration"))
	if err := m.get(r.name, document); err != nil {
		m.fatalf("%s could not be read", r.name)
	}
	m.scanObject(document.Object, r.name+" status")
	migration := &ptahv1alpha1.PtahMigration{}
	m.check(runtime.DefaultUnstructuredConverter.FromUnstructured(document.Object, migration), "decode %s", r.name)
	r.last = migration
	return document, migration
}

// revisions is every revision row the database holds, as "1,2".
func (r *drillRow) revisions() string {
	return r.m.query(drillRevisionsQuery(r.m.engine.name), r.database)
}

func (r *drillRow) waitForConvergence() {
	m := r.m
	m.t.Helper()
	if !m.miUntil(migrationPoll, func() bool {
		_, migration := r.read()
		return drillConverged(migration.Status)
	}) {
		r.report()
		m.fatalf("%s did not converge within %s", r.name, waitTimeout)
	}
}

// waitForPlan waits until the resource asks for a decision on a plan, and
// returns the document that asked and the plan.
func (r *drillRow) waitForPlan() (*ptahv1alpha1.PtahMigration, string) {
	m := r.m
	m.t.Helper()
	var matched *ptahv1alpha1.PtahMigration
	var plan string
	if !m.miUntil(migrationPoll, func() bool {
		_, migration := r.read()
		name, ok := drillAwaitingDecision(migration.Status)
		if ok {
			matched, plan = migration, name
		}
		return ok
	}) {
		r.report()
		m.fatalf("%s did not ask for a decision on a plan within %s", r.name, waitTimeout)
	}
	return matched, plan
}

// waitForClaim waits for the approved Apply to be claimed, and returns the
// stored document that shows it, which is the backup.
func (r *drillRow) waitForClaim() *unstructured.Unstructured {
	m := r.m
	m.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		document, migration := r.read()
		if drillApplyClaimed(migration.Status) {
			return document
		}
		if !time.Now().Before(deadline) {
			r.report()
			m.fatalf("%s did not claim the approved Apply within %s", r.name, waitTimeout)
		}
		m.sleep(time.Second)
	}
}

// publishFourthMigration publishes what the tag moves on to while the cluster
// is gone: the three migrations the database holds, and a fourth it does not,
// built from the engine's own fixtures so it cannot fall out of step with
// them.
func (r *drillRow) publishFourthMigration() {
	m := r.m
	m.t.Helper()
	directory := filepath.Join(m.workDir, m.engine.name+"-drill-next")
	m.check(os.RemoveAll(directory), "clear %s", directory)
	m.check(os.MkdirAll(directory, 0o700), "create %s", directory)
	entries, err := os.ReadDir(m.fixtureDir(""))
	m.check(err, "read the %s migration fixtures", m.engine.name)
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(m.fixtureDir(""), entry.Name()))
		m.check(err, "read %s", entry.Name())
		m.check(os.WriteFile(filepath.Join(directory, entry.Name()), content, 0o600), "copy %s", entry.Name())
	}
	m.check(os.WriteFile(filepath.Join(directory, "0000000004_create_drill_marker.up.sql"),
		[]byte("CREATE TABLE e2e_drill_marker (id INTEGER PRIMARY KEY);\n"), 0o600), "write the fourth migration")
	m.check(os.WriteFile(filepath.Join(directory, "0000000004_create_drill_marker.down.sql"),
		[]byte("DROP TABLE e2e_drill_marker;\n"), 0o600), "write the fourth migration's reversal")
	m.publish("drill-next", directory, r.reference)
}

func (r *drillRow) report() {
	m := r.m
	migration := r.last
	if migration == nil {
		migration = &ptahv1alpha1.PtahMigration{}
		if m.get(r.name, migration) != nil {
			migration = nil
		}
	}
	state := map[string]any{}
	if migration != nil {
		status := migration.Status
		entry := map[string]any{
			"uid": migration.UID, "phase": cmpOrNone(string(status.Phase)), "plan": "<none>",
			"conditions": miConditions(status.Conditions, 160),
		}
		if status.Plan != nil {
			entry["plan"] = cmpOrNone(status.Plan.Name)
		}
		if claim := status.ActiveOperation; claim != nil {
			entry["activeOperation"] = string(claim.Type) + "/" + cmpOrNone(claim.JobName)
		}
		if run := status.LastRun; run != nil {
			entry["lastRun"] = string(run.Outcome) + " " + run.Message
		}
		state["migration"] = entry
	}
	nodes := &corev1.NodeList{}
	if m.cluster.Client.List(m.ctx, nodes, client.HasLabels{applyGateLabel}) == nil {
		var open []string
		for _, node := range nodes.Items {
			open = append(open, node.Name)
		}
		state["gateOpenOn"] = open
	}
	m.miReport(r.name+" state when the wait ended", state)
}

// transactionModeProof ports run_transaction_mode_proof: a migration that
// names the transaction mode it runs under, on a database of its own, taken
// to applied history rather than stopped at the plan. A resource that only
// reached the gate would prove the flag was carried and say nothing about
// whether the migrations ran.
func (m *migrationRun) transactionModeProof() {
	m.t.Helper()
	m.logf("%s names its transaction mode and applies the sequence", m.engine.kind)
	name, secret := "e2e-txmode-"+m.engine.name, "e2e-"+m.engine.name+"-txmode-db"
	m.isolatedDatabase("ptah_e2e_txmode", secret)
	reference := m.reference("-txmode")
	m.publish("txmode", m.fixtureDir(""), reference)
	// policy.transactionMode is the whole point of the row: it is the only
	// place in the phase that names one, and without it the operator passes
	// no mode at all.
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: reference, coordinationKey: "e2e/txmode/" + m.engine.name,
		edit: func(spec map[string]any) { spec["policy"].(map[string]any)["transactionMode"] = "none" },
	}))
	m.miWaitForPhase(name, ptahv1alpha1.MigrationPhaseAwaitingApproval)
	m.approveTxmodePlan(name)
	m.miWaitForPhase(name, ptahv1alpha1.MigrationPhaseInSync)
	// The claim, and the reason the row does not stop at the gate: the
	// database carries the whole sequence, and nothing is left pending.
	if !txmodeHistoryApplied(m.migration(name).Status) {
		m.fatalf("%s did not apply its sequence under the transaction mode it named", name)
	}
	m.logf("PASS %s applied its sequence under the transaction mode it named", m.engine.kind)
}

// miWaitForPhase waits for a migration to reach a phase.
func (m *migrationRun) miWaitForPhase(name string, phase ptahv1alpha1.MigrationPhase) {
	m.t.Helper()
	observed := ""
	if !m.miUntil(migrationPoll, func() bool {
		migration := &ptahv1alpha1.PtahMigration{}
		observed = ""
		if m.get(name, migration) == nil {
			observed = string(migration.Status.Phase)
		}
		return observed == string(phase)
	}) {
		m.fatalf("%s did not reach %s within %s; it is in %s", name, phase, waitTimeout, cmpOrNone(observed))
	}
}

func (m *migrationRun) approveTxmodePlan(name string) {
	m.t.Helper()
	migration := &ptahv1alpha1.PtahMigration{}
	m.check(m.get(name, migration), "read %s", name)
	if migration.Status.Plan == nil || migration.Status.Plan.Name == "" {
		m.fatalf("%s published no plan to approve", name)
	}
	planName := migration.Status.Plan.Name
	plan := &ptahv1alpha1.PtahMigrationPlan{}
	_ = m.get(planName, plan)
	if plan.UID == "" || plan.Spec.Fingerprint == "" {
		m.fatalf("transaction-mode plan %s has no UID or fingerprint", planName)
	}
	err := m.create(migrationApprovalDocument(m.in.TestNamespace, name+"-approval", name, string(migration.UID),
		planName, string(plan.UID), plan.Spec.Fingerprint))
	if err != nil {
		refusal := err.Error()
		m.scan([]byte(refusal), "the transaction-mode approval's refusal")
		m.fatalf("the transaction-mode approval was refused: %s", refusal)
	}
}
