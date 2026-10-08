package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const alUpgradeObserver = "upgrade-observer"
const alUpgradeAlert = "PtahOperatorUpgradeFailed"

var alUpgradePinnedImage = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

type alUpgradeProbe struct {
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}
type alUpgradeIntent struct {
	Namespace    string            `json:"namespace"`
	Release      string            `json:"release"`
	HookJob      string            `json:"hookJob"`
	Manager      string            `json:"manager"`
	Rotator      string            `json:"rotator"`
	Image        string            `json:"image"`
	HookArgs     []string          `json:"hookArgs"`
	ChartDigest  string            `json:"chartDigest"`
	ValuesDigest string            `json:"valuesDigest"`
	CRDDigests   map[string]string `json:"crdDigests"`
	Probes       []alUpgradeProbe  `json:"probes"`
}
type alUpgradeAttempt struct {
	UID         string     `json:"uid"`
	CreatedAt   time.Time  `json:"createdAt"`
	FailedAt    *time.Time `json:"failedAt"`
	CompletedAt *time.Time `json:"completedAt"`
}
type alUpgradeState struct {
	Version         int                `json:"version"`
	Intent          alUpgradeIntent    `json:"intent"`
	StartedAt       time.Time          `json:"startedAt"`
	Deadline        time.Time          `json:"deadline"`
	BaselineJobUID  string             `json:"baselineJobUID"`
	ResourceVersion string             `json:"resourceVersion"`
	HistoryLost     bool               `json:"historyLost"`
	Attempts        []alUpgradeAttempt `json:"attempts"`
	FailedAt        *time.Time         `json:"failedAt"`
	RecoveredAt     *time.Time         `json:"recoveredAt"`
	RecoveryJobUID  string             `json:"recoveryJobUID"`
}

func alUpgradeDigest(body []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(body)) }
func alUpgradeReadState(body []byte, intent alUpgradeIntent, original *alUpgradeState) (alUpgradeState, error) {
	var s alUpgradeState
	if err := json.Unmarshal(body, &s); err != nil {
		return s, errors.New("invalid observer state")
	}
	got, _ := json.Marshal(s.Intent)
	want, _ := json.Marshal(intent)
	if string(got) != string(want) || s.Version != 1 || s.StartedAt.IsZero() || !s.Deadline.Equal(s.StartedAt.Add(15*time.Minute)) || s.ResourceVersion == "" || s.HistoryLost {
		return s, errors.New("observer changed its transaction or lost hook history")
	}
	if original != nil && (!s.StartedAt.Equal(original.StartedAt) || !s.Deadline.Equal(original.Deadline) || s.BaselineJobUID != original.BaselineJobUID) {
		return s, errors.New("observer restarted the original upgrade boundary")
	}
	return s, nil
}

