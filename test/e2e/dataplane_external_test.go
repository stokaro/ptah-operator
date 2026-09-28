package e2e

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

func externalDigest(character byte) string {
	return "sha256:" + strings.Repeat(string(character), 64)
}

const (
	externalReference = "oci://registry.ns.svc.cluster.local:5000/schemas/postgresql-external@sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111"
	externalKey       = "e2e/postgresql-external/app"
	externalSchema    = "e2e-postgresql-external-longpod"
	externalSchemaUID = types.UID("schema-uid")
	externalEpoch     = "v1-0123456789abcdef0123456789abcdef"
)

var externalController = controllerIdentity{image: "ghcr.io/stokaro/ptah-operator@" + externalDigest('c'), revision: "abc123", stateVersion: "4"}

func externalExpectation() automaticExpectation {
	return automaticExpectation{
		reference: externalReference, digest: externalDigest('1'), coordinationKey: externalKey,
		coordinationDigest: externalDigest('2'), controller: externalController, stateVersion: 4,
		runnerImage: "ghcr.io/stokaro/ptah-runner@" + externalDigest('r'),
	}
}

func condition(kind string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message}
}

// convergedExternalSchema is the schema the automatic row reads when it
// converged: every claim automaticConvergenceExact makes holds.
func convergedExternalSchema() *ptahv1alpha1.PtahSchema {
	want := externalExpectation()
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Name, schema.UID, schema.Generation = externalSchema, externalSchemaUID, 3
	schema.Spec.Policy.Apply = "Always"
	schema.Spec.Target.CoordinationKey = externalKey
	binding := &ptahv1alpha1.ExecutionBindingStatus{
		Epoch: externalEpoch, ControllerStateVersion: 4, PtahVersion: "v1.2.3",
		ExecutorImage: "ghcr.io/stokaro/ptah@" + externalDigest('e'), RunnerProtocolVersion: 7,
	}
	schema.Status = ptahv1alpha1.PtahSchemaStatus{
		ObservedGeneration: 3,
		Phase:              ptahv1alpha1.PhaseInSync,
		ExecutionBinding:   binding,
		Source: ptahv1alpha1.SchemaSourceStatus{
			RequestedReference: want.reference, ResolvedReference: want.reference, Digest: want.digest,
			Verified: true, ArtifactType: schemaArtifactType, VerificationPolicyDigest: externalDigest('3'),
		},
		Target: ptahv1alpha1.TargetStatus{
			CoordinationDigest: want.coordinationDigest, IdentityDigest: externalDigest('4'),
			DriftReportDigest: externalDigest('5'),
		},
		Applied: &ptahv1alpha1.AppliedStatus{
			ArtifactDigest: want.digest, PlanFingerprint: externalDigest('6'),
			PlanRef:            ptahv1alpha1.ImmutableObjectReference{Name: "plan-a", UID: "plan-uid"},
			CoordinationDigest: want.coordinationDigest, TargetIdentityDigest: externalDigest('4'),
			ExecutionBindingID: externalEpoch, ControllerImage: want.controller.image,
			ControllerRevision: want.controller.revision, ControllerStateVersion: 4,
			PtahVersion: binding.PtahVersion, ExecutorImage: binding.ExecutorImage, RunnerImage: want.runnerImage,
			RunnerProtocolVersion: binding.RunnerProtocolVersion,
			CompletedAt:           metav1.NewTime(time.Date(2026, 9, 1, 0, 1, 0, 0, time.UTC)),
		},
	}
	for _, expected := range automaticConditions {
		schema.Status.Conditions = append(schema.Status.Conditions,
			condition(expected.kind, expected.status, expected.reason, "the schema is converged"))
	}
	return schema
}

func setCondition(schema *ptahv1alpha1.PtahSchema, kind string, status metav1.ConditionStatus, reason, message string) {
	for index := range schema.Status.Conditions {
		if schema.Status.Conditions[index].Type == kind {
			schema.Status.Conditions[index] = condition(kind, status, reason, message)
		}
	}
}

