package e2e

import (
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestPtahVersionDeclarationUsesTheSameBuildAndRestoresItsExactAlias(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"v0.9.0-73-gf6e562c5b", "0.9.0-73-gf6e562c5b"} {
		alias, err := ptahVersionAlias(version)
		if err != nil || alias == version {
			t.Fatalf("alternate declaration = %q, %v", alias, err)
		}
		original, err := ptahVersionAlias(alias)
		if err != nil || original != version {
			t.Fatal("the version declaration cannot restore the exact pinned input")
		}
	}
	for _, version := range []string{"", "v", "vv1", " v1", "v1 ", "v1\n", "v1\x00", "v1\u00a0", string([]byte{0xff}), strings.Repeat("x", 128)} {
		if _, err := ptahVersionAlias(version); err == nil {
			t.Fatalf("an ambiguous or oversized version control passed: %q", version)
		}
	}
}

func TestRunnerImageRolloutKeepsTheSupportedContractAndAllOtherInputs(t *testing.T) {
	t.Parallel()
	change := executionComponentChange{"runner-image", "registry.example/operator@sha256:" + strings.Repeat("a", 64),
		"registry.example/operator@sha256:" + strings.Repeat("b", 64)}
	fixture := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "unchanged-manager", Args: []string{
			"--leader-elect", "--executor-image=unchanged", "--ptah-version=v0.9.0", "--controller-state-version=1", "--runner-image=" + change.original,
		}}}}}}}
	want, got := fixture.DeepCopy(), fixture.DeepCopy()
	want.Spec.Template.Spec.Containers[0].Args[4] = "--runner-image=" + change.replacement
	if err := replaceControllerExecutionComponent(got, change); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("runner rollout changed another execution input: %v", err)
	}
	for range 2 {
		if err := replaceControllerExecutionComponent(got, change.reverse()); err != nil || !reflect.DeepEqual(got, fixture) {
			t.Fatalf("runner cleanup failed to restore the exact supported image: %v", err)
		}
	}
	for name, mutate := range map[string]func(*appsv1.Deployment){
		"duplicate manager": func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, d.Spec.Template.Spec.Containers[0])
		},
		"missing argument": func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].Args = d.Spec.Template.Spec.Containers[0].Args[:4]
		},
		"uncontrolled image": func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Args[4] = "--runner-image=other" },
		"duplicate argument": func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].Args = append(d.Spec.Template.Spec.Containers[0].Args, "--runner-image="+change.original)
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := fixture.DeepCopy()
			mutate(d)
			if replaceControllerExecutionComponent(d, change) == nil {
				t.Fatal("an uncontrolled runner rollout passed")
			}
		})
	}
	before := &ptahv1alpha1.ExecutionBindingStatus{Epoch: "v1-" + strings.Repeat("a", 32), ExecutorImage: "unchanged",
		PtahVersion: "v0.9.0", RunnerProtocolVersion: 1, ControllerStateVersion: 1}
	after := before.DeepCopy()
	after.Epoch = "v1-" + strings.Repeat("b", 32)
	if change.binding(before, after) {
		t.Fatal("the recorded runner image was treated as an execution-binding change")
	}
	if (executionComponentChange{"runner-image", change.original, "registry.example/operator:mutable"}).valid() {
		t.Fatal("a mutable runner replacement passed")
	}
}

