package workload

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	batchv1 "k8s.io/api/batch/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiserver/pkg/cel/environment"
	"sigs.k8s.io/yaml"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// declaredMetadataFixture is a declaration a service mesh and a policy engine
// would each ask for: an opt-out annotation and a label a policy selects on.
func declaredMetadataFixture() *operatorv1alpha1.PodMetadataSpec {
	return &operatorv1alpha1.PodMetadataSpec{
		Labels: map[string]operatorv1alpha1.PodLabelValue{
			"acme.example/team":  "platform",
			"mesh-opt-out":       "true",
			"policy.example/tag": "",
		},
		Annotations: map[string]operatorv1alpha1.PodAnnotationValue{
			"sidecar.istio.io/inject":                          "false",
			"traffic.sidecar.istio.io/excludeOutboundIPRanges": "10.0.0.0/8",
		},
	}
}

// Every operation of both families carries the declaration on the Job and on
// its Pod template, under the operator's own metadata, so the Pod-intent
// webhook, which holds a Pod to its template, admits exactly the declared Pod.
func TestBuildCarriesDeclaredPodMetadataUnderTheOperatorsOwn(t *testing.T) {
	t.Parallel()

	builder := builderFixture()
	declared := declaredMetadataFixture()
	builds := map[string]func() (*batchv1.Job, error){
		"schema resolve": func() (*batchv1.Job, error) {
			schema := schemaFixture()
			schema.Spec.Execution.PodMetadata = declared.DeepCopy()
			return builder.Build(schema, operationFixture(operatorv1alpha1.OperationResolve), nil)
		},
		"schema observe": func() (*batchv1.Job, error) {
			schema := schemaFixture()
			schema.Spec.Execution.PodMetadata = declared.DeepCopy()
			return builder.Build(schema, operationFixture(operatorv1alpha1.OperationObserve), nil)
		},
		"migration history": func() (*batchv1.Job, error) {
			migration := migrationFixture()
			migration.Spec.Execution.PodMetadata = declared.DeepCopy()
			return builder.BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationHistory), nil)
		},
	}
	for name, build := range builds {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			job, err := build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			for key, value := range declared.Labels {
				if job.Labels[key] != string(value) || job.Spec.Template.Labels[key] != string(value) {
					t.Errorf("label %s = %q on the Job and %q on its template, want %q on both",
						key, job.Labels[key], job.Spec.Template.Labels[key], value)
				}
			}
			for key, value := range declared.Annotations {
				if job.Annotations[key] != string(value) || job.Spec.Template.Annotations[key] != string(value) {
					t.Errorf("annotation %s = %q on the Job and %q on its template, want %q on both",
						key, job.Annotations[key], job.Spec.Template.Annotations[key], value)
				}
			}
			if len(job.Labels) != 5+len(declared.Labels) {
				t.Errorf("Job labels = %v, want the operator's five and the %d declared", job.Labels, len(declared.Labels))
			}
			if job.Labels[LabelManagedBy] != "ptah-operator" || job.Annotations[AnnotationOperationID] == "" {
				t.Errorf("the operator's own metadata is missing beside the declaration: %v %v", job.Labels, job.Annotations)
			}
		})
	}
}

