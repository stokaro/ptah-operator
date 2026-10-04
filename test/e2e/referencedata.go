package e2e

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// The reference-data phase's own names and bounds. The fixtures the data
// plane stood up in the namespace are reused rather than rebuilt: a second
// PostgreSQL, a second MySQL and a second registry credential would be a
// second answer to questions the earlier phase already answered, and the first
// one to drift would do so silently. What this phase adds per engine is a
// database of its own, so a declared row set meets tables that do not exist
// yet.
const (
	// referenceDatabase is the database the proof reconciles. It is one no
	// other phase touched: "works on first creation of a database, when the
	// target tables do not exist yet" is a scope line rather than a hope only
	// if the tables really do not exist.
	referenceDatabase = "ptah_e2e_reference"
	// referencePolicy is the verification policy the reference schemas name,
	// and referencePolicyKey the key it is stored under.
	referencePolicy    = "e2e-reference-verification-policy"
	referencePolicyKey = "policy.yaml"
	// referenceSchemaArtifactType is the artifact type the policy has to pin.
	referenceSchemaArtifactType = "application/vnd.stokaro.ptah.schema.v1"
	// referenceInterval is the reconciliation interval the schema runs at,
	// and referenceRepeatInterval the shorter one the repeated-reconciliation
	// row sets for its window and then takes back.
	referenceInterval       = "45s"
	referenceRepeatInterval = "20s"
	// referencePoll is how often the phase reads the schema, the plan and the
	// publisher.
	referencePoll = 5 * time.Second
	// referenceRepeatWindow is how long the repeated-reconciliation row watches
	// the schema stay in its converged cycle, reading every referenceRepeatPoll.
	referenceRepeatWindow = 90 * time.Second
	referenceRepeatPoll   = 10 * time.Second
	// referencePublisherComponent labels the Jobs that publish the fixtures.
	referencePublisherComponent = "e2e-reference-publisher"
	// referenceExternalEditLine is the reference-data line the view has to
	// print for the drift the stale-approval row makes by hand: one managed
	// row edited outside the operator, nothing added and nothing removed. The
	// whole line is the literal rather than three numbers a format string here
	// would arrange, because the renderer arranges it; a unit test compares it
	// with what the renderer writes.
	referenceExternalEditLine = "Reference data:   0 to insert, 1 to update, 0 to delete"
	// referenceSchemaViewPrefix is the start of the line the view names the
	// schema it read with, pinned to the renderer the same way.
	referenceSchemaViewPrefix = "Schema:           "
	// referencePlanDocumentKey is the plan document's own top-level key,
	// present in every plaintext plan Ptah has written and vanishingly unlikely
	// to appear by chance in the base64 of a sealed box.
	referencePlanDocumentKey = `"format_version"`
	// referenceMinimumRowValues is how many declared values the row scanner
	// needs before it means anything.
	referenceMinimumRowValues = 5
)

// referenceNames are the names one engine's reference-data run uses.
type referenceNames struct {
	// secret holds the reference database's URL.
	secret string
	// schema is the PtahSchema, and approval the prefix every approval of its
	// plans starts with.
	schema, approval string
	// coordinationKey is the realm key the schema claims.
	coordinationKey string
	// artifact is where the fixtures are published.
	artifact string
	// configMapPrefix and jobPrefix start the publisher objects' names, which
	// carry the revision they published.
	configMapPrefix, jobPrefix string
}

// referenceNamesFor names everything one engine's reference-data run needs.
// The two engines run the same proof against different servers, and naming
// the differences in one place is what keeps the second engine from becoming
// a second proof.
func referenceNamesFor(engine, registryHost, repository string) referenceNames {
	return referenceNames{
		secret:          "e2e-" + engine + "-reference-db",
		schema:          "e2e-reference-" + engine,
		approval:        "e2e-reference-" + engine + "-approval",
		coordinationKey: "e2e/reference/" + engine,
		artifact:        "oci://" + registryHost + "/" + repository + "/reference-" + engine + ":stable",
		configMapPrefix: "e2e-reference-" + engine + "-",
		jobPrefix:       "e2e-push-reference-" + engine + "-",
	}
}

// referenceRepository is where the phase publishes: a repository of its own
// on a rerun, because Ptah refuses to move a version tag that already names a
// different digest, so an earlier run's tags can be neither reused nor
// replaced.
func referenceRepository(rerun string) (string, error) {
	if rerun == "" {
		return "schemas", nil
	}
	if !rerunMarker.MatchString(rerun) {
		return "", fmt.Errorf("E2E_PHASE_RERUN must be r followed by digits, not %s", rerun)
	}
	return "schemas-" + rerun, nil
}

