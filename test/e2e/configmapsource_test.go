package e2e

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
)

const testContentDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func readConfigMap() *ptahv1alpha1.ConfigMapSourceRevision {
	return &ptahv1alpha1.ConfigMapSourceRevision{Name: configMapSchemaSource, UID: "configmap-uid", ResourceVersion: "41"}
}

func boundSnapshot() *ptahv1alpha1.SourceSnapshotBinding {
	return &ptahv1alpha1.SourceSnapshotBinding{
		Name: "ptah-src-0123456789abcdef01234567", UID: "snapshot-uid", ContentDigest: testContentDigest, SchemaFile: "schema.sql",
	}
}

func boundPlanSpec() ptahv1alpha1.PtahSchemaPlanSpec {
	return ptahv1alpha1.PtahSchemaPlanSpec{
		ContractVersion: 2,
		Source: &ptahv1alpha1.PlanSourceBinding{
			Kind: ptahv1alpha1.SourceKindConfigMap, Revision: "configmap-uid", ContentDigest: testContentDigest,
		},
	}
}

func TestConfigMapPlanBoundRefusesAPlanOfAnotherSource(t *testing.T) {
	t.Parallel()
	if err := configMapPlanBound(boundPlanSpec(), readConfigMap(), boundSnapshot()); err != nil {
		t.Fatalf("the plan of the read source was refused: %v", err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchemaPlanSpec, **ptahv1alpha1.ConfigMapSourceRevision){
		"an OCI plan": func(spec *ptahv1alpha1.PtahSchemaPlanSpec, _ **ptahv1alpha1.ConfigMapSourceRevision) {
			spec.ContractVersion, spec.Source, spec.ArtifactDigest = 1, nil, testContentDigest
		},
		"version 1 with a source": func(spec *ptahv1alpha1.PtahSchemaPlanSpec, _ **ptahv1alpha1.ConfigMapSourceRevision) {
			spec.ContractVersion = 1
		},
		"a verification policy": func(spec *ptahv1alpha1.PtahSchemaPlanSpec, _ **ptahv1alpha1.ConfigMapSourceRevision) {
			spec.VerificationPolicyUID = "uid"
		},
		"another ConfigMap": func(spec *ptahv1alpha1.PtahSchemaPlanSpec, _ **ptahv1alpha1.ConfigMapSourceRevision) {
			spec.Source.Revision = "other"
		},
		"other content": func(spec *ptahv1alpha1.PtahSchemaPlanSpec, _ **ptahv1alpha1.ConfigMapSourceRevision) {
			spec.Source.ContentDigest = "sha256:2"
		},
		"a source read by nobody yet": func(_ *ptahv1alpha1.PtahSchemaPlanSpec, read **ptahv1alpha1.ConfigMapSourceRevision) { *read = nil },
	} {
		spec, read := boundPlanSpec(), readConfigMap()
		mutate(&spec, &read)
		if err := configMapPlanBound(spec, read, boundSnapshot()); err == nil {
			t.Errorf("configMapPlanBound accepted %s", name)
		}
	}
	migration := ptahv1alpha1.PtahMigrationPlanSpec{ContractVersion: 2, Source: boundPlanSpec().Source}
	if err := configMapMigrationPlanBound(migration, readConfigMap(), boundSnapshot()); err != nil {
		t.Fatalf("the migration plan of the read source was refused: %v", err)
	}
	migration.ArtifactDigest = testContentDigest
	if err := configMapMigrationPlanBound(migration, readConfigMap(), boundSnapshot()); err == nil {
		t.Error("configMapMigrationPlanBound accepted a plan that also binds an artifact")
	}
}

func TestSnapshotOfRefusesAnotherObjectOrOtherBytes(t *testing.T) {
	t.Parallel()
	owner := sourcesnapshot.Owner{Kind: "PtahSchema", Name: configMapSchema, UID: "schema-uid"}
	stored, digest, err := sourcesnapshot.Build("e2e", owner, []sourcecontent.File{{Path: "schema.sql", Data: []byte("CREATE TABLE a (id int);\n")}})
	if err != nil {
		t.Fatal(err)
	}
	stored.UID = "snapshot-uid"
	binding := &ptahv1alpha1.SourceSnapshotBinding{Name: stored.Name, UID: stored.UID, ContentDigest: digest}
	if err := snapshotOf(stored, "PtahSchema", "schema-uid", binding); err != nil {
		t.Fatalf("the bound snapshot was refused: %v", err)
	}
	recreated := stored.DeepCopy()
	recreated.UID = "another-uid"
	tampered := stored.DeepCopy()
	tampered.BinaryData[sourcecontent.SnapshotFileKey(0)] = []byte("DROP TABLE a;\n")
	for name, check := range map[string]error{
		"another object under the name": snapshotOf(recreated, "PtahSchema", "schema-uid", binding),
		"changed bytes":                 snapshotOf(tampered, "PtahSchema", "schema-uid", binding),
		"another owner":                 snapshotOf(stored, "PtahMigration", "schema-uid", binding),
		"another owner UID":             snapshotOf(stored, "PtahSchema", "other-uid", binding),
		"no binding":                    snapshotOf(stored, "PtahSchema", "schema-uid", nil),
	} {
		if check == nil {
			t.Errorf("snapshotOf accepted %s", name)
		}
	}
}

func TestSnapshotJobRefusesARegistryPath(t *testing.T) {
	t.Parallel()
	job := func(initContainers []string, volumes ...string) *batchv1.Job {
		object := &batchv1.Job{}
		for _, name := range initContainers {
			object.Spec.Template.Spec.InitContainers = append(object.Spec.Template.Spec.InitContainers, corev1.Container{Name: name})
		}
		for _, name := range volumes {
			object.Spec.Template.Spec.Volumes = append(object.Spec.Template.Spec.Volumes, corev1.Volume{Name: name})
		}
		return object
	}
	if err := snapshotJob(job([]string{"install-runner", snapshotCopyContainer}, "runner", "source-snapshot", "schema-source")); err != nil {
		t.Fatalf("a snapshot Job was refused: %v", err)
	}
	for name, candidate := range map[string]*batchv1.Job{
		"the OCI fetch pair":    job([]string{"install-runner", "validate-source-authority", "fetch-schema"}),
		"no snapshot copy":      job([]string{"install-runner"}),
		"a registry credential": job([]string{"install-runner", snapshotCopyContainer}, "registry-docker-config"),
		"a fetch work volume":   job([]string{"install-runner", snapshotCopyContainer}, "fetch-work"),
	} {
		if err := snapshotJob(candidate); err == nil {
			t.Errorf("snapshotJob accepted %s", name)
		}
	}
}

func TestSnapshotRefusedNeedsTheCopyToRefuseAndTheOperationNotToStart(t *testing.T) {
	t.Parallel()
	refusal := "ptah-runner: source_snapshot_mismatch: snapshot does not hold the bound content: schema.sql does not match its manifest line"
	pod := func(copyExit int32, message string, mainState corev1.ContainerState) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "install-runner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: snapshotCopyContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: copyExit, Message: message}}},
			},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "ptah", State: mainState}},
		}}
	}
	waiting := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
	if refused, message := snapshotRefused(pod(2, refusal, waiting)); !refused || message != refusal {
		t.Fatalf("the refusal was not recognized: %v %q", refused, message)
	}
	for name, candidate := range map[string]*corev1.Pod{
		"a copy that succeeded":     pod(0, "", waiting),
		"another failure":           pod(2, "ptah-runner: snapshot mode takes exactly three flags", waiting),
		"an operation that started": pod(2, refusal, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}),
		"an operation that ended":   pod(2, refusal, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}),
	} {
		if refused, _ := snapshotRefused(candidate); refused {
			t.Errorf("snapshotRefused accepted %s", name)
		}
	}
}

func TestSourceReadAtNeedsTheEditedVersion(t *testing.T) {
	t.Parallel()
	if !sourceReadAt(readConfigMap(), boundSnapshot(), "41") {
		t.Fatal("a read at the edited version was refused")
	}
	if sourceReadAt(readConfigMap(), boundSnapshot(), "40") || sourceReadAt(nil, boundSnapshot(), "41") ||
		sourceReadAt(readConfigMap(), nil, "41") || sourceReadAt(readConfigMap(), boundSnapshot(), "") {
		t.Fatal("sourceReadAt accepted a read of another version or no read")
	}
}