func TestAutomaticConvergenceExact(t *testing.T) {
	t.Parallel()
	if err := automaticConvergenceExact(convergedExternalSchema(), externalExpectation()); err != nil {
		t.Fatalf("a converged schema was refused: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchema)
	}{
		{"apply on approval", func(s *ptahv1alpha1.PtahSchema) { s.Spec.Policy.Apply = "OnApproval" }},
		{"destructive allowed", func(s *ptahv1alpha1.PtahSchema) { s.Spec.Policy.AllowDestructive = true }},
		{"resolved by tag", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Source.ResolvedReference = strings.Replace(externalReference, "@sha256:", ":stable@sha256:", 1)
		}},
		{"source not verified", func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.Verified = false }},
		{"another realm", func(s *ptahv1alpha1.PtahSchema) { s.Status.Target.CoordinationDigest = externalDigest('9') }},
		{"no applied evidence", func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied = nil }},
		{"no execution binding", func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding = nil }},
		{"applied by another runner", func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied.RunnerImage = "runner:latest" }},
		{"applied under another binding", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Applied.ExecutionBindingID = "v1-ffffffffffffffffffffffffffffffff"
		}},
		{"applied on another target", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Applied.TargetIdentityDigest = externalDigest('8')
		}},
		{"never completed", func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied.CompletedAt = metav1.Time{} }},
		{"a plan still current", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{Name: "plan-b"}
		}},
		{"an operation still claimed", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationObserve}
		}},
		{"waited for an approval", func(s *ptahv1alpha1.PtahSchema) {
			setCondition(s, "ApprovalRequired", metav1.ConditionTrue, "Waiting", "")
		}},
		{"a failure recorded", func(s *ptahv1alpha1.PtahSchema) {
			setCondition(s, "ReconciliationFailed", metav1.ConditionTrue, "OperationFailed", "")
		}},
		{"the coordination key in a condition", func(s *ptahv1alpha1.PtahSchema) {
			setCondition(s, "Ready", metav1.ConditionTrue, "InSync", externalKey)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := convergedExternalSchema()
			test.mutate(schema)
			if err := automaticConvergenceExact(schema, externalExpectation()); err == nil {
				t.Fatal("the mutated schema was accepted")
			}
		})
	}
}

var sequenceStart = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// serializedJobs are the seven Jobs of one automatic lifecycle, each starting
// when the one before it completed.
func serializedJobs() []batchv1.Job {
	operations := []string{"resolve", "verify", "observe", "plan", "apply", "observe", "plan"}
	jobs := make([]batchv1.Job, 0, len(operations))
	for index, operation := range operations {
		start := sequenceStart.Add(time.Duration(index) * 10 * time.Second)
		job := batchv1.Job{}
		job.Name = "job-" + string(rune('a'+index))
		job.UID = types.UID("uid-" + string(rune('1'+index)))
		job.CreationTimestamp = metav1.NewTime(start)
		job.Labels = map[string]string{labelOperation: operation}
		job.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: ptahSchemaAPIVersion, Kind: "PtahSchema", Name: externalSchema, UID: externalSchemaUID,
			Controller: ptr.To(true),
		}}
		job.Spec.BackoffLimit = ptr.To[int32](0)
		job.Spec.PodReplacementPolicy = ptr.To(batchv1.Failed)
		job.Status.StartTime = ptr.To(metav1.NewTime(start))
		job.Status.CompletionTime = ptr.To(metav1.NewTime(start.Add(10 * time.Second)))
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		jobs = append(jobs, job)
	}
	return jobs
}

func sortedUIDs(jobs []batchv1.Job) []string {
	var uids []string
	for _, job := range jobs {
		uids = append(uids, string(job.UID))
	}
	slices.Sort(uids)
	return uids
}

func TestAutomaticJobSequence(t *testing.T) {
	t.Parallel()
	jobs := serializedJobs()
	// The archive hands the Jobs over sorted by UID, not by when they ran, so
	// the order below has to come from their start times.
	shuffled := slices.Clone(jobs)
	slices.Reverse(shuffled)
	sequence, err := automaticJobSequence(shuffled, checkpoint{"uid-0"}, sortedUIDs(jobs), externalSchema, externalSchemaUID)
	if err != nil {
		t.Fatalf("one serialized lifecycle was refused: %v", err)
	}
	if want := []string{"uid-1", "uid-2", "uid-3", "uid-4", "uid-5", "uid-6", "uid-7"}; !slices.Equal(sequence, want) {
		t.Fatalf("sequence = %v, want %v", sequence, want)
	}
	for _, test := range []struct {
		name     string
		mutate   func([]batchv1.Job) []batchv1.Job
		observed func([]string) []string
		before   checkpoint
	}{
		{name: "six Jobs", mutate: func(jobs []batchv1.Job) []batchv1.Job { return jobs[:6] }},
		{name: "a Job the checkpoint already held", before: checkpoint{"uid-3"}},
		{name: "the ledger holds another UID", observed: func(uids []string) []string {
			uids[0] = "uid-9"
			slices.Sort(uids)
			return uids
		}},
		{name: "the ledger holds six", observed: func(uids []string) []string { return uids[:6] }},
		{name: "two Applies", mutate: func(jobs []batchv1.Job) []batchv1.Job {
			jobs[6].Labels[labelOperation] = "apply"
			return jobs
		}},
		{name: "an Apply before its Plan completed", mutate: func(jobs []batchv1.Job) []batchv1.Job {
			jobs[4].Status.StartTime = ptr.To(metav1.NewTime(jobs[3].Status.CompletionTime.Add(-time.Second)))
			return jobs
		}},
		{name: "a failed Job", mutate: func(jobs []batchv1.Job) []batchv1.Job {
			jobs[2].Status.Conditions = append(jobs[2].Status.Conditions,
				batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue})
			return jobs
		}},
		{name: "another schema's Job", mutate: func(jobs []batchv1.Job) []batchv1.Job {
			jobs[1].OwnerReferences[0].UID = "other-schema-uid"
			return jobs
		}},
		{name: "a Job that retries", mutate: func(jobs []batchv1.Job) []batchv1.Job {
			jobs[5].Spec.BackoffLimit = ptr.To[int32](1)
			return jobs
		}},
		{name: "a Job that replaces a terminating Pod", mutate: func(jobs []batchv1.Job) []batchv1.Job {
			jobs[5].Spec.PodReplacementPolicy = ptr.To(batchv1.TerminatingOrFailed)
			return jobs
		}},
		{name: "a Job with no completion time", mutate: func(jobs []batchv1.Job) []batchv1.Job {
			jobs[6].Status.CompletionTime = nil
			return jobs
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutated := serializedJobs()
			if test.mutate != nil {
				mutated = test.mutate(mutated)
			}
			observed := sortedUIDs(serializedJobs())
			if test.observed != nil {
				observed = test.observed(observed)
			}
			before := test.before
			if before == nil {
				before = checkpoint{}
			}
			if _, err := automaticJobSequence(mutated, before, observed, externalSchema, externalSchemaUID); err == nil {
				t.Fatal("the mutated history was accepted")
			}
		})
	}
}

