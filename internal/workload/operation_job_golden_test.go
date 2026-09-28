package workload

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

var updateGoldenJobs = flag.Bool("update", false, "rewrite testdata/jobs from what the builders produce now")

const goldenJobDirectory = "testdata/jobs"

type goldenJobCase struct {
	name  string
	build func() (*batchv1.Job, error)
}

// The operation Job is read by more than the kubelet. The controller-write
// guard rebuilds it and compares, the pod-intent webhook digests its Pod
// template into the admission snapshot, and the typed Job policy in the chart
// matches its shape. A change to one byte of it is a change to each of them,
// so a representative set is pinned here whole, and such a change arrives as a
// diff a reviewer reads rather than as a Pod that stops being admitted.
//
// The set covers both families, every operation type, each registry auth mode,
// the CA snapshot with and without credentials, plain HTTP, the development
// database and the plan fence, the Apply window and termination grace, the
// admission snapshot, and every scheduling knob spec.execution offers, set and
// left unset.
//
// After an intended change, rewrite the files and review their diff:
//
//	go test ./internal/workload -run TestOperationJobsMatchTheirGoldenFiles -update
func TestOperationJobsMatchTheirGoldenFiles(t *testing.T) {
	t.Parallel()

	cases := goldenJobCases()
	if *updateGoldenJobs {
		if err := os.MkdirAll(goldenJobDirectory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			job, err := test.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			got, err := json.MarshalIndent(job, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join(goldenJobDirectory, test.name+".json")
			if *updateGoldenJobs {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read the golden Job: %v", err)
			}
			if !bytes.Equal(got, want) {
				line, wantLine, gotLine := firstDifferentLine(want, got)
				t.Fatalf("%s differs from what the builder produces, first at line %d:\n want: %s\n  got: %s\n"+
					"If the change is intended, rerun with -update and review the diff.", path, line, wantLine, gotLine)
			}
		})
	}

	// A golden file no case produces is a contract nothing checks any longer,
	// and an empty directory would let every row above fail to exist unnoticed.
	t.Run("every golden file has a case", func(t *testing.T) {
		if *updateGoldenJobs {
			t.Skip("the files are being rewritten")
		}
		entries, err := os.ReadDir(goldenJobDirectory)
		if err != nil {
			t.Fatal(err)
		}
		var files []string
		for _, entry := range entries {
			files = append(files, strings.TrimSuffix(entry.Name(), ".json"))
		}
		var names []string
		for _, test := range cases {
			names = append(names, test.name)
		}
		slices.Sort(files)
		slices.Sort(names)
		if len(names) == 0 || !slices.Equal(files, names) {
			t.Fatalf("%s holds %q, want exactly the cases %q", goldenJobDirectory, files, names)
		}
	})
}

