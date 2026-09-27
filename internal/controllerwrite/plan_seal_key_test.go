package controllerwrite_test

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planseal"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// twoReplicaBuilders returns two real workload.Builders sharing everything an
// execution binding checks, differing only in the process-generated Plan seal
// key: exactly the shape a two-replica install actually produces, since every
// replica generates its own key pair at startup (internal/planseal) and
// nothing else about them differs.
func twoReplicaBuilders(t *testing.T) (dispatcher, validating workload.Builder) {
	t.Helper()

	dispatcherKey, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	validatingKey, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	base := workload.Builder{
		ExecutorImage:          "example.test/executor@" + digest('2'),
		RunnerImage:            testRunnerImage,
		PtahVersion:            "v0.3.0",
		ControllerImage:        testControllerImage,
		ControllerRevision:     testControllerRevision,
		ControllerStateVersion: 1,
	}
	dispatcher = base
	dispatcher.PlanSealPublicKey = dispatcherKey.PublicKey()
	validating = base
	validating.PlanSealPublicKey = validatingKey.PublicKey()
	return dispatcher, validating
}

// realPlanSchemaFixture is a PtahSchema and Plan claim complete enough for a
// real workload.Builder.Build() to accept, unlike schemaFixture's minimal
// shape, which staticJobBuilder never inspects.
func realPlanSchemaFixture(t *testing.T) *operatorv1alpha1.PtahSchema {
	t.Helper()
	schema := schemaFixture(operatorv1alpha1.OperationPlan)
	schema.Spec.Target.URLFrom = corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url",
	}
	schema.Spec.Target.CoordinationKey = "tenant-a/orders-primary"
	schema.Status.Source.ResolvedReference = "oci://registry.test/team/schema@" + schema.Status.Source.Digest
	operation := schema.Status.ActiveOperation
	// schemaFixture stamps a JobUID for a Plan claim, since most of this
	// package's tests validate an already-dispatched Job. validateJobCreate
	// requires the opposite: no UID yet, since this Job is being created.
	operation.JobUID = ""
	operation.Target = &operatorv1alpha1.DatabaseTargetBinding{
		Engine: operatorv1alpha1.DatabaseEnginePostgreSQL,
		URLFrom: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url",
		},
	}
	operation.Source = &operatorv1alpha1.OCIArtifactAccessBinding{
		ResolvedReference: schema.Status.Source.ResolvedReference,
		Digest:            schema.Status.Source.Digest,
	}
	operation.LeaseDurationSeconds = 900
	operation.Attempt = 1
	jobName, err := workload.NameFor(schema, *operation)
	if err != nil {
		t.Fatalf("NameFor() error = %v", err)
	}
	operation.JobName = jobName
	return schema
}

