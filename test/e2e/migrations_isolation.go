package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The readings the isolated-node, unknown-layer, egress, retarget, rebuild
// and transaction-mode rows hold the cluster to. Each is the jq the shell
// phase ran, on typed objects where the claim is about meaning, with jq's null
// handling kept: an absent field equals no string, the empty one included, and
// a condition list that is absent cannot be iterated, so a claim over it
// fails.

const (
	// unknownLayerMediaType is the layer the unknown-layer artifact carries:
	// the next version of the format, which this executor must refuse.
	unknownLayerMediaType = "application/vnd.stokaro.ptah.migration.capability.v1"
	// egressProbeManager is what the egress probes carry where the operation
	// Pods carry the operator's manager label. Admission refuses a Pod that
	// claims the operator's without a Job the operator made.
	egressProbeManager = "ptah-operator-e2e-egress-probe"
	// egressProofLabel marks every policy the egress row applies, and
	// egressProbeLabel every probe it starts, so both are removed by label.
	egressProofLabel = "operator.ptah.run/e2e-proof"
	egressProbeLabel = "operator.ptah.run/e2e-probe"
	// egressOpenReading is what a probe reaches when no policy selects it.
	egressOpenReading = "api=open database=open dns=open registry=open"
)

// isolatedApplyHeld is an Apply whose
// node the API server cannot reach, still held by its resource. The claim is
// what a replacement would have to get past, so each clause is one way of
// letting go of a run that may still be writing: a retired or replaced claim,
// a new epoch or a continuity loss, an owed release, a run recorded for this
// Job, or the finalizer gone. The phase is not read.
func isolatedApplyHeld(migration *ptahv1alpha1.PtahMigration, job, epoch string) bool {
	status := migration.Status
	claim := status.ActiveOperation
	if claim == nil {
		return false
	}
	lastRunJob := ""
	if status.LastRun != nil {
		lastRunJob = string(status.LastRun.JobUID)
	}
	return claim.Type == ptahv1alpha1.MigrationOperationApply &&
		presentIs(string(claim.JobUID), job) &&
		presentIs(claim.LeaseEpoch, epoch) &&
		!claim.LeaseContinuityLost &&
		status.PendingLockRelease == nil &&
		status.UnresolvedRun == nil &&
		lastRunJob != job &&
		slices.Contains(migration.Finalizers, migrationFinalizer)
}

// isolatedRunUnknown is the first
// reading after an isolated Apply's claim is retired, the run recorded
// against its own Job as one nobody accounted for. Applied is refused on
// purpose for legacy Jobs, whose result lived in the removed Pod's log.
func isolatedRunUnknown(status ptahv1alpha1.PtahMigrationStatus, job string) bool {
	return status.ActiveOperation == nil &&
		status.LastRun != nil &&
		presentIs(string(status.LastRun.JobUID), job) &&
		status.LastRun.Outcome == ptahv1alpha1.MigrationRunOutcomeUnknown &&
		status.UnresolvedRun != nil &&
		presentIs(string(status.UnresolvedRun.JobUID), job) &&
		status.UnresolvedRun.Outcome == ptahv1alpha1.MigrationRunOutcomeUnknown &&
		conditionWithReason(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, "ApplyOutcomeUnknown")
}

// isolatedRunApplied is the durable run recorded against the original Job.
// The caller also verifies its receipt against the original Job and Pod.
func isolatedRunApplied(status ptahv1alpha1.PtahMigrationStatus, job string) bool {
	return status.ActiveOperation == nil && status.UnresolvedRun == nil &&
		status.LastRun != nil && presentIs(string(status.LastRun.JobUID), job) &&
		status.LastRun.Outcome == ptahv1alpha1.MigrationRunOutcomeApplied &&
		slices.Equal(status.LastRun.AppliedVersions, []int64{1, 2, 3})
}

// isolatedRunSettled is a history read taken after the run ended, with no
// unresolved record or pending work and the recorded outcome unchanged. The jq
// dropped the fraction before comparing, so the two instants are compared to
// the second: a reading in the same second as the run's end is not after it.
func isolatedRunSettled(status ptahv1alpha1.PtahMigrationStatus, job string, outcome ptahv1alpha1.MigrationRunOutcome) bool {
	if status.ActiveOperation != nil || status.UnresolvedRun != nil || status.LastRun == nil || status.History == nil {
		return false
	}
	run, history := status.LastRun, status.History
	return (outcome == ptahv1alpha1.MigrationRunOutcomeUnknown || outcome == ptahv1alpha1.MigrationRunOutcomeApplied) &&
		presentIs(string(run.JobUID), job) && run.Outcome == outcome &&
		history.PendingCount == 0 &&
		!history.ObservedAt.IsZero() && run.FinishedAt != nil && !run.FinishedAt.IsZero() &&
		history.ObservedAt.Unix() > run.FinishedAt.Unix()
}

// refusedAtBoundary is a
// Progressing condition that names the init step which ended the run before
// the runner could speak. The step is a pattern, as jq's test() read it, and a
// step that does not compile matches nothing.
func refusedAtBoundary(status ptahv1alpha1.PtahMigrationStatus, step string) bool {
	pattern, err := regexp.Compile(step)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(status.Conditions, func(condition metav1.Condition) bool {
		return condition.Type == ptahv1alpha1.ConditionMigrationProgressing && pattern.MatchString(condition.Message)
	})
}

