package e2e

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func alUpgradeFixture() (alUpgradeState, *batchv1.Job) {
	start := time.Unix(1700000000, 0).UTC()
	digest := "sha256:" + strings.Repeat("a", 64)
	intent := alUpgradeIntent{Namespace: "operator", Release: "ptah", HookJob: "ptah-upgrade", Manager: "manager", Rotator: "rotator", Image: "example.invalid/operator@" + digest, HookArgs: []string{"reconcile"}, ChartDigest: alUpgradeDigest([]byte("chart")), ValuesDigest: alUpgradeDigest([]byte("values")), CRDDigests: map[string]string{}}
	for _, name := range crdupgrade.Names() {
		intent.CRDDigests[name] = digest
	}
	for _, family := range []string{"PtahSchema", "PtahMigration"} {
		for _, engine := range []string{"postgresql", "mysql"} {
			intent.Probes = append(intent.Probes, alUpgradeProbe{Kind: family, Namespace: "workloads", Name: strings.ToLower(family) + "-" + engine, UID: family + engine, Generation: 1})
		}
	}
	failed := start.Add(time.Minute)
	s := alUpgradeState{Version: 1, Intent: intent, StartedAt: start, Deadline: start.Add(15 * time.Minute), ResourceVersion: "2", Attempts: []alUpgradeAttempt{{UID: "hook-uid", CreatedAt: start.Add(time.Second), FailedAt: &failed}}, FailedAt: &failed}
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: intent.HookJob, Namespace: intent.Namespace, UID: "hook-uid", CreationTimestamp: metav1.NewTime(s.Attempts[0].CreatedAt), Labels: map[string]string{"app.kubernetes.io/instance": "ptah", "app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/component": "crd-manager"}, Annotations: map[string]string{"helm.sh/hook": "pre-install,pre-upgrade"}}, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: intent.Image, Command: []string{"/ptah-crd-manager"}, Args: intent.HookArgs}}}}}, Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(failed)}}}}
	return s, j
}

func TestAlUpgradeIncidentRequiresIndependentExactHook(t *testing.T) {
	s, j := alUpgradeFixture()
	events := []watchEvent[*batchv1.Job]{{Type: watch.Modified, Object: j}, {Type: watch.Deleted, Object: j}}
	at, err := alUpgradeIncident(s, events, "failed")
	if err != nil || !at.Equal(*s.FailedAt) {
		t.Fatal(at, err)
	}
	for name, mutate := range map[string]func(*batchv1.Job){
		"another uid":          func(j *batchv1.Job) { j.UID = "replacement" },
		"changed creation":     func(j *batchv1.Job) { j.CreationTimestamp = metav1.NewTime(s.StartedAt) },
		"another image":        func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Image = "another" },
		"another command":      func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Command = []string{"/manager"} },
		"another args":         func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Args = []string{"wrong"} },
		"another release":      func(j *batchv1.Job) { j.Labels["app.kubernetes.io/instance"] = "other" },
		"not a hook":           func(j *batchv1.Job) { j.Annotations = nil },
		"failure date changed": func(j *batchv1.Job) { j.Status.Conditions[0].LastTransitionTime = metav1.NewTime(s.StartedAt) },
		"not failed":           func(j *batchv1.Job) { j.Status.Conditions = nil },
	} {
		t.Run(name, func(t *testing.T) {
			changed := j.DeepCopy()
			mutate(changed)
			if _, err := alUpgradeIncident(s, []watchEvent[*batchv1.Job]{{Type: watch.Deleted, Object: changed}}, "failed"); err == nil {
				t.Fatal("unbound incident accepted")
			}
		})
	}
	if _, err := alUpgradeIncident(s, nil, "failed"); err == nil {
		t.Fatal("empty history accepted")
	}
	complete := s.StartedAt.Add(2 * time.Minute)
	s.FailedAt = nil
	s.Attempts[0].FailedAt = nil
	s.Attempts[0].CompletedAt = &complete
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(complete.Add(time.Second))}}
	j.Status.CompletionTime = &metav1.Time{Time: complete}
	at, err = alUpgradeIncident(s, events, "deadline")
	if err != nil || !at.Equal(s.Deadline) {
		t.Fatal(at, err)
	}
	// CompletionTime and the terminal condition can legitimately differ. The
	// observer and the independent proof bind to the actual CompletionTime.
	changed := complete.Add(time.Second)
	s.Attempts[0].CompletedAt = &changed
	if _, err := alUpgradeIncident(s, events, "deadline"); err == nil {
		t.Fatal("changed completion timestamp accepted")
	}
}

