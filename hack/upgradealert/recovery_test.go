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
	probes int
	fail   bool
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
			mutate(&s, objects)
			c := &admissionClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
			if err := verifyRecovery(context.Background(), c, s); err == nil {
				t.Fatal("invalid recovery accepted")
			}
		})
	}
}
