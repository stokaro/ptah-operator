package admissionpolicy_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
	"github.com/stokaro/ptah-operator/test/envtest/internal/harness"
)

// The tenant is where a person declares databases: one PtahSchema and one
// PtahMigration, each with the execution binding the manager records before
// it dispatches anything. There is no controller here, so the suite writes
// that status itself.
const (
	tenantNamespace    = "team-a"
	executionBindingID = "v1-0123456789abcdef0123456789abcdef"
	controllerRevision = "envtest-revision"
)

var (
	tenantSchema    *operatorv1alpha1.PtahSchema
	tenantMigration *operatorv1alpha1.PtahMigration
	// The unattended pair applies with no approval: an administrator set them
	// to Always, and the rows on the apply-policy guard edit them.
	tenantAlwaysSchema    *operatorv1alpha1.PtahSchema
	tenantAlwaysMigration *operatorv1alpha1.PtahMigration
)

// declared is a desired-state resource written the way a person writes it, as
// the fields they set: a typed object would send every unset duration as "0s",
// which the CRD refuses. apply is the policy the person selects, or "" for one
// who leaves the field to its default.
func declared(kind, name string, apply operatorv1alpha1.ApplyPolicy) *unstructured.Unstructured {
	source := "desired"
	if kind == "PtahMigration" {
		source = "artifact"
	}
	spec := map[string]any{
		"target": map[string]any{
			"engine":          "PostgreSQL",
			"coordinationKey": "production/" + name + "-primary",
			"urlFrom":         map[string]any{"name": name + "-database", "key": "url"},
		},
		source: map[string]any{
			"ociRef":                 "oci://registry.example/acme/" + name + ":1.4.0",
			"verificationPolicyFrom": map[string]any{"name": "ptah-verification-policy", "key": "policy.yaml"},
		},
		"execution": map[string]any{"serviceAccountName": "ptah-execution", "activeDeadlineSeconds": int64(900)},
	}
	if apply != "" {
		spec["policy"] = map[string]any{"apply": string(apply)}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": operatorv1alpha1.GroupVersion.String(),
		"kind":       kind,
		"metadata":   map[string]any{"namespace": tenantNamespace, "name": name},
		"spec":       spec,
	}}
}

func digest(character string) string { return "sha256:" + strings.Repeat(character, 64) }

// managerBuilder is the manager as the chart configures it: its own image,
// the release's execution images and Ptah version, and the state version it
// compiles.
func managerBuilder() workload.Builder {
	return workload.Builder{
		ExecutorImage:          harness.ExecutorImage,
		RunnerImage:            harness.RunnerImage,
		PtahVersion:            harness.PtahVersion,
		ControllerImage:        harness.ManagerImage,
		ControllerRevision:     controllerRevision,
		ControllerStateVersion: controllerstate.CurrentVersion,
	}
}

func executionBinding() *operatorv1alpha1.ExecutionBindingStatus {
	builder := managerBuilder()
	return &operatorv1alpha1.ExecutionBindingStatus{
		Epoch:                  executionBindingID,
		ControllerStateVersion: builder.ControllerStateVersion,
		PtahVersion:            builder.PtahVersion,
		ExecutorImage:          builder.ExecutorImage,
		RunnerProtocolVersion:  int32(runner.ProtocolVersion),
	}
}

func setupTenant(ctx context.Context) error {
	admin := env.Admin
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNamespace}}); err != nil {
		return fmt.Errorf("create the tenant namespace: %w", err)
	}
	schema, migration := &operatorv1alpha1.PtahSchema{}, &operatorv1alpha1.PtahMigration{}
	alwaysSchema, alwaysMigration := &operatorv1alpha1.PtahSchema{}, &operatorv1alpha1.PtahMigration{}
	// The administrator is the one identity the installed apply-policy guard
	// exempts, so it is the one that may declare the unattended pair.
	for _, tenant := range []struct {
		object *unstructured.Unstructured
		typed  client.Object
	}{
		{declared("PtahSchema", "orders", ""), schema},
		{declared("PtahMigration", "ledger", ""), migration},
		{declared("PtahSchema", "orders-unattended", operatorv1alpha1.ApplyPolicyAlways), alwaysSchema},
		{declared("PtahMigration", "ledger-unattended", operatorv1alpha1.ApplyPolicyAlways), alwaysMigration},
	} {
		if err := admin.Create(ctx, tenant.object); err != nil {
			return fmt.Errorf("create the tenant %s %s: %w", tenant.object.GetKind(), tenant.object.GetName(), err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(tenant.object), tenant.typed); err != nil {
			return fmt.Errorf("read the tenant %s %s back: %w", tenant.object.GetKind(), tenant.object.GetName(), err)
		}
	}
	schema.Status.ExecutionBinding = executionBinding()
	if err := admin.Status().Update(ctx, schema); err != nil {
		return fmt.Errorf("record the tenant PtahSchema's execution binding: %w", err)
	}
	migration.Status.ExecutionBinding = executionBinding()
	if err := admin.Status().Update(ctx, migration); err != nil {
		return fmt.Errorf("record the tenant PtahMigration's execution binding: %w", err)
	}
	tenantSchema, tenantMigration = schema.DeepCopy(), migration.DeepCopy()
	tenantAlwaysSchema, tenantAlwaysMigration = alwaysSchema.DeepCopy(), alwaysMigration.DeepCopy()
	return nil
}

