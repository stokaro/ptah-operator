package e2e

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// lifecycleGuardSampleJob is a read-only Job as the API server returns one the
// manager dispatched, with every field the baseline has to clear.
const lifecycleGuardSampleJob = `{
  "apiVersion": "batch/v1", "kind": "Job",
  "metadata": {
    "name": "ptah-resolve-abc", "namespace": "proof",
    "creationTimestamp": "2026-01-01T00:00:00Z", "generation": 1, "resourceVersion": "42",
    "uid": "11111111-2222-3333-4444-555555555555",
    "managedFields": [{"manager": "ptah-operator"}],
    "labels": {"operator.ptah.run/operation": "resolve", "operator.ptah.run/schema": "read-only-job-current"},
    "annotations": {"operator.ptah.run/controller-image": "registry/old@sha256:aa", "operator.ptah.run/operation-id": "op-1"}
  },
  "spec": {
    "selector": {"matchLabels": {"batch.kubernetes.io/controller-uid": "x"}},
    "ttlSecondsAfterFinished": 300, "backoffLimit": 0,
    "template": {
      "metadata": {
        "creationTimestamp": null, "uid": "t",
        "labels": {
          "batch.kubernetes.io/controller-uid": "x", "batch.kubernetes.io/job-name": "ptah-resolve-abc",
          "controller-uid": "x", "job-name": "ptah-resolve-abc", "operator.ptah.run/operation": "resolve"
        },
        "annotations": {"stale": "template"}
      },
      "spec": {
        "containers": [{"name": "ptah", "volumeMounts": [{"name": "tmp", "mountPath": "/tmp"}, {"name": "work", "mountPath": "/work"}]}],
        "volumes": [{"name": "tmp", "emptyDir": {}}, {"name": "work", "emptyDir": {"sizeLimit": "64Mi"}}]
      }
    }
  },
  "status": {"active": 1}
}`

const lifecycleGuardImage = "registry.example/ptah-operator@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func lifecycleGuardSampleBase(t *testing.T) map[string]any {
	t.Helper()
	base, err := lifecycleGuardBaseManifest([]byte(lifecycleGuardSampleJob), lifecycleGuardImage, "1")
	if err != nil {
		t.Fatal(err)
	}
	return base
}

// lifecycleGuardRoundTrip is what the API server's answer to a manifest looks
// like once decoded: the same document through JSON.
func lifecycleGuardRoundTrip(t *testing.T, manifest map[string]any) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(lifecycleGuardJSON(manifest), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// The probe table names exactly the minors the support catalog carries. A new
// minor fails here until someone decides what it adds; a minor that adds
// nothing is an explicit empty entry rather than an absence.
func TestLifecycleGuardProbeTableCoversTheSupportWindow(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../support/kubernetes.json")
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Releases []struct {
			Minor string `json:"minor"`
		} `json:"releases"`
	}
	if err := json.Unmarshal(content, &catalog); err != nil {
		t.Fatal(err)
	}
	var supported []string
	for _, release := range catalog.Releases {
		supported = append(supported, release.Minor)
	}
	if len(supported) == 0 {
		t.Fatal("support/kubernetes.json names no minor, so the table was held to nothing")
	}
	var declared []string
	for minor := range lifecycleGuardFieldProbeTable {
		declared = append(declared, minor)
	}
	slices.Sort(supported)
	slices.Sort(declared)
	if !slices.Equal(declared, supported) {
		t.Fatalf("the guarded-field probe table names %v, and support/kubernetes.json supports %v", declared, supported)
	}
	// While 1.36 is in the window, its entry is the explicit decision that it
	// adds no guarded field. When the window moves past it, the equality above
	// already requires the entry to go.
	if slices.Contains(supported, "1.36") {
		if probes, ok := lifecycleGuardFieldProbes("1.36"); !ok || len(probes) != 0 {
			t.Fatalf("1.36 has to be the explicit decision that it adds no guarded field; the table holds %d probes, declared %v", len(probes), ok)
		}
	}
	if _, ok := lifecycleGuardFieldProbes("1.99"); ok {
		t.Fatal("a minor nobody declared was read as having a probe set")
	}
	fields := map[string]bool{}
	for minor, probes := range lifecycleGuardFieldProbeTable {
		for _, probe := range probes {
			if probe.field == "" || probe.mutate == nil || probe.retained == nil || fields[probe.field] {
				t.Fatalf("%s declares an incomplete or repeated probe %q", minor, probe.field)
			}
			fields[probe.field] = true
		}
	}
}