func TestAutomaticResultPredicates(t *testing.T) {
	t.Parallel()
	realm, target, report, content := externalDigest('2'), externalDigest('4'), externalDigest('5'), externalDigest('7')
	resolve := runner.Result{
		ResolvedReference: externalReference, ResolvedDigest: externalDigest('1'),
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 512,
	}
	verify := runner.Result{
		ResolvedDigest: externalDigest('1'), VerificationPolicyDigest: externalDigest('3'),
		ObservedArtifactType: schemaArtifactType,
	}
	initialObserve := runner.Result{
		ChildExitCode: 1, ObservedDialect: "postgres", ObservedDrift: true, DriftFindingCount: 2,
		HighestDriftSeverity: "safe", DriftReportDigest: report, CoordinationDigest: realm, TargetIdentityDigest: target,
	}
	initialPlan := runner.Result{
		PlanOutcome: runner.PlanOutcomeChanges, Stdout: "sealed", PlanContentDigest: content,
		CoordinationDigest: realm, TargetIdentityDigest: target,
	}
	apply := runner.Result{
		PlanContentDigest: content, CoordinationDigest: realm, TargetIdentityDigest: target, MutationStarted: true,
	}
	finalObserve := runner.Result{
		ObservedDialect: "postgres", CoordinationDigest: realm, TargetIdentityDigest: target, DriftReportDigest: report,
	}
	finalPlan := runner.Result{PlanOutcome: runner.PlanOutcomeNoChanges, CoordinationDigest: realm, TargetIdentityDigest: target}
	checks := map[string]func(runner.Result) error{
		"resolve": func(r runner.Result) error { return automaticResolveResult(r, externalReference, externalDigest('1')) },
		"verify": func(r runner.Result) error {
			return automaticVerifyResult(r, externalDigest('1'), externalDigest('3'))
		},
		"initial observe": func(r runner.Result) error { return automaticInitialObserveResult(r, realm, target) },
		"initial plan":    func(r runner.Result) error { return automaticInitialPlanResult(r, realm, target) },
		"apply":           func(r runner.Result) error { return automaticApplyResult(r, content, realm, target) },
		"final observe":   func(r runner.Result) error { return automaticFinalObserveResult(r, realm, target, report) },
		"final plan":      func(r runner.Result) error { return automaticFinalPlanResult(r, realm, target) },
	}
	accepted := map[string]runner.Result{
		"resolve": resolve, "verify": verify, "initial observe": initialObserve, "initial plan": initialPlan,
		"apply": apply, "final observe": finalObserve, "final plan": finalPlan,
	}
	for name, result := range accepted {
		if err := checks[name](result); err != nil {
			t.Errorf("%s: a correct result was refused: %v", name, err)
		}
	}
	for _, test := range []struct {
		name, check string
		mutate      func(*runner.Result)
	}{
		{"resolve by tag", "resolve", func(r *runner.Result) { r.ResolvedReference = "oci://registry/schemas/x:stable" }},
		{"resolve of nothing", "resolve", func(r *runner.Result) { r.ResolvedSize = 0 }},
		{"resolve that mutated", "resolve", func(r *runner.Result) { r.MutationStarted = true }},
		{"verify under another policy", "verify", func(r *runner.Result) { r.VerificationPolicyDigest = externalDigest('9') }},
		{"verify with a requirement unmet", "verify", func(r *runner.Result) {
			r.VerificationRequirements = []string{"require_digest_pin"}
		}},
		{"verify of another artifact type", "verify", func(r *runner.Result) { r.ObservedArtifactType = "application/json" }},
		{"observe with no drift", "initial observe", func(r *runner.Result) { r.ObservedDrift = false }},
		{"observe of MySQL", "initial observe", func(r *runner.Result) { r.ObservedDialect = "mysql" }},
		{"observe that failed", "initial observe", func(r *runner.Result) { r.ChildExitCode = 2 }},
		{"observe in another realm", "initial observe", func(r *runner.Result) { r.CoordinationDigest = externalDigest('9') }},
		{"observe unsure it mutated nothing", "initial observe", func(r *runner.Result) { r.Uncertain = true }},
		{"plan of no changes", "initial plan", func(r *runner.Result) { r.PlanOutcome = runner.PlanOutcomeNoChanges }},
		{"plan with nothing sealed", "initial plan", func(r *runner.Result) { r.Stdout = "" }},
		{"plan on another target", "initial plan", func(r *runner.Result) { r.TargetIdentityDigest = externalDigest('9') }},
		{"apply that started nothing", "apply", func(r *runner.Result) { r.MutationStarted = false }},
		{"apply of another plan", "apply", func(r *runner.Result) { r.PlanContentDigest = externalDigest('9') }},
		{"apply unsure of its outcome", "apply", func(r *runner.Result) { r.Uncertain = true }},
		{"apply that planned", "apply", func(r *runner.Result) { r.PlanOutcome = runner.PlanOutcomeChanges }},
		{"converged observe with drift", "final observe", func(r *runner.Result) { r.ObservedDrift = true }},
		{"converged observe with a finding", "final observe", func(r *runner.Result) { r.DriftFindingCount = 1 }},
		{"converged observe of another report", "final observe", func(r *runner.Result) {
			r.DriftReportDigest = externalDigest('9')
		}},
		{"converged plan of changes", "final plan", func(r *runner.Result) { r.PlanOutcome = runner.PlanOutcomeChanges }},
		{"converged plan with content", "final plan", func(r *runner.Result) { r.PlanContentDigest = content }},
		{"converged plan with an error", "final plan", func(r *runner.Result) {
			r.Error = &runner.ResultError{Code: "invalid_target"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := accepted[test.check]
			test.mutate(&result)
			if err := checks[test.check](result); err == nil {
				t.Fatal("the mutated result was accepted")
			}
		})
	}
}

func safeAdditiveDocument() planDocument {
	return planDocument{
		FormatVersion: 1, Dialect: "postgres", FromFingerprint: externalDigest('a'), ToFingerprint: externalDigest('b'),
		Destructive: ptr.To(false),
		Statements: []planStatement{
			{SQL: "CREATE TABLE public.e2e_widgets (id bigint NOT NULL, name text NOT NULL)", Severity: "safe"},
			{SQL: "ALTER TABLE public.e2e_widgets ADD PRIMARY KEY (id)", Severity: "safe"},
		},
	}
}

func TestAutomaticPlanDocument(t *testing.T) {
	t.Parallel()
	if err := automaticPlanDocument(safeAdditiveDocument()); err != nil {
		t.Fatalf("a safe additive document was refused: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*planDocument)
	}{
		{"another format", func(p *planDocument) { p.FormatVersion = 2 }},
		{"MySQL", func(p *planDocument) { p.Dialect = "mysql" }},
		{"destructive", func(p *planDocument) { p.Destructive = ptr.To(true) }},
		{"no destructive key", func(p *planDocument) { p.Destructive = nil }},
		{"no movement", func(p *planDocument) { p.ToFingerprint = p.FromFingerprint }},
		{"no statement", func(p *planDocument) { p.Statements = nil }},
		{"a warning", func(p *planDocument) { p.Statements[1].Severity = "warning" }},
		{"a drop", func(p *planDocument) { p.Statements[1].SQL = "drop table public.e2e_old" }},
		{"a truncate", func(p *planDocument) { p.Statements[1].SQL = "TRUNCATE public.e2e_widgets" }},
		{"a delete", func(p *planDocument) { p.Statements[1].SQL = "DELETE FROM public.e2e_widgets" }},
		{"no table created", func(p *planDocument) { p.Statements[0].SQL = "CREATE INDEX e2e_idx ON public.e2e_widgets (name)" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			document := safeAdditiveDocument()
			test.mutate(&document)
			if err := automaticPlanDocument(document); err == nil {
				t.Fatal("the mutated document was accepted")
			}
		})
	}
}

func appliedExternalPlan() *ptahv1alpha1.PtahSchemaPlan {
	schema := convergedExternalSchema()
	document := safeAdditiveDocument()
	want := externalExpectation()
	plan := &ptahv1alpha1.PtahSchemaPlan{}
	plan.Name, plan.UID = "plan-a", "plan-uid"
	plan.Spec = ptahv1alpha1.PtahSchemaPlanSpec{
		ContractVersion: 3, ArtifactDigest: want.digest, Fingerprint: schema.Status.Applied.PlanFingerprint,
		ContentDigest: externalDigest('7'), CoordinationDigest: want.coordinationDigest,
		TargetIdentityDigest: schema.Status.Target.IdentityDigest, ActualStateFingerprint: document.FromFingerprint,
		DesiredStateFingerprint: document.ToFingerprint, StatementCount: int32(len(document.Statements)),
		Chunks:             []ptahv1alpha1.PlanChunkReference{{Name: "plan-a-0", Index: 0, Size: 64}},
		ExecutionBindingID: externalEpoch, ControllerImage: want.controller.image,
		ControllerRevision: want.controller.revision, ControllerStateVersion: 4,
		PtahVersion: schema.Status.ExecutionBinding.PtahVersion, ExecutorImage: schema.Status.ExecutionBinding.ExecutorImage,
		RunnerImage: want.runnerImage, RunnerProtocolVersion: schema.Status.ExecutionBinding.RunnerProtocolVersion,
		Dialect: "postgres",
	}
	plan.Status.Conditions = []metav1.Condition{condition("Ready", metav1.ConditionTrue, "Published", "")}
	return plan
}

func TestAutomaticPlanBound(t *testing.T) {
	t.Parallel()
	schema, document, content := convergedExternalSchema(), safeAdditiveDocument(), externalDigest('7')
	if err := automaticPlanBound(appliedExternalPlan(), schema, document, content, externalExpectation()); err != nil {
		t.Fatalf("the applied plan was refused: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchemaPlan)
	}{
		{"another contract", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ContractVersion = 2 }},
		{"another fingerprint", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Fingerprint = externalDigest('9') }},
		{"another content", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ContentDigest = externalDigest('9') }},
		{"another realm", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.CoordinationDigest = externalDigest('9') }},
		{"another starting state", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ActualStateFingerprint = externalDigest('9') }},
		{"destructive", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Destructive = true }},
		{"another statement count", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.StatementCount = 1 }},
		{"no chunk", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Chunks = nil }},
		{"another binding", func(p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.ExecutionBindingID = "v1-ffffffffffffffffffffffffffffffff"
		}},
		{"another runner", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.RunnerImage = "runner:latest" }},
		{"another executor", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ExecutorImage = "ptah:latest" }},
		{"not ready", func(p *ptahv1alpha1.PtahSchemaPlan) {
			p.Status.Conditions = []metav1.Condition{condition("Ready", metav1.ConditionFalse, "Publishing", "")}
		}},
		{"the coordination key on the plan", func(p *ptahv1alpha1.PtahSchemaPlan) {
			p.Status.Conditions[0].Message = externalKey
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			plan := appliedExternalPlan()
			test.mutate(plan)
			if err := automaticPlanBound(plan, convergedExternalSchema(), safeAdditiveDocument(), externalDigest('7'),
				externalExpectation()); err == nil {
				t.Fatal("the mutated plan was accepted")
			}
		})
	}
}