// Date the incident from the independently retained API watch, not from the
// observer's own assertion. Every recorded attempt must match a concrete Job.
func alUpgradeIncident(s alUpgradeState, events []watchEvent[*batchv1.Job], mode string) (time.Time, error) {
	if len(s.Attempts) != 1 || s.RecoveredAt != nil {
		return time.Time{}, errors.New("incident has no unique unrecovered original hook")
	}
	attempt := s.Attempts[0]
	if attempt.UID == "" || attempt.CreatedAt.Before(s.StartedAt.Truncate(time.Second)) {
		return time.Time{}, errors.New("hook has no current attempt identity")
	}
	var failed, complete *metav1.Time
	seen := 0
	deleted := false
	for _, event := range events {
		j := event.Object
		if j == nil || string(j.UID) != attempt.UID {
			continue
		}
		if event.Type == watch.Bookmark {
			continue
		}
		seen++
		deleted = deleted || event.Type == watch.Deleted
		if j.Namespace != s.Intent.Namespace || j.Name != s.Intent.HookJob || string(j.UID) == s.BaselineJobUID || len(j.Spec.Template.Spec.Containers) != 1 || j.Spec.Template.Spec.Containers[0].Image != s.Intent.Image || !slices.Equal(j.Spec.Template.Spec.Containers[0].Args, s.Intent.HookArgs) {
			return time.Time{}, errors.New("observed hook differs from the candidate")
		}
		if !j.CreationTimestamp.Time.Equal(attempt.CreatedAt) || j.Labels["app.kubernetes.io/instance"] != s.Intent.Release || j.Labels["app.kubernetes.io/managed-by"] != "Helm" || j.Labels["app.kubernetes.io/component"] != "crd-manager" || !slices.Contains(strings.Split(j.Annotations["helm.sh/hook"], ","), "pre-upgrade") || !slices.Equal(j.Spec.Template.Spec.Containers[0].Command, []string{"/ptah-crd-manager"}) {
			return time.Time{}, errors.New("hook ownership or creation boundary differs")
		}
		for _, c := range j.Status.Conditions {
			if c.Status == corev1.ConditionTrue {
				switch c.Type {
				case batchv1.JobFailed:
					v := c.LastTransitionTime
					if failed != nil && !failed.Equal(&v) {
						return time.Time{}, errors.New("native hook failure timestamp changed")
					}
					failed = &v
				case batchv1.JobComplete:
					v := c.LastTransitionTime
					if j.Status.CompletionTime == nil || j.Status.CompletionTime.IsZero() || j.Status.CompletionTime.Before(&j.CreationTimestamp) || v.Before(j.Status.CompletionTime) {
						return time.Time{}, errors.New("hook has no valid completion timestamp")
					}
					if complete != nil && !complete.Equal(j.Status.CompletionTime) {
						return time.Time{}, errors.New("native hook completion timestamp changed")
					}
					complete = j.Status.CompletionTime
				}
			}
		}
	}
	if seen == 0 {
		return time.Time{}, errors.New("no independently observed hook")
	}
	if !deleted {
		return time.Time{}, errors.New("hook deletion is absent from independent history")
	}
	switch mode {
	case "failed":
		if failed == nil || complete != nil || failed.IsZero() || s.FailedAt == nil || attempt.FailedAt == nil || !failed.Time.Equal(*s.FailedAt) || !failed.Time.Equal(*attempt.FailedAt) || attempt.CompletedAt != nil {
			return time.Time{}, errors.New("no exact Failed hook transition")
		}
		if failed.Time.Before(attempt.CreatedAt) || !failed.Time.Before(s.Deadline) {
			return time.Time{}, errors.New("failed-hook proof did not precede the deadline")
		}
		return failed.Time, nil
	case "deadline":
		if failed != nil || s.FailedAt != nil || attempt.FailedAt != nil || attempt.CompletedAt == nil || complete == nil || complete.IsZero() {
			return time.Time{}, errors.New("deadline incident did not follow a successful hook")
		}
		if !complete.Time.Equal(*attempt.CompletedAt) || !complete.Time.Before(s.Deadline) {
			return time.Time{}, errors.New("successful hook is not bound to the original deadline")
		}
		return s.Deadline, nil
	default:
		return time.Time{}, errors.New("unknown upgrade fault")
	}
}