// resolveJob is the Job the manager dispatches to resolve the tenant schema's
// artifact, built by the manager's own builder from the stored resource.
func resolveJob() (*batchv1.Job, error) {
	return resolveJobBuiltBy(managerBuilder())
}

// resolveJobBuiltBy is the same Job built by builder, which a row sets to
// another manager release. The builder refuses a binding recorded at another
// controller-state version, so the copy it builds from records the builder's.
func resolveJobBuiltBy(builder workload.Builder) (*batchv1.Job, error) {
	schema := tenantSchema.DeepCopy()
	schema.Status.ExecutionBinding.ControllerStateVersion = builder.ControllerStateVersion
	operation := operatorv1alpha1.ActiveOperationStatus{
		Type:               operatorv1alpha1.OperationResolve,
		ID:                 "operation-01",
		InputFingerprint:   digest("a"),
		ExecutionBindingID: executionBindingID,
		StartedAt:          metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
		Attempt:            1,
		AdmissionSnapshot:  &operatorv1alpha1.PodAdmissionSnapshot{Digest: digest("b"), TemplateDigest: digest("c")},
	}
	return builder.Build(schema, operation, nil)
}

// resolveJobDeclaring is resolveJob with spec.execution.podMetadata declared
// on the schema the builder builds from.
func resolveJobDeclaring(metadata *operatorv1alpha1.PodMetadataSpec) (*batchv1.Job, error) {
	schema := tenantSchema.DeepCopy()
	schema.Spec.Execution.PodMetadata = metadata
	operation := operatorv1alpha1.ActiveOperationStatus{
		Type:               operatorv1alpha1.OperationResolve,
		ID:                 "operation-01",
		InputFingerprint:   digest("a"),
		ExecutionBindingID: executionBindingID,
		StartedAt:          metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
		Attempt:            1,
		AdmissionSnapshot:  &operatorv1alpha1.PodAdmissionSnapshot{Digest: digest("b"), TemplateDigest: digest("c")},
	}
	return managerBuilder().Build(schema, operation, nil)
}

// migrationResolveJob is the migration family's Job for the same step.
func migrationResolveJob() (*batchv1.Job, error) {
	migration := tenantMigration.DeepCopy()
	operation := operatorv1alpha1.MigrationOperationStatus{
		Type:               operatorv1alpha1.MigrationOperationResolve,
		ID:                 digest("8"),
		InputFingerprint:   digest("a"),
		ExecutionBindingID: executionBindingID,
		Attempt:            1,
		StartedAt:          metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
		CoordinationDigest: digest("6"),
		AdmissionSnapshot:  &operatorv1alpha1.PodAdmissionSnapshot{Digest: digest("c"), TemplateDigest: digest("d")},
		Source: &operatorv1alpha1.OCIArtifactAccessBinding{
			ResolvedReference: "oci://registry.example/acme/ledger-migrations@" + digest("4"),
			Digest:            digest("4"),
		},
		Target: &operatorv1alpha1.DatabaseTargetBinding{
			Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, URLFrom: migration.Spec.Target.URLFrom,
		},
	}
	name, err := workload.NameForMigration(migration, operation)
	if err != nil {
		return nil, err
	}
	operation.JobName = name
	return managerBuilder().BuildMigration(migration, operation, nil)
}

// stored reads the current form of object into a fresh copy, so a row edits
// what the API server holds rather than what an earlier row assumed.
func stored[T client.Object](ctx context.Context, object T) (T, error) {
	fresh := object.DeepCopyObject().(T)
	if err := env.Admin.Get(ctx, client.ObjectKeyFromObject(object), fresh); err != nil {
		return fresh, fmt.Errorf("read %s back: %w", client.ObjectKeyFromObject(object), err)
	}
	return fresh, nil
}
