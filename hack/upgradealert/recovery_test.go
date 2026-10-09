package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

type admissionClient struct {
	client.Client
	probes   int
	fail     bool
	conflict string
}

func (c *admissionClient) Patch(ctx context.Context, o client.Object, p client.Patch, opts ...client.PatchOption) error {
	options := &client.PatchOptions{}
	for _, option := range opts {
		option.ApplyToPatch(options)
	}
	if len(options.DryRun) != 1 || options.DryRun[0] != metav1.DryRunAll {
		return fmt.Errorf("probe attempted a real write")
	}
	if c.fail {
		return fmt.Errorf("admission unavailable")
	}
	if o.GetName() == c.conflict {
		c.conflict = ""
		return apierrors.NewConflict(ptahv1.GroupVersion.WithResource("ptahschemas").GroupResource(), o.GetName(), fmt.Errorf("status advanced during dry run"))
	}
	c.probes++
	return nil
}
func recoveryFixture(t *testing.T) (State, []client.Object, *runtime.Scheme) {
	t.Helper()
	s := fixtureState()
	job := fixtureJob(s, "completed", time.Second)
	at := s.StartedAt.Add(time.Minute)
	terminateJob(job, batchv1.JobComplete, at)
	if err := s.observe(job, at); err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, ptahv1.AddToScheme, apiextensionsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	var objects []client.Object
	for name := range s.Intent.CRDDigests {
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "operator.ptah.run_"+name[:len(name)-len(".operator.ptah.run")]+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		c := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(raw, c); err != nil {
			t.Fatal(err)
		}
		digest, err := crdupgrade.ComputeSchemaDigest(c)
		if err != nil {
			t.Fatal(err)
		}
		s.Intent.CRDDigests[name] = digest
		c.Annotations[crdupgrade.SchemaDigestAnnotation] = digest
		c.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}, {Type: apiextensionsv1.NamesAccepted, Status: apiextensionsv1.ConditionTrue}}
		objects = append(objects, c)
	}
	for _, name := range []string{s.Intent.Manager, s.Intent.Rotator} {
		labels := map[string]string{"app": name, "app.kubernetes.io/instance": s.Intent.Release}
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Intent.Namespace, UID: types.UID(name + "-uid"), Generation: 2, Labels: labels}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime", Image: s.Intent.Image}}}}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1}}
		rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: name + "-rs", Namespace: s.Intent.Namespace, UID: types.UID(name + "-rs-uid"), OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(d, appsv1.SchemeGroupVersion.WithKind("Deployment"))}}}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-pod", Namespace: s.Intent.Namespace, UID: types.UID(name + "-pod-uid"), Labels: labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}}, Spec: d.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "runtime", Ready: true, ImageID: "runtime-digest", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
		objects = append(objects, d, rs, pod)
	}
	probe := s.Intent.Probes[0]
	v := &ptahv1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Name: probe.Name, Namespace: probe.Namespace, UID: types.UID(probe.UID), Generation: probe.Generation}, Status: ptahv1.PtahSchemaStatus{ObservedGeneration: probe.Generation, Conditions: []metav1.Condition{{Type: ptahv1.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: probe.Generation}}}}
	stamp := metav1.NewTime(at.Add(time.Minute))
	v.Status.Target.LastObservedAt = &stamp
	objects = append(objects, v)
	return s, objects, scheme
}

func TestRecoveryRequiresTheCandidateAndFreshAdmission(t *testing.T) {
	s, objects, scheme := recoveryFixture(t)
	c := &admissionClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
	if err := verifyRecovery(context.Background(), c, s); err != nil {
		t.Fatal(err)
	}
	if c.probes != len(s.Intent.Probes) {
		t.Fatal("recovery omitted its admission probes")
	}
	c.fail = true
	if err := verifyRecovery(context.Background(), c, s); err == nil {
		t.Fatal("unavailable admission accepted")
	}
}