var lifecycleGuardMinorLiteral = regexp.MustCompile(`^1\.[0-9]+$`)

// lifecycleGuardMinorLiterals reports how many Kubernetes-minor string
// literals a Go source holds inside the probe table's declaration, and which
// ones it holds anywhere else.
func lifecycleGuardMinorLiterals(filename string, source []byte) (inTable int, stray []string, err error) {
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, filename, source, parser.SkipObjectResolution)
	if err != nil {
		return 0, nil, err
	}
	var tableStart, tableEnd token.Pos
	ast.Inspect(file, func(node ast.Node) bool {
		if spec, ok := node.(*ast.ValueSpec); ok {
			for _, name := range spec.Names {
				if name.Name == "lifecycleGuardFieldProbeTable" {
					tableStart, tableEnd = spec.Pos(), spec.End()
				}
			}
		}
		return true
	})
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil || !lifecycleGuardMinorLiteral.MatchString(value) {
			return true
		}
		if tableStart.IsValid() && literal.Pos() >= tableStart && literal.End() <= tableEnd {
			inTable++
		} else {
			stray = append(stray, files.Position(literal.Pos()).String()+" "+value)
		}
		return true
	})
	return inTable, stray, nil
}

// The probe table is the one place the guard proofs name a Kubernetes minor:
// a second list would agree with the table until one of them moved.
func TestLifecycleGuardNamesNoOtherMinorList(t *testing.T) {
	t.Parallel()
	total := 0
	for _, name := range []string{"lifecycle_guards.go", "lifecycle_guards_e2e_test.go"} {
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		inTable, stray, err := lifecycleGuardMinorLiterals(name, source)
		if err != nil {
			t.Fatal(err)
		}
		if len(stray) != 0 {
			t.Errorf("%s names Kubernetes minors outside the probe table: %v", name, stray)
		}
		total += inTable
	}
	if total != len(lifecycleGuardFieldProbeTable) {
		t.Fatalf("the scan found %d minors in the probe table, which declares %d", total, len(lifecycleGuardFieldProbeTable))
	}

	// The scan refuses what it exists to refuse: a minor written anywhere
	// else, however it is spelled into a switch or a list.
	for _, source := range []string{
		"package e2e\nvar lifecycleGuardFieldProbeTable = map[string]int{\"1.35\": 1}\nfunc f(m string) bool { return m == \"1.37\" }\n",
		"package e2e\nvar minors = []string{\"1.35\", \"1.36\"}\n",
		"package e2e\nfunc f(m string) { switch m { case `1.38`: } }\n",
	} {
		_, stray, err := lifecycleGuardMinorLiterals("sample.go", []byte(source))
		if err != nil {
			t.Fatal(err)
		}
		if len(stray) == 0 {
			t.Errorf("the scan passed a stray minor in:\n%s", source)
		}
	}
}

