package crdupgrade

import (
	"maps"
	"reflect"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/utils/ptr"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

func testJobOrphanDeletionGuard() *JobOrphanDeletionGuard {
	return &JobOrphanDeletionGuard{ReleaseName: "ptah", ReleaseNamespace: "ptah-system"}
}

// The guard fails closed, denies, is bound to exactly what it matches, and
// narrows its match by one condition on the Job and none on the requester.
func TestJobOrphanDeletionGuardIsFailClosedAndBoundAsMatched(t *testing.T) {
	t.Parallel()

	guard := testJobOrphanDeletionGuard()
	policy, binding := guard.Policy(), guard.Binding()
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatalf("%s does not fail closed", policy.Name)
	}
	if binding.Spec.PolicyName != policy.Name ||
		!reflect.DeepEqual(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) ||
		!reflect.DeepEqual(binding.Spec.MatchResources, policy.Spec.MatchConstraints) {
		t.Fatalf("%s binding is not exact deny-only enforcement of what the policy matches: %#v", policy.Name, binding.Spec)
	}
	if len(policy.Spec.MatchConditions) != 1 || policy.Spec.MatchConditions[0].Expression != operationJobMatchExpression() {
		t.Fatalf("%s match conditions = %#v, want the operation Job condition alone", policy.Name, policy.Spec.MatchConditions)
	}
	if strings.Contains(policy.Spec.MatchConditions[0].Expression, "request.userInfo") {
		t.Fatalf("%s exempts a requester: %s", policy.Name, policy.Spec.MatchConditions[0].Expression)
	}
	if want := JobOrphanDeletionGuardPolicyName("ptah-system", "ptah"); policy.Name != want {
		t.Fatalf("policy name = %q, want %q", policy.Name, want)
	}
	if strings.HasPrefix(policy.Name, orphanDeletionGuardNamePrefix) {
		t.Fatalf("policy name %q carries the prefix of the resource orphan-deletion guard", policy.Name)
	}
}

// The guard's rules select the deletion of a Job, in every version the API
// serves it under, and no other operation, kind or subresource.
func TestJobOrphanDeletionGuardSelectsTheDeletionOfAJob(t *testing.T) {
	t.Parallel()

	rules := testJobOrphanDeletionGuard().Policy().Spec.MatchConstraints.ResourceRules
	selected := 0
	for _, candidate := range []struct {
		resource schema.GroupVersionResource
		kind     string
	}{
		{schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, "Job"},
		{schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}, "CronJob"},
		{schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}, "Pod"},
		{schema.GroupVersionResource{Group: operatorAPIGroup, Version: "v1alpha1", Resource: migrationResource}, "PtahMigration"},
	} {
		kind := candidate.resource.GroupVersion().WithKind(candidate.kind)
		for _, operation := range []admission.Operation{admission.Create, admission.Update, admission.Delete, admission.Connect} {
			for _, subresource := range []string{"", "status"} {
				attributes := admission.NewAttributesRecord(nil, nil, kind, "team-a", "orders", candidate.resource, subresource,
					operation, nil, false, &user.DefaultInfo{Name: "someone"})
				want := candidate.resource.Resource == "jobs" && operation == admission.Delete && subresource == ""
				if got := controllerWriteRulesMatch(rules, attributes); got != want {
					t.Errorf("the guard selects %s of %s/%s = %t, want %t", operation, candidate.resource.Resource, subresource, got, want)
				}
				if want {
					selected++
				}
			}
		}
	}
	if selected != 1 {
		t.Fatalf("the guard was held to %d requests it selects, want the one deletion of a Job", selected)
	}
}