// buildRealPlanJob builds the Plan Job schema's active operation authorizes
// with builder, resolves its Pod admission snapshot from the given
// ServiceAccount, and stamps the claim to match: everything a Plan Job
// created through the real dispatch path would carry.
func buildRealPlanJob(
	t *testing.T,
	builder workload.Builder,
	schema *operatorv1alpha1.PtahSchema,
	serviceAccount *corev1.ServiceAccount,
) *batchv1.Job {
	t.Helper()

	operation := schema.Status.ActiveOperation
	job, err := builder.Build(schema.DeepCopy(), *operation, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	job.TypeMeta = metav1.TypeMeta{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job"}
	templateDigest, err := podintent.DigestTemplate(&job.Spec.Template)
	if err != nil {
		t.Fatalf("DigestTemplate() error = %v", err)
	}
	snapshot := &operatorv1alpha1.PodAdmissionSnapshot{
		Version:        podintent.SnapshotVersion,
		TemplateDigest: templateDigest,
		ServiceAccount: operatorv1alpha1.ServiceAccountAdmissionSnapshot{
			Object: operatorv1alpha1.AdmissionObjectBinding{
				Name: serviceAccount.Name, UID: string(serviceAccount.UID), ResourceVersion: serviceAccount.ResourceVersion,
			},
		},
	}
	snapshotDigest, err := fingerprint.DigestCanonicalJSON(*snapshot)
	if err != nil {
		t.Fatalf("DigestCanonicalJSON() error = %v", err)
	}
	snapshot.Digest = snapshotDigest
	operation.AdmissionSnapshot = snapshot
	job.Annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshotDigest
	job.Spec.Template.Annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshotDigest
	return job
}

// TestPlanJobCreateAdmitsAndRefusesByTheClaimsRecordedSealKeyDigest
// reproduces the multi-replica install directly: one real Builder dispatches
// the Plan Job (recording its own key's digest on the claim, the way
// claimAt's dispatch boundary does), and a second real Builder, holding a
// different key, is the one validating the admission request -- the shape
// every default two-replica install actually runs, since the webhook Service
// routes to any ready replica and each generates its own key at startup.
//
// It must admit the Job the dispatching replica actually built and claimed,
// and refuse a Job whose key the claim's digest does not name -- a wrong key
// entirely, and the validating replica's own key substituted for the live
// one, which is what a validator that trusted its own rebuild over the live
// Job would let through.
func TestPlanJobCreateAdmitsAndRefusesByTheClaimsRecordedSealKeyDigest(t *testing.T) {
	t.Parallel()

	dispatcherBuilder, validatingBuilder := twoReplicaBuilders(t)
	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Namespace: "tenant-a", Name: "ptah-orders", UID: "service-account-uid", ResourceVersion: "1",
	}}

	for _, test := range []struct {
		name    string
		liveJob func(t *testing.T, schema *operatorv1alpha1.PtahSchema) *batchv1.Job
		allow   bool
	}{
		{
			name: "the key the dispatching replica actually sealed to",
			liveJob: func(t *testing.T, schema *operatorv1alpha1.PtahSchema) *batchv1.Job {
				return buildRealPlanJob(t, dispatcherBuilder, schema, serviceAccount)
			},
			allow: true,
		},
		{
			name: "a key nobody claimed sealing to",
			liveJob: func(t *testing.T, schema *operatorv1alpha1.PtahSchema) *batchv1.Job {
				job := buildRealPlanJob(t, dispatcherBuilder, schema, serviceAccount)
				stray, err := planseal.Generate()
				if err != nil {
					t.Fatalf("Generate() error = %v", err)
				}
				if !setJobSealKeyEnv(job, stray.PublicKey().Encode()) {
					t.Fatal("test fixture Job does not carry a seal key to tamper with")
				}
				return job
			},
		},
		{
			name: "the validating replica's own key substituted for the live one",
			liveJob: func(t *testing.T, schema *operatorv1alpha1.PtahSchema) *batchv1.Job {
				job := buildRealPlanJob(t, dispatcherBuilder, schema, serviceAccount)
				if !setJobSealKeyEnv(job, validatingBuilder.PlanSealPublicKey.Encode()) {
					t.Fatal("test fixture Job does not carry a seal key to tamper with")
				}
				return job
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema := realPlanSchemaFixture(t)
			operation := schema.Status.ActiveOperation
			// The dispatch boundary records the digest of the key the Job
			// about to be created is sealed to, before the Create itself.
			operation.PlanSealPublicKeyDigest = fingerprint.DigestBytes([]byte(dispatcherBuilder.PlanSealPublicKey.Encode()))
			live := test.liveJob(t, schema)
			operation.JobName = live.Name

			handler := handlerFixture(t, validatingBuilder, schema, serviceAccount)
			response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, live))
			if response.Allowed != test.allow {
				t.Fatalf("Handle() allowed = %t (%s), want %t", response.Allowed, responseMessage(response), test.allow)
			}
			if !test.allow && !strings.Contains(responseMessage(response), "seal key") {
				t.Fatalf("refusal = %q, want one naming the seal key", responseMessage(response))
			}
		})
	}
}

// setJobSealKeyEnv overwrites the Plan seal key environment variable on
// job's main container, and reports whether it found one to overwrite.
func setJobSealKeyEnv(job *batchv1.Job, value string) bool {
	found := false
	for _, containers := range [][]corev1.Container{
		job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers,
	} {
		for index := range containers {
			for envIndex := range containers[index].Env {
				if containers[index].Env[envIndex].Name == runner.EnvPlanSealPublicKey {
					containers[index].Env[envIndex].Value = value
					found = true
				}
			}
		}
	}
	return found
}