func TestLifecycleGuardFieldProbesAreKeptAndDetected(t *testing.T) {
	t.Parallel()
	refusals := map[string]func(map[string]any){
		"PodSpec.workloadRef": func(m map[string]any) {
			_ = lifecycleGuardSet(m, map[string]any{"name": "probe", "podGroup": "other"}, "spec", "template", "spec", "workloadRef")
		},
		"JobSpec.scheduling": func(m map[string]any) {
			_ = lifecycleGuardSet(m, map[string]any{"x": 1}, "spec", "scheduling", "schedulingPolicy", "basic")
		},
		"PodSpec.evictionResponders": func(m map[string]any) {
			_ = lifecycleGuardSet(m, []any{map[string]any{"name": "example.com/probe", "priority": 999}},
				"spec", "template", "spec", "evictionResponders")
		},
		"EmptyDirVolumeSource.mode": func(m map[string]any) {
			volumes := lifecycleGuardPath(m, "spec", "template", "spec", "volumes").([]any)
			volumes[1].(map[string]any)["emptyDir"].(map[string]any)["mode"] = 420
		},
		"VolumeMount.bindMountOptions": func(m map[string]any) {
			container, _ := lifecycleGuardFirstContainer(m)
			container["volumeMounts"].([]any)[1].(map[string]any)["bindMountOptions"] = []any{"nosuid"}
		},
	}
	checked := 0
	for minor, probes := range lifecycleGuardFieldProbeTable {
		for _, probe := range probes {
			t.Run(minor+"/"+probe.field, func(t *testing.T) {
				t.Parallel()
				base := lifecycleGuardSampleBase(t)
				if probe.retained(lifecycleGuardRoundTrip(t, base)) {
					t.Fatal("the baseline without the field was read as keeping it")
				}
				manifest := lifecycleGuardCopy(base).(map[string]any)
				if err := probe.mutate(manifest); err != nil {
					t.Fatal(err)
				}
				if !probe.retained(lifecycleGuardRoundTrip(t, manifest)) {
					t.Fatal("the field as set was not read as kept")
				}
				// Only the entry named work changes: the tmp volume and its
				// mount stay as the baseline had them.
				baseContainer, _ := lifecycleGuardFirstContainer(base)
				container, _ := lifecycleGuardFirstContainer(manifest)
				if !lifecycleGuardJSONEqual(lifecycleGuardPath(manifest, "spec", "template", "spec", "volumes").([]any)[0],
					string(lifecycleGuardJSON(lifecycleGuardPath(base, "spec", "template", "spec", "volumes").([]any)[0]))) ||
					!lifecycleGuardJSONEqual(container["volumeMounts"].([]any)[0],
						string(lifecycleGuardJSON(baseContainer["volumeMounts"].([]any)[0]))) {
					t.Fatal("setting the field changed an entry other than the one named work")
				}
				refuse, ok := refusals[probe.field]
				if !ok {
					t.Fatal("no refusal sample for this probe")
				}
				refuse(manifest)
				if probe.retained(lifecycleGuardRoundTrip(t, manifest)) {
					t.Fatal("a different value was read as the field kept")
				}
			})
			checked++
		}
	}
	if checked != len(refusals) {
		t.Fatalf("checked %d probes, and %d have refusal samples", checked, len(refusals))
	}
}

func TestLifecycleGuardFieldProbesRefuseAManifestWithoutTheirTarget(t *testing.T) {
	t.Parallel()
	for field, strip := range map[string]func(map[string]any){
		"EmptyDirVolumeSource.mode": func(m map[string]any) {
			delete(lifecycleGuardPath(m, "spec", "template", "spec").(map[string]any), "volumes")
		},
		"VolumeMount.bindMountOptions": func(m map[string]any) {
			lifecycleGuardPath(m, "spec", "template", "spec").(map[string]any)["containers"] = []any{}
		},
	} {
		var probe *lifecycleGuardFieldProbe
		for _, probes := range lifecycleGuardFieldProbeTable {
			for index := range probes {
				if probes[index].field == field {
					probe = &probes[index]
				}
			}
		}
		if probe == nil {
			t.Fatalf("no probe declares %s", field)
		}
		manifest := lifecycleGuardSampleBase(t)
		strip(manifest)
		if err := probe.mutate(manifest); err == nil {
			t.Errorf("%s was set on a manifest that has nothing to set it on", field)
		}
		if probe.retained(lifecycleGuardRoundTrip(t, manifest)) {
			t.Errorf("%s was read as kept on a manifest without it", field)
		}
	}
}

