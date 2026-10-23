package admissionpolicy_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
	"github.com/stokaro/ptah-operator/test/envtest/internal/harness"
)

// tenantSettings is an ordinary ConfigMap in the tenant namespace, and
// tenantSnapshot a source snapshot the manager wrote for the tenant schema.
// The rows on the snapshot delete guard delete them, as dry runs.
var tenantSettings, tenantSnapshot *corev1.ConfigMap

// snapshotFiles are the files a ConfigMap source of the tenant schema
// selects.
func snapshotFiles(content string) []sourcecontent.File {
	return []sourcecontent.File{{Path: "schema.sql", Data: []byte(content)}}
}

// sourceSnapshot is the snapshot the manager writes for the tenant schema's
// ConfigMap source, built by the manager's own function.
func sourceSnapshot(content string) (*corev1.ConfigMap, string, error) {
	owner := sourcesnapshot.Owner{Kind: "PtahSchema", Name: tenantSchema.Name, UID: tenantSchema.UID}
	return sourcesnapshot.Build(tenantNamespace, owner, snapshotFiles(content))
}

func setupSnapshots(ctx context.Context) error {
	settings := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "orders-settings"},
		Data:       map[string]string{"database": "orders"},
	}
	snapshot, _, err := sourceSnapshot("CREATE TABLE orders (id int);\n")
	if err != nil {
		return err
	}
	for _, object := range []*corev1.ConfigMap{settings, snapshot} {
		if err := env.Admin.Create(ctx, object); err != nil {
			return fmt.Errorf("create the tenant ConfigMap %s: %w", object.Name, err)
		}
	}
	tenantSettings, tenantSnapshot = settings.DeepCopy(), snapshot.DeepCopy()
	return nil
}

// dryRunDelete deletes the stored object, without removing it, so a row can
// be sent as often as the mutations need.
func dryRunDelete(object client.Object) func(context.Context, client.Client) error {
	return func(ctx context.Context, api client.Client) error {
		return api.Delete(ctx, object.DeepCopyObject().(client.Object), client.DryRunAll)
	}
}

// snapshotObserveJob is the Observe Job the manager dispatches for a schema
// whose source is a ConfigMap: the runner copies the checked snapshot into
// the source volume before the operation container starts.
func snapshotObserveJob(edit func(*batchv1.Job)) func() (client.Object, error) {
	return func() (client.Object, error) {
		_, contentDigest, err := sourceSnapshot("CREATE TABLE orders (id int);\n")
		if err != nil {
			return nil, err
		}
		schema := tenantSchema.DeepCopy()
		schema.Spec.Desired = operatorv1alpha1.SourceSpec{ConfigMap: &operatorv1alpha1.ConfigMapSourceSpec{
			Name: "orders-schema", Items: []operatorv1alpha1.ConfigMapSourceItem{{Key: "schema.sql", Path: "schema.sql"}},
		}}
		snapshot := &operatorv1alpha1.SourceSnapshotBinding{
			Name: tenantSnapshot.Name, UID: tenantSnapshot.UID, ContentDigest: contentDigest, SchemaFile: "schema.sql",
		}
		schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
			ConfigMap: &operatorv1alpha1.ConfigMapSourceRevision{Name: "orders-schema", UID: "configmap-uid", ResourceVersion: "1"},
			Snapshot:  snapshot,
		}
		operation := operatorv1alpha1.ActiveOperationStatus{
			Type:               operatorv1alpha1.OperationObserve,
			ID:                 "operation-01",
			InputFingerprint:   contentDigest,
			ExecutionBindingID: executionBindingID,
			StartedAt:          metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
			Attempt:            1,
			AdmissionSnapshot:  &operatorv1alpha1.PodAdmissionSnapshot{Digest: digest("b"), TemplateDigest: digest("c")},
			CoordinationDigest: digest("6"),
			Target:             &operatorv1alpha1.DatabaseTargetBinding{Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, URLFrom: schema.Spec.Target.URLFrom},
			Source:             &operatorv1alpha1.SourceAccessBinding{Snapshot: snapshot},
		}
		job, err := managerBuilder().Build(schema, operation, nil)
		if err != nil {
			return nil, err
		}
		if edit != nil {
			edit(job)
		}
		return job, nil
	}
}

// configMapPlanSource is what a plan computed from a ConfigMap binds.
func configMapPlanSource() *operatorv1alpha1.PlanSourceBinding {
	return &operatorv1alpha1.PlanSourceBinding{
		Kind: operatorv1alpha1.SourceKindConfigMap, Revision: "configmap-uid", ContentDigest: digest("4"),
	}
}