func TestRecoveryRefusesIncompleteOrSubstitutedInstallation(t *testing.T) {
	for name, mutate := range map[string]func(*State, []client.Object){
		"watch gap":         func(s *State, _ []client.Object) { s.HistoryLost = true },
		"no completed hook": func(s *State, _ []client.Object) { s.Attempts = nil },
		"CRD not established": func(_ *State, o []client.Object) {
			o[0].(*apiextensionsv1.CustomResourceDefinition).Status.Conditions = nil
		},
		"CRD changed": func(_ *State, o []client.Object) {
			o[0].(*apiextensionsv1.CustomResourceDefinition).Spec.Group = "other.test"
		},
		"stopped runtime": func(_ *State, o []client.Object) {
			for _, v := range o {
				if d, ok := v.(*appsv1.Deployment); ok {
					d.Spec.Replicas = ptr.To(int32(0))
					break
				}
			}
		},
		"stale deployment status": func(_ *State, o []client.Object) {
			for _, v := range o {
				if d, ok := v.(*appsv1.Deployment); ok {
					d.Generation++
					break
				}
			}
		},
		"another image": func(_ *State, o []client.Object) {
			for _, v := range o {
				if d, ok := v.(*appsv1.Deployment); ok {
					d.Spec.Template.Spec.Containers[0].Image = "other"
					break
				}
			}
		},
		"borrowed Pod": func(_ *State, o []client.Object) {
			for _, v := range o {
				if p, ok := v.(*corev1.Pod); ok {
					p.OwnerReferences[0].UID = "another"
					break
				}
			}
		},
		"stale observation": func(_ *State, o []client.Object) {
			v := o[len(o)-1].(*ptahv1.PtahSchema)
			v.Status.Target.LastObservedAt = nil
		},
		"future observation": func(_ *State, o []client.Object) {
			v := o[len(o)-1].(*ptahv1.PtahSchema)
			stamp := metav1.NewTime(time.Now().Add(time.Hour))
			v.Status.Target.LastObservedAt = &stamp
		},
		"replacement workload": func(_ *State, o []client.Object) { o[len(o)-1].SetUID("replacement") },
		"changed workload":     func(_ *State, o []client.Object) { o[len(o)-1].SetGeneration(2) },
		"unknown Apply": func(_ *State, o []client.Object) {
			o[len(o)-1].(*ptahv1.PtahSchema).Status.PendingObservation = &ptahv1.PendingObservationStatus{}
		},
		"suspended workload": func(_ *State, o []client.Object) { o[len(o)-1].(*ptahv1.PtahSchema).Spec.Suspend = true },
		"failed workload": func(_ *State, o []client.Object) {
			v := o[len(o)-1].(*ptahv1.PtahSchema)
			v.Status.Conditions = append(v.Status.Conditions, metav1.Condition{Type: "ReconciliationFailed", Status: metav1.ConditionTrue})
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, objects, scheme := recoveryFixture(t)
			baseline := &admissionClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
			var verifier recoveryVerifier
			if err := verifier.verify(context.Background(), baseline, s); err != nil {
				t.Fatal(err)
			}
			mutate(&s, objects)
			c := &admissionClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
			if err := verifier.verify(context.Background(), c, s); err == nil {
				t.Fatal("invalid recovery accepted")
			}
		})
	}
}