// The match condition selects the Jobs the manager's builder produces, of both
// families, and refuses the Jobs that carry only part of that identity: the
// mistakes a condition reading one label, or the wrong value, would make.
func TestJobOrphanDeletionGuardSelectsExactlyTheOperationJobs(t *testing.T) {
	t.Parallel()

	builder := workload.Builder{
		ExecutorImage:          "example.test/executor@sha256:" + strings.Repeat("2", 64),
		RunnerImage:            "example.test/runner@sha256:" + strings.Repeat("3", 64),
		PtahVersion:            "v0.3.0",
		ControllerImage:        "example.test/controller@sha256:" + strings.Repeat("1", 64),
		ControllerRevision:     "test-revision",
		ControllerStateVersion: ourStateVersion,
	}
	built := map[string]*batchv1.Job{}
	schemaResource, schemaOperation := contractSchemaFixture(t, builder)
	schemaJob, err := builder.Build(schemaResource, schemaOperation, nil)
	if err != nil {
		t.Fatal(err)
	}
	built["the schema Job the builder produces"] = schemaJob
	for _, operationType := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationResolve, operatorv1alpha1.MigrationOperationHistory,
	} {
		migration, operation := contractMigrationFixture(t, builder, operationType)
		job, err := builder.BuildMigration(migration, operation, nil)
		if err != nil {
			t.Fatal(err)
		}
		built["the migration "+string(operationType)+" Job the builder produces"] = job
	}
	// A Job an orphaning deletion of its owner left without an owner reference
	// keeps the labels, and its Pods keep them from the template.
	orphaned := schemaJob.DeepCopy()
	orphaned.OwnerReferences = nil
	built["a schema Job left with no owner"] = orphaned
	// Anyone who may update the Job can change its own labels, and the
	// template still says what its Pods were made from.
	stripped := schemaJob.DeepCopy()
	delete(stripped.Labels, workload.LabelComponent)
	built["a schema Job a person took the component label off"] = stripped
	// The template can change only on a suspended Job that never started, and
	// the Job's own labels still say whose it is.
	suspended := schemaJob.DeepCopy()
	suspended.Spec.Suspend = ptr.To(true)
	delete(suspended.Spec.Template.Labels, workload.LabelComponent)
	built["a suspended schema Job whose template a person took the component label off"] = suspended

	// Each near miss carries its labels on the Job and on its template, so the
	// condition refuses it whichever map it reads.
	withLabels := func(labels map[string]string) *batchv1.Job {
		job := &batchv1.Job{
			TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
			ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "nightly-report", Labels: labels},
		}
		if labels != nil {
			job.Spec.Template.Labels = maps.Clone(labels)
		}
		return job
	}
	other := map[string]*batchv1.Job{
		"a Job with no labels": withLabels(nil),
		"a Job with unrelated labels": withLabels(map[string]string{
			"app.kubernetes.io/name": "nightly-report",
		}),
		"a Job another tool manages under an operation component": withLabels(map[string]string{
			workload.LabelManagedBy: "Helm", workload.LabelComponent: workload.ComponentMigrationOperation,
		}),
		"a Job with only the operation component": withLabels(map[string]string{
			workload.LabelComponent: workload.ComponentSchemaOperation,
		}),
		"a Job the operator manages under another component": withLabels(map[string]string{
			workload.LabelManagedBy: "ptah-operator", workload.LabelComponent: "crd-manager",
		}),
		"a Job with only the operator as its manager": withLabels(map[string]string{
			workload.LabelManagedBy: "ptah-operator",
		}),
		"a Job whose component differs in case": withLabels(map[string]string{
			workload.LabelManagedBy: "ptah-operator", workload.LabelComponent: "Schema-Operation",
		}),
	}

	policy := testJobOrphanDeletionGuard().Policy()
	request := map[string]any{
		"operation": "DELETE",
		"resource":  map[string]any{"group": "batch", "version": "v1", "resource": "jobs"},
		"options":   map[string]any{"propagationPolicy": "Orphan"},
	}
	for name, job := range built {
		if !evaluatePolicyMatchConditions(t, policy, nil, celObject(t, job), request) {
			t.Errorf("the guard does not select %s, labels %v, template labels %v", name, job.Labels, job.Spec.Template.Labels)
		}
	}
	for name, job := range other {
		if evaluatePolicyMatchConditions(t, policy, nil, celObject(t, job), request) {
			t.Errorf("the guard selects %s, labels %v, template labels %v", name, job.Labels, job.Spec.Template.Labels)
		}
	}
	if len(built) != 6 || len(other) != 7 {
		t.Fatalf("the condition was held to %d Jobs it selects and %d it refuses, want 6 and 7", len(built), len(other))
	}
}