// A declared key the operator writes itself, or one that would select the Pod
// into an object the operator owns, is not written under it: the envelope is
// written last, and the key is refused before that. The CRD refuses these
// first; this is the builder refusing a Job admission would refuse, and saying
// which key.
func TestBuildRefusesReservedOrInvalidPodMetadata(t *testing.T) {
	t.Parallel()

	label := func(key, value string) *operatorv1alpha1.PodMetadataSpec {
		return &operatorv1alpha1.PodMetadataSpec{Labels: map[string]operatorv1alpha1.PodLabelValue{key: operatorv1alpha1.PodLabelValue(value)}}
	}
	annotation := func(key, value string) *operatorv1alpha1.PodMetadataSpec {
		return &operatorv1alpha1.PodMetadataSpec{Annotations: map[string]operatorv1alpha1.PodAnnotationValue{key: operatorv1alpha1.PodAnnotationValue(value)}}
	}
	tooMany := &operatorv1alpha1.PodMetadataSpec{Labels: map[string]operatorv1alpha1.PodLabelValue{}}
	for i := 0; i <= operatorv1alpha1.MaxPodMetadataEntries; i++ {
		tooMany.Labels["declared.example/"+strings.Repeat("k", i+1)] = "v"
	}
	rows := map[string]struct {
		metadata *operatorv1alpha1.PodMetadataSpec
		want     string
	}{
		"the operator's subject label":     {label("operator.ptah.run/schema", "other"), "reserved"},
		"the operator's operation label":   {label("operator.ptah.run/operation", "apply"), "reserved"},
		"a label under the bare ptah.run":  {label("ptah.run/team", "x"), "reserved"},
		"a label under a ptah.run domain":  {label("mesh.ptah.run/team", "x"), "reserved"},
		"the chart's selector label":       {label("app.kubernetes.io/name", "ptah-operator"), "reserved"},
		"the Job controller's label":       {label("batch.kubernetes.io/job-name", "other"), "reserved"},
		"the legacy controller-uid label":  {label("controller-uid", "x"), "reserved"},
		"the legacy job-name label":        {label("job-name", "x"), "reserved"},
		"a label under k8s.io":             {label("node.k8s.io/role", "x"), "reserved"},
		"the LimitRanger annotation":       {annotation("kubernetes.io/limit-ranger", "x"), "reserved"},
		"the eviction annotation":          {annotation("cluster-autoscaler.kubernetes.io/safe-to-evict", "true"), "reserved"},
		"the operator's digest annotation": {annotation("operator.ptah.run/admission-snapshot-digest", "sha256:0"), "reserved"},
		"a label key that is not a name":   {label("-mesh", "x"), "invalid"},
		"a label key with two slashes":     {label("a/b/c", "x"), "invalid"},
		"a label value with a space":       {label("acme.example/team", "plat form"), "invalid value"},
		"an annotation past its length":    {annotation("acme.example/note", strings.Repeat("n", operatorv1alpha1.MaxPodAnnotationValueLength+1)), "longer than"},
		"more labels than the bound":       {tooMany, "more than"},
	}
	builder := builderFixture()
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := schemaFixture()
			schema.Spec.Execution.PodMetadata = row.metadata
			_, err := builder.Build(schema, operationFixture(operatorv1alpha1.OperationResolve), nil)
			if err == nil {
				t.Fatalf("Build() accepted %s", name)
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Fatalf("Build() error = %q, want one saying %q", err, row.want)
			}
		})
	}
}

// ReservedPodMetadataKey is the CRD's reserved-key rule written again in Go,
// and the two are held to the same answers by evaluating the rule the shipped
// CRD carries, in the API server's own CEL environment, on keys that sit on
// each side of it.
func TestReservedPodMetadataKeyMirrorsTheCRDRule(t *testing.T) {
	t.Parallel()

	rules := podMetadataLabelRules(t)
	reservedRule := ""
	for _, rule := range rules {
		if strings.Contains(rule.Message, "reserved") {
			reservedRule = rule.Rule
		}
	}
	if reservedRule == "" {
		t.Fatalf("the shipped CRD carries no reserved-key rule on podMetadata.labels: %#v", rules)
	}
	env, err := environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()).
		Env(environment.StoredExpressions)
	if err != nil {
		t.Fatal(err)
	}
	env, err = env.Extend(cel.Variable("self", cel.MapType(cel.StringType, cel.StringType)))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(reservedRule)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile the CRD rule: %v", issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{
		"operator.ptah.run/schema", "ptah.run/x", "mesh.ptah.run/x", "app.kubernetes.io/name",
		"batch.kubernetes.io/job-name", "kubernetes.io/limit-ranger", "k8s.io/x", "node.k8s.io/x",
		"controller-uid", "job-name",
		"sidecar.istio.io/inject", "acme.example/team", "team", "linkerd.io/inject",
		"ptah.run", "kubernetes.io", "notptah.run/x", "k8s.io.example/x", "controller-uid-2",
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			out, _, err := program.Eval(map[string]any{"self": map[string]string{key: "v"}})
			if err != nil {
				t.Fatalf("evaluate the CRD rule on %q: %v", key, err)
			}
			admitted, ok := out.Value().(bool)
			if !ok {
				t.Fatalf("the CRD rule evaluated to %T, want bool", out.Value())
			}
			if admitted == ReservedPodMetadataKey(key) {
				t.Fatalf("the CRD rule admits %q = %t, and ReservedPodMetadataKey says reserved = %t",
					key, admitted, ReservedPodMetadataKey(key))
			}
		})
	}
}

// podMetadataLabelRules reads the CEL rules the shipped PtahSchema CRD sets
// on spec.execution.podMetadata.labels.
func podMetadataLabelRules(t *testing.T) []apiextensionsv1.ValidationRule {
	t.Helper()
	path := filepath.Join("..", "..", "config", "crd", "bases", "operator.ptah.run_ptahschemas.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the shipped CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parse the shipped CRD: %v", err)
	}
	for _, version := range crd.Spec.Versions {
		if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			continue
		}
		labels := version.Schema.OpenAPIV3Schema.
			Properties["spec"].Properties["execution"].Properties["podMetadata"].Properties["labels"]
		return labels.XValidations
	}
	t.Fatal("the shipped CRD serves no version with a schema")
	return nil
}