// actedOnNothing is no plan
// published, no run recorded, no phase that invites work, and nothing counted
// as applied.
func actedOnNothing(status ptahv1alpha1.PtahMigrationStatus) bool {
	return status.Plan == nil &&
		status.LastRun == nil &&
		status.Phase != ptahv1alpha1.MigrationPhaseAwaitingApproval &&
		status.Phase != ptahv1alpha1.MigrationPhaseInSync &&
		(status.History == nil || status.History.AppliedCount == 0)
}

// conditionWithReason is jq's any(.conditions[]?; .type == T and .status ==
// "True" and .reason == R).
func conditionWithReason(conditions []metav1.Condition, kind, reason string) bool {
	return slices.ContainsFunc(conditions, func(condition metav1.Condition) bool {
		return condition.Type == kind && condition.Status == metav1.ConditionTrue && condition.Reason == reason
	})
}

// isolationWorkerReady is the node hack/e2e-kind.sh provisions for the
// isolated-node row: Ready, labeled with the isolation key, and tainted under
// that key with the isolation taint and nothing else. jq compared the taints
// as whole objects, so a timeAdded on the one taint makes it another taint.
func isolationWorkerReady(node *corev1.Node) bool {
	var taints []corev1.Taint
	for _, taint := range node.Spec.Taints {
		if taint.Key == isolationNodeKey {
			taints = append(taints, taint)
		}
	}
	return labelIs(node, isolationNodeKey, "true") &&
		len(taints) == 1 &&
		taints[0] == corev1.Taint{Key: isolationNodeKey, Value: "true", Effect: corev1.TaintEffectNoSchedule} &&
		miNodeReady(node)
}

// miNodeReady is a node whose Ready condition is True.
func miNodeReady(node *corev1.Node) bool {
	return slices.ContainsFunc(node.Status.Conditions, func(condition corev1.NodeCondition) bool {
		return condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue
	})
}

// miNodeNotReady is a node with a Ready condition that is not True: the node
// controller has lost it. A node with no Ready condition is neither.
func miNodeNotReady(node *corev1.Node) bool {
	return slices.ContainsFunc(node.Status.Conditions, func(condition corev1.NodeCondition) bool {
		return condition.Type == corev1.NodeReady && condition.Status != corev1.ConditionTrue
	})
}

// isolationRuleCount is how many lines of `iptables -S` carry the isolated
// node row's comment, as grep -cF counted them.
func isolationRuleCount(rules []byte) int {
	count := 0
	for line := range strings.SplitSeq(string(rules), "\n") {
		if strings.Contains(line, isolationRuleComment) {
			count++
		}
	}
	return count
}

// isolatedClaim is the claim the isolated-node row holds its resource to: the
// Apply's Job, and the epoch and duration of the Lease it took.
type isolatedClaim struct {
	job, jobUID, epoch string
	duration           int32
}

// isolatedApplyClaim reads the claim from one document: an Apply with its Job
// named and identified, under a Lease epoch taken for a positive duration.
func isolatedApplyClaim(migration *ptahv1alpha1.PtahMigration) (isolatedClaim, bool) {
	claim := migration.Status.ActiveOperation
	if claim == nil || claim.Type != ptahv1alpha1.MigrationOperationApply || claim.JobName == "" ||
		claim.JobUID == "" || claim.LeaseEpoch == "" || claim.LeaseDurationSeconds <= 0 {
		return isolatedClaim{}, false
	}
	return isolatedClaim{
		job: claim.JobName, jobUID: string(claim.JobUID), epoch: claim.LeaseEpoch, duration: claim.LeaseDurationSeconds,
	}, true
}

// isolatedLease is the one Lease, in any namespace, that carries the epoch the
// claim recorded, held to the duration the claim recorded.
func isolatedLease(leases []coordinationv1.Lease, epoch string, duration int32) (*coordinationv1.Lease, error) {
	var matched []*coordinationv1.Lease
	for index := range leases {
		if annotationIs(&leases[index], annotationLeaseEpoch, epoch) {
			matched = append(matched, &leases[index])
		}
	}
	if len(matched) != 1 {
		return nil, errors.New("expected exactly one realm Lease for the epoch of the claim")
	}
	if matched[0].Spec.LeaseDurationSeconds == nil || *matched[0].Spec.LeaseDurationSeconds != duration {
		return nil, errors.New("the realm Lease is not the duration the claim recorded")
	}
	return matched[0], nil
}

// leaseTermEnd is when a Lease would have lapsed had nothing renewed it, in
// Unix seconds: its acquisition, without the fraction, plus its duration. It
// is false when the Lease records no acquisition.
func leaseTermEnd(lease *coordinationv1.Lease) (int64, bool) {
	if lease.Spec.AcquireTime == nil || lease.Spec.AcquireTime.IsZero() {
		return 0, false
	}
	return lease.Spec.AcquireTime.Unix() + int64(leaseDuration(lease)), true
}

// leaseDuration is a Lease's duration, and zero where it has none, which is
// what jq added for a null.
func leaseDuration(lease *coordinationv1.Lease) int32 {
	if lease.Spec.LeaseDurationSeconds == nil {
		return 0
	}
	return *lease.Spec.LeaseDurationSeconds
}

