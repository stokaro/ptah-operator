package e2e

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
)

// The ConfigMap source phase's fixtures. It runs on the data plane's
// PostgreSQL server, on two databases no other phase touches: one the schema
// converges, one the migration sequence runs against.
const (
	configMapSchemaDatabase    = "ptah_e2e_configmap_schema"
	configMapMigrationDatabase = "ptah_e2e_configmap_migrations"
	configMapSchema            = "e2e-configmap-schema"
	configMapMigration         = "e2e-configmap-migration"
	configMapSchemaSource      = "e2e-configmap-schema-source"
	configMapMigrationSource   = "e2e-configmap-migration-source"
	configMapSchemaSecret      = "e2e-configmap-schema-database"
	configMapMigrationSecret   = "e2e-configmap-migration-database"
	// configMapUnselectedKey is a key both source ConfigMaps hold and neither
	// resource selects: editing it is the edit that must change nothing.
	configMapUnselectedKey = "README"
	// configMapInterval is long enough that no periodic refresh lands inside
	// the phase: every read the phase sees is one an edit caused.
	configMapInterval = "1h"
	// snapshotCopyContainer is the init container that checks a snapshot and
	// copies it.
	snapshotCopyContainer = "copy-source-snapshot"
)

// snapshotContentRefusal is what the copy reports when the snapshot does not
// hold the bound content, whichever check found it: the refusal code and the
// content-mismatch text. The same code with any other text is a copy that
// failed to write, which says nothing about the content.
var snapshotContentRefusal = runner.CodeSourceSnapshotMismatch + ": " + sourcecontent.ErrContentMismatch.Error()

// configMapSchemaRevisions are the desired schema the phase declares, in
// order: a table, then a column on it, then an index the refusal row plans.
var configMapSchemaRevisions = []string{
	"CREATE TABLE e2e_configmap_items (\n  id bigint NOT NULL PRIMARY KEY,\n  name text NOT NULL\n);\n",
	"CREATE TABLE e2e_configmap_items (\n  id bigint NOT NULL PRIMARY KEY,\n  name text NOT NULL,\n  sku text\n);\n",
	"CREATE TABLE e2e_configmap_items (\n  id bigint NOT NULL PRIMARY KEY,\n  name text NOT NULL,\n  sku text\n);\n\nCREATE INDEX e2e_configmap_items_sku ON e2e_configmap_items (sku);\n",
}

// configMapMigrationFiles are the migration directory the phase declares,
// by the ConfigMap key each file is stored under.
var configMapMigrationFiles = []struct{ key, path, body string }{
	{"0001-up", "0000000001_create_parts.up.sql", "CREATE TABLE e2e_configmap_parts (\n    id INTEGER PRIMARY KEY,\n    name TEXT NOT NULL\n);\n"},
	{"0001-down", "0000000001_create_parts.down.sql", "DROP TABLE e2e_configmap_parts;\n"},
	{"0002-up", "0000000002_add_part_weight.up.sql", "ALTER TABLE e2e_configmap_parts ADD COLUMN weight INTEGER;\n"},
	{"0002-down", "0000000002_add_part_weight.down.sql", "ALTER TABLE e2e_configmap_parts DROP COLUMN weight;\n"},
}

// configMapPlanBound holds a schema plan to the source it was computed from:
// contract version 2, the ConfigMap's UID and the snapshot's content digest,
// and nothing of an OCI artifact or a verification policy.
func configMapPlanBound(spec ptahv1alpha1.PtahSchemaPlanSpec, read *ptahv1alpha1.ConfigMapSourceRevision, snapshot *ptahv1alpha1.SourceSnapshotBinding) error {
	return planSourceBound(spec.ContractVersion, spec.ArtifactDigest, string(spec.VerificationPolicyUID), spec.VerificationPolicyDigest, spec.Source, read, snapshot)
}

// configMapMigrationPlanBound is configMapPlanBound for a migration plan.
func configMapMigrationPlanBound(spec ptahv1alpha1.PtahMigrationPlanSpec, read *ptahv1alpha1.ConfigMapSourceRevision, snapshot *ptahv1alpha1.SourceSnapshotBinding) error {
	return planSourceBound(spec.ContractVersion, spec.ArtifactDigest, string(spec.VerificationPolicyUID), spec.VerificationPolicyDigest, spec.Source, read, snapshot)
}

