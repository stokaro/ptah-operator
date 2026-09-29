package e2e

import (
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestExecutorRolloutChangesOnlyTheDeclaredArgument(t *testing.T) {
	t.Parallel()
	old, next := "registry.test/executor@sha256:"+strings.Repeat("a", 64), "registry.test/executor@sha256:"+strings.Repeat("b", 64)
	fixture := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "unchanged-manager", Args: []string{"--leader-elect", "--executor-image=" + old, "--ptah-version=unchanged"}}}}}}}
	changed := fixture.DeepCopy()
	if err := replaceControllerExecutor(changed, old, next); err != nil {
		t.Fatal(err)
	}
	want := fixture.DeepCopy()
	want.Spec.Template.Spec.Containers[0].Args[1] = "--executor-image=" + next
	if !reflect.DeepEqual(changed, want) {
		t.Fatal("the executor rollout changed unrelated manager configuration")
	}
	if err := replaceControllerExecutor(changed, next, old); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(changed, fixture) {
		t.Fatal("the rollout did not restore the original configuration")
	}
	if err := replaceControllerExecutor(changed, next, old); err != nil {
		t.Fatal("cleanup could not repeat a completed restore:", err)
	}
	for name, edit := range map[string]func(*appsv1.Deployment){
		"rolling update":   func(d *appsv1.Deployment) { d.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType },
		"missing manager":  func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers = nil },
		"missing argument": func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Args = []string{"--leader-elect"} },
		"duplicate argument": func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].Args = append(d.Spec.Template.Spec.Containers[0].Args, "--executor-image="+old)
		},
		"unrelated executor": func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Args[1] = "--executor-image=another" },
	} {
		t.Run(name, func(t *testing.T) {
			d := fixture.DeepCopy()
			edit(d)
			if replaceControllerExecutor(d, old, next) == nil {
				t.Fatal("unsafe rollout input passed")
			}
		})
	}
	if replaceControllerExecutor(fixture.DeepCopy(), old, old) == nil || replaceControllerExecutor(fixture.DeepCopy(), old, "registry.test/executor:mutable") == nil {
		t.Fatal("unchanged or unpinned executor passed")
	}
}

func TestSchemaExecutorDecisionNeedsANewEpochForTheSameWork(t *testing.T) {
	t.Parallel()
	before, current, old, fresh := schemaReplacementFixture()
	current.UID, fresh.Spec.SchemaRef.UID = before.UID, before.UID
	original, replacement := "registry.test/executor@sha256:"+strings.Repeat("a", 64), "registry.test/executor@sha256:"+strings.Repeat("b", 64)
	before.Status.ExecutionBinding = &ptahv1alpha1.ExecutionBindingStatus{Epoch: "v1-" + strings.Repeat("a", 32), ExecutorImage: original, PtahVersion: "v0.9.0", RunnerProtocolVersion: 1, ControllerStateVersion: 1}
	current.Status.ExecutionBinding = before.Status.ExecutionBinding.DeepCopy()
	current.Status.ExecutionBinding.Epoch = "v1-" + strings.Repeat("b", 32)
	current.Status.ExecutionBinding.ExecutorImage = replacement
	old.Spec.ExecutionBindingID, old.Spec.ExecutorImage = before.Status.ExecutionBinding.Epoch, original
	fresh.Spec.ExecutionBindingID, fresh.Spec.ExecutorImage = current.Status.ExecutionBinding.Epoch, replacement
	if err := changedSchemaExecutorDecision(before, current, old, fresh, original, replacement); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*ptahv1alpha1.PtahSchema, *ptahv1alpha1.PtahSchemaPlan){
		"same epoch": func(s *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) {
			s.Status.ExecutionBinding.Epoch = before.Status.ExecutionBinding.Epoch
			p.Spec.ExecutionBindingID = s.Status.ExecutionBinding.Epoch
		},
		"invalid epoch": func(s *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) {
			s.Status.ExecutionBinding.Epoch = "not-an-epoch"
		},
		"old executor": func(s *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) {
			s.Status.ExecutionBinding.ExecutorImage = original
			p.Spec.ExecutorImage = original
		},
		"other resource":    func(s *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) { s.UID = "other" },
		"spec edit":         func(s *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) { s.Spec.Suspend = true },
		"generation edit":   func(s *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) { s.Generation++ },
		"stale observation": func(s *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) { s.Status.ObservedGeneration = 0 },
		"version change": func(s *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) {
			s.Status.ExecutionBinding.PtahVersion = "v2"
		},
		"protocol change": func(s *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) {
			s.Status.ExecutionBinding.RunnerProtocolVersion++
		},
		"state change": func(s *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) {
			s.Status.ExecutionBinding.ControllerStateVersion++
		},
		"wrong plan epoch": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.ExecutionBindingID = old.Spec.ExecutionBindingID
		},
		"old fingerprint": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.Fingerprint = old.Spec.Fingerprint
		},
		"wrong plan resource": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.SchemaRef.UID = "other" },
		"different target": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.TargetIdentityDigest = "other"
		},
		"different schema": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.ActualStateFingerprint = "other"
		},
		"different artifact": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ArtifactDigest = "other" },
		"different policy":   func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.PolicyFingerprint = "other" },
		"retirement pending": func(s *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) {
			s.Status.PendingBindingRetirement = &ptahv1alpha1.BindingRetirementStatus{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, p := current.DeepCopy(), fresh.DeepCopy()
			edit(s, p)
			if changedSchemaExecutorDecision(before, s, old, p, original, replacement) == nil {
				t.Fatal("unrelated or incomplete transition passed")
			}
		})
	}
	if changedSchemaExecutorDecision(nil, current, old, fresh, original, replacement) == nil {
		t.Fatal("missing initial decision passed")
	}
}

func TestExecutorApplyUsesTheReplacementImage(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "run", Image: "replacement"}}}}}}
	if !jobUsesExecutor(job, "replacement") {
		t.Fatal("replacement executor was refused")
	}
	if jobUsesExecutor(job, "original") || jobUsesExecutor(nil, "replacement") {
		t.Fatal("wrong or missing executor passed")
	}
	job.Spec.Template.Spec.Containers = append(job.Spec.Template.Spec.Containers, corev1.Container{Name: "extra", Image: "replacement"})
	if jobUsesExecutor(job, "replacement") {
		t.Fatal("ambiguous operation container passed")
	}
}