// A retained failure cannot prove survival of deletion unless the independent
// history contains deletion of that same hook, not only a termination request.
func TestAlUpgradeIncidentRequiresActualDeletion(t *testing.T) {
	s, j := alUpgradeFixture()
	for _, mode := range []string{"failed", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			state, job := s, j.DeepCopy()
			state.Attempts = append([]alUpgradeAttempt(nil), s.Attempts...)
			if mode == "deadline" {
				completed := state.StartedAt.Add(2 * time.Minute)
				state.FailedAt, state.Attempts[0].FailedAt = nil, nil
				state.Attempts[0].CompletedAt = &completed
				job.Status.CompletionTime = &metav1.Time{Time: completed}
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(completed)}}
			}
			events := []watchEvent[*batchv1.Job]{{Type: watch.Modified, Object: job}}
			if _, err := alUpgradeIncident(state, events, mode); err == nil {
				t.Fatal("terminal hook without a deletion event accepted")
			}
			terminating := job.DeepCopy()
			terminating.DeletionTimestamp = &metav1.Time{Time: state.StartedAt.Add(3 * time.Minute)}
			if _, err := alUpgradeIncident(state, append(events, watchEvent[*batchv1.Job]{Type: watch.Modified, Object: terminating}), mode); err == nil {
				t.Fatal("deletion request substituted for actual deletion")
			}
			other := job.DeepCopy()
			other.UID = "unrelated-hook"
			if _, err := alUpgradeIncident(state, append(events, watchEvent[*batchv1.Job]{Type: watch.Deleted, Object: other}), mode); err == nil {
				t.Fatal("another hook's deletion accepted")
			}
			if _, err := alUpgradeIncident(state, append(events, watchEvent[*batchv1.Job]{Type: watch.Deleted, Object: job}), mode); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAlUpgradeStateCannotMoveDeadlineOrCandidate(t *testing.T) {
	s, _ := alUpgradeFixture()
	original := s
	encode := func(v alUpgradeState) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if _, err := alUpgradeReadState(encode(s), s.Intent, &original); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*alUpgradeState){
		"restart": func(v *alUpgradeState) {
			v.StartedAt = v.StartedAt.Add(time.Minute)
			v.Deadline = v.Deadline.Add(time.Minute)
		},
		"extended deadline": func(v *alUpgradeState) { v.Deadline = v.Deadline.Add(time.Minute) },
		"lost history":      func(v *alUpgradeState) { v.HistoryLost = true },
		"missing cursor":    func(v *alUpgradeState) { v.ResourceVersion = "" },
		"candidate changed": func(v *alUpgradeState) { v.Intent.ValuesDigest = "other" },
		"changed baseline":  func(v *alUpgradeState) { v.BaselineJobUID = "new" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := s
			mutate(&changed)
			if _, err := alUpgradeReadState(encode(changed), s.Intent, &original); err == nil {
				t.Fatal("changed transaction accepted")
			}
		})
	}
}

func TestAlUpgradeRBACRefusesIncompleteInventory(t *testing.T) {
	s, _ := alUpgradeFixture()
	makeObjects := func(i alUpgradeIntent) ([]client.Object, error) {
		return alUpgradeObserverObjects(i, i.Image, []byte("chart"), []byte("values"), []byte("kubeconfig"))
	}
	objects, err := makeObjects(s.Intent)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*alUpgradeIntent){
		"no CRDs": func(i *alUpgradeIntent) { i.CRDDigests = nil },
		"missing result records": func(i *alUpgradeIntent) {
			i.CRDDigests = maps.Clone(i.CRDDigests)
			delete(i.CRDDigests, crdupgrade.PtahResultRecordCRDName)
		},
		"unknown CRD": func(i *alUpgradeIntent) {
			i.CRDDigests = maps.Clone(i.CRDDigests)
			delete(i.CRDDigests, crdupgrade.PtahSchemaCRDName)
			i.CRDDigests["unrelated.example.com"] = "sha256:" + strings.Repeat("b", 64)
		},
		"no probes":        func(i *alUpgradeIntent) { i.Probes = nil },
		"empty probe name": func(i *alUpgradeIntent) { i.Probes = append([]alUpgradeProbe(nil), i.Probes...); i.Probes[0].Name = "" },
		"empty hook name":  func(i *alUpgradeIntent) { i.HookJob = "" },
		"changed chart":    func(i *alUpgradeIntent) { i.ChartDigest = "sha256:" + strings.Repeat("c", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			intent := s.Intent
			mutate(&intent)
			if _, err := makeObjects(intent); err == nil {
				t.Fatal("unsafe RBAC inputs accepted")
			}
		})
	}
	var deployment *appsv1.Deployment
	crds := 0
	probes := 0
	for _, o := range objects {
		switch v := o.(type) {
		case *appsv1.Deployment:
			deployment = v
		case *rbacv1.ClusterRole:
			for _, r := range v.Rules {
				crds += len(r.ResourceNames)
				if len(r.Verbs) != 1 || r.Verbs[0] != "get" {
					t.Fatal("writable CRD grant")
				}
			}
		case *rbacv1.Role:
			for _, r := range v.Rules {
				if len(r.APIGroups) == 1 && r.APIGroups[0] == "operator.ptah.run" {
					probes++
					if len(r.ResourceNames) != 1 || r.ResourceNames[0] == "" {
						t.Fatal("unbounded probe grant")
					}
				}
			}
		}
	}
	if crds != len(crdupgrade.Names()) || probes != 4 || deployment == nil {
		t.Fatal("missing observer grant or workload")
	}
	pod := deployment.Spec.Template.Spec
	if pod.ServiceAccountName == "default" || pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot || len(pod.InitContainers) != 1 {
		t.Fatal("observer lost restricted bootstrap")
	}
	for _, c := range []corev1.Container{pod.Containers[0], pod.InitContainers[0]} {
		if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation || len(c.Resources.Requests) == 0 || len(c.Resources.Limits) == 0 {
			t.Fatal("observer lost security or resource bounds")
		}
	}
}