// sourceSchemaPlan is a plan computed from a ConfigMap or a Git source:
// contract version 2, the source in place of the artifact digest and the
// verification policy.
func sourceSchemaPlan(source *operatorv1alpha1.PlanSourceBinding) (*operatorv1alpha1.PtahSchemaPlan, error) {
	identity := make([]byte, 32)
	if _, err := rand.Read(identity); err != nil {
		return nil, err
	}
	builder := managerBuilder()
	spec := operatorv1alpha1.PtahSchemaPlanSpec{
		ContractVersion:         fingerprint.SourcePlanContractVersion,
		SchemaRef:               operatorv1alpha1.ImmutableObjectReference{Name: tenantSchema.Name, UID: tenantSchema.UID},
		Fingerprint:             "sha256:" + hex.EncodeToString(identity),
		Source:                  source,
		CoordinationDigest:      digest("6"),
		TargetIdentityDigest:    digest("7"),
		ActualStateFingerprint:  digest("8"),
		DesiredStateFingerprint: digest("9"),
		PolicyFingerprint:       digest("b"),
		ExecutionBindingID:      executionBindingID,
		ControllerImage:         harness.ManagerImage,
		ControllerRevision:      builder.ControllerRevision,
		ControllerStateVersion:  builder.ControllerStateVersion,
		PtahVersion:             builder.PtahVersion,
		ExecutorImage:           builder.ExecutorImage,
		RunnerImage:             builder.RunnerImage,
		RunnerProtocolVersion:   int32(runner.ProtocolVersion),
		Dialect:                 "postgresql",
		StatementCount:          1,
	}
	plan, _, err := planstore.Prepare(tenantSchema, spec, bytes.Repeat([]byte("x"), 64))
	return plan, err
}

// sourceMigrationPlan is a migration plan computed from a ConfigMap or a Git
// source.
func sourceMigrationPlan(source *operatorv1alpha1.PlanSourceBinding) (*operatorv1alpha1.PtahMigrationPlan, error) {
	builder := managerBuilder()
	planned := []operatorv1alpha1.PlannedMigration{{Version: 1, Checksum: digest("1")}}
	sequence, err := migrationplan.SequenceDigest(planned)
	if err != nil {
		return nil, err
	}
	binding := migrationplan.Binding{
		MigrationUID:           tenantMigration.UID,
		HistoryFingerprint:     digest("5"),
		SequenceDigest:         sequence,
		Source:                 &fingerprint.PlanSource{Kind: string(source.Kind), Revision: source.Revision, ContentDigest: source.ContentDigest},
		CoordinationDigest:     digest("6"),
		TargetIdentityDigest:   digest("7"),
		PolicyFingerprint:      digest("b"),
		ExecutionBindingID:     executionBindingID,
		ControllerStateVersion: builder.ControllerStateVersion,
		PtahVersion:            builder.PtahVersion,
		ExecutorImage:          builder.ExecutorImage,
		RunnerProtocolVersion:  int32(runner.ProtocolVersion),
	}
	planFingerprint, err := binding.Fingerprint()
	if err != nil {
		return nil, err
	}
	return migrationplan.Desired(tenantMigration, operatorv1alpha1.PtahMigrationPlanSpec{
		ContractVersion:        binding.ContractVersion(),
		MigrationRef:           operatorv1alpha1.ImmutableObjectReference{Name: tenantMigration.Name, UID: tenantMigration.UID},
		Fingerprint:            planFingerprint,
		HistoryFingerprint:     binding.HistoryFingerprint,
		Source:                 source,
		CoordinationDigest:     binding.CoordinationDigest,
		TargetIdentityDigest:   binding.TargetIdentityDigest,
		PolicyFingerprint:      binding.PolicyFingerprint,
		ExecutionBindingID:     binding.ExecutionBindingID,
		ControllerImage:        harness.ManagerImage,
		ControllerRevision:     builder.ControllerRevision,
		ControllerStateVersion: binding.ControllerStateVersion,
		PtahVersion:            binding.PtahVersion,
		ExecutorImage:          binding.ExecutorImage,
		RunnerImage:            builder.RunnerImage,
		RunnerProtocolVersion:  binding.RunnerProtocolVersion,
		Migrations:             planned,
		CreatedAt:              metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
	})
}