func TestRecoveryRetainsEachProbesPostHookBoundary(t *testing.T) {
	s, objects, scheme := recoveryFixture(t)
	first := objects[len(objects)-1].(*ptahv1.PtahSchema)
	second := first.DeepCopy()
	second.Name, second.UID = "second", "second-uid"
	s.Intent.Probes = append(s.Intent.Probes, Probe{Kind: "PtahSchema", Namespace: second.Namespace, Name: second.Name, UID: string(second.UID), Generation: second.Generation})
	idle := first.Status.DeepCopy()
	observing := idle.DeepCopy()
	observing.ActiveOperation = &ptahv1.ActiveOperationStatus{Type: ptahv1.OperationObserve, Attempt: 1}
	observing.Conditions = []metav1.Condition{{Type: ptahv1.ConditionReady, Status: metav1.ConditionFalse, Reason: "OperationInProgress", ObservedGeneration: first.Generation}}
	second.Status = *observing.DeepCopy()
	objects = append(objects, second)
	c := &admissionClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(first, second).WithObjects(objects...).Build()}
	var verifier recoveryVerifier
	if err := verifier.verify(context.Background(), c, s); err == nil {
		t.Fatal("probe with no post-hook boundary was accepted")
	}
	setStatus := func(v *ptahv1.PtahSchema, status *ptahv1.PtahSchemaStatus) {
		t.Helper()
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(v), v); err != nil {
			t.Fatal(err)
		}
		v.Status = *status.DeepCopy()
		if err := c.Status().Update(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
	// The first resource starts its next ordinary read before the second one
	// reaches its boundary. There is never a simultaneous idle snapshot.
	setStatus(first, observing)
	setStatus(second, idle)
	if err := verifier.verify(context.Background(), c, s); err != nil {
		t.Fatalf("independently recovered probes refused: %v", err)
	}
	if c.probes < 2 {
		t.Fatal("recovery omitted live admission checks")
	}
	retried := s
	retried.Attempts = append([]Attempt(nil), s.Attempts...)
	retried.Attempts[len(retried.Attempts)-1].UID = "another-retry"
	if err := verifier.verify(context.Background(), c, retried); err == nil {
		t.Fatal("new retry borrowed previous probe progress")
	}
	setStatus(first, idle)
	if err := verifier.verify(context.Background(), c, s); err != nil {
		t.Fatal(err)
	}
	setStatus(first, observing)
	// Current admission and execution authority still matter after a receipt.
	c.fail = true
	if err := verifier.verify(context.Background(), c, s); err == nil {
		t.Fatal("cached progress hid failed admission")
	}
	c.fail = false
	setStatus(first, idle)
	if err := verifier.verify(context.Background(), c, s); err != nil {
		t.Fatal(err)
	}
	applying := observing.DeepCopy()
	applying.ActiveOperation.Type = ptahv1.OperationApply
	setStatus(first, applying)
	if err := verifier.verify(context.Background(), c, s); err == nil {
		t.Fatal("cached progress hid Apply")
	}

}

func TestRecoveryKeepsVerifiedProgressAcrossAdmissionConflict(t *testing.T) {
	s, objects, scheme := recoveryFixture(t)
	first := objects[len(objects)-1].(*ptahv1.PtahSchema)
	second := first.DeepCopy()
	second.Name, second.UID = "second", "second-uid"
	s.Intent.Probes = append(s.Intent.Probes, Probe{Kind: "PtahSchema", Namespace: second.Namespace, Name: second.Name, UID: string(second.UID), Generation: second.Generation})
	idle := first.Status.DeepCopy()
	reading := idle.DeepCopy()
	reading.ActiveOperation = &ptahv1.ActiveOperationStatus{Type: ptahv1.OperationObserve, Attempt: 1}
	reading.Conditions = []metav1.Condition{{Type: ptahv1.ConditionReady, Status: metav1.ConditionFalse, Reason: "OperationInProgress", ObservedGeneration: first.Generation}}
	second.Status = *reading.DeepCopy()
	objects = append(objects, second)
	c := &admissionClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(first, second).WithObjects(objects...).Build()}
	var verifier recoveryVerifier
	if err := verifier.verify(t.Context(), c, s); err == nil || !verifier.probes[string(first.UID)] {
		t.Fatalf("first probe did not establish its independent boundary: %v", err)
	}
	for _, pair := range []struct {
		object *ptahv1.PtahSchema
		status *ptahv1.PtahSchemaStatus
	}{{first, reading}, {second, idle}} {
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(pair.object), pair.object); err != nil {
			t.Fatal(err)
		}
		pair.object.Status = *pair.status.DeepCopy()
		if err := c.Status().Update(t.Context(), pair.object); err != nil {
			t.Fatal(err)
		}
	}
	c.conflict = first.Name
	if err := verifier.verify(t.Context(), c, s); !apierrors.IsConflict(err) {
		t.Fatalf("conflicting admission probe passed recovery: %v", err)
	}
	// A successful retry must use current admission. It must not wait for the
	// first probe's next idle period after that probe already proved recovery.
	before := c.probes
	if err := verifier.verify(t.Context(), c, s); err != nil {
		t.Fatalf("status conflict discarded verified post-hook progress: %v", err)
	}
	if c.probes-before != len(s.Intent.Probes) {
		t.Fatal("recovery skipped fresh admission after the conflict")
	}
}