func goldenJobCases() []goldenJobCase {
	builder := builderFixture()
	return []goldenJobCase{
		{
			name: "schema-resolve",
			build: func() (*batchv1.Job, error) {
				return builder.Build(schemaFixture(), operationFixture(operatorv1alpha1.OperationResolve), nil)
			},
		},
		{
			name: "schema-resolve-docker-config-plain-http",
			build: func() (*batchv1.Job, error) {
				schema := sourceContractSchemaFixture(operatorv1alpha1.RegistryAuthDockerConfigJSON)
				schema.Spec.Desired.RegistryAuthFrom.DockerConfigJSONKey = "config"
				return builder.Build(schema, operationFixture(operatorv1alpha1.OperationResolve), nil)
			},
		},
		{
			name: "schema-resolve-anonymous-unset-execution",
			build: func() (*batchv1.Job, error) {
				schema := schemaFixture()
				schema.Spec.Desired.RegistryAuthFrom = nil
				schema.Spec.Desired.Transport = operatorv1alpha1.OCITransportSpec{}
				schema.Spec.Execution = operatorv1alpha1.ExecutionSpec{}
				return builder.Build(schema, operationFixture(operatorv1alpha1.OperationResolve), nil)
			},
		},
		{
			name: "schema-verify-admitted",
			build: func() (*batchv1.Job, error) {
				operation := operationFixture(operatorv1alpha1.OperationVerify)
				operation.AdmissionSnapshot = goldenAdmissionSnapshot()
				return builder.Build(schemaFixture(), operation, nil)
			},
		},
		{
			name: "schema-observe",
			build: func() (*batchv1.Job, error) {
				return builder.Build(schemaFixture(), operationFixture(operatorv1alpha1.OperationObserve), nil)
			},
		},
		{
			// The declared metadata of #447: a mesh opt-out and a label a
			// policy engine selects on, under the operator's own.
			name: "schema-observe-pod-metadata",
			build: func() (*batchv1.Job, error) {
				schema := schemaFixture()
				schema.Spec.Execution.PodMetadata = declaredMetadataFixture()
				return builder.Build(schema, operationFixture(operatorv1alpha1.OperationObserve), nil)
			},
		},
		{
			name: "schema-observe-anonymous-ca-unset-policy",
			build: func() (*batchv1.Job, error) {
				schema := schemaFixture()
				schema.Spec.Desired.RegistryAuthFrom = nil
				operation := operationFixture(operatorv1alpha1.OperationObserve)
				operation.Source.RegistryAuthFrom = nil
				operation.ObservationSeverity = ""
				operation.ObservationConnectTimeout = metav1.Duration{}
				operation.ObservationLockTimeout = metav1.Duration{}
				return builder.Build(schema, operation, nil)
			},
		},
		{
			name: "schema-plan-dev-fence-scheduling",
			build: func() (*batchv1.Job, error) {
				schema := schemaFixture()
				setGoldenScheduling(&schema.Spec.Execution)
				operation := operationFixture(operatorv1alpha1.OperationPlan)
				operation.ObservationProtectedTables = []string{"public.orders", "audit.log"}
				return builder.Build(schema, operation, nil)
			},
		},
		{
			name: "schema-plan-docker-config-plain-http",
			build: func() (*batchv1.Job, error) {
				schema := sourceContractSchemaFixture(operatorv1alpha1.RegistryAuthDockerConfigJSON)
				operation := operationFixture(operatorv1alpha1.OperationPlan)
				operation.Source = &operatorv1alpha1.OCIArtifactAccessBinding{
					ResolvedReference: schema.Status.Source.ResolvedReference,
					Digest:            schema.Status.Source.Digest,
					RegistryAuthFrom:  schema.Spec.Desired.RegistryAuthFrom.DeepCopy(),
					Transport:         *schema.Spec.Desired.Transport.DeepCopy(),
				}
				operation.ObservationDev = nil
				operation.ObservationExclude = nil
				return builder.Build(schema, operation, nil)
			},
		},
		{
			name: "schema-apply-admitted-scheduling",
			build: func() (*batchv1.Job, error) {
				schema := schemaFixture()
				setGoldenScheduling(&schema.Spec.Execution)
				plan := planFixture(schema, builder)
				operation := operationFixture(operatorv1alpha1.OperationApply)
				operation.AdmissionSnapshot = goldenAdmissionSnapshot()
				dispatchNotAfter := metav1.NewTime(operation.StartedAt.Add(300 * time.Second))
				executionNotAfter := metav1.NewTime(operation.StartedAt.Add(540 * time.Second))
				operation.DispatchNotAfter = &dispatchNotAfter
				operation.ExecutionNotAfter = &executionNotAfter
				operation.TerminationGracePeriodSeconds = 45
				return builder.Build(schema, operation, plan)
			},
		},
		{
			name: "migration-resolve",
			build: func() (*batchv1.Job, error) {
				return builder.BuildMigration(migrationFixture(), migrationOperationFixture(operatorv1alpha1.MigrationOperationResolve), nil)
			},
		},
		{
			name: "migration-resolve-docker-config-ca",
			build: func() (*batchv1.Job, error) {
				migration := migrationFixture()
				migration.Spec.Artifact.RegistryAuthFrom = &operatorv1alpha1.RegistryAuthSource{
					Name: "registry-docker", Mode: operatorv1alpha1.RegistryAuthDockerConfigJSON,
				}
				migration.Spec.Artifact.Transport.CAFrom = goldenRegistryCA()
				return builder.BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationResolve), nil)
			},
		},
		{
			name: "migration-verify-admitted",
			build: func() (*batchv1.Job, error) {
				operation := migrationOperationFixture(operatorv1alpha1.MigrationOperationVerify)
				operation.AdmissionSnapshot = goldenAdmissionSnapshot()
				return builder.BuildMigration(migrationFixture(), operation, nil)
			},
		},
		{
			name: "migration-history",
			build: func() (*batchv1.Job, error) {
				return builder.BuildMigration(migrationFixture(), migrationOperationFixture(operatorv1alpha1.MigrationOperationHistory), nil)
			},
		},
		{
			name: "migration-history-pod-metadata",
			build: func() (*batchv1.Job, error) {
				migration := migrationFixture()
				migration.Spec.Execution.PodMetadata = declaredMetadataFixture()
				return builder.BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationHistory), nil)
			},
		},
		{
			name: "migration-history-anonymous-ca-unset-execution",
			build: func() (*batchv1.Job, error) {
				migration := migrationFixture()
				migration.Spec.Artifact.RegistryAuthFrom = nil
				migration.Spec.Execution = operatorv1alpha1.ExecutionSpec{}
				migration.Spec.Policy.LockTimeout = metav1.Duration{}
				operation := migrationOperationFixture(operatorv1alpha1.MigrationOperationHistory)
				operation.Source.RegistryAuthFrom = nil
				operation.Source.Transport.CAFrom = goldenRegistryCA()
				return builder.BuildMigration(migration, operation, nil)
			},
		},
		{
			name: "migration-apply-admitted-scheduling",
			build: func() (*batchv1.Job, error) {
				migration := migrationFixture()
				setGoldenScheduling(&migration.Spec.Execution)
				migration.Spec.Policy.TransactionMode = "file"
				operation := migrationOperationFixture(operatorv1alpha1.MigrationOperationApply)
				operation.AdmissionSnapshot = goldenAdmissionSnapshot()
				operation.Source.RegistryAuthFrom = &operatorv1alpha1.RegistryAuthSource{
					Name: "registry-docker", Mode: operatorv1alpha1.RegistryAuthDockerConfigJSON,
				}
				operation.Source.Transport.CAFrom = goldenRegistryCA()
				return builder.BuildMigration(migration, operation, migrationPlanFixture())
			},
		},
	}
}