// leaseRenewedSince is a Lease renewed after the node was cut off and not yet
// lapsed at now: the controller kept it rather than leaving it to run out
// under a Pod it cannot see stop. Both instants are Unix seconds.
func leaseRenewedSince(lease *coordinationv1.Lease, isolatedAt, now int64) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.RenewTime.IsZero() {
		return false
	}
	renewed := lease.Spec.RenewTime.Unix()
	return renewed > isolatedAt && renewed+int64(leaseDuration(lease)) > now
}

// isolatedApplyPodRunning is the one Pod of the Apply's Job, running on the
// isolation worker.
func isolatedApplyPodRunning(pods []corev1.Pod, node string) bool {
	return len(pods) == 1 && pods[0].Spec.NodeName == node && pods[0].Status.Phase == corev1.PodRunning
}

// podPlacedElsewhere is a Pod bound to a node other than the one given, which
// waiting longer does not fix.
func podPlacedElsewhere(pods []corev1.Pod, node string) bool {
	return slices.ContainsFunc(pods, func(pod corev1.Pod) bool {
		return pod.Spec.NodeName != "" && pod.Spec.NodeName != node
	})
}

// unreachableTolerationSeconds is how long the Pod tolerates an unreachable
// node: exactly one such toleration, bounded and positive.
func unreachableTolerationSeconds(pod *corev1.Pod) (int64, bool) {
	var matched []corev1.Toleration
	for _, toleration := range pod.Spec.Tolerations {
		if toleration.Key == corev1.TaintNodeUnreachable && toleration.Effect == corev1.TaintEffectNoExecute {
			matched = append(matched, toleration)
		}
	}
	if len(matched) != 1 || matched[0].TolerationSeconds == nil || *matched[0].TolerationSeconds <= 0 {
		return 0, false
	}
	return *matched[0].TolerationSeconds, true
}

// jobDeadlineAt is when the Job's own deadline runs out, in Unix seconds: its
// start plus its activeDeadlineSeconds, provided it is the Job the claim
// named. jq added a missing deadline as nothing and measured the hold from the
// start; a Job without one has no deadline to measure against, so it is
// refused here.
func jobDeadlineAt(job *batchv1.Job, uid string) (int64, error) {
	switch {
	case string(job.UID) != uid:
		return 0, errors.New("the Job under the claimed name is another one")
	case job.Status.StartTime == nil || job.Status.StartTime.IsZero():
		return 0, errors.New("the Job records no start")
	case job.Spec.ActiveDeadlineSeconds == nil:
		return 0, errors.New("the Job carries no activeDeadlineSeconds")
	}
	return job.Status.StartTime.Unix() + *job.Spec.ActiveDeadlineSeconds, nil
}

// onlyIsolatedApplyOnNode is the node's live Pods, less the DaemonSets', all
// this resource's in this namespace, the Apply Pod among them. An empty node
// fails it: the check has to have read the Pod it isolates.
func onlyIsolatedApplyOnNode(pods []corev1.Pod, namespace, migration, podUID string) bool {
	found := false
	for index := range pods {
		pod := &pods[index]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if slices.ContainsFunc(pod.OwnerReferences, func(owner metav1.OwnerReference) bool { return owner.Kind == "DaemonSet" }) {
			continue
		}
		if presentIs(string(pod.UID), podUID) {
			found = true
		}
		if pod.Namespace != namespace || !labelIs(pod, labelMigration, migration) {
			return false
		}
	}
	return found
}

// onlyPod is a Pod list that is the one Pod given and nothing else.
func onlyPod(pods []corev1.Pod, uid string) bool {
	return len(pods) == 1 && string(pods[0].UID) == uid
}

// isolatedReading is one reading of the isolated database: the revision rows,
// the distinct versions among them, the versions recorded applied, and the
// widgets the first migration inserted.
type isolatedReading struct{ rows, versions, applied, widgets int }

var isolatedReadingShape = regexp.MustCompile(`^[0-9]+,[0-9]+,[0-9]+,[0-9]+$`)

// parseIsolatedReading reads the four counts the engine joined with commas.
func parseIsolatedReading(value string) (isolatedReading, bool) {
	if !isolatedReadingShape.MatchString(value) {
		return isolatedReading{}, false
	}
	var counts [4]int
	for index, field := range strings.Split(value, ",") {
		count, err := strconv.Atoi(field)
		if err != nil {
			return isolatedReading{}, false
		}
		counts[index] = count
	}
	return isolatedReading{rows: counts[0], versions: counts[1], applied: counts[2], widgets: counts[3]}, true
}

// ranOnce is a database that never holds more than the three migrations,
// never records one twice, and holds the three rows the first migration
// inserted once: a replay shows up here whatever the status says.
func (r isolatedReading) ranOnce() bool {
	return r.rows <= 3 && r.versions == r.rows && r.applied <= 3 && r.widgets == 3
}

