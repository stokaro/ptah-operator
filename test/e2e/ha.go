package e2e

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The high-availability phase's names and bounds.
const (
	// haSchema is the schema the phase drives after the failover.
	haSchema = "leader-failover"
	// haPullSecret is the registry credential the operation namespace pulls
	// the operation images with.
	haPullSecret = "e2e-ha-registry-pull"
	// haDatabaseURLSecret and haVerificationPolicy are the schema's fixtures.
	haDatabaseURLSecret  = "e2e-ha-database-url"
	haVerificationPolicy = "e2e-ha-verification-policy"
	// haDatabaseURL is a database the Resolve never reaches: it fails at the
	// unreachable artifact first.
	haDatabaseURL = "postgres://e2e:unused@database.invalid/e2e"

	haLeaderTimeout   = 120 * time.Second
	haWorkloadTimeout = 120 * time.Second
	haMetricsTimeout  = 30 * time.Second
	haRolloutTimeout  = 180 * time.Second
	haPodDeleteBound  = 90 * time.Second
	haPoll            = time.Second
)

// haSchemaCRD is the CRD the operation needs established before the phase
// creates one.
const haSchemaCRD = "ptahschemas.operator.ptah.run"

// haResolveFailureSample and haReconciliationSample are the two counter
// samples the post-failover reading has to carry, spelled as Prometheus
// writes them.
const (
	haReconciliationSample = `ptah_operator_reconciliations_total{family="schema",result="success"}`
	haResolveFailureSample = `ptah_operator_failures_total{category="operation",family="schema",stage="resolve"}`
)

// haAlwaysPresent is every family a running manager publishes whether or not
// anything has happened yet, with its type: gauges rebuilt from durable state
// on every scrape, and the counters that say a scrape could not read it. They
// are named here rather than allowed by prefix, so a family added without a
// decision still fails.
var haAlwaysPresent = map[string]string{
	"ptah_operator_unresolved_attempts":                          "gauge",
	"ptah_operator_unresolved_owed_seconds":                      "gauge",
	"ptah_operator_unresolved_view_synced":                       "gauge",
	"ptah_operator_unresolved_view_read_failures_total":          "counter",
	"ptah_operator_resources":                                    "gauge",
	"ptah_operator_overdue_resources":                            "gauge",
	"ptah_operator_overdue_seconds":                              "gauge",
	"ptah_operator_active_operations":                            "gauge",
	"ptah_operator_active_operation_seconds":                     "gauge",
	"ptah_operator_pending_lock_releases":                        "gauge",
	"ptah_operator_stored_plans":                                 "gauge",
	"ptah_operator_stored_plan_bytes":                            "gauge",
	"ptah_operator_webhook_certificate_expiry_timestamp_seconds": "gauge",
	"ptah_operator_webhook_certificate_read_failures_total":      "counter",
}

// haMetricVerdict is what the custom-metric validator says about one reading.
type haMetricVerdict int

const (
	// haMetricsComplete is the exact post-failure evidence: both counters
	// declared and sampled once, and nothing else of ours but the families
	// that are always present.
	haMetricsComplete haMetricVerdict = iota
	// haMetricsWaiting is a well-formed reading that does not carry both
	// counters yet, which the caller polls through.
	haMetricsWaiting
	// haMetricsMalformed is a reading no later scrape can fix: a family
	// nobody decided to publish, a duplicate, a wrong type or a bad value.
	haMetricsMalformed
)

func (v haMetricVerdict) String() string {
	switch v {
	case haMetricsComplete:
		return "complete"
	case haMetricsWaiting:
		return "waiting"
	default:
		return "malformed"
	}
}

// haMetricValue is the numeric shape awk's checks accepted: a decimal with an
// optional exponent, no sign, no NaN, no hexadecimal.
var haMetricValue = regexp.MustCompile(`^([0-9]+([.][0-9]*)?|[.][0-9]+)([eE][+-]?[0-9]+)?$`)