// referenceInputsOK is the refusal the reference-data phase gives before it
// reads the cluster: the engine the driver named has to be the phase's own,
// and the images have to be pinned.
func referenceInputsOK(engine, phaseEngine string, images ...string) error {
	for _, image := range images {
		if !digestPinnedImage.MatchString(image) {
			return fmt.Errorf("reference-data phase images must be pinned by a lowercase SHA-256 digest: %s", image)
		}
	}
	if _, err := migrationEngineFor(engine); err != nil {
		return err
	}
	if engine != phaseEngine {
		return fmt.Errorf("E2E_ENGINE names %q, and this phase runs %s", engine, phaseEngine)
	}
	return nil
}

// referenceSchemaViewLine is the whole line the view names the schema it read
// with.
func referenceSchemaViewLine(namespace, schema string) string {
	return referenceSchemaViewPrefix + namespace + "/" + schema
}

// referenceViewHasLine reports whether the view printed the line whole, as grep -Fx
// matched it.
func referenceViewHasLine(view []byte, line string) bool {
	return slices.Contains(strings.Split(strings.TrimRight(string(view), "\n"), "\n"), line)
}

// referenceNameLine is one `name:` line of a row fixture, as the script's sed
// read it: a key at the start of the line, after nothing but whitespace.
var referenceNameLine = regexp.MustCompile(`^[[:space:]]*name:[[:space:]]*(.*)$`)

// referenceDeclaredRowValues reads the values the row scanner looks for out of the
// fixtures rather than out of a list here, so a fixture that gains a row
// cannot leave the scanner behind. Only the names are used: a two-letter code
// would match base64 and turn the scanner into one that fires on everything.
func referenceDeclaredRowValues(root string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(root, "testdata", "e2e", "reference", "*", "[a-z]*.yaml"))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		content, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		lines := bufio.NewScanner(bytes.NewReader(content))
		for lines.Scan() {
			match := referenceNameLine.FindStringSubmatch(lines.Text())
			if match == nil {
				continue
			}
			if value := strings.TrimRight(match[1], " \t\r\n\v\f"); value != "" {
				seen[value] = true
			}
		}
		if err := lines.Err(); err != nil {
			return nil, err
		}
	}
	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	sort.Strings(values)
	if len(values) < referenceMinimumRowValues {
		return nil, errors.New("the declared row fixtures yielded too few values for the row scanner to mean anything")
	}
	return values, nil
}

// referenceRowScanner finds a declared row value in evidence the phase reads: "table
// rows never reach status, Events, or ordinary logs". The values are the
// declared ones, so a leak is a literal match rather than a shape to
// recognize. It is grep -F -f over the values, line by line.
type referenceRowScanner struct {
	values [][]byte
}

func newReferenceRowScanner(values ...string) (referenceRowScanner, error) {
	if len(values) == 0 {
		return referenceRowScanner{}, errors.New("row scanner has no declared values to look for")
	}
	scanner := referenceRowScanner{}
	for _, value := range values {
		// An empty value matches every line, which would turn the scanner into
		// one that always fires, and a scanner that always fires is one
		// somebody removes.
		if value == "" || strings.Contains(value, "\n") {
			return referenceRowScanner{}, errors.New("row scanner has an empty declared value")
		}
		scanner.values = append(scanner.values, []byte(value))
	}
	return scanner, nil
}

// ready reports whether the scanner has values to look for.
func (s referenceRowScanner) ready() bool {
	return len(s.values) > 0
}

// matches returns every line of content that carries a declared value.
func (s referenceRowScanner) matches(content []byte) []string {
	var lines []string
	for line := range bytes.SplitSeq(content, []byte("\n")) {
		for _, value := range s.values {
			if bytes.Contains(line, value) {
				lines = append(lines, string(line))
				break
			}
		}
	}
	return lines
}