// isolatedReadingQuery is the one statement that takes an isolatedReading on
// the engine. The rows are joined with commas because the reading helper
// strips whitespace.
func isolatedReadingQuery(engine string) string {
	if engine == "mysql" {
		return "SELECT CONCAT((SELECT COUNT(*) FROM schema_migrations), ',', " +
			"(SELECT COUNT(DISTINCT version) FROM schema_migrations), ',', " +
			"(SELECT COUNT(*) FROM schema_migrations WHERE state = 'applied'), ',', " +
			"(SELECT COUNT(*) FROM e2e_migration_widgets))"
	}
	return "SELECT (SELECT count(*) FROM schema_migrations) || ',' || " +
		"(SELECT count(DISTINCT version) FROM schema_migrations) || ',' || " +
		"(SELECT count(*) FROM schema_migrations WHERE state = 'applied') || ',' || " +
		"(SELECT count(*) FROM e2e_migration_widgets)"
}

// appliedVersionsQuery is the versions a database records applied, in order,
// joined with commas.
func appliedVersionsQuery(engine string) string {
	if engine == "mysql" {
		return "SELECT COALESCE(GROUP_CONCAT(version ORDER BY version SEPARATOR ','), '') FROM schema_migrations WHERE state = 'applied'"
	}
	return "SELECT COALESCE(string_agg(version::text, ',' ORDER BY version), '') FROM schema_migrations WHERE state = 'applied'"
}

// isolatedHoldBound is the instant by which the hold must have seen every
// clock run out, in Unix seconds: the Lease term plus the phase's timeout, or
// the Job's deadline plus it when the deadline falls at or past that.
func isolatedHoldBound(termEnd, jobDeadline, timeout int64) int64 {
	bound := termEnd + timeout
	if jobDeadline >= bound {
		bound = jobDeadline + timeout
	}
	return bound
}

// podStopRequestedAt is when a Pod was asked to stop, in Unix seconds: its
// deletion less its grace period. It is false for a Pod nobody deleted.
func podStopRequestedAt(pod *corev1.Pod) (int64, bool) {
	if pod.DeletionTimestamp == nil {
		return 0, false
	}
	grace := int64(0)
	if pod.DeletionGracePeriodSeconds != nil {
		grace = *pod.DeletionGracePeriodSeconds
	}
	return pod.DeletionTimestamp.Unix() - grace, true
}

// unreachableTaintAddedAt is when the node lifecycle controller tainted the
// node unreachable for execution, in Unix seconds, from the first such taint
// that records it.
func unreachableTaintAddedAt(node *corev1.Node) (int64, bool) {
	for _, taint := range node.Spec.Taints {
		if taint.Key == corev1.TaintNodeUnreachable && taint.Effect == corev1.TaintEffectNoExecute && taint.TimeAdded != nil {
			return taint.TimeAdded.Unix(), true
		}
	}
	return 0, false
}

// isolatedRunRecorded is the claim retired and the run recorded against the
// isolated Job.
func isolatedRunRecorded(status ptahv1alpha1.PtahMigrationStatus, job string) bool {
	return status.ActiveOperation == nil && status.LastRun != nil && presentIs(string(status.LastRun.JobUID), job)
}

// newUIDs is jq's `$now - $before`: the identities now names that before does
// not.
func newUIDs(before, now []string) []string {
	var added []string
	for _, uid := range now {
		if !slices.Contains(before, uid) {
			added = append(added, uid)
		}
	}
	return added
}

// miDistinct is `LC_ALL=C sort -u`.
func miDistinct(values []string) []string {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return slices.Compact(sorted)
}

// egressExpectation is one operation of one family, and what the egress
// example grants its Pods: the name service, the database and the registry,
// each open or closed.
type egressExpectation struct {
	family, operation, dns, database, registry string
}

// egressExpectations is every operation of both families. The table is the
// one internal/workload/egress_example_test.go derives from the built Pods;
// the row holds the network to it.
var egressExpectations = []egressExpectation{
	{"schema", "resolve", "dns=open", "database=closed", "registry=open"},
	{"schema", "verify", "dns=open", "database=closed", "registry=open"},
	{"schema", "observe", "dns=open", "database=open", "registry=open"},
	{"schema", "plan", "dns=open", "database=open", "registry=open"},
	{"schema", "apply", "dns=open", "database=open", "registry=closed"},
	{"migration", "resolve", "dns=open", "database=closed", "registry=open"},
	{"migration", "verify", "dns=open", "database=closed", "registry=open"},
	{"migration", "history", "dns=open", "database=open", "registry=open"},
	{"migration", "apply", "dns=open", "database=open", "registry=open"},
}

// want is the reading a probe of the operation has to report, in the order a
// reading sorts: the API server closed, since nothing an operation dials
// outside the table may connect, and the rest as the example grants.
func (e egressExpectation) want() string {
	return "api=closed " + e.database + " " + e.dns + " " + e.registry
}