func TestAlUpgradeProbeRecoveryRequiresCurrentReadOnlyProgress(t *testing.T) {
	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			v := negativeFixtureState(t, family, ptahv1.ApplyPolicyNever)
			after := time.Unix(1700000000, 0).UTC()
			observed := metav1.NewTime(after.Add(time.Minute))
			switch r := v.(type) {
			case *ptahv1.PtahSchema:
				r.Status.Target.LastObservedAt = &observed
				r.Status.Conditions = append(r.Status.Conditions, metav1.Condition{Type: "PlanReady", Status: metav1.ConditionTrue, Reason: "Published", ObservedGeneration: r.Generation, LastTransitionTime: observed})
			case *ptahv1.PtahMigration:
				r.Status.History.ObservedAt = observed
				r.Status.Conditions = append(r.Status.Conditions, metav1.Condition{Type: "Progressing", Status: metav1.ConditionFalse, Reason: "ApplyDisabled", ObservedGeneration: r.Generation, LastTransitionTime: observed})
			}
			original := v.DeepCopyObject().(client.Object)
			if at, ok := alUpgradeProbeProgress(v, original, after); !ok || !at.Equal(observed.Time) {
				t.Fatal("fresh probe refused", at)
			}
			if _, ok := alUpgradeProbeProgress(v, original, observed.Time); ok {
				t.Fatal("pre-hook read accepted")
			}
			replacement := v.DeepCopyObject().(client.Object)
			replacement.SetUID("replacement")
			if alUpgradeProbeSafe(replacement, original) {
				t.Fatal("replacement probe accepted")
			}
			switch r := v.(type) {
			case *ptahv1.PtahSchema:
				r.Status.ActiveOperation = &ptahv1.ActiveOperationStatus{Type: ptahv1.OperationApply}
			case *ptahv1.PtahMigration:
				r.Status.ActiveOperation = &ptahv1.MigrationOperationStatus{Type: ptahv1.MigrationOperationApply}
			}
			if alUpgradeProbeSafe(v, original) {
				t.Fatal("Apply claim accepted")
			}
		})
	}
}

func TestAlUpgradeGaugeBindsTargetAndFreshness(t *testing.T) {
	now := time.Unix(1700000000, 0)
	body := func(instance string, at int64, count int) []byte {
		rows := []any{}
		for n := 0; n < count; n++ {
			rows = append(rows, map[string]any{"metric": map[string]string{"job": "ptah-upgrade-observer", "instance": instance}, "value": []any{at, "1"}})
		}
		b, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": rows}})
		return b
	}
	instance := alUpgradeObserver + "." + alMonitoringNamespace + ".svc:9812"
	if !alUpgradeGauge(body(instance, now.Unix(), 1), 1, now) {
		t.Fatal("fresh target refused")
	}
	for _, row := range []struct {
		instance string
		stamp    int64
		count    int
	}{{instance, now.Unix() - 20, 1}, {instance, now.Unix() + 20, 1}, {"other", now.Unix(), 1}, {instance, now.Unix(), 0}, {instance, now.Unix(), 2}} {
		if alUpgradeGauge(body(row.instance, row.stamp, row.count), 1, now) {
			t.Fatal(fmt.Sprintf("invalid metric accepted: %+v", row))
		}
	}
}

