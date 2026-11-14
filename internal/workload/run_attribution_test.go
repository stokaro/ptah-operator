package workload

import (
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The word beside a migration's actor follows the claim: admission when the
// claim names the approval it consumed, policy when it names none. The claim
// is all the builder reads, so the same claim builds the same Job whatever
// happened to the approval since.
func TestBuildMigrationApplyNamesWhereTheActorCameFrom(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		approval *operatorv1alpha1.ImmutableObjectReference
		actor    string
		want     string
	}{
		{
			name:     "an approval",
			approval: &operatorv1alpha1.ImmutableObjectReference{Name: "approved-by-someone", UID: types.UID("approval-uid")},
			actor:    "oidc:Jos%C3%A9 Garc%C3%ADa", want: ActorOriginAdmission,
		},
		{name: "apply: Always", actor: "system:ptah-operator:apply-always:billing/orders", want: ActorOriginPolicy},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			operation := migrationOperationFixture(operatorv1alpha1.MigrationOperationApply)
			operation.ApprovalRef, operation.Actor = row.approval, row.actor
			job, err := builderFixture().BuildMigration(migrationFixture(), operation, migrationPlanFixture())
			if err != nil {
				t.Fatalf("BuildMigration() error = %v", err)
			}
			if got := requireEnv(t, job, EnvActor); got.Value != row.actor || got.ValueFrom != nil {
				t.Fatalf("the executor's actor = %#v, want the literal %q", got, row.actor)
			}
			if got := requireEnv(t, job, EnvActorOrigin); got.Value != row.want || got.ValueFrom != nil {
				t.Fatalf("the executor's actor origin = %#v, want the literal %q", got, row.want)
			}
			requireAttributionOnTheExecutorAlone(t, job)
		})
	}
}

func TestSchemaApplyActorIsTheConsumedApprovalsStamp(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name       string
		approval   *operatorv1alpha1.ConsumedApprovalStatus
		wantActor  string
		wantOrigin string
	}{
		{
			name:      "a plain name",
			approval:  &operatorv1alpha1.ConsumedApprovalStatus{Approver: operatorv1alpha1.ApprovalIdentity{Username: "kubernetes-admin"}},
			wantActor: "kubernetes-admin", wantOrigin: ActorOriginAdmission,
		},
		{
			// Encoded as a migration's is, so both logs read one name one way.
			name:      "a name the log could not store as written",
			approval:  &operatorv1alpha1.ConsumedApprovalStatus{Approver: operatorv1alpha1.ApprovalIdentity{Username: "oidc:José García $(HOME)"}},
			wantActor: "oidc:Jos%C3%A9 Garc%C3%ADa %24(HOME)", wantOrigin: ActorOriginAdmission,
		},
		{
			// Not refused at admission for a schema, unlike a migration: the
			// origin is what says a person approved it.
			name: "a person whose name starts like the operator's",
			approval: &operatorv1alpha1.ConsumedApprovalStatus{
				Approver: operatorv1alpha1.ApprovalIdentity{Username: "system:ptah-operator:apply-always:billing/orders"},
			},
			wantActor: "system:ptah-operator:apply-always:billing/orders", wantOrigin: ActorOriginAdmission,
		},
		{
			// A name of only spaces is none. Ptah would record the process
			// user, and an origin with no name is refused, so neither goes.
			name:     "an approval with no name",
			approval: &operatorv1alpha1.ConsumedApprovalStatus{Approver: operatorv1alpha1.ApprovalIdentity{Username: "   "}},
		},
		{name: "no approval", wantActor: "system:ptah-operator:apply-always:billing/orders", wantOrigin: ActorOriginPolicy},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			schema := schemaFixture()
			schema.Namespace, schema.Name = "billing", "orders"
			schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{Approval: row.approval}
			actor, origin := SchemaApplyActor(schema)
			if actor != row.wantActor || origin != row.wantOrigin {
				t.Fatalf("SchemaApplyActor() = %q, %q, want %q, %q", actor, origin, row.wantActor, row.wantOrigin)
			}
		})
	}
}