func goldenAdmissionSnapshot() *operatorv1alpha1.PodAdmissionSnapshot {
	return &operatorv1alpha1.PodAdmissionSnapshot{Digest: digest('8'), TemplateDigest: digest('9')}
}

func goldenRegistryCA() *corev1.ConfigMapKeySelector {
	return &corev1.ConfigMapKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "registry-ca"},
		Key:                  "ca.pem",
	}
}

// setGoldenScheduling sets every scheduling and resource field spec.execution
// offers, so the rows that use it prove each one reaches the Pod.
func setGoldenScheduling(execution *operatorv1alpha1.ExecutionSpec) {
	runtimeClass := "gvisor"
	tolerationSeconds := int64(120)
	execution.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	execution.NodeSelector = map[string]string{"workload": "database", "zone": "eu-west-1a"}
	execution.Tolerations = []corev1.Toleration{
		{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "database", Effect: corev1.TaintEffectNoSchedule},
		{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &tolerationSeconds},
	}
	execution.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64", "arm64"},
					}},
				}},
			},
		},
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight: 50,
				PodAffinityTerm: corev1.PodAffinityTerm{
					TopologyKey: "kubernetes.io/hostname",
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{LabelManagedBy: "ptah-operator"},
					},
				},
			}},
		},
	}
	execution.RuntimeClassName = &runtimeClass
	execution.PriorityClassName = "database-maintenance"
	execution.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "image-pull"}, {Name: "mirror-pull"}}
}

func firstDifferentLine(want, got []byte) (int, string, string) {
	wantLines := strings.Split(string(want), "\n")
	gotLines := strings.Split(string(got), "\n")
	for index := 0; index < max(len(wantLines), len(gotLines)); index++ {
		var wantLine, gotLine string
		if index < len(wantLines) {
			wantLine = wantLines[index]
		}
		if index < len(gotLines) {
			gotLine = gotLines[index]
		}
		if wantLine != gotLine {
			return index + 1, wantLine, gotLine
		}
	}
	return 0, "", ""
}