func externalApplyWorkload() applyWorkload {
	return applyWorkload{
		schema: externalSchema, jobName: "apply-job", jobUID: "apply-uid", podName: "apply-job-x", podUID: "pod-uid",
		planFingerprint: externalDigest('6'), contentDigest: externalDigest('7'), executionBinding: externalEpoch,
		executorImage: "ghcr.io/stokaro/ptah@" + externalDigest('e'), runnerImage: "ghcr.io/stokaro/ptah-runner@" + externalDigest('r'),
	}
}

func applyRuntimeSpec(want applyWorkload) corev1.PodSpec {
	return corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "install-runner", Image: want.runnerImage}},
		Containers: []corev1.Container{{
			Name: "ptah", Image: want.executorImage,
			Env: []corev1.EnvVar{
				{Name: "PTAH_DB_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: externalPGSecret}, Key: "url",
				}}},
				{Name: "PTAH_EXPECTED_DATABASE_ENGINE", Value: "PostgreSQL"},
			},
		}},
	}
}

func applyJobAndPod() (*batchv1.Job, *corev1.Pod) {
	want := externalApplyWorkload()
	annotations := func() map[string]string {
		return map[string]string{
			"operator.ptah.run/plan-fingerprint":     want.planFingerprint,
			"operator.ptah.run/plan-content-digest":  want.contentDigest,
			"operator.ptah.run/execution-binding-id": want.executionBinding,
		}
	}
	job := &batchv1.Job{}
	job.Name, job.UID = want.jobName, types.UID(want.jobUID)
	job.Labels = map[string]string{labelSchema: want.schema, labelOperation: "apply"}
	job.Annotations = annotations()
	job.Spec.Template.Annotations = annotations()
	job.Spec.Template.Spec = applyRuntimeSpec(want)
	pod := &corev1.Pod{}
	pod.Name, pod.UID = want.podName, types.UID(want.podUID)
	pod.Annotations = annotations()
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1", Kind: "Job", Name: want.jobName, UID: types.UID(want.jobUID), Controller: ptr.To(true),
	}}
	pod.Spec = applyRuntimeSpec(want)
	return job, pod
}