// referenceRefusalComplete is one reading that carries the whole
// protected-table refusal: no plan, Blocked, and exactly InSync, PlanReady and
// Ready false for ProtectedTable.
//
// A blocked resource still resolves, verifies and observes at its interval,
// and each of those passes legitimately writes Ready while the refusal stands,
// so a wait on one condition can return a document whose other conditions
// belong to the pass after it. The refusal is written in a single status
// patch, so a reading that holds all of it cannot be half of two.
func referenceRefusalComplete(schema *ptahv1alpha1.PtahSchema) bool {
	status := schema.Status
	if status.Plan != nil || status.Phase != ptahv1alpha1.PhaseBlocked {
		return false
	}
	var refused []string
	for _, condition := range status.Conditions {
		if condition.Status == metav1.ConditionFalse && condition.Reason == "ProtectedTable" {
			refused = append(refused, condition.Type)
		}
	}
	sort.Strings(refused)
	return slices.Equal(refused, []string{"InSync", "PlanReady", "Ready"})
}

// referenceExactlyOneCondition reports whether exactly one condition has the type,
// status and reason given.
func referenceExactlyOneCondition(conditions []metav1.Condition, kind string, status metav1.ConditionStatus, reason string) bool {
	count := 0
	for _, condition := range conditions {
		if condition.Type == kind && condition.Status == status && condition.Reason == reason {
			count++
		}
	}
	return count == 1
}

// referenceRepeatAllowed is a phase a schema may pass through while it
// reconciles with nothing to change.
//
// Pending is allowed because the row puts it there: the spec patch bumps the
// generation, the generation is part of the operation input fingerprint, and
// an operation already in flight when the patch lands is discarded into
// Pending. The phases that would mean a plan appeared stay out: ReadyToApply,
// AwaitingApproval and Applying each say the operator decided there was work.
func referenceRepeatAllowed(phase ptahv1alpha1.ReconciliationPhase) bool {
	switch phase {
	case ptahv1alpha1.PhaseInSync, ptahv1alpha1.PhaseObserving, ptahv1alpha1.PhasePlanning,
		ptahv1alpha1.PhaseVerifyingConvergence, ptahv1alpha1.PhaseResolving, ptahv1alpha1.PhaseVerifying,
		ptahv1alpha1.PhasePending:
		return true
	}
	return false
}

// referenceRemovalSettled is a phase the withdrawn declaration may settle in:
// converged with nothing to do, or holding a plan for approval.
func referenceRemovalSettled(phase ptahv1alpha1.ReconciliationPhase) bool {
	return phase == ptahv1alpha1.PhaseInSync || phase == ptahv1alpha1.PhaseAwaitingApproval
}

// referenceRemovalReconciled is the withdrawn declaration settled: the schema
// resolved the digest the revision was published as, and is converged or
// holding a plan for it. A phase read alone would pass on the InSync the
// schema held for the revision before.
func referenceRemovalReconciled(schema *ptahv1alpha1.PtahSchema, digest string) bool {
	return digest != "" && schema.Status.Source.Digest == digest && referenceRemovalSettled(schema.Status.Phase)
}

// referencePlanName is the plan the schema publishes, or nothing.
func referencePlanName(schema *ptahv1alpha1.PtahSchema) string {
	if schema.Status.Plan == nil {
		return ""
	}
	return schema.Status.Plan.Name
}

// referencePlanHasStatements is a published plan with at least one statement: a
// data-only change that produced a plan with none would reconcile nothing.
func referencePlanHasStatements(schema *ptahv1alpha1.PtahSchema) bool {
	return schema.Status.Plan != nil && schema.Status.Plan.StatementCount >= 1
}

// referencePlanDestructive is a published plan marked destructive.
func referencePlanDestructive(schema *ptahv1alpha1.PtahSchema) bool {
	return schema.Status.Plan != nil && schema.Status.Plan.Destructive
}

// referenceRefusalSubject is what a replaced-plan refusal has to name. Which refusal
// fires depends on where the resource sits in its cycle when the approval
// lands: the plan is no longer current, the schema is not awaiting approval,
// or it has an operation in flight. All three are the refusal the row is
// about, and only the first says "plan", so pinning that spelling would make
// the proof about the timing.
var referenceRefusalSubject = regexp.MustCompile(`(?i)plan|approval|schema`)

// referenceRefusalNamesWhatItRefused reports whether a refusal says what it refused.
func referenceRefusalNamesWhatItRefused(message string) bool {
	return referenceRefusalSubject.MatchString(message)
}

// referencePlanInTheClear reports whether a log carries the plan document's
// own shape, which neither result transport may write to diagnostics.
func referencePlanInTheClear(log []byte) bool {
	return bytes.Contains(log, []byte(referencePlanDocumentKey))
}