func TestLifecycleGuardBaseManifest(t *testing.T) {
	t.Parallel()
	base := lifecycleGuardSampleBase(t)
	metadata := base["metadata"].(map[string]any)
	for _, key := range []string{"creationTimestamp", "generation", "managedFields", "resourceVersion", "uid"} {
		if _, ok := metadata[key]; ok {
			t.Errorf("metadata.%s survived", key)
		}
	}
	spec := base["spec"].(map[string]any)
	for _, key := range []string{"selector", "ttlSecondsAfterFinished"} {
		if _, ok := spec[key]; ok {
			t.Errorf("spec.%s survived", key)
		}
	}
	if _, ok := base["status"]; ok {
		t.Error("status survived")
	}
	if spec["backoffLimit"] == nil {
		t.Error("a field the Job carries was removed")
	}
	if metadata["name"] != "ptah-resolve-vap-probe-0123456789abcdef" || metadata["namespace"] != "proof" {
		t.Errorf("the probe is named %v in %v", metadata["name"], metadata["namespace"])
	}
	annotations := metadata["annotations"].(map[string]any)
	want := map[string]any{
		"operator.ptah.run/controller-image":         lifecycleGuardImage,
		"operator.ptah.run/controller-revision":      "e2e-controller-object-guard",
		"operator.ptah.run/controller-state-version": "1",
		"operator.ptah.run/operation-id":             "op-1",
	}
	if !lifecycleGuardJSONEqual(annotations, string(lifecycleGuardJSON(want))) {
		t.Fatalf("annotations = %v", annotations)
	}
	template := lifecycleGuardPath(base, "spec", "template", "metadata").(map[string]any)
	if !lifecycleGuardJSONEqual(template["annotations"], string(lifecycleGuardJSON(want))) {
		t.Fatalf("the Pod template carries %v, not the Job's annotations", template["annotations"])
	}
	for _, key := range []string{"creationTimestamp", "uid"} {
		if _, ok := template[key]; ok {
			t.Errorf("template metadata.%s survived", key)
		}
	}
	labels := template["labels"].(map[string]any)
	if len(labels) != 1 || labels["operator.ptah.run/operation"] != "resolve" {
		t.Errorf("template labels = %v, want the Job-controller labels gone and the rest kept", labels)
	}
	// A copy, not a shared object: stamping one leaves the other.
	annotations["x"] = "y"
	if _, shared := template["annotations"].(map[string]any)["x"]; shared {
		t.Error("the Pod template shares the Job's annotation object")
	}

	// The baseline starts from annotations the Job may not carry at all.
	var bare map[string]any
	_ = json.Unmarshal([]byte(lifecycleGuardSampleJob), &bare)
	delete(bare["metadata"].(map[string]any), "annotations")
	if manifest, err := lifecycleGuardBaseManifest(lifecycleGuardJSON(bare), lifecycleGuardImage, "3"); err != nil ||
		lifecycleGuardPath(manifest, "spec", "template", "metadata", "annotations", "operator.ptah.run/controller-state-version") != "3" {
		t.Fatalf("a Job without annotations was not stamped: %v", err)
	}

	for name, job := range map[string]string{
		"not JSON":             `{`,
		"no metadata":          `{"spec": {}}`,
		"no operation label":   `{"metadata": {"labels": {}}}`,
		"operation not a word": `{"metadata": {"labels": {"operator.ptah.run/operation": 3}}}`,
	} {
		if _, err := lifecycleGuardBaseManifest([]byte(job), lifecycleGuardImage, "1"); err == nil {
			t.Errorf("%s: the baseline was built", name)
		}
	}
}