func TestAutomaticApplyWorkload(t *testing.T) {
	t.Parallel()
	job, pod := applyJobAndPod()
	if err := automaticApplyWorkload(job, pod, externalApplyWorkload()); err != nil {
		t.Fatalf("the Apply workload was refused: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*batchv1.Job, *corev1.Pod)
	}{
		{"another Job", func(j *batchv1.Job, _ *corev1.Pod) { j.UID = "other-uid" }},
		{"a Plan Job", func(j *batchv1.Job, _ *corev1.Pod) { j.Labels[labelOperation] = "plan" }},
		{"the template without the plan", func(j *batchv1.Job, _ *corev1.Pod) {
			delete(j.Spec.Template.Annotations, "operator.ptah.run/plan-content-digest")
		}},
		{"the Job under another binding", func(j *batchv1.Job, _ *corev1.Pod) {
			j.Annotations["operator.ptah.run/execution-binding-id"] = "v1-ffffffffffffffffffffffffffffffff"
		}},
		{"a second executor", func(j *batchv1.Job, _ *corev1.Pod) {
			j.Spec.Template.Spec.Containers = append(j.Spec.Template.Spec.Containers, j.Spec.Template.Spec.Containers[0])
		}},
		{"another runner", func(j *batchv1.Job, _ *corev1.Pod) {
			j.Spec.Template.Spec.InitContainers[0].Image = "runner:latest"
		}},
		{"the engine read from a Secret", func(j *batchv1.Job, _ *corev1.Pod) {
			j.Spec.Template.Spec.Containers[0].Env[1] = corev1.EnvVar{
				Name: "PTAH_EXPECTED_DATABASE_ENGINE", ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{Key: "engine"},
				},
			}
		}},
		{"the engine given twice", func(j *batchv1.Job, _ *corev1.Pod) {
			j.Spec.Template.Spec.Containers[0].Env = append(j.Spec.Template.Spec.Containers[0].Env,
				corev1.EnvVar{Name: "PTAH_EXPECTED_DATABASE_ENGINE", Value: "PostgreSQL"})
		}},
		{"another Pod", func(_ *batchv1.Job, p *corev1.Pod) { p.UID = "other-pod" }},
		{"a Pod another Job owns", func(_ *batchv1.Job, p *corev1.Pod) { p.OwnerReferences[0].UID = "other-uid" }},
		{"a Pod the Job does not control", func(_ *batchv1.Job, p *corev1.Pod) { p.OwnerReferences[0].Controller = nil }},
		{"the Pod without the plan", func(_ *batchv1.Job, p *corev1.Pod) {
			delete(p.Annotations, "operator.ptah.run/plan-fingerprint")
		}},
		{"the Pod running another executor", func(_ *batchv1.Job, p *corev1.Pod) { p.Spec.Containers[0].Image = "ptah:latest" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			job, pod := applyJobAndPod()
			test.mutate(job, pod)
			if err := automaticApplyWorkload(job, pod, externalApplyWorkload()); err == nil {
				t.Fatal("the mutated workload was accepted")
			}
		})
	}
}