// haMetricNumber reads a sample value the way the script's awk did: the
// shape above, and a finite number once converted, so an exponent that
// overflows to infinity is refused rather than compared.
func haMetricNumber(value string) (float64, bool) {
	if !haMetricValue.MatchString(value) {
		return 0, false
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	if math.IsInf(number, 0) || math.IsNaN(number) {
		return 0, false
	}
	return number, true
}

// haFields splits a line as awk's default field separator does: on runs of
// spaces and tabs, and on nothing else.
func haFields(line string) []string {
	return strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' })
}

// haMetricLines is the text as awk read it: one record per newline.
func haMetricLines(body []byte) []string {
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

// haValidateCustomOperatorMetrics is validate_custom_operator_metrics: the
// leader's reading has to carry the reconciliation and Resolve failure
// counters once each, declared as counters and positive, and no custom family
// but those and the always-present ones. Any line that mentions one of ours
// and fits none of those shapes is malformed.
func haValidateCustomOperatorMetrics(body []byte) haMetricVerdict {
	var reconciliationHelp, failureHelp, reconciliationType, failureType int
	var reconciliationSample, failureSample int
	malformed := false
	for _, line := range haMetricLines(body) {
		fields := haFields(line)
		switch {
		case strings.HasPrefix(line, "# HELP ptah_operator_"):
			if len(fields) < 4 {
				malformed = true
				continue
			}
			switch name := fields[2]; {
			case name == "ptah_operator_reconciliations_total":
				reconciliationHelp++
			case name == "ptah_operator_failures_total":
				failureHelp++
			default:
				if _, known := haAlwaysPresent[name]; !known {
					malformed = true
				}
			}
		case strings.HasPrefix(line, "# TYPE ptah_operator_"):
			if len(fields) != 4 {
				malformed = true
				continue
			}
			name, kind := fields[2], fields[3]
			switch {
			case name == "ptah_operator_reconciliations_total":
				if kind != "counter" {
					malformed = true
					continue
				}
				reconciliationType++
			case name == "ptah_operator_failures_total":
				if kind != "counter" {
					malformed = true
					continue
				}
				failureType++
			default:
				if want, known := haAlwaysPresent[name]; !known || kind != want {
					malformed = true
				}
			}
		case strings.HasPrefix(line, "ptah_operator_"):
			if len(fields) != 2 {
				malformed = true
				continue
			}
			series, _, _ := strings.Cut(fields[0], "{")
			// A gauge rebuilt from durable state is present on every scrape and
			// is legitimately zero. A counter at zero is a different thing and
			// stays excluded, because this phase measures one going up.
			if _, known := haAlwaysPresent[series]; known {
				if number, ok := haMetricNumber(fields[1]); !ok || number < 0 {
					malformed = true
				}
				continue
			}
			if number, ok := haMetricNumber(fields[1]); !ok || number <= 0 {
				malformed = true
				continue
			}
			switch fields[0] {
			case haReconciliationSample:
				reconciliationSample++
			case haResolveFailureSample:
				failureSample++
			default:
				malformed = true
			}
		case strings.Contains(line, "ptah_operator_"):
			malformed = true
		}
	}
	if malformed || reconciliationHelp > 1 || failureHelp > 1 || reconciliationType > 1 || failureType > 1 ||
		reconciliationSample > 1 || failureSample > 1 {
		return haMetricsMalformed
	}
	if reconciliationHelp == 1 && failureHelp == 1 && reconciliationType == 1 && failureType == 1 &&
		reconciliationSample == 1 && failureSample == 1 {
		return haMetricsComplete
	}
	return haMetricsWaiting
}

// errHAFailureCounter is a Resolve failure counter no later scrape can make
// readable: a duplicated sample, a timestamp, or a value that is not a
// nonnegative finite number.
var errHAFailureCounter = errors.New("invalid Resolve operation failure counter")

// haResolveFailureCounter is resolve_operation_failure_counter_from_metrics:
// the Resolve failure counter's value as the reading spells it, or "0" when
// the counter has not been published yet.
func haResolveFailureCounter(body []byte) (string, error) {
	value, samples := "", 0
	for _, line := range haMetricLines(body) {
		fields := haFields(line)
		if len(fields) == 0 || fields[0] != haResolveFailureSample {
			continue
		}
		if len(fields) != 2 {
			return "", errHAFailureCounter
		}
		if number, ok := haMetricNumber(fields[1]); !ok || number < 0 {
			return "", errHAFailureCounter
		}
		samples++
		value = fields[1]
	}
	switch samples {
	case 0:
		return "0", nil
	case 1:
		return value, nil
	}
	return "", errHAFailureCounter
}

// haCounterIncreased compares two counter readings as numbers, as awk's
// (current + 0) > (baseline + 0) did.
func haCounterIncreased(baseline, current string) (bool, error) {
	before, okBefore := haMetricNumber(baseline)
	after, okAfter := haMetricNumber(current)
	if !okBefore || !okAfter {
		return false, fmt.Errorf("%w: cannot compare %q with %q", errHAFailureCounter, current, baseline)
	}
	return after > before, nil
}

// errHAMetricsMalformed is a reading the validator refused outright.
var errHAMetricsMalformed = errors.New("malformed, duplicate, or unexpected custom metric evidence")

// errHAMetricsTimeout is a wait that ended without the increased counter.
var errHAMetricsTimeout = errors.New("no increased Resolve operation failure counter with the exact post-failure custom metric families")

// haAwaitIncreasedFailureCounter is assert_custom_operator_metrics' loop:
// scrape until the reading is the complete post-failure evidence with a
// Resolve failure counter above the baseline. A scrape that fails and a
// reading still waiting are polled through; a malformed reading and an
// unreadable counter end the wait at once, since no later scrape can fix
// them. It returns how many scrapes it took.
func haAwaitIncreasedFailureCounter(
	ctx context.Context,
	scrape func(context.Context) ([]byte, error),
	baseline string,
	timeout, interval time.Duration,
	now func() time.Time,
	pause func(context.Context, time.Duration) error,
) (int, error) {
	deadline := now().Add(timeout)
	scrapes := 0
	for now().Before(deadline) {
		scrapes++
		if body, err := scrape(ctx); err == nil {
			switch haValidateCustomOperatorMetrics(body) {
			case haMetricsComplete:
				current, err := haResolveFailureCounter(body)
				if err != nil {
					return scrapes, err
				}
				increased, err := haCounterIncreased(baseline, current)
				if err != nil {
					return scrapes, err
				}
				if increased {
					return scrapes, nil
				}
			case haMetricsMalformed:
				return scrapes, errHAMetricsMalformed
			case haMetricsWaiting:
			}
		}
		if err := pause(ctx, interval); err != nil {
			return scrapes, err
		}
	}
	return scrapes, errHAMetricsTimeout
}

// haActiveLeaderLine is a line saying the replica holds the leader Lease.
var haActiveLeaderLine = regexp.MustCompile(`^leader_election_master_status(\{[^}]*\})?[[:space:]]+1(\.0+)?$`)

// haReportsActiveLeader is the grep the script ran over a replica's metrics:
// some line reports the leader election status as 1.
func haReportsActiveLeader(body []byte) bool {
	return slices.ContainsFunc(strings.Split(string(body), "\n"), haActiveLeaderLine.MatchString)
}

// haLeaderPodName is the Pod a leader holder identity names: the identity
// with its last underscore-separated part removed, as sed 's/_[^_]*$//' did.
func haLeaderPodName(holder string) string {
	if index := strings.LastIndex(holder, "_"); index >= 0 {
		return holder[:index]
	}
	return holder
}

// haLeaseHolder is the Lease's holder identity, or nothing when it names none.
func haLeaseHolder(lease *coordinationv1.Lease) string {
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

// haLeaseTransitions is .spec.leaseTransitions // 0.
func haLeaseTransitions(lease *coordinationv1.Lease) int32 {
	if lease.Spec.LeaseTransitions == nil {
		return 0
	}
	return *lease.Spec.LeaseTransitions
}

// haHolderIsReadyManagerPod is the Pod a new holder has to be: not being
// deleted, the release's controller, and Ready.
func haHolderIsReadyManagerPod(pod *corev1.Pod, release string) bool {
	return pod.DeletionTimestamp == nil &&
		labelIs(pod, "app.kubernetes.io/instance", release) &&
		labelIs(pod, "app.kubernetes.io/component", "controller") &&
		slices.ContainsFunc(pod.Status.Conditions, func(condition corev1.PodCondition) bool {
			return condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue
		})
}

// haLeaderMoved is a holder that names a replica and is not the one the
// failover started from.
func haLeaderMoved(holder, previous string) bool {
	return holder != "" && holder != previous
}

// haCountLeasesNamed is how many Leases across the cluster carry the name.
func haCountLeasesNamed(leases []coordinationv1.Lease, name string) int {
	count := 0
	for _, lease := range leases {
		if lease.Name == name {
			count++
		}
	}
	return count
}

// haManagerLeaseRules is the one rule the manager's Lease Role may hold,
// compared as the stored JSON, so a rule that gained a key -- resourceNames,
// even empty -- is not the same rule.
var haManagerLeaseRules = []any{map[string]any{
	"apiGroups": []any{"coordination.k8s.io"},
	"resources": []any{"leases"},
	"verbs":     []any{"get", "create", "update"},
}}

// haRoleRulesExact is the manager Role's rules compared with the one rule it
// may hold, in order.
func haRoleRulesExact(rules any) bool {
	return sameJSON(rules, haManagerLeaseRules)
}

// haClusterRoleGrantsLeases is a ClusterRole rule that reaches Leases: one
// naming the coordination group and the leases resource. A wildcard in either
// place reaches them too, which the script's literal index lookup missed.
func haClusterRoleGrantsLeases(rules []any) bool {
	for _, raw := range rules {
		rule, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if haListNames(rule["apiGroups"], "coordination.k8s.io") && haListNames(rule["resources"], "leases") {
			return true
		}
	}
	return false
}

// haListNames is a JSON list that holds the value, or the wildcard.
func haListNames(list any, value string) bool {
	items, ok := list.([]any)
	if !ok {
		return false
	}
	return slices.ContainsFunc(items, func(item any) bool { return item == value || item == "*" })
}

// haSchemaQuiesced is an earlier schema that cannot move the Resolve metrics
// the phase measures a delta against: suspended, reporting Suspended, and
// holding no operation.
func haSchemaQuiesced(schema *ptahv1alpha1.PtahSchema) bool {
	return schema.Spec.Suspend && schema.Status.Phase == ptahv1alpha1.PhaseSuspended &&
		schema.Status.ActiveOperation == nil
}

// haResolveJobTerminal is a Resolve Job that can no longer fail or succeed:
// no active Pod, and a Complete or Failed condition that is True.
func haResolveJobTerminal(job *batchv1.Job) bool {
	if job.Status.Active != 0 {
		return false
	}
	return slices.ContainsFunc(job.Status.Conditions, func(condition batchv1.JobCondition) bool {
		return condition.Status == corev1.ConditionTrue &&
			(condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed)
	})
}

// haOwnedBySchema is a Job the failover schema controls: a controller
// reference to that PtahSchema, by kind, name and UID.
func haOwnedBySchema(job *batchv1.Job, schemaUID types.UID) bool {
	return schemaUID != "" && slices.ContainsFunc(job.OwnerReferences, func(reference metav1.OwnerReference) bool {
		return reference.Controller != nil && *reference.Controller &&
			reference.APIVersion == ptahv1alpha1.GroupVersion.String() &&
			reference.Kind == "PtahSchema" && reference.Name == haSchema && reference.UID == schemaUID
	})
}

// haSchemaJobs is the operation Jobs the failover schema controls.
func haSchemaJobs(jobs []batchv1.Job, schemaUID types.UID) []batchv1.Job {
	var owned []batchv1.Job
	for index := range jobs {
		if haOwnedBySchema(&jobs[index], schemaUID) {
			owned = append(owned, jobs[index])
		}
	}
	return owned
}

// haAdmissionSnapshotAnnotation carries the digest of the admission snapshot
// the operation Pod was admitted under, on the Job and on its Pod.
const haAdmissionSnapshotAnnotation = "operator.ptah.run/admission-snapshot-digest"

// haAdmittedPod is the Job's admission: the Job names a snapshot digest, and
// exactly one of its live Pods carries that digest and names the Job as its
// controller.
func haAdmittedPod(job *batchv1.Job, pods []corev1.Pod) bool {
	digest := job.Annotations[haAdmissionSnapshotAnnotation]
	if !sha256Pattern.MatchString(digest) {
		return false
	}
	admitted := 0
	for _, pod := range pods {
		if pod.DeletionTimestamp == nil && annotationIs(&pod, haAdmissionSnapshotAnnotation, digest) &&
			controlledByJob(pod.OwnerReferences, job.UID) {
			admitted++
		}
	}
	return admitted == 1
}

// haFailedResolveLifecycle is the post-failover Resolve settled in its typed
// failure: the schema observed its own generation, is Failed with a retry
// scheduled, still holds the Resolve claim, and says the operation failed.
func haFailedResolveLifecycle(schema *ptahv1alpha1.PtahSchema, schemaUID types.UID) bool {
	status := schema.Status
	return schemaUID != "" && schema.UID == schemaUID &&
		status.ObservedGeneration == schema.Generation &&
		status.Phase == ptahv1alpha1.PhaseFailed &&
		status.NextReconciliationTime != nil &&
		status.ActiveOperation != nil && status.ActiveOperation.Type == ptahv1alpha1.OperationResolve &&
		slices.ContainsFunc(status.Conditions, func(condition metav1.Condition) bool {
			return condition.Type == "ReconciliationFailed" && condition.Status == metav1.ConditionTrue &&
				condition.Reason == "OperationFailed"
		})
}

// haSchemaDocument is the schema the new leader has to reconcile into an
// admitted operation: a Resolve against an artifact nobody serves, so it
// reaches a Pod and then fails, which is the counter the phase measures.
func haSchemaDocument(namespace string) map[string]any {
	return map[string]any{
		"apiVersion": ptahv1alpha1.GroupVersion.String(), "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": namespace, "name": haSchema},
		"spec": map[string]any{
			"target": map[string]any{
				"engine":          "PostgreSQL",
				"coordinationKey": "e2e/ha/leader-failover",
				"urlFrom":         map[string]any{"name": haDatabaseURLSecret, "key": "url"},
			},
			"desired": map[string]any{
				"ociRef":                 "oci://127.0.0.1:1/e2e/leader-failover:unreachable",
				"verificationPolicyFrom": map[string]any{"name": haVerificationPolicy, "key": "policy.yaml"},
				"transport":              map[string]any{"plainHTTP": true},
			},
			"interval": "24h",
			// The Job write guard requires an execution ServiceAccount, and the
			// API leaves the field optional, so the schema has to name one for
			// the operation to reach a Pod (stokaro/ptah-operator#11).
			"execution": map[string]any{
				"activeDeadlineSeconds": int64(120), "failureRetryInterval": "1h",
				"serviceAccountName": "default",
				"imagePullSecrets":   []any{map[string]any{"name": haPullSecret}},
			},
		},
	}
}

// haRegistryHost is the registry the manager image comes from: everything
// before its first slash. An image with no slash names none.
func haRegistryHost(image string) (string, bool) {
	host, _, found := strings.Cut(image, "/")
	return host, found && host != ""
}

// haNamespacesDistinct is the script's refusal of namespaces that overlap:
// the operation, foreign and proof namespaces are each apart from the
// operator's, and the proof namespace from the operation's.
func haNamespacesDistinct(operator, operation, foreign, proof string) error {
	switch {
	case operation == operator:
		return errors.New("HA workload namespace must differ from the operator namespace")
	case foreign == operator:
		return errors.New("foreign namespace must differ from the coordination namespace")
	case proof == operator:
		return errors.New("upgrade proof namespace must differ from the coordination namespace")
	case proof == operation:
		return errors.New("upgrade proof namespace must differ from the HA workload namespace")
	}
	return nil
}