// probeReading is what a finished probe reached, as one line: its log lines
// in byte order, joined with spaces, as sort, tr and sed made it.
func probeReading(log []byte) string {
	text := strings.TrimSuffix(string(log), "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	slices.Sort(lines)
	return strings.Join(lines, " ")
}

// egressProbeLabels is the labels an operation's Pods carry, with the probe
// manager in place of the operator's.
func egressProbeLabels(manager, component, operation string) map[string]any {
	return map[string]any{labelManagedBy: manager, labelComponent: component, labelOperation: operation}
}

// egressProbeScript is what a probe does. It waits first: a policy engine
// learns about a new Pod from the API, and a connection made before it has is
// judged as coming from a Pod nothing selects. Addresses are dialed directly,
// so a refusal is the policy and not a name that did not resolve; resolution
// is its own line.
const egressProbeScript = `
sleep 5
reach() {
	if timeout 8 nc -z -w 4 "$1" "$2" >/dev/null 2>&1; then
		printf "%s=open\n" "$3"
	else
		printf "%s=closed\n" "$3"
	fi
}
if timeout 8 nslookup kubernetes.default.svc.cluster.local >/dev/null 2>&1; then
	echo dns=open
else
	echo dns=closed
fi
reach "$DATABASE" "$DATABASE_PORT" database
reach "$REGISTRY" 5000 registry
reach "$API" "$API_PORT" api
`

// egressProbeTarget is what a probe dials.
type egressProbeTarget struct {
	image, registry, database, databasePort, api, apiPort string
}

// egressProbePod is one probe Pod, carrying the probe's own label and the
// labels given on top of it; no labels is a Pod no policy selects.
func egressProbePod(namespace, name string, labels map[string]any, target egressProbeTarget) map[string]any {
	podLabels := map[string]any{egressProbeLabel: "egress"}
	for key, value := range labels {
		podLabels[key] = value
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"namespace": namespace, "name": name, "labels": podLabels},
		"spec": map[string]any{
			"restartPolicy":                "Never",
			"automountServiceAccountToken": false,
			"enableServiceLinks":           false,
			"securityContext":              map[string]any{"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532)},
			"containers": []any{map[string]any{
				"name": "probe", "image": target.image, "imagePullPolicy": "IfNotPresent",
				"env": []any{
					map[string]any{"name": "REGISTRY", "value": target.registry},
					map[string]any{"name": "DATABASE", "value": target.database},
					map[string]any{"name": "DATABASE_PORT", "value": target.databasePort},
					map[string]any{"name": "API", "value": target.api},
					map[string]any{"name": "API_PORT", "value": target.apiPort},
				},
				"command": []any{"/bin/sh", "-c", egressProbeScript},
				"securityContext": map[string]any{
					"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
					"capabilities": map[string]any{"drop": []any{"ALL"}},
				},
			}},
		},
	}
}

// soleEndpointAddress is the one address a Service's EndpointSlices carry. A
// probe dialing nothing reports every connection refused, which is the verdict
// a working policy produces too, so none or several is false.
func soleEndpointAddress(endpointSlices []discoveryv1.EndpointSlice) (string, bool) {
	var addresses []string
	for _, endpointSlice := range endpointSlices {
		for _, endpoint := range endpointSlice.Endpoints {
			addresses = append(addresses, endpoint.Addresses...)
		}
	}
	if len(addresses) != 1 || addresses[0] == "" {
		return "", false
	}
	return addresses[0], true
}

// soleRunningPodIP is the address of the one Pod that has one.
func soleRunningPodIP(pods []corev1.Pod) (string, bool) {
	var addresses []string
	for _, pod := range pods {
		if pod.Status.PodIP != "" {
			addresses = append(addresses, pod.Status.PodIP)
		}
	}
	if len(addresses) != 1 {
		return "", false
	}
	return addresses[0], true
}

// soleProbeImage is the one image the Pods' first containers run.
func soleProbeImage(pods []corev1.Pod) (string, bool) {
	var images []string
	for _, pod := range pods {
		image := ""
		if len(pod.Spec.Containers) > 0 {
			image = pod.Spec.Containers[0].Image
		}
		images = append(images, image)
	}
	images = miDistinct(images)
	if len(images) != 1 || images[0] == "" {
		return "", false
	}
	return images[0], true
}

// firstAPIEndpoint is the first address and the first port the API server's
// EndpointSlices carry.
func firstAPIEndpoint(endpointSlices []discoveryv1.EndpointSlice) (address, port string, ok bool) {
	var addresses []string
	for _, endpointSlice := range endpointSlices {
		for _, endpoint := range endpointSlice.Endpoints {
			addresses = append(addresses, endpoint.Addresses...)
		}
	}
	if len(addresses) > 0 {
		address = addresses[0]
	}
	for _, endpointSlice := range endpointSlices {
		if len(endpointSlice.Ports) > 0 {
			if endpointSlice.Ports[0].Port != nil {
				port = strconv.Itoa(int(*endpointSlice.Ports[0].Port))
			}
			break
		}
	}
	return address, port, address != "" && port != ""
}

// egressExampleItems is every object the egress example declares, as kubectl
// read it: one per YAML document, a List's items in its place.
func egressExampleItems(content []byte) ([]map[string]any, error) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(content), 4096)
	var items []map[string]any
	for {
		var document map[string]any
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				return items, nil
			}
			return nil, err
		}
		if len(document) == 0 {
			continue
		}
		if document["kind"] != "List" {
			items = append(items, document)
			continue
		}
		listed, _ := document["items"].([]any)
		for _, item := range listed {
			object, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("a List item is not an object")
			}
			items = append(items, object)
		}
	}
}