func TestRecoveryKeepsFirstProgressAcrossAdmissionConflict(t *testing.T) {
	s, objects, scheme := recoveryFixture(t)
	probe := objects[len(objects)-1].(*ptahv1.PtahSchema)
	c := &admissionClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(probe).WithObjects(objects...).Build(), conflict: probe.Name}
	var verifier recoveryVerifier
	if err := verifier.verify(t.Context(), c, s); !apierrors.IsConflict(err) {
		t.Fatalf("first admission conflict passed recovery: %v", err)
	}
	// The first post-hook healthy boundary is already verified. A status write
	// can race that very first dry run, before any admission receipt exists.
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(probe), probe); err != nil {
		t.Fatal(err)
	}
	probe.Status.ActiveOperation = &ptahv1.ActiveOperationStatus{Type: ptahv1.OperationObserve, Attempt: 1}
	probe.Status.Conditions = []metav1.Condition{{Type: ptahv1.ConditionReady, Status: metav1.ConditionFalse, Reason: "OperationInProgress", ObservedGeneration: probe.Generation}}
	if err := c.Status().Update(t.Context(), probe); err != nil {
		t.Fatal(err)
	}
	if err := verifier.verify(t.Context(), c, s); err != nil {
		t.Fatalf("first admission conflict discarded verified post-hook progress: %v", err)
	}
	if c.probes != 1 {
		t.Fatal("recovery skipped fresh admission after the first conflict")
	}
	// A real admission failure still invalidates the retained progress. The
	// next ordinary read cannot supply a new healthy boundary on its own.
	c.fail = true
	if err := verifier.verify(t.Context(), c, s); err == nil {
		t.Fatal("retained progress hid failed admission")
	}
	c.fail = false
	if err := verifier.verify(t.Context(), c, s); err == nil {
		t.Fatal("failed admission retained the previous healthy boundary")
	}
}

func TestVerifiedMigrationMayContinueHistoryButNotApplyOrRetry(t *testing.T) {
	after := time.Now().Add(-time.Hour)
	probe := Probe{Kind: "PtahMigration", Namespace: "workloads", Name: "migration", UID: "migration-uid", Generation: 1}
	v := &ptahv1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Namespace: probe.Namespace, Name: probe.Name, UID: types.UID(probe.UID), Generation: probe.Generation}}
	v.Status.ObservedGeneration = 1
	v.Status.History = &ptahv1.MigrationHistoryStatus{ObservedAt: metav1.NewTime(after.Add(time.Minute))}
	v.Status.ActiveOperation = &ptahv1.MigrationOperationStatus{Type: ptahv1.MigrationOperationHistory, Attempt: 1}
	if err := verifyProbeState(v, probe, after, true); err != nil {
		t.Fatal(err)
	}
	if err := verifyProbe(v, probe, after); err == nil {
		t.Fatal("unverified migration accepted during History")
	}
	v.Status.ActiveOperation.Type = ptahv1.MigrationOperationApply
	if err := verifyProbeState(v, probe, after, true); err == nil {
		t.Fatal("cached progress hid Apply")
	}
	v.Status.ActiveOperation.Type = ptahv1.MigrationOperationHistory
	v.Status.ActiveOperation.RetryNotBefore = &metav1.Time{Time: time.Now().Add(time.Minute)}
	if err := verifyProbeState(v, probe, after, true); err == nil {
		t.Fatal("cached progress hid retry backoff")
	}
}