// The hold is the proof the privileged row exists for, and the shell pinned
// its two comparisons in the source: a refresh dated before the deadline, or a
// deadline closer than one interval to the plan, is a hold that measured less
// than it claims.
func TestPrivilegedHoldMeasured(t *testing.T) {
	t.Parallel()
	planCompleted := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	deadline := planCompleted.Add(90 * time.Second)
	refresh := func(created time.Time) observedJob {
		return observedJob{UID: "refresh", Name: "refresh", Created: created.UTC().Format(time.RFC3339), Operation: "resolve"}
	}
	if err := privilegedHoldMeasured([]observedJob{refresh(deadline), refresh(deadline.Add(time.Minute))},
		deadline, planCompleted, 90*time.Second); err != nil {
		t.Fatalf("a hold through the whole deadline was refused: %v", err)
	}
	for _, test := range []struct {
		name          string
		refreshes     []observedJob
		deadline      time.Time
		planCompleted time.Time
	}{
		{name: "no refresh", deadline: deadline, planCompleted: planCompleted},
		{name: "a refresh before the deadline", refreshes: []observedJob{refresh(deadline.Add(-time.Second))},
			deadline: deadline, planCompleted: planCompleted},
		{name: "one early refresh among late ones",
			refreshes: []observedJob{refresh(deadline.Add(time.Second)), refresh(deadline.Add(-time.Second))},
			deadline:  deadline, planCompleted: planCompleted},
		{name: "a deadline closer than the interval", refreshes: []observedJob{refresh(deadline)},
			deadline: deadline, planCompleted: planCompleted.Add(time.Second)},
		{name: "a refresh with no creation time", refreshes: []observedJob{{UID: "refresh"}},
			deadline: deadline, planCompleted: planCompleted},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := privilegedHoldMeasured(test.refreshes, test.deadline, test.planCompleted, 90*time.Second); err == nil {
				t.Fatal("a hold that measured less was accepted")
			}
		})
	}
}