// miObjectName is a decoded object's metadata.name, or empty.
func miObjectName(object map[string]any) string {
	metadata, _ := object["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	return name
}

// egressExampleShaped holds the complete policy set: two each for default
// deny, the registry and the database, plus DNS and durable result delivery. The
// policies are found by the suffix the example names them with, so a renamed
// one fails here rather than keeping its in-cluster selector.
func egressExampleShaped(items []map[string]any) bool {
	if len(items) != 8 {
		return false
	}
	counts := map[string]int{}
	for _, item := range items {
		if item["kind"] != "NetworkPolicy" {
			return false
		}
		for _, suffix := range []string{"-registry", "-database", "-default-deny", "-dns", "-results"} {
			if strings.HasSuffix(miObjectName(item), suffix) {
				counts[suffix]++
			}
		}
	}
	return counts["-registry"] == 2 && counts["-database"] == 2 && counts["-default-deny"] == 2 && counts["-dns"] == 1 && counts["-results"] == 1
}

// renderEgressPolicies is the example with what it tells a reader to replace
// replaced: the namespace; the registry, which is outside the cluster here and
// so takes the ipBlock form the example names for an external registry; and
// the database, which runs in the namespace under the suite's own labels and
// port. Every policy carries the proof label, so removing them does not depend
// on a name list that could fall behind the example.
func renderEgressPolicies(items []map[string]any, namespace, registry, database string, databasePort int64, receiverNamespace string, receiverLabels map[string]string) ([]map[string]any, error) {
	if receiverNamespace == "" || len(receiverLabels) == 0 {
		return nil, errors.New("receiver namespace and Pod selector are required")
	}
	rendered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		policy := runtime.DeepCopyJSON(item)
		metadata, ok := policy["metadata"].(map[string]any)
		if !ok {
			return nil, errors.New("a policy has no metadata")
		}
		metadata["namespace"] = namespace
		labels, _ := metadata["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
		}
		labels[egressProofLabel] = "egress"
		metadata["labels"] = labels
		delete(metadata, "creationTimestamp")
		name := miObjectName(policy)
		var rewrite func(rule map[string]any)
		switch {
		case strings.HasSuffix(name, "-results"):
			rewrite = func(rule map[string]any) {
				labels := map[string]any{}
				for key, value := range receiverLabels {
					labels[key] = value
				}
				rule["to"] = []any{map[string]any{
					"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": receiverNamespace}},
					"podSelector":       map[string]any{"matchLabels": labels},
				}}
			}
		case strings.HasSuffix(name, "-registry"):
			rewrite = func(rule map[string]any) {
				rule["to"] = []any{map[string]any{"ipBlock": map[string]any{"cidr": registry + "/32"}}}
			}
		case strings.HasSuffix(name, "-database"):
			rewrite = func(rule map[string]any) {
				rule["to"] = []any{map[string]any{"podSelector": map[string]any{
					"matchLabels": map[string]any{"app.kubernetes.io/name": database},
				}}}
				rule["ports"] = []any{map[string]any{"protocol": "TCP", "port": databasePort}}
			}
		}
		if rewrite != nil {
			spec, _ := policy["spec"].(map[string]any)
			rules, ok := spec["egress"].([]any)
			if !ok {
				return nil, fmt.Errorf("policy %s has no egress rules to adapt", name)
			}
			for _, rule := range rules {
				object, ok := rule.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("policy %s has an egress rule that is not an object", name)
				}
				rewrite(object)
			}
		}
		rendered = append(rendered, policy)
	}
	return rendered, nil
}

// egressSelection is each policy's podSelector and policyTypes, by name: what
// the row is about, which has to be the example's.
func egressSelection(items []map[string]any) map[string]any {
	selection := map[string]any{}
	for _, item := range items {
		spec, _ := item["spec"].(map[string]any)
		selection[miObjectName(item)] = map[string]any{"podSelector": spec["podSelector"], "policyTypes": spec["policyTypes"]}
	}
	return selection
}

// egressSelectorsKept is a rendering that moved no selector and no policy
// type of the example.
func egressSelectorsKept(example, rendered []map[string]any) bool {
	return reflect.DeepEqual(egressSelection(example), egressSelection(rendered))
}

// egressProbeCopy is the rendering with the probe manager where each policy
// selects the operator's, under names of its own. A policy that does not
// select the operator's manager selects no operation Pod, and is refused.
func egressProbeCopy(rendered []map[string]any, manager string) ([]map[string]any, error) {
	copies := make([]map[string]any, 0, len(rendered))
	for _, policy := range rendered {
		probe := runtime.DeepCopyJSON(policy)
		metadata, _ := probe["metadata"].(map[string]any)
		spec, _ := probe["spec"].(map[string]any)
		selector, _ := spec["podSelector"].(map[string]any)
		matchLabels, _ := selector["matchLabels"].(map[string]any)
		if metadata == nil || matchLabels[labelManagedBy] != managedByOperator {
			return nil, errors.New("a policy selects no operation Pod")
		}
		metadata["name"] = miObjectName(probe) + "-probe"
		matchLabels[labelManagedBy] = manager
		copies = append(copies, probe)
	}
	return copies, nil
}

// egressProbeCopyFaithful is a probe copy that, mapped back, is the rendering
// it was made from: it differs in the manager label and the name and nothing
// else.
func egressProbeCopyFaithful(rendered, copies []map[string]any) bool {
	mapped := make([]map[string]any, 0, len(copies))
	for _, probe := range copies {
		policy := runtime.DeepCopyJSON(probe)
		metadata, _ := policy["metadata"].(map[string]any)
		if metadata == nil {
			return false
		}
		metadata["name"] = strings.TrimSuffix(miObjectName(policy), "-probe")
		matchLabels := miMapAt(policy, "spec", "podSelector", "matchLabels")
		if matchLabels == nil {
			return false
		}
		matchLabels[labelManagedBy] = managedByOperator
		mapped = append(mapped, policy)
	}
	return reflect.DeepEqual(rendered, mapped)
}