func alUpgradeObserverObjects(intent alUpgradeIntent, fixtureImage string, chart, values, kubeconfig []byte) ([]client.Object, error) {
	raw, err := json.Marshal(intent)
	if err != nil {
		return nil, err
	}
	if err := alUpgradeIntentValid(intent); err != nil {
		return nil, err
	}
	if !alUpgradePinnedImage.MatchString(fixtureImage) {
		return nil, errors.New("observer fixture image is not pinned")
	}
	if len(raw)+len(chart)+len(values)+len(kubeconfig) > 900<<10 || alUpgradeDigest(chart) != intent.ChartDigest || alUpgradeDigest(values) != intent.ValuesDigest {
		return nil, errors.New("observer inputs exceed the ConfigMap bound or differ from the candidate")
	}
	ns, name := alMonitoringNamespace, alUpgradeObserver
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	subject := []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: ns}}
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, BinaryData: map[string][]byte{"intent.json": raw, "candidate.tgz": chart, "values.yaml": values, "kubeconfig": kubeconfig}}
	runtimeRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: intent.Namespace, Name: "e2e-" + name}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, ResourceNames: []string{intent.HookJob}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, ResourceNames: []string{intent.Manager, intent.Rotator}, Verbs: []string{"get"}},
		{APIGroups: []string{"apps"}, Resources: []string{"replicasets"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"list"}},
	}}
	runtimeBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: intent.Namespace, Name: runtimeRole.Name}, Subjects: subject, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: runtimeRole.Name}}
	var crds []string
	for name := range intent.CRDDigests {
		crds = append(crds, name)
	}
	slices.Sort(crds)
	crdRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "e2e-" + name}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"apiextensions.k8s.io"}, Resources: []string{"customresourcedefinitions"}, ResourceNames: crds, Verbs: []string{"get"}}}}
	crdBinding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: crdRole.Name}, Subjects: subject, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: crdRole.Name}}
	objects := []client.Object{account, config, runtimeRole, runtimeBinding, crdRole, crdBinding}
	for index, p := range intent.Probes {
		resourceName := "ptahschemas"
		if p.Kind == "PtahMigration" {
			resourceName = "ptahmigrations"
		} else if p.Kind != "PtahSchema" {
			return nil, errors.New("unsupported upgrade probe")
		}
		role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: fmt.Sprintf("e2e-%s-probe-%d", name, index)}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"operator.ptah.run"}, Resources: []string{resourceName}, ResourceNames: []string{p.Name}, Verbs: []string{"get", "patch"}}}}
		binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: role.Namespace, Name: role.Name}, Subjects: subject, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}}
		objects = append(objects, role, binding)
	}
	workload := alWorkload{namespace: ns, name: name, image: fixtureImage, port: 9812, serviceAccount: name, pullSecret: alPullSecret, configMap: name, command: []string{"/e2e-upgrade-observer"}, args: []string{"serve", "--state", "/data/state.json", "--kubeconfig", "/etc/" + name + "/kubeconfig", "--listen", ":9812"}}
	deployment, service := workload.objects()
	deployment.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	container := &deployment.Spec.Template.Spec.Containers[0]
	container.Resources = corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi")}}
	container.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(9812)}}, PeriodSeconds: 2, FailureThreshold: 3}
	init := container.DeepCopy()
	init.Name = "prepare"
	init.Ports = nil
	init.ReadinessProbe = nil
	init.Args = []string{"prepare", "--state", "/data/state.json", "--kubeconfig", "/etc/" + name + "/kubeconfig", "--intent", "/etc/" + name + "/intent.json", "--chart", "/etc/" + name + "/candidate.tgz", "--values", "/etc/" + name + "/values.yaml"}
	deployment.Spec.Template.Spec.InitContainers = []corev1.Container{*init}
	return append(objects, service, deployment), nil
}

func alUpgradeFaultPolicy(intent alUpgradeIntent, hookAccount, mode string) (*admissionv1.ValidatingAdmissionPolicy, *admissionv1.ValidatingAdmissionPolicyBinding) {
	name := "e2e-upgrade-receiver-" + mode
	group, resourceName, scope := "apps", "deployments", admissionv1.NamespacedScope
	resourceNames := []string{intent.Manager, intent.Rotator}
	conditions := []admissionv1.MatchCondition{{Name: "hook-identity", Expression: fmt.Sprintf("request.namespace == %q && request.userInfo.username == %q", intent.Namespace, "system:serviceaccount:"+intent.Namespace+":"+hookAccount)}}
	expression := "false"
	if mode == "deadline" {
		group, resourceName, scope = "apps", "deployments", admissionv1.NamespacedScope
		resourceNames = []string{intent.Manager, intent.Rotator}
		conditions = []admissionv1.MatchCondition{{Name: "installation", Expression: fmt.Sprintf("request.namespace == %q", intent.Namespace)}}
		expression = fmt.Sprintf("request.userInfo.username == %q || object.spec.replicas == 0", "system:serviceaccount:"+intent.Namespace+":"+hookAccount)
	}
	policy := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: admissionv1.ValidatingAdmissionPolicySpec{FailurePolicy: ptr.To(admissionv1.Fail), MatchConditions: conditions, MatchConstraints: &admissionv1.MatchResources{ResourceRules: []admissionv1.NamedRuleWithOperations{{ResourceNames: resourceNames, RuleWithOperations: admissionv1.RuleWithOperations{Operations: []admissionv1.OperationType{admissionv1.Update}, Rule: admissionv1.Rule{APIGroups: []string{group}, APIVersions: []string{"v1"}, Resources: []string{resourceName}, Scope: &scope}}}}}, Validations: []admissionv1.Validation{{Expression: expression, Message: name}}}}
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: name, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny}}}
	return policy, binding
}