func TestPtahVersionRolloutRetainsEveryOtherManagerInput(t *testing.T) {
	t.Parallel()
	change := executionComponentChange{"ptah-version", "v0.9.0", "0.9.0"}
	fixture := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "unchanged-manager", Args: []string{
			"--leader-elect", "--executor-image=unchanged", "--ptah-version=v0.9.0",
		}}}}}}}
	want, got := fixture.DeepCopy(), fixture.DeepCopy()
	want.Spec.Template.Spec.Containers[0].Args[2] = "--ptah-version=0.9.0"
	if err := replaceControllerExecutionComponent(got, change); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("version rollout changed another input: %v", err)
	}
	for range 2 {
		if err := replaceControllerExecutionComponent(got, change.reverse()); err != nil || !reflect.DeepEqual(got, fixture) {
			t.Fatalf("version rollback or repeated cleanup changed another input: %v", err)
		}
	}
	for name, mutate := range map[string]func(*appsv1.Deployment){
		"missing manager": func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Name = "sidecar" },
		"duplicate manager": func(d *appsv1.Deployment) {
			p := &d.Spec.Template.Spec
			p.Containers = append(p.Containers, p.Containers[0])
		},
		"missing argument":  func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Args = []string{"--leader-elect"} },
		"unrelated version": func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Args[2] = "--ptah-version=v2" },
		"duplicate argument": func(d *appsv1.Deployment) {
			c := &d.Spec.Template.Spec.Containers[0]
			c.Args = append(c.Args, "--ptah-version=v0.9.0")
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := fixture.DeepCopy()
			mutate(d)
			if replaceControllerExecutionComponent(d, change) == nil {
				t.Fatal("uncontrolled version rollout passed")
			}
		})
	}
}

func TestPtahVersionBindingRequiresExactlyOneComponentAndANewEpoch(t *testing.T) {
	t.Parallel()
	change := executionComponentChange{"ptah-version", "v0.9.0", "0.9.0"}
	before := &ptahv1alpha1.ExecutionBindingStatus{Epoch: "v1-" + strings.Repeat("a", 32),
		ExecutorImage: "executor", PtahVersion: change.original, RunnerProtocolVersion: 1, ControllerStateVersion: 1}
	after := before.DeepCopy()
	after.Epoch, after.PtahVersion = "v1-"+strings.Repeat("b", 32), change.replacement
	if !change.binding(before, after) {
		t.Fatal("exact version declaration transition was refused")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.ExecutionBindingStatus){
		"same epoch":       func(b *ptahv1alpha1.ExecutionBindingStatus) { b.Epoch = before.Epoch },
		"invalid epoch":    func(b *ptahv1alpha1.ExecutionBindingStatus) { b.Epoch = "new" },
		"same version":     func(b *ptahv1alpha1.ExecutionBindingStatus) { b.PtahVersion = before.PtahVersion },
		"other version":    func(b *ptahv1alpha1.ExecutionBindingStatus) { b.PtahVersion = "v2" },
		"image changed":    func(b *ptahv1alpha1.ExecutionBindingStatus) { b.ExecutorImage = "other" },
		"protocol changed": func(b *ptahv1alpha1.ExecutionBindingStatus) { b.RunnerProtocolVersion++ },
		"state changed":    func(b *ptahv1alpha1.ExecutionBindingStatus) { b.ControllerStateVersion++ },
	} {
		t.Run(name, func(t *testing.T) {
			b := after.DeepCopy()
			mutate(b)
			if change.binding(before, b) {
				t.Fatal("unrelated or incomplete version transition passed")
			}
		})
	}
	if change.binding(nil, after) || change.binding(before, nil) || (executionComponentChange{"unknown", "old", "new"}).binding(before, after) {
		t.Fatal("missing or undeclared component evidence passed")
	}
}