// referencePlanLogEvidence requires a successful Plan bound to this schema,
// Job and Pod. A durable runner may produce no diagnostic text; its validated
// receipt proves that the empty log belongs to an operation that ran.
func referencePlanLogEvidence(ctx context.Context, reader client.Reader, schema *ptahv1alpha1.PtahSchema,
	job *batchv1.Job, pod *corev1.Pod, logs []byte,
) error {
	if schema == nil || schema.UID == "" || job == nil || job.Namespace != schema.Namespace ||
		!ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, "PtahSchema", schema.Name, schema.UID) {
		return errors.New("Plan diagnostic evidence does not belong to the reference schema")
	}
	result, err := readOperationResult(ctx, reader, job, pod, runner.OperationPlan, job.Annotations[annotationOperationID], logs)
	if err != nil {
		return err
	}
	if result.Error != nil || result.ChildExitCode != 0 {
		return errors.New("Plan diagnostic evidence does not prove a successful operation")
	}
	if durableResultJob(job) && result.PlanOutcome == runner.PlanOutcomeChanges {
		if err := confidentialPlanDelivery(result, []byte(result.Stdout), logs, true); err != nil {
			return err
		}
	}
	if referencePlanInTheClear(logs) {
		return errors.New("Plan diagnostics carry the plan document in the clear")
	}
	return nil
}

// referenceRowsMismatch compares what the database holds with what the
// declaration says: the two counts and the Czech row's name. The expected
// name is folded the way the query folds its value, so a caller writes the
// name it declared. An empty expected name means the child table has no
// declared rows yet.
func referenceRowsMismatch(regions, countries, czechia, wantRegions, wantCountries, wantCzechia string) error {
	wantCzechia = trimmedSQL(wantCzechia)
	switch {
	case regions != wantRegions:
		return fmt.Errorf("regions holds %s rows, want %s", regions, wantRegions)
	case countries != wantCountries:
		return fmt.Errorf("countries holds %s rows, want %s", countries, wantCountries)
	case czechia != wantCzechia:
		return fmt.Errorf("countries.CZ is %s, want %s", czechia, wantCzechia)
	}
	return nil
}

// referencePublisherState is where a publisher Job stands: complete, failed,
// or still running. Complete is read first, as the script read it.
func referencePublisherState(job *batchv1.Job) string {
	for _, want := range []batchv1.JobConditionType{batchv1.JobComplete, batchv1.JobFailed} {
		for _, condition := range job.Status.Conditions {
			if condition.Type == want && condition.Status == corev1.ConditionTrue {
				if want == batchv1.JobComplete {
					return "complete"
				}
				return "failed"
			}
		}
	}
	return "running"
}

// referenceLatestPod is the most recently created Pod, as kubectl's
// --sort-by=.metadata.creationTimestamp and the last item chose it.
func referenceLatestPod(pods []corev1.Pod) (corev1.Pod, bool) {
	if len(pods) == 0 {
		return corev1.Pod{}, false
	}
	sorted := slices.Clone(pods)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].CreationTimestamp.Before(&sorted[j].CreationTimestamp)
	})
	return sorted[len(sorted)-1], true
}

// referenceFixtureFiles reads one revision's fixtures: every regular file in
// the directory, which is what kubectl create configmap --from-file stored,
// and the Go and YAML files among them, which are the files the publisher
// mounts. os.ReadDir returns names sorted bytewise, the order LC_ALL=C sort
// gave the script.
func referenceFixtureFiles(directory string) (map[string]any, []string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, nil, err
	}
	data := map[string]any{}
	var mounts []string
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, nil, err
		}
		data[entry.Name()] = string(content)
		if strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), ".yaml") {
			mounts = append(mounts, entry.Name())
		}
	}
	return data, mounts, nil
}