func TestAlUpgradeRetryRequiresNativeCompletion(t *testing.T) {
	s, j := alUpgradeFixture()
	complete := s.Deadline.Add(time.Minute)
	recovered := complete.Add(time.Second)
	j.UID = "retry"
	j.CreationTimestamp = metav1.NewTime(s.Deadline)
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(complete)}}
	j.Status.CompletionTime = &metav1.Time{Time: complete}
	s.Attempts = append(s.Attempts, alUpgradeAttempt{UID: "retry", CreatedAt: s.Deadline, CompletedAt: &complete})
	s.RecoveryJobUID = "retry"
	s.RecoveredAt = &recovered
	events := []watchEvent[*batchv1.Job]{{Type: watch.Modified, Object: j}}
	if err := alUpgradeRetryEvidence(s, events); err != nil {
		t.Fatal(err)
	}
	if err := alUpgradeRetryEvidence(s, nil); err == nil {
		t.Fatal("missing retry accepted")
	}
	j.Spec.Template.Spec.Containers[0].Image = "different"
	if err := alUpgradeRetryEvidence(s, events); err == nil {
		t.Fatal("changed retry candidate accepted")
	}
}

// These native readings keep Ready=False across the read and its completed
// plan. The read timestamp must not start the recovery deadline early.
func TestAlUpgradeRecoveryDatesTheCompletedReadCycle(t *testing.T) {
	body, err := os.ReadFile("../../testdata/e2e/readings/upgrade-recovery-boundaries.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Kind        string          `json:"kind"`
		AfterHook   time.Time       `json:"afterHook"`
		CompletedAt time.Time       `json:"completedAt"`
		Object      json.RawMessage `json:"object"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"e2e-upgrade-schema-mysql":         "PtahSchema",
		"e2e-upgrade-schema-postgresql":    "PtahSchema",
		"e2e-upgrade-migration-mysql":      "PtahMigration",
		"e2e-upgrade-migration-postgresql": "PtahMigration",
	}
	if len(rows) != len(want) {
		t.Fatal("native recovery inventory must contain both families on both engines")
	}
	for _, row := range rows {
		var v client.Object
		switch row.Kind {
		case "PtahSchema":
			v = &ptahv1.PtahSchema{}
		case "PtahMigration":
			v = &ptahv1.PtahMigration{}
		default:
			t.Fatalf("unknown probe kind %q", row.Kind)
		}
		if err := json.Unmarshal(row.Object, v); err != nil {
			t.Fatal(err)
		}
		if want[v.GetName()] != row.Kind {
			t.Fatal("unexpected or repeated native recovery probe", v.GetName())
		}
		delete(want, v.GetName())
		t.Run(v.GetName(), func(t *testing.T) {
			original := v.DeepCopyObject().(client.Object)
			if at, ok := alUpgradeProbeProgress(v, original, row.AfterHook); !ok || !at.Equal(row.CompletedAt) {
				t.Fatalf("recovery was dated at %s (accepted=%t), want the completed read cycle at %s", at, ok, row.CompletedAt)
			}
			read := alNegativeReading(v).readAt
			for name, mutate := range map[string]func(*metav1.Condition){
				"missing":             func(c *metav1.Condition) { c.Type = "Unrelated" },
				"zero timestamp":      func(c *metav1.Condition) { c.LastTransitionTime = metav1.Time{} },
				"before current read": func(c *metav1.Condition) { c.LastTransitionTime = metav1.NewTime(read.Add(-time.Second)) },
				"future":              func(c *metav1.Condition) { c.LastTransitionTime = metav1.NewTime(time.Now().Add(time.Hour)) },
				"old generation":      func(c *metav1.Condition) { c.ObservedGeneration-- },
				"unknown":             func(c *metav1.Condition) { c.Status = metav1.ConditionUnknown },
				"other verdict":       func(c *metav1.Condition) { c.Reason = "OperationInProgress" },
			} {
				bad := v.DeepCopyObject().(client.Object)
				var conditions []metav1.Condition
				var kind string
				switch r := bad.(type) {
				case *ptahv1.PtahSchema:
					conditions, kind = r.Status.Conditions, "PlanReady"
				case *ptahv1.PtahMigration:
					conditions, kind = r.Status.Conditions, "Progressing"
				}
				changed := false
				for i := range conditions {
					if conditions[i].Type == kind {
						mutate(&conditions[i])
						changed = true
					}
				}
				if !changed {
					t.Fatal("native reading omitted its completion condition")
				}
				if _, ok := alUpgradeProbeProgress(bad, original, row.AfterHook); ok {
					t.Errorf("accepted %s recovery evidence", name)
				}
			}
		})
	}
}