// Refuse empty resourceNames before generating RBAC; empty means all resources.
func alUpgradeIntentValid(i alUpgradeIntent) error {
	for _, name := range []string{i.Namespace, i.Release, i.HookJob, i.Manager, i.Rotator} {
		if name == "" || len(validation.IsDNS1123Subdomain(name)) != 0 {
			return errors.New("invalid observer resource name")
		}
	}
	if len(validation.IsDNS1123Label(i.Namespace)) != 0 || i.Manager == i.Rotator || len(i.HookArgs) == 0 || !alUpgradePinnedImage.MatchString(i.Image) || len(i.CRDDigests) != len(crdupgrade.Names()) || len(i.Probes) != 4 {
		return errors.New("incomplete native observer inventory")
	}
	for _, name := range crdupgrade.Names() {
		if !sha256Pattern.MatchString(i.CRDDigests[name]) {
			return errors.New("missing candidate CRD identity")
		}
	}
	seen := map[string]bool{}
	families := map[string]int{}
	for _, p := range i.Probes {
		key := p.Kind + "/" + p.Namespace + "/" + p.Name
		if p.Kind != "PtahSchema" && p.Kind != "PtahMigration" || p.Name == "" || p.Namespace == "" || len(validation.IsDNS1123Subdomain(p.Name)) != 0 || len(validation.IsDNS1123Label(p.Namespace)) != 0 || p.UID == "" || p.Generation < 1 || seen[key] {
			return errors.New("invalid or duplicated probe identity")
		}
		seen[key] = true
		families[p.Kind]++
	}
	if families["PtahSchema"] != 2 || families["PtahMigration"] != 2 {
		return errors.New("both engines need both resource families")
	}
	return nil
}

func alUpgradeProbeSafe(v, original client.Object) bool {
	if v.GetUID() == "" || v.GetUID() != original.GetUID() || v.GetNamespace() != original.GetNamespace() || v.GetName() != original.GetName() || v.GetGeneration() != original.GetGeneration() || v.GetDeletionTimestamp() != nil {
		return false
	}
	switch current := v.(type) {
	case *ptahv1.PtahSchema:
		old, ok := original.(*ptahv1.PtahSchema)
		return ok && equality.Semantic.DeepEqual(current.Spec, old.Spec) && current.Spec.Policy.Apply == ptahv1.ApplyPolicyNever && !current.Spec.Suspend && current.Status.Applied == nil && current.Status.PendingObservation == nil && (current.Status.ActiveOperation == nil || current.Status.ActiveOperation.Type != ptahv1.OperationApply) && (current.Status.Plan == nil || current.Status.Plan.Approval == nil)
	case *ptahv1.PtahMigration:
		old, ok := original.(*ptahv1.PtahMigration)
		return ok && equality.Semantic.DeepEqual(current.Spec, old.Spec) && current.Spec.Policy.Apply == ptahv1.ApplyPolicyNever && !current.Spec.Suspend && current.Status.LastRun == nil && current.Status.UnresolvedRun == nil && (current.Status.ActiveOperation == nil || current.Status.ActiveOperation.Type != ptahv1.MigrationOperationApply)
	}
	return false
}

// A controller status write can invalidate the dry-run's resource version.
// Re-read and revalidate the original probe on conflict; admission refusals
// and changed probe identities still fail immediately.
func alUpgradeProbeAdmission(ctx context.Context, c client.Client, original client.Object) (before, admitted client.Object, err error) {
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := original.DeepCopyObject().(client.Object)
		if err := c.Get(ctx, client.ObjectKeyFromObject(original), live); err != nil {
			return err
		}
		before = live.DeepCopyObject().(client.Object)
		if !alUpgradeProbeSafe(live, original) {
			return errors.New("upgrade changed the read-only probe")
		}
		annotations := live.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["qualification.ptah.run/native-upgrade-admission"] = "verified"
		live.SetAnnotations(annotations)
		if err := c.Patch(ctx, live, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}), client.DryRunAll); err != nil {
			return err
		}
		admitted = live
		return nil
	})
	return before, admitted, err
}

// Prometheus instant-vector timestamps are evaluation times, so query the
// source timestamp as a separate expression when asserting scrape freshness.
func alUpgradeGauge(body []byte, want float64, now time.Time) bool {
	var r struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &r) != nil || r.Status != "success" || r.Data.ResultType != "vector" || len(r.Data.Result) != 1 {
		return false
	}
	value := r.Data.Result[0]
	if len(value.Value) != 2 || value.Metric["job"] != "ptah-upgrade-observer" || value.Metric["instance"] != alUpgradeObserver+"."+alMonitoringNamespace+".svc:9812" {
		return false
	}
	var text string
	var stamp float64
	if json.Unmarshal(value.Value[0], &stamp) != nil || json.Unmarshal(value.Value[1], &text) != nil {
		return false
	}
	n, err := strconv.ParseFloat(text, 64)
	return err == nil && n == want && stamp <= float64(now.UnixNano())/1e9+1 && stamp >= float64(now.Add(-15*time.Second).UnixNano())/1e9
}