func planSourceBound(version int32, artifactDigest, policyUID, policyDigest string, source *ptahv1alpha1.PlanSourceBinding,
	read *ptahv1alpha1.ConfigMapSourceRevision, snapshot *ptahv1alpha1.SourceSnapshotBinding,
) error {
	switch {
	case read == nil || snapshot == nil:
		return errors.New("the resource has not read its ConfigMap source")
	case version != 2:
		return fmt.Errorf("the plan has contract version %d, not 2", version)
	case artifactDigest != "" || policyUID != "" || policyDigest != "":
		return errors.New("the plan binds an OCI artifact or a verification policy")
	case source == nil || source.Kind != ptahv1alpha1.SourceKindConfigMap:
		return errors.New("the plan binds no ConfigMap source")
	case source.Revision != string(read.UID):
		return fmt.Errorf("the plan binds ConfigMap %s, and the resource read %s", source.Revision, read.UID)
	case source.ContentDigest != snapshot.ContentDigest:
		return fmt.Errorf("the plan binds content %s, and the snapshot holds %s", source.ContentDigest, snapshot.ContentDigest)
	}
	return nil
}

// snapshotOf holds a stored ConfigMap to the snapshot a binding names: the
// object it names, owned by the resource, holding exactly the content digest.
func snapshotOf(configMap *corev1.ConfigMap, kind string, owner types.UID, binding *ptahv1alpha1.SourceSnapshotBinding) error {
	if binding == nil {
		return errors.New("no snapshot is bound")
	}
	if configMap.Name != binding.Name || configMap.UID != binding.UID {
		return fmt.Errorf("ConfigMap %s/%s is not the bound snapshot %s/%s", configMap.Name, configMap.UID, binding.Name, binding.UID)
	}
	stored, digest, err := sourcesnapshot.Verify(configMap)
	if err != nil {
		return err
	}
	if stored.Kind != kind || stored.UID != owner {
		return fmt.Errorf("snapshot %s belongs to %s %s", configMap.Name, stored.Kind, stored.UID)
	}
	if digest != binding.ContentDigest {
		return fmt.Errorf("snapshot %s holds %s, and the binding names %s", configMap.Name, digest, binding.ContentDigest)
	}
	return nil
}

// snapshotJob holds an operation Job of a ConfigMap source to its shape: the
// runner installed, then the snapshot checked and copied, and nothing that
// reaches a registry.
func snapshotJob(job *batchv1.Job) error {
	spec := job.Spec.Template.Spec
	names := make([]string, len(spec.InitContainers))
	for index, container := range spec.InitContainers {
		names[index] = container.Name
	}
	if !slices.Equal(names, []string{"install-runner", snapshotCopyContainer}) {
		return fmt.Errorf("Job %s runs the init containers %v", job.Name, names)
	}
	for _, volume := range spec.Volumes {
		if slices.Contains([]string{"fetch-work", "registry-docker-config", "registry-ca", "registry-ca-snapshot", "verification-policy"}, volume.Name) {
			return fmt.Errorf("Job %s mounts the registry volume %s", job.Name, volume.Name)
		}
	}
	return nil
}

// snapshotRefused reports whether a Pod is one whose snapshot copy refused
// the snapshot and whose operation container therefore never started, and
// returns the refusal the copy reported.
func snapshotRefused(pod *corev1.Pod) (bool, string) {
	refused, message := false, ""
	for _, status := range pod.Status.InitContainerStatuses {
		terminated := status.State.Terminated
		if status.Name != snapshotCopyContainer || terminated == nil || terminated.ExitCode == 0 {
			continue
		}
		if strings.Contains(terminated.Message, snapshotContentRefusal) {
			refused, message = true, terminated.Message
		}
	}
	if !refused {
		return false, ""
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Running != nil || status.State.Terminated != nil || status.LastTerminationState.Terminated != nil {
			return false, ""
		}
	}
	return true, message
}

// sourceReadAt reports whether a resource has read its source ConfigMap at
// a resourceVersion, and holds a snapshot of it.
func sourceReadAt(read *ptahv1alpha1.ConfigMapSourceRevision, snapshot *ptahv1alpha1.SourceSnapshotBinding, resourceVersion string) bool {
	return read != nil && snapshot != nil && resourceVersion != "" && read.ResourceVersion == resourceVersion
}