func TestSchemaPtahVersionDecisionPreservesTheApprovedWork(t *testing.T) {
	t.Parallel()
	before, current, old, fresh := schemaReplacementFixture()
	change := executionComponentChange{"ptah-version", "v0.9.0", "0.9.0"}
	current.UID, fresh.Spec.SchemaRef.UID = before.UID, before.UID
	before.Status.ExecutionBinding = &ptahv1alpha1.ExecutionBindingStatus{Epoch: "v1-" + strings.Repeat("a", 32),
		ExecutorImage: "unchanged", PtahVersion: change.original, RunnerProtocolVersion: 1, ControllerStateVersion: 1}
	current.Status.ExecutionBinding = before.Status.ExecutionBinding.DeepCopy()
	current.Status.ExecutionBinding.Epoch, current.Status.ExecutionBinding.PtahVersion = "v1-"+strings.Repeat("b", 32), change.replacement
	old.Spec.ExecutionBindingID, old.Spec.PtahVersion, old.Spec.ExecutorImage = before.Status.ExecutionBinding.Epoch, change.original, "unchanged"
	fresh.Spec.ExecutionBindingID, fresh.Spec.PtahVersion, fresh.Spec.ExecutorImage = current.Status.ExecutionBinding.Epoch, change.replacement, "unchanged"
	if err := changedSchemaExecutionDecision(before, current, old, fresh, change); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchemaPlan){
		"old declaration": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.PtahVersion = change.original },
		"changed image":   func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ExecutorImage = "other" },
		"changed target":  func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.TargetIdentityDigest = "other" },
		"changed policy":  func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.PolicyFingerprint = "other" },
		"changed source":  func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ArtifactDigest = "other" },
		"old epoch":       func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ExecutionBindingID = old.Spec.ExecutionBindingID },
		"old fingerprint": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Fingerprint = old.Spec.Fingerprint },
	} {
		t.Run(name, func(t *testing.T) {
			p := fresh.DeepCopy()
			mutate(p)
			if changedSchemaExecutionDecision(before, current, old, p, change) == nil {
				t.Fatal("unrelated or incomplete version-bound plan passed")
			}
		})
	}
}

func TestMigrationPtahVersionDecisionPreservesTheExactSequence(t *testing.T) {
	t.Parallel()
	before, current, old, fresh := migrationReplacementFixture()
	change := executionComponentChange{"ptah-version", "v0.9.0", "0.9.0"}
	current.UID, fresh.Spec.MigrationRef.UID = before.UID, before.UID
	before.Status.ExecutionBinding = &ptahv1alpha1.ExecutionBindingStatus{Epoch: "v1-" + strings.Repeat("a", 32),
		ExecutorImage: "unchanged", PtahVersion: change.original, RunnerProtocolVersion: 1, ControllerStateVersion: 1}
	current.Status.ExecutionBinding = before.Status.ExecutionBinding.DeepCopy()
	current.Status.ExecutionBinding.Epoch, current.Status.ExecutionBinding.PtahVersion = "v1-"+strings.Repeat("b", 32), change.replacement
	old.Spec.ExecutionBindingID, old.Spec.PtahVersion, old.Spec.ExecutorImage = before.Status.ExecutionBinding.Epoch, change.original, "unchanged"
	fresh.Spec.ExecutionBindingID, fresh.Spec.PtahVersion, fresh.Spec.ExecutorImage = current.Status.ExecutionBinding.Epoch, change.replacement, "unchanged"
	old.Spec.Migrations = append(old.Spec.Migrations, ptahv1alpha1.PlannedMigration{Version: 2, Checksum: "second"}, ptahv1alpha1.PlannedMigration{Version: 3, Checksum: "third"})
	fresh.Spec.Migrations = old.DeepCopy().Spec.Migrations
	if err := changedMigrationExecutionDecision(before, current, old, fresh, change); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigrationPlan){
		"old declaration":  func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.PtahVersion = change.original },
		"changed image":    func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.ExecutorImage = "other" },
		"changed history":  func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.HistoryFingerprint = "other" },
		"changed sequence": func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.Migrations[1].Checksum = "other" },
		"changed target":   func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.TargetIdentityDigest = "other" },
		"changed source":   func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.ArtifactDigest = "other" },
		"old epoch":        func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.ExecutionBindingID = old.Spec.ExecutionBindingID },
		"old fingerprint":  func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.Fingerprint = old.Spec.Fingerprint },
	} {
		t.Run(name, func(t *testing.T) {
			p := fresh.DeepCopy()
			mutate(p)
			if changedMigrationExecutionDecision(before, current, old, p, change) == nil {
				t.Fatal("unrelated or incomplete version-bound migration plan passed")
			}
		})
	}
}