func TestLatestCompletion(t *testing.T) {
	t.Parallel()
	completed := func(at time.Time) *batchv1.Job {
		job := &batchv1.Job{}
		job.Status.CompletionTime = ptr.To(metav1.NewTime(at))
		return job
	}
	early, late := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 1, 0, 5, 0, 0, time.UTC)
	latest, err := latestCompletion([]*batchv1.Job{completed(late), completed(early)})
	if err != nil || !latest.Equal(late) {
		t.Fatalf("latestCompletion() = %s, %v, want %s", latest, err, late)
	}
	if _, err := latestCompletion(nil); err == nil {
		t.Error("no Job was read as a completion")
	}
	if _, err := latestCompletion([]*batchv1.Job{completed(late), {}}); err == nil {
		t.Error("a Job with no completion time was passed over")
	}
}

func heldSchema(generation int64, planUID, fingerprint string, next time.Time) *ptahv1alpha1.PtahSchema {
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Generation = generation
	schema.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{UID: types.UID(planUID), Fingerprint: fingerprint}
	schema.Status.NextReconciliationTime = ptr.To(metav1.NewTime(next))
	return schema
}

func TestSamePlanHeld(t *testing.T) {
	t.Parallel()
	deadline := time.Date(2026, 9, 1, 0, 1, 30, 0, time.UTC)
	after := samePlanHeldAfter("plan-uid", externalDigest('6'), deadline)
	if !after(heldSchema(4, "plan-uid", externalDigest('6'), deadline.Add(90*time.Second))) {
		t.Error("the same plan held past the deadline was not matched")
	}
	for name, schema := range map[string]*ptahv1alpha1.PtahSchema{
		"the reading the deadline came from": heldSchema(4, "plan-uid", externalDigest('6'), deadline),
		"another plan":                       heldSchema(4, "other-uid", externalDigest('6'), deadline.Add(time.Minute)),
		"another fingerprint":                heldSchema(4, "plan-uid", externalDigest('9'), deadline.Add(time.Minute)),
		"no plan":                            {},
	} {
		if after(schema) {
			t.Errorf("%s was matched as the plan held after its deadline", name)
		}
	}
	atGeneration := samePlanHeldAtGeneration("plan-uid", externalDigest('6'), 5)
	if !atGeneration(heldSchema(5, "plan-uid", externalDigest('6'), deadline)) {
		t.Error("the same plan at the generation was not matched")
	}
	if atGeneration(heldSchema(4, "plan-uid", externalDigest('6'), deadline)) {
		t.Error("a reading from before the spec change was matched")
	}
}

func TestApprovalAbsenceAndPrivilegeEvents(t *testing.T) {
	t.Parallel()
	approval := func(name string, uid types.UID) ptahv1alpha1.PtahSchemaApproval {
		item := ptahv1alpha1.PtahSchemaApproval{}
		item.Spec.SchemaRef = ptahv1alpha1.ImmutableObjectReference{Name: name, UID: uid}
		return item
	}
	approvals := []ptahv1alpha1.PtahSchemaApproval{approval(externalSchema, "earlier-uid"), approval("other", externalSchemaUID)}
	if approvalsFor(approvals, externalSchema, externalSchemaUID) != 0 {
		t.Error("an approval of another schema, or of an earlier one by the name, was counted")
	}
	if approvalsFor(append(approvals, approval(externalSchema, externalSchemaUID)), externalSchema, externalSchemaUID) != 1 {
		t.Error("the schema's own approval was not counted")
	}
	event := func(reason, message string, uid types.UID) corev1.Event {
		return corev1.Event{
			InvolvedObject: corev1.ObjectReference{Kind: "PtahSchema", Name: externalSchema, UID: uid},
			Reason:         reason, Message: message,
		}
	}
	events := []corev1.Event{event("PlanPublished", "", externalSchemaUID), event("ApprovalAccepted", "", "earlier-uid")}
	if approvalTransitions(events, externalSchema, externalSchemaUID) != 0 {
		t.Error("an unrelated Event, or another schema's approval, was counted as a transition")
	}
	for _, reason := range []string{"ApprovalRequired", "ApprovalAccepted"} {
		if approvalTransitions(append(events, event(reason, "", externalSchemaUID)), externalSchema, externalSchemaUID) != 1 {
			t.Errorf("%s was not counted as a transition", reason)
		}
	}
	named := event("ApprovalRequired", "the plan changes privileges (SecurityDefiner) and waits", externalSchemaUID)
	if privilegeEvents([]corev1.Event{named}, externalSchemaUID, "SecurityDefiner") != 1 {
		t.Error("the Event naming the kinds was not counted")
	}
	for name, unnamed := range map[string]corev1.Event{
		"no kinds":       event("ApprovalRequired", "the plan waits for a person", externalSchemaUID),
		"another kind":   event("ApprovalRequired", "the plan changes privileges (Grant)", externalSchemaUID),
		"another schema": event("ApprovalRequired", named.Message, "other-uid"),
		"another reason": event("PlanPublished", named.Message, externalSchemaUID),
	} {
		if privilegeEvents([]corev1.Event{unnamed}, externalSchemaUID, "SecurityDefiner") != 0 {
			t.Errorf("an Event with %s was counted", name)
		}
	}
}