func TestLifecycleGuardStampOtherReleaseImage(t *testing.T) {
	t.Parallel()
	manifest := lifecycleGuardSampleBase(t)
	if err := lifecycleGuardStampOtherReleaseImage(manifest); err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{
		{"metadata", "annotations", "operator.ptah.run/controller-image"},
		{"spec", "template", "metadata", "annotations", "operator.ptah.run/controller-image"},
	} {
		if got := lifecycleGuardPath(manifest, path...); got != lifecycleGuardOtherReleaseImage {
			t.Errorf("%s = %v", strings.Join(path, "."), got)
		}
	}
	if lifecycleGuardOtherReleaseImage == lifecycleGuardImage || !lifecycleImageIdentityOK(lifecycleGuardOtherReleaseImage) {
		t.Fatal("the other release's image is not a distinct exact identity")
	}
	if lifecycleGuardPath(manifest, "metadata", "annotations", "operator.ptah.run/controller-revision") != "e2e-controller-object-guard" {
		t.Error("stamping the image changed another annotation")
	}
	if err := lifecycleGuardStampOtherReleaseImage(map[string]any{"metadata": map[string]any{}}); err == nil {
		t.Error("a manifest with no annotations was stamped")
	}
}

func TestLifecycleGuardWriteEvidence(t *testing.T) {
	t.Parallel()
	schema := func() map[string]any {
		return map[string]any{
			"metadata": map[string]any{"uid": "u", "resourceVersion": "1", "name": "s"},
			"spec":     map[string]any{"suspend": true},
		}
	}
	evidence, err := lifecycleGuardWriteEvidence(schema())
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"annotations":{},"finalizers":[],"labels":{},"ownerReferences":[],"spec":{"suspend":true},"status":{},"uid":"u"}`; string(evidence) != want {
		t.Fatalf("evidence = %s, want %s", evidence, want)
	}
	unchanged := schema()
	unchanged["metadata"].(map[string]any)["resourceVersion"] = "2"
	if again, _ := lifecycleGuardWriteEvidence(unchanged); string(again) != string(evidence) {
		t.Fatal("a new resourceVersion changed the evidence")
	}
	for name, change := range map[string]func(map[string]any){
		"uid":             func(s map[string]any) { s["metadata"].(map[string]any)["uid"] = "v" },
		"labels":          func(s map[string]any) { s["metadata"].(map[string]any)["labels"] = map[string]any{"a": "b"} },
		"annotations":     func(s map[string]any) { s["metadata"].(map[string]any)["annotations"] = map[string]any{"a": "b"} },
		"ownerReferences": func(s map[string]any) { s["metadata"].(map[string]any)["ownerReferences"] = []any{"o"} },
		"finalizers":      func(s map[string]any) { s["metadata"].(map[string]any)["finalizers"] = []any{"f"} },
		"spec":            func(s map[string]any) { s["spec"] = map[string]any{"suspend": false} },
		"status":          func(s map[string]any) { s["status"] = map[string]any{"phase": "Suspended"} },
	} {
		changed := schema()
		change(changed)
		if other, _ := lifecycleGuardWriteEvidence(changed); string(other) == string(evidence) {
			t.Errorf("a change of %s left the evidence as it was", name)
		}
	}
}

func TestLifecycleGuardStatusEvidence(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		object         map[string]any
		orEmpty, exact string
	}{
		"absent":  {map[string]any{}, `{}`, `null`},
		"null":    {map[string]any{"status": nil}, `{}`, `null`},
		"present": {map[string]any{"status": map[string]any{"b": 1, "a": 2}}, `{"a":2,"b":1}`, `{"a":2,"b":1}`},
	} {
		orEmpty, _ := lifecycleGuardStatusEvidence(test.object)
		exact, _ := lifecycleGuardStatusExactly(test.object)
		if string(orEmpty) != test.orEmpty || string(exact) != test.exact {
			t.Errorf("%s: status // {} = %s, status = %s", name, orEmpty, exact)
		}
	}
}

func TestLifecycleGuardSuspendPatch(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		spec any
		want string
	}{
		"no spec":           {nil, `{"spec":{"suspend":true}}`},
		"no suspend":        {map[string]any{}, `{"spec":{"suspend":true}}`},
		"suspend null":      {map[string]any{"suspend": nil}, `{"spec":{"suspend":true}}`},
		"not suspended":     {map[string]any{"suspend": false}, `{"spec":{"suspend":true}}`},
		"already suspended": {map[string]any{"suspend": true}, `{"spec":{"suspend":false}}`},
	} {
		patch, err := lifecycleGuardSuspendPatch(map[string]any{"spec": test.spec})
		if err != nil || string(patch) != test.want {
			t.Errorf("%s: patch = %s, %v; want %s", name, patch, err, test.want)
		}
	}
	for _, suspend := range []any{"true", 1.0, map[string]any{}} {
		if _, err := lifecycleGuardSuspendPatch(map[string]any{"spec": map[string]any{"suspend": suspend}}); err == nil {
			t.Errorf("a suspend of %#v was flipped", suspend)
		}
	}
}

func TestLifecycleGuardOwnerPatch(t *testing.T) {
	t.Parallel()
	want := `{"metadata":{"ownerReferences":[{"apiVersion":"v1","kind":"ConfigMap","name":"owner","uid":"u-1"}]}}`
	if got := string(lifecycleGuardOwnerPatch("owner", "u-1")); got != want {
		t.Fatalf("patch = %s, want %s", got, want)
	}
}

func TestLifecycleGuardFinalizers(t *testing.T) {
	t.Parallel()
	withFinalizers := func(finalizers any) map[string]any {
		metadata := map[string]any{}
		if finalizers != nil {
			metadata["finalizers"] = finalizers
		}
		return map[string]any{"metadata": metadata}
	}
	before, err := lifecycleGuardFinalizers(withFinalizers(nil))
	if err != nil || len(before) != 0 {
		t.Fatalf("absent finalizers read as %v, %v", before, err)
	}
	if _, err := lifecycleGuardFinalizers(withFinalizers("x")); err == nil {
		t.Fatal("a finalizers field that is not a list was read")
	}
	if lifecycleGuardHasActiveOperation([]any{"a"}) || !lifecycleGuardHasActiveOperation([]any{"a", lifecycleGuardActiveOperationFinalizer}) {
		t.Fatal("the active-operation finalizer was not told apart")
	}
	existing := []any{"example.com/keep"}
	if got := string(lifecycleGuardFinalizersPatch(existing, true)); got != `{"metadata":{"finalizers":["example.com/keep","operator.ptah.run/active-operation"]}}` {
		t.Fatalf("add patch = %s", got)
	}
	if got := string(lifecycleGuardFinalizersPatch(existing, false)); got != `{"metadata":{"finalizers":["example.com/keep"]}}` {
		t.Fatalf("remove patch = %s", got)
	}
	if got := string(lifecycleGuardFinalizersPatch([]any{}, false)); got != `{"metadata":{"finalizers":[]}}` {
		t.Fatalf("remove patch from none = %s", got)
	}
	if len(existing) != 1 {
		t.Fatal("building the add patch changed the list it was given")
	}
	if !lifecycleGuardAddedExactly(withFinalizers([]any{"example.com/keep", lifecycleGuardActiveOperationFinalizer}), existing) {
		t.Fatal("the finalizer appended to the list was not recognized")
	}
	if !lifecycleGuardAddedExactly(withFinalizers([]any{lifecycleGuardActiveOperationFinalizer}), []any{}) {
		t.Fatal("the finalizer added to none was not recognized")
	}
	for name, after := range map[string]any{
		"not added":        []any{"example.com/keep"},
		"added first":      []any{lifecycleGuardActiveOperationFinalizer, "example.com/keep"},
		"added twice":      []any{"example.com/keep", lifecycleGuardActiveOperationFinalizer, lifecycleGuardActiveOperationFinalizer},
		"another replaced": []any{"example.com/other", lifecycleGuardActiveOperationFinalizer},
		"all removed":      nil,
	} {
		if lifecycleGuardAddedExactly(withFinalizers(after), existing) {
			t.Errorf("%s: read as exactly the manager's finalizer added", name)
		}
	}
}

func TestLifecycleGuardRefusalTexts(t *testing.T) {
	t.Parallel()
	refusal := []byte("Error from server (Forbidden): admission webhook denied the request: " + lifecycleGuardWriteDenial + ": spec")
	if !lifecycleGuardSaid(lifecycleGuardWriteDenial, nil, refusal) || !lifecycleGuardSaid(lifecycleGuardWriteDenial, refusal, nil) {
		t.Fatal("the refusal was not found on either stream")
	}
	if lifecycleGuardSaid(lifecycleGuardWriteDenial, []byte("Ptah controller write guard rejected"), []byte("")) {
		t.Fatal("a refusal for another reason passed")
	}
	if lifecycleGuardSaid(lifecycleGuardWriteDenial) {
		t.Fatal("no streams passed")
	}

	long := strings.Repeat("a\n", 400)
	head := lifecycleGuardHead([]byte(long))
	if len(head) != 600 || strings.Contains(head, "\n") {
		t.Fatalf("head is %d bytes and keeps newlines: %v", len(head), strings.Contains(head, "\n"))
	}
	if lifecycleGuardHead([]byte("short\nrefusal")) != "short refusal" {
		t.Fatal("a short refusal was not kept whole on one line")
	}

	for _, unreachable := range []string{
		`Internal error occurred: failed calling webhook "vjob.operator.ptah.run": Post "https://...": dial tcp: connect: connection refused`,
		`no endpoints available for service "ptah-operator-webhook"`,
		`service unavailable`,
	} {
		if !lifecycleGuardWebhookUnreachable.MatchString(unreachable) {
			t.Errorf("%q was not read as an unreachable webhook", unreachable)
		}
	}
	for _, answered := range []string{
		"admission webhook \"vjob.operator.ptah.run\" denied the request: " + lifecycleGuardJobSemanticBoundary,
		"ValidatingAdmissionPolicy denied request: " + lifecycleGuardJobVAPDenial,
		"Service Unavailable",
	} {
		if lifecycleGuardWebhookUnreachable.MatchString(answered) {
			t.Errorf("%q was read as an unreachable webhook", answered)
		}
	}
}

func TestLifecycleGuardDirectWriteProbes(t *testing.T) {
	t.Parallel()
	probes := lifecycleGuardDirectWriteProbes("proof", "crd-upgrade-proof")
	if len(probes) != 2 || probes[0].kind != "plan projection ConfigMap" || probes[1].kind != "PtahSchemaPlanChunk" {
		t.Fatalf("probes = %v", probes)
	}
	for _, probe := range probes {
		manifest := probe.manifest
		if lifecycleGuardPath(manifest, "metadata", "name") != "ptah-plan-111111111111111111111111-000" ||
			lifecycleGuardPath(manifest, "metadata", "namespace") != "proof" ||
			lifecycleGuardPath(manifest, "metadata", "labels", "operator.ptah.run/plan") != lifecycleGuardPlanName ||
			lifecycleGuardPath(manifest, "metadata", "labels", "operator.ptah.run/schema") != "crd-upgrade-proof" {
			t.Errorf("%s metadata = %v", probe.kind, manifest["metadata"])
		}
		owners := lifecycleGuardPath(manifest, "metadata", "ownerReferences").([]any)
		if !lifecycleGuardJSONEqual(owners, `[{"apiVersion":"operator.ptah.run/v1alpha1","kind":"PtahSchemaPlan","name":"ptah-plan-111111111111111111111111","uid":"11111111-1111-1111-1111-111111111111","controller":true,"blockOwnerDeletion":true}]`) {
			t.Errorf("%s owners = %v", probe.kind, owners)
		}
	}
	if !lifecycleGuardJSONEqual(probes[0].manifest["binaryData"], `{"chunk":"cHJvYmU="}`) || probes[0].manifest["immutable"] != true {
		t.Errorf("the ConfigMap probe carries %v", probes[0].manifest)
	}
	if !lifecycleGuardJSONEqual(probes[1].manifest["spec"], `{"data":"cHJvYmU="}`) {
		t.Errorf("the chunk probe carries %v", probes[1].manifest["spec"])
	}
	// Each probe has a metadata object of its own.
	probes[0].manifest["metadata"].(map[string]any)["name"] = "changed"
	if lifecycleGuardPath(probes[1].manifest, "metadata", "name") == "changed" {
		t.Error("the two probes share a metadata object")
	}
}

func TestLifecycleGuardAdmissionSingleton(t *testing.T) {
	t.Parallel()
	configuration := map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingWebhookConfiguration",
		"metadata": map[string]any{
			"name": "ptah-operator-admission", "uid": "u", "resourceVersion": "1", "generation": 2,
			"creationTimestamp": "t", "managedFields": []any{"m"}, "annotations": map[string]any{"a": "b"},
		},
		"webhooks": []any{
			map[string]any{"name": "vapproval.operator.ptah.run", "clientConfig": map[string]any{"service": map[string]any{"name": "ptah-webhook"}}},
			map[string]any{"name": "vschema.operator.ptah.run"},
		},
	}
	snapshot, err := lifecycleGuardAdmissionSnapshot(configuration)
	if err != nil {
		t.Fatal(err)
	}
	var restored map[string]any
	_ = json.Unmarshal(snapshot, &restored)
	if !lifecycleGuardJSONEqual(restored["metadata"], `{"name":"ptah-operator-admission","annotations":{"a":"b"}}`) ||
		len(restored["webhooks"].([]any)) != 2 {
		t.Fatalf("snapshot = %s", snapshot)
	}
	if configuration["metadata"].(map[string]any)["uid"] != "u" {
		t.Fatal("the snapshot changed the configuration it was taken from")
	}
	if name, service := lifecycleGuardFirstWebhook(configuration); name != lifecycleGuardApprovalWebhook || service != "ptah-webhook" {
		t.Fatalf("first webhook = %q, %q", name, service)
	}
	if name, service := lifecycleGuardFirstWebhook(map[string]any{"webhooks": []any{}}); name != "" || service != "" {
		t.Fatal("an empty configuration named a webhook")
	}
	if name, service := lifecycleGuardFirstWebhook(map[string]any{"webhooks": []any{map[string]any{"name": "w"}}}); name != "w" || service != "" {
		t.Fatalf("a webhook with no Service read as %q, %q", name, service)
	}
}

func TestLifecycleGuardJSONEqual(t *testing.T) {
	t.Parallel()
	if !lifecycleGuardJSONEqual(map[string]any{"priority": 1000}, `{"priority":1000}`) ||
		!lifecycleGuardJSONEqual(448.0, `448`) || !lifecycleGuardJSONEqual(int64(448), `448`) {
		t.Fatal("equal values of different Go types were told apart")
	}
	for name, test := range map[string]struct {
		value   any
		literal string
	}{
		"extra key":     {map[string]any{"a": 1, "b": 2}, `{"a":1}`},
		"missing key":   {map[string]any{}, `{"a":1}`},
		"list order":    {[]any{"b", "a"}, `["a","b"]`},
		"null":          {nil, `{}`},
		"string number": {"448", `448`},
		"bad literal":   {1, `{`},
	} {
		if lifecycleGuardJSONEqual(test.value, test.literal) {
			t.Errorf("%s: read as equal", name)
		}
	}
}