func alUpgradeProbeProgress(v, original client.Object, after time.Time) (time.Time, bool) {
	if !alUpgradeProbeSafe(v, original) {
		return time.Time{}, false
	}
	r := alNegativeReading(v)
	if r.observed != v.GetGeneration() || !r.plan || !r.disabled || r.failed || r.claim.id != "" || !r.readAt.After(after) || r.readAt.After(time.Now()) {
		return time.Time{}, false
	}
	at := r.readAt
	var conditions []metav1.Condition
	var boundaryType, boundaryReason string
	var boundaryStatus metav1.ConditionStatus
	switch v := v.(type) {
	case *ptahv1.PtahSchema:
		conditions = v.Status.Conditions
		boundaryType, boundaryStatus, boundaryReason = "PlanReady", metav1.ConditionTrue, "Published"
	case *ptahv1.PtahMigration:
		conditions = v.Status.Conditions
		boundaryType, boundaryStatus, boundaryReason = "Progressing", metav1.ConditionFalse, "ApplyDisabled"
	}
	// Ready remains False from an ordinary read through ApplyDisabled. Its
	// transition can predate the fresh database read, which itself precedes
	// planning. Date recovery from the condition that completes this cycle.
	completed := false
	for _, c := range conditions {
		if c.ObservedGeneration != v.GetGeneration() {
			continue
		}
		if c.Type == boundaryType && c.Status == boundaryStatus && c.Reason == boundaryReason &&
			!c.LastTransitionTime.IsZero() && !c.LastTransitionTime.Time.Before(r.readAt) {
			completed = true
			if c.LastTransitionTime.After(at) {
				at = c.LastTransitionTime.Time
			}
		}
		if c.Type == "Ready" && c.LastTransitionTime.After(at) {
			at = c.LastTransitionTime.Time
		}
	}
	return at, completed && !at.After(time.Now())
}

func alUpgradeRetryEvidence(s alUpgradeState, events []watchEvent[*batchv1.Job]) error {
	if len(s.Attempts) != 2 || s.RecoveredAt == nil || s.Attempts[1].CompletedAt == nil || s.RecoveryJobUID != s.Attempts[1].UID || s.Attempts[0].UID == s.Attempts[1].UID || !s.RecoveredAt.After(*s.Attempts[1].CompletedAt) {
		return errors.New("recovery has no successful distinct retry")
	}
	// Locate the completed API document, then apply the common identity checks
	// to its full history. The completion is permitted after the original deadline.
	a := s.Attempts[1]
	seen := false
	for _, event := range events {
		j := event.Object
		if j == nil || string(j.UID) != a.UID {
			continue
		}
		if j.Namespace != s.Intent.Namespace || j.Name != s.Intent.HookJob || !j.CreationTimestamp.Time.Equal(a.CreatedAt) || j.Labels["app.kubernetes.io/instance"] != s.Intent.Release || j.Labels["app.kubernetes.io/component"] != "crd-manager" || j.Labels["app.kubernetes.io/managed-by"] != "Helm" || !slices.Contains(strings.Split(j.Annotations["helm.sh/hook"], ","), "pre-upgrade") || len(j.Spec.Template.Spec.Containers) != 1 {
			return errors.New("retry lost its native candidate identity")
		}
		c := j.Spec.Template.Spec.Containers[0]
		if c.Image != s.Intent.Image || !slices.Equal(c.Command, []string{"/ptah-crd-manager"}) || !slices.Equal(c.Args, s.Intent.HookArgs) {
			return errors.New("retry changed its executable")
		}
		for _, condition := range j.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
				continue
			}
			if condition.Type == batchv1.JobFailed {
				return errors.New("retry failed")
			}
			if condition.Type == batchv1.JobComplete {
				if j.Status.CompletionTime == nil || !j.Status.CompletionTime.Time.Equal(*a.CompletedAt) || j.Status.CompletionTime.Before(&j.CreationTimestamp) || condition.LastTransitionTime.Before(j.Status.CompletionTime) {
					return errors.New("retry lost its completion timestamp")
				}
				seen = true
			}
		}
	}
	if !seen {
		return errors.New("retry completion is absent from independent history")
	}
	return nil
}