func TestPrivilegedPlanRecorded(t *testing.T) {
	t.Parallel()
	recorded := func() *ptahv1alpha1.PtahSchemaPlan {
		plan := &ptahv1alpha1.PtahSchemaPlan{}
		plan.UID = "plan-uid"
		plan.Spec.Fingerprint, plan.Spec.ArtifactDigest = externalDigest('6'), externalDigest('1')
		plan.Spec.PrivilegeChanges = []ptahv1alpha1.PrivilegeChange{"SecurityDefiner"}
		plan.Status.Conditions = []metav1.Condition{condition("Ready", metav1.ConditionTrue, "Published", "")}
		return plan
	}
	kinds := []ptahv1alpha1.PrivilegeChange{"SecurityDefiner"}
	if err := privilegedPlanRecorded(recorded(), "plan-uid", externalDigest('6'), externalDigest('1'), kinds); err != nil {
		t.Fatalf("the recorded plan was refused: %v", err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchemaPlan){
		"another plan":     func(p *ptahv1alpha1.PtahSchemaPlan) { p.UID = "other-uid" },
		"another artifact": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ArtifactDigest = externalDigest('9') },
		"destructive":      func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Destructive = true },
		"no kinds":         func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.PrivilegeChanges = nil },
		"another kind": func(p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.PrivilegeChanges = []ptahv1alpha1.PrivilegeChange{"Grant"}
		},
		"a second kind": func(p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.PrivilegeChanges = append(p.Spec.PrivilegeChanges, "Grant")
		},
		"not ready": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Status.Conditions = nil },
	} {
		plan := recorded()
		mutate(plan)
		if err := privilegedPlanRecorded(plan, "plan-uid", externalDigest('6'), externalDigest('1'), kinds); err == nil {
			t.Errorf("a plan with %s was accepted", name)
		}
	}
}

// A gate that gave up prints its last reading. The reading has to parse, and
// it has to carry nothing but names, digests, states and reasons: a condition
// message is where a plan's text could reach the log.
func TestGateReadings(t *testing.T) {
	t.Parallel()
	for name, reading := range map[string]func(*ptahv1alpha1.PtahSchema) string{
		"privileged": privilegedGateReading, "grant-only": grantGateReading,
	} {
		if got := reading(nil); got != noReadableSchema {
			t.Errorf("%s: a missing reading printed %q", name, got)
		}
		schema := heldSchema(4, "plan-uid", externalDigest('6'), sequenceStart)
		schema.Status.Plan.Approval = &ptahv1alpha1.ConsumedApprovalStatus{Name: "approval"}
		schema.Status.Conditions = []metav1.Condition{
			condition("ApprovalRequired", metav1.ConditionTrue, "PrivilegeChanges", "GRANT SELECT ON e2e_widgets TO PUBLIC"),
			condition("DriftDetected", metav1.ConditionTrue, "ScopedChanges", "e2e_widgets"),
		}
		got := reading(schema)
		var parsed map[string]any
		if err := json.Unmarshal([]byte(got), &parsed); err != nil {
			t.Fatalf("%s: the reading %q does not parse: %v", name, got, err)
		}
		if strings.Contains(got, "e2e_widgets") {
			t.Errorf("%s: the reading carries a condition message: %s", name, got)
		}
		plan, _ := parsed["plan"].(map[string]any)
		if plan["approved"] != true || plan["uid"] != "plan-uid" {
			t.Errorf("%s: the reading lost the plan it held: %s", name, got)
		}
	}
}