// miMapAt is the object at the path, created where it is missing, as jq's
// assignment creates it; nil where something on the path is not an object.
func miMapAt(object map[string]any, path ...string) map[string]any {
	current := object
	for _, key := range path {
		next, found := current[key]
		if !found || next == nil {
			created := map[string]any{}
			current[key] = created
			current = created
			continue
		}
		nested, ok := next.(map[string]any)
		if !ok {
			return nil
		}
		current = nested
	}
	return current
}

// egressConverged is a migration that applied its sequence and settled.
func egressConverged(status ptahv1alpha1.PtahMigrationStatus) bool {
	return status.Phase == ptahv1alpha1.MigrationPhaseInSync && status.LastRun != nil &&
		status.LastRun.Outcome == ptahv1alpha1.MigrationRunOutcomeApplied
}

// egressJobsSelected is the Jobs that ran the egress row's migration, each
// counted once by UID: one of them the Apply, and every one of them a Pod
// template the example's migration policies select. An Apply that converged
// because nothing selected it would pass everything else the row asserts.
func egressJobsSelected(jobs []batchv1.Job) bool {
	seen := map[string]bool{}
	apply := false
	for _, job := range jobs {
		if seen[string(job.UID)] {
			continue
		}
		seen[string(job.UID)] = true
		labels := job.Spec.Template.Labels
		if value, found := labels[labelOperation]; found && value == "apply" {
			apply = true
		}
		if manager, found := labels[labelManagedBy]; !found || manager != managedByOperator {
			return false
		}
		if component, found := labels[labelComponent]; !found || component != migrationOperationComponent {
			return false
		}
	}
	return apply
}

// retargetRefused is the refusal the retarget row waits for: the runner's, for
// the target binding the row changed, recorded against the Job that was
// dispatched. A refusal for any other reason leaves both databases untouched
// too and proves nothing, so the reason is part of the claim.
func retargetRefused(status ptahv1alpha1.PtahMigrationStatus, jobUID string) bool {
	run := status.UnresolvedRun
	return run != nil && presentIs(string(run.JobUID), jobUID) &&
		slices.ContainsFunc(status.Conditions, func(condition metav1.Condition) bool {
			return condition.Type == ptahv1alpha1.ConditionMigrationBlocked && condition.Status == metav1.ConditionTrue &&
				condition.Reason == "ApplyOutcomeUnknown" && strings.Contains(condition.Message, "target_binding_mismatch")
		})
}

// retargetRecoveryPlan is the fresh decision after a person accounted for
// the refused run. The target changes; the resource and selected artifact do
// not. A plan from the old target, a stale history, or a different resource
// cannot serve as the allowed control for this refusal.
func retargetRecoveryPlan(resource *ptahv1alpha1.PtahMigration, plan, original *ptahv1alpha1.PtahMigrationPlan, operation string) error {
	if resource == nil || plan == nil || original == nil || resource.UID == "" ||
		!changedMigrationApprovalRefused(resource, original.UID, resource.Generation, false) {
		return errors.New("the recovered migration is not waiting for a fresh approval")
	}
	status := resource.Status
	if plan.UID != status.Plan.UID || plan.Name != status.Plan.Name || plan.Namespace != resource.Namespace ||
		plan.Spec.MigrationRef.Name != resource.Name || plan.Spec.MigrationRef.UID != resource.UID ||
		original.Spec.MigrationRef != plan.Spec.MigrationRef ||
		plan.Spec.Fingerprint == "" || plan.Spec.Fingerprint == original.Spec.Fingerprint {
		return errors.New("the recovery plan has no new binding to this migration")
	}
	resolved := status.ResolvedRun
	if operation == "" || resolved == nil || resolved.OperationID != operation ||
		resolved.Resolution != ptahv1alpha1.MigrationRunResolvedByAcknowledgment ||
		resolved.AcknowledgmentRef == nil || resolved.AcknowledgmentRef.UID == "" ||
		resolved.AcknowledgedBy == nil || resolved.AcknowledgedBy.Username == "" || resolved.ResolvedAt.IsZero() ||
		resource.Annotations[ptahv1alpha1.UnresolvedRunAnnotation] != "" {
		return errors.New("the refused run was not accounted for by its acknowledgment")
	}
	history := status.History
	if history == nil || !history.ObservedAt.After(resolved.ResolvedAt.Time) ||
		!sha256Pattern.MatchString(history.Fingerprint) || plan.Spec.HistoryFingerprint != history.Fingerprint ||
		!sha256Pattern.MatchString(plan.Spec.TargetIdentityDigest) ||
		plan.Spec.TargetIdentityDigest != history.TargetIdentityDigest ||
		plan.Spec.TargetIdentityDigest == original.Spec.TargetIdentityDigest {
		return errors.New("the recovery plan does not name fresh history from the changed target")
	}
	if status.Artifact == nil || !sha256Pattern.MatchString(plan.Spec.ArtifactDigest) ||
		plan.Spec.ArtifactDigest != original.Spec.ArtifactDigest || plan.Spec.ArtifactDigest != status.Artifact.Digest ||
		len(plan.Spec.Migrations) == 0 || !reflect.DeepEqual(plan.Spec.Migrations, original.Spec.Migrations) {
		return errors.New("the recovery plan changed the selected artifact or sequence")
	}
	return nil
}