// referencePublisherJob is the Job that publishes one revision with the same
// Ptah the operator runs, from Go source and YAML row files, because that is
// how a person declares reference data. The harness owns no artifact format
// of its own.
//
// A ConfigMap volume is not a directory of files: the kubelet writes the keys
// into a timestamped directory and leaves one symlink per key beside it, so a
// walker that descends finds every file twice. A subPath mount per file gives
// the plain directory Ptah's Go parser expects.
func referencePublisherJob(namespace, name, image, configMap, reference, version string, mounts []string) map[string]any {
	registryEnv := func(name, key string) map[string]any {
		return map[string]any{"name": name, "valueFrom": map[string]any{
			"secretKeyRef": map[string]any{"name": registryAuthSecret, "key": key},
		}}
	}
	volumeMounts := []any{map[string]any{"name": "work", "mountPath": "/work"}}
	for _, file := range mounts {
		volumeMounts = append(volumeMounts, map[string]any{
			"name": "schema", "mountPath": "/schema/" + file, "subPath": file, "readOnly": true,
		})
	}
	labels := map[string]any{labelComponent: referencePublisherComponent}
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"namespace": namespace, "name": name, "labels": labels},
		"spec": map[string]any{
			"backoffLimit": int64(0), "activeDeadlineSeconds": int64(300), "ttlSecondsAfterFinished": int64(600),
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"restartPolicy": "Never", "automountServiceAccountToken": false,
					"imagePullSecrets": []any{map[string]any{"name": registryPullSecret}},
					"securityContext": map[string]any{
						"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532), "fsGroup": int64(65532),
						"seccompProfile": map[string]any{"type": "RuntimeDefault"},
					},
					"containers": []any{map[string]any{
						"name": "publisher", "image": image, "imagePullPolicy": "IfNotPresent",
						"command": []any{"/usr/local/bin/ptah"},
						"args": []any{
							"schema", "push", reference, "--root-dir", "/schema",
							"--version", version, "--plain-http",
						},
						"env": []any{
							map[string]any{"name": "HOME", "value": "/work"},
							map[string]any{"name": "TMPDIR", "value": "/work"},
							registryEnv("PTAH_OCI_USERNAME", "username"),
							registryEnv("PTAH_OCI_PASSWORD", "password"),
							registryEnv("PTAH_OCI_REGISTRY", "registry"),
						},
						"securityContext": map[string]any{
							"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
							"capabilities": map[string]any{"drop": []any{"ALL"}},
						},
						"volumeMounts": volumeMounts,
					}},
					"volumes": []any{
						map[string]any{"name": "schema", "configMap": map[string]any{"name": configMap}},
						map[string]any{"name": "work", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
					},
				},
			},
		},
	}
}

// referenceSchemaDocument is the reference schema: the engine's reference
// database, the published fixtures, a plan held for approval, and destructive
// changes allowed through that approval.
func referenceSchemaDocument(namespace, engineKind string, names referenceNames) map[string]any {
	return map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": namespace, "name": names.schema},
		"spec": map[string]any{
			"target": map[string]any{
				"engine":          engineKind,
				"coordinationKey": names.coordinationKey,
				"urlFrom":         map[string]any{"name": names.secret, "key": "url"},
			},
			"desired": map[string]any{
				"ociRef": names.artifact,
				"registryAuthFrom": map[string]any{
					"name": registryAuthSecret, "mode": "Environment",
					"usernameKey": "username", "passwordKey": "password",
				},
				"verificationPolicyFrom": map[string]any{"name": referencePolicy, "key": referencePolicyKey},
				"transport":              map[string]any{"plainHTTP": true},
			},
			"interval":  referenceInterval,
			"policy":    map[string]any{"apply": "OnApproval", "allowDestructive": true},
			"execution": map[string]any{"activeDeadlineSeconds": int64(600)},
		},
	}
}

// referenceApprovalDocument approves the plan given: the schema, the plan by
// name and UID, and the plan's own fingerprint, read off the plan itself. The
// webhook holds all three to the live plan, so an approval that named another
// value would be refused.
func referenceApprovalDocument(namespace, name, schema string, plan *ptahv1alpha1.PtahSchemaPlan) map[string]any {
	return map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchemaApproval",
		"metadata": map[string]any{"namespace": namespace, "name": name},
		"spec": map[string]any{
			"schemaRef":       map[string]any{"name": schema, "uid": string(plan.Spec.SchemaRef.UID)},
			"planRef":         map[string]any{"name": plan.Name, "uid": string(plan.UID)},
			"planFingerprint": plan.Spec.Fingerprint,
		},
	}
}

// referenceLeftover reports whether an object an earlier run may have left
// is this engine's: an approval of its schema, or a publisher ConfigMap or
// Job, each found by the prefix its name starts with. The data plane's
// objects share the kinds and the namespace, so nothing is taken by kind
// alone.
func referenceLeftover(names referenceNames, kind, name string) bool {
	switch kind {
	case "PtahSchemaApproval":
		return strings.HasPrefix(name, names.approval)
	case "ConfigMap":
		return strings.HasPrefix(name, names.configMapPrefix)
	case "Job":
		return strings.HasPrefix(name, names.jobPrefix)
	}
	return false
}