// The schema Apply carries who approved it to the process that writes the log,
// through the environment and to that container alone, as a migration Apply
// does.
func TestBuildSchemaApplyCarriesTheApproverToTheExecutorAlone(t *testing.T) {
	t.Parallel()

	const approver = "oidc:José García"
	schema := schemaFixture()
	builder := builderFixture()
	plan := planFixture(schema, builder)
	schema.Status.Plan.Approval.Approver.Username = approver
	job, err := builder.Build(schema, operationFixture(operatorv1alpha1.OperationApply), plan)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got := requireEnv(t, job, EnvActor); got.Value != "oidc:Jos%C3%A9 Garc%C3%ADa" || got.ValueFrom != nil {
		t.Fatalf("the executor's actor = %#v, want the literal encoded name", got)
	}
	if got := requireEnv(t, job, EnvActorOrigin); got.Value != ActorOriginAdmission || got.ValueFrom != nil {
		t.Fatalf("the executor's actor origin = %#v, want the literal %q", got, ActorOriginAdmission)
	}
	requireAttributionOnTheExecutorAlone(t, job)
}

// Under apply: Always a plan that needs no person runs without an approval, and
// the log names the policy and the resource, as a migration's does.
func TestBuildSchemaApplyUnderAlwaysNamesThePolicy(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	builder := builderFixture()
	plan := planFixture(schema, builder)
	schema.Status.Plan.Approval = nil
	job, err := builder.Build(schema, operationFixture(operatorv1alpha1.OperationApply), plan)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	want := "system:ptah-operator:apply-always:" + schema.Namespace + "/" + schema.Name
	if got := requireEnv(t, job, EnvActor); got.Value != want {
		t.Fatalf("the executor's actor = %q, want %q", got.Value, want)
	}
	if got := requireEnv(t, job, EnvActorOrigin); got.Value != ActorOriginPolicy {
		t.Fatalf("the executor's actor origin = %q, want %q", got.Value, ActorOriginPolicy)
	}
}

// An approval that names nobody gives Ptah neither variable: no name to record,
// and an origin alone is a command line Ptah refuses.
func TestBuildSchemaApplyWithAnUnnamedApprovalSetsNeitherVariable(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	builder := builderFixture()
	plan := planFixture(schema, builder)
	schema.Status.Plan.Approval.Approver.Username = ""
	job, err := builder.Build(schema, operationFixture(operatorv1alpha1.OperationApply), plan)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	for _, name := range []string{EnvActor, EnvActorOrigin} {
		if _, found := envMap(job)[name]; found {
			t.Fatalf("the Apply Job carries %s for an approval that names nobody", name)
		}
	}
}

// Only an Apply writes the log. A schema's other operations carry no name,
// whatever status.plan holds.
func TestBuildSchemaGivesOtherOperationsNoActor(t *testing.T) {
	t.Parallel()

	for _, operation := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationResolve, operatorv1alpha1.OperationVerify,
		operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan,
	} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			schema := schemaFixture()
			schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{Approval: &operatorv1alpha1.ConsumedApprovalStatus{
				Name: "approve-plan", UID: types.UID("approval-uid"),
				Approver: operatorv1alpha1.ApprovalIdentity{Username: "kubernetes-admin"},
			}}
			job, err := builderFixture().Build(schema, operationFixture(operation), nil)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			for _, container := range slices.Concat(job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers) {
				for _, variable := range container.Env {
					if variable.Name == EnvActor || variable.Name == EnvActorOrigin {
						t.Fatalf("a %s Job's container %s carries %s", operation, container.Name, variable.Name)
					}
				}
			}
		})
	}
}

// requireAttributionOnTheExecutorAlone holds the actor and its origin to the
// executor container: no other container, no command line and no annotation
// carries either.
func requireAttributionOnTheExecutorAlone(t *testing.T, job *batchv1.Job) {
	t.Helper()
	template := job.Spec.Template
	executor := template.Spec.Containers[0].Name
	actor := requireEnv(t, job, EnvActor).Value
	for _, container := range slices.Concat(template.Spec.InitContainers, template.Spec.Containers) {
		if slices.ContainsFunc(slices.Concat(container.Command, container.Args), func(argument string) bool {
			return strings.Contains(argument, actor) || strings.Contains(argument, "--actor")
		}) {
			t.Errorf("container %s carries the actor on its command line", container.Name)
		}
		if container.Name == executor {
			continue
		}
		for _, variable := range container.Env {
			if variable.Name == EnvActor || variable.Name == EnvActorOrigin || strings.Contains(variable.Value, actor) {
				t.Errorf("container %s carries the attribution in %s", container.Name, variable.Name)
			}
		}
	}
	if strings.Contains(strings.Join(mapValues(job.Annotations), "\n"), actor) ||
		strings.Contains(strings.Join(mapValues(template.Annotations), "\n"), actor) {
		t.Error("the Job's annotations carry the actor")
	}
}