// drillConverged is a database that holds every migration the artifact
// carries. The condition is asserted rather than the phase, which moves on
// every read.
func drillConverged(status ptahv1alpha1.PtahMigrationStatus) bool {
	return conditionWithReason(status.Conditions, ptahv1alpha1.ConditionMigrationReady, "HistoryMatched")
}

// drillAwaitingDecision is a resource asking for a decision on a plan, and the
// plan.
func drillAwaitingDecision(status ptahv1alpha1.PtahMigrationStatus) (string, bool) {
	if status.Plan == nil || status.Plan.Name == "" ||
		!conditionWithReason(status.Conditions, ptahv1alpha1.ConditionMigrationApprovalRequired, "AwaitingApproval") {
		return "", false
	}
	return status.Plan.Name, true
}

// drillApplyClaimed is a claimed Apply with its Job named.
func drillApplyClaimed(status ptahv1alpha1.PtahMigrationStatus) bool {
	claim := status.ActiveOperation
	return claim != nil && claim.Type == ptahv1alpha1.MigrationOperationApply && claim.JobName != ""
}

// drillRebuiltIdle is the stored document of a rebuilt resource that recorded
// no run and claimed no Apply. The run is a key's presence in what the API
// server stores, so the document is read as it is stored; a status that is
// absent fails it, as jq failed has() on null.
func drillRebuiltIdle(document map[string]any) bool {
	status, ok := document["status"].(map[string]any)
	if !ok {
		return false
	}
	if _, found := status["lastRun"]; found {
		return false
	}
	claim, _ := status["activeOperation"].(map[string]any)
	kind, _ := claim["type"].(string)
	return kind != string(ptahv1alpha1.MigrationOperationApply)
}

// drillOwnedApplyUIDs is the Jobs the owner owns, by UID.
func drillOwnedApplyUIDs(jobs []batchv1.Job, owner string) []string {
	var owned []string
	for _, job := range jobs {
		if slices.ContainsFunc(job.OwnerReferences, func(reference metav1.OwnerReference) bool {
			return presentIs(string(reference.UID), owner)
		}) {
			owned = append(owned, string(job.UID))
		}
	}
	return owned
}

// strippedForRestore is a backed-up object without the fields a server
// stamps, so it can be created again: what is left is what a rebuild
// reapplies, the spec, and for an approval the bindings it was admitted with.
func strippedForRestore(document map[string]any) map[string]any {
	stripped := runtime.DeepCopyJSON(document)
	delete(stripped, "status")
	if metadata, ok := stripped["metadata"].(map[string]any); ok {
		for _, field := range []string{
			"uid", "resourceVersion", "creationTimestamp", "generation", "managedFields", "finalizers",
			"ownerReferences", "deletionTimestamp", "deletionGracePeriodSeconds",
		} {
			delete(metadata, field)
		}
	}
	return stripped
}

// drillPlanVersions is the migrations a plan approves, in order, as "3 4".
func drillPlanVersions(plan *ptahv1alpha1.PtahMigrationPlan) string {
	versions := make([]string, 0, len(plan.Spec.Migrations))
	for _, migration := range plan.Spec.Migrations {
		versions = append(versions, strconv.FormatInt(migration.Version, 10))
	}
	return strings.Join(versions, " ")
}

// drillRevisionsQuery is every revision row a database holds, as "1,2". The
// state is not filtered: a row a refused run left behind in any state is a row
// it wrote.
func drillRevisionsQuery(engine string) string {
	if engine == "mysql" {
		return "SELECT COALESCE(GROUP_CONCAT(version ORDER BY version SEPARATOR ','), '') FROM schema_migrations"
	}
	return "SELECT COALESCE(string_agg(version::text, ',' ORDER BY version), '') FROM schema_migrations"
}

// drillMarkerQuery counts the fourth migration's table in the database.
func drillMarkerQuery(engine string) string {
	schema := "table_schema = current_schema()"
	if engine == "mysql" {
		schema = "table_schema = database()"
	}
	return "SELECT count(*) FROM information_schema.tables WHERE " + schema + " AND table_name = 'e2e_drill_marker'"
}

// txmodeHistoryApplied is the claim the transaction-mode row does not stop
// short of: the database carries the whole sequence, nothing is left pending
// or dirty, and the run that got it there applied.
func txmodeHistoryApplied(status ptahv1alpha1.PtahMigrationStatus) bool {
	history := status.History
	return history != nil &&
		history.CurrentVersion == 3 &&
		history.AppliedCount == 3 &&
		history.PendingCount == 0 &&
		!history.Dirty &&
		status.LastRun != nil && status.LastRun.Outcome == ptahv1alpha1.MigrationRunOutcomeApplied &&
		conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue)
}

// miClip is the first n characters of a message, as jq's .message[0:n].
func miClip(message string, n int) string {
	runes := []rune(message)
	if len(runes) <= n {
		return message
	}
	return string(runes[:n])
}