// The verdict follows the registry's own decision whether to orphan a Job's
// Pods: the legacy field, then the propagation policy, then the finalizer the
// Job carries, and last batch/v1's default, which orphans. Only a deletion that
// orphans is refused, and the validation that refuses it says why.
func TestJobOrphanDeletionGuardRefusesExactlyTheOrphaningDeletions(t *testing.T) {
	t.Parallel()

	const (
		admitted = iota
		refusedForOptions
		refusedForDefault
	)
	policy := testJobOrphanDeletionGuard().Policy()
	if len(policy.Spec.Validations) != 2 ||
		policy.Spec.Validations[0].Message != jobOrphanDeletionDenialMessage() ||
		policy.Spec.Validations[1].Message != jobDefaultOrphanDenialMessage() {
		t.Fatalf("validations = %#v, want the options rule and then the default rule", policy.Spec.Validations)
	}
	for _, message := range []string{jobOrphanDeletionDenialMessage(), jobDefaultOrphanDenialMessage()} {
		if !strings.Contains(message, "Background") {
			t.Errorf("refusal %q does not name the propagation to use instead", message)
		}
	}
	tracking := batchv1.JobTrackingFinalizer
	for _, test := range []struct {
		name       string
		options    map[string]any
		finalizers []string
		want       int
	}{
		{name: "propagationPolicy Orphan", options: map[string]any{"propagationPolicy": "Orphan"}, want: refusedForOptions},
		{name: "orphanDependents true", options: map[string]any{"orphanDependents": true}, want: refusedForOptions},
		{
			name: "propagationPolicy Orphan on a Job that carries the foreground finalizer", options: map[string]any{"propagationPolicy": "Orphan"},
			finalizers: []string{metav1.FinalizerDeleteDependents}, want: refusedForOptions,
		},
		{name: "propagationPolicy Background", options: map[string]any{"propagationPolicy": "Background"}, want: admitted},
		{name: "propagationPolicy Foreground", options: map[string]any{"propagationPolicy": "Foreground"}, want: admitted},
		{name: "orphanDependents false", options: map[string]any{"orphanDependents": false}, want: admitted},
		{name: "no options", options: map[string]any{}, want: refusedForDefault},
		{name: "only a dry run", options: map[string]any{"dryRun": []any{"All"}}, want: refusedForDefault},
		{name: "only a precondition", options: map[string]any{"preconditions": map[string]any{"uid": "job-uid"}}, want: refusedForDefault},
		{
			name: "no options on a Job that carries another finalizer", options: map[string]any{},
			finalizers: []string{tracking}, want: refusedForDefault,
		},
		{
			name: "no options on a Job that carries the orphan finalizer", options: map[string]any{},
			finalizers: []string{metav1.FinalizerOrphanDependents}, want: refusedForDefault,
		},
		{
			name: "no options on a Job that carries the foreground finalizer", options: map[string]any{},
			finalizers: []string{tracking, metav1.FinalizerDeleteDependents}, want: admitted,
		},
		{
			name: "propagationPolicy Background on a Job that carries the orphan finalizer", options: map[string]any{"propagationPolicy": "Background"},
			finalizers: []string{metav1.FinalizerOrphanDependents}, want: admitted,
		},
		{
			name: "orphanDependents false on a Job that carries the orphan finalizer", options: map[string]any{"orphanDependents": false},
			finalizers: []string{metav1.FinalizerOrphanDependents}, want: admitted,
		},
		{
			name: "orphanDependents true on a Job that carries the foreground finalizer", options: map[string]any{"orphanDependents": true},
			finalizers: []string{metav1.FinalizerDeleteDependents}, want: refusedForOptions,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			metadata := map[string]any{
				"namespace": "tenant-a",
				"name":      "ptah-m-apply-orders-0123456789abcdef",
				"labels": map[string]any{
					workload.LabelManagedBy: "ptah-operator",
					workload.LabelComponent: workload.ComponentMigrationOperation,
				},
			}
			if len(test.finalizers) != 0 {
				carried := make([]any, 0, len(test.finalizers))
				for _, finalizer := range test.finalizers {
					carried = append(carried, finalizer)
				}
				metadata["finalizers"] = carried
			}
			job := map[string]any{"apiVersion": "batch/v1", "kind": "Job", "metadata": metadata}
			request := map[string]any{
				"operation": "DELETE",
				"namespace": "tenant-a",
				"name":      metadata["name"],
				"resource":  map[string]any{"group": "batch", "version": "v1", "resource": "jobs"},
				"userInfo":  map[string]any{"username": "dba@example.test"},
				"options":   test.options,
			}
			if !evaluatePolicyMatchConditions(t, policy, nil, job, request) {
				t.Fatal("the guard does not select the operation Job the verdicts are about")
			}
			results := evaluatePolicyValidations(t, policy, nil, job, request)
			if !results[0] && !results[1] {
				t.Fatalf("both validations refuse, so the refusal would give two reasons for one request: %v", results)
			}
			got := admitted
			switch {
			case !results[0]:
				got = refusedForOptions
			case !results[1]:
				got = refusedForDefault
			}
			if got != test.want {
				t.Fatalf("verdict = %d, want %d (0 admitted, 1 refused for the options, 2 refused for the default): %v", got, test.want, results)
			}
		})
	}
}
