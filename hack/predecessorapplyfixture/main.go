// Command predecessorapplyfixture prepares a current-contract plan and status
// bundle for the upgrade E2E, so the release the upgrade retires claims and
// dispatches a real Apply of its own. That manager remains responsible for
// claiming the operation, resolving admission, and creating the Job: what this
// command writes is the plan and the status that make the Apply the manager's
// own decision rather than an object the harness invented.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controller"
	"github.com/stokaro/ptah-operator/internal/coordination"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/ocireference"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const verificationPolicyKey = "policy.yaml"

type options struct {
	schemaPath     string
	managerJobPath string
	planPath       string
	policyUID      string
	policyPath     string
	databaseURL    string
}

// managerIdentity is the release of the manager the plan records as its
// publisher. The execution binding in status no longer carries it, because it
// binds nothing, so it is read from a Job that manager dispatched.
type managerIdentity struct {
	controllerImage    string
	controllerRevision string
	runnerImage        string
}

type fixtureBundle struct {
	Plan         operatorv1alpha1.PtahSchemaPlan   `json:"plan"`
	SchemaStatus operatorv1alpha1.PtahSchemaStatus `json:"schemaStatus"`
}

func main() {
	var opts options
	flag.StringVar(&opts.schemaPath, "schema", "", "path to the live predecessor PtahSchema JSON")
	flag.StringVar(&opts.managerJobPath, "manager-job", "",
		"path to the JSON of a Job the live predecessor manager dispatched, which records its identity")
	flag.StringVar(&opts.planPath, "plan", "", "path to the exact native plan JSON")
	flag.StringVar(&opts.policyUID, "policy-uid", "", "UID of the immutable verification policy ConfigMap")
	flag.StringVar(&opts.policyPath, "policy", "", "path to the projected verification policy bytes")
	flag.StringVar(&opts.databaseURL, "database-url", "",
		"exact database URL the Apply Job resolves, used to bind the target identity")
	flag.Parse()
	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "predecessorapplyfixture:", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	if opts.schemaPath == "" || opts.managerJobPath == "" || opts.planPath == "" || opts.policyUID == "" ||
		opts.policyPath == "" {
		return errors.New("--schema, --manager-job, --plan, --policy-uid, and --policy are required")
	}
	if opts.databaseURL == "" {
		return errors.New("--database-url is required")
	}
	schemaData, err := os.ReadFile(opts.schemaPath)
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	planData, err := os.ReadFile(opts.planPath)
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}
	policyData, err := os.ReadFile(opts.policyPath)
	if err != nil {
		return fmt.Errorf("read policy: %w", err)
	}
	var schema operatorv1alpha1.PtahSchema
	if err := json.Unmarshal(schemaData, &schema); err != nil {
		return fmt.Errorf("decode schema: %w", err)
	}
	managerJobData, err := os.ReadFile(opts.managerJobPath)
	if err != nil {
		return fmt.Errorf("read manager Job: %w", err)
	}
	var managerJob batchv1.Job
	if err := json.Unmarshal(managerJobData, &managerJob); err != nil {
		return fmt.Errorf("decode manager Job: %w", err)
	}
	manager, err := managerIdentityOf(&managerJob)
	if err != nil {
		return err
	}
	bundle, err := buildFixture(
		&schema, manager, planData, opts.policyUID, policyData, opts.databaseURL, time.Now().UTC())
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(bundle); err != nil {
		return fmt.Errorf("encode fixture: %w", err)
	}
	return nil
}

// managerIdentityOf reads the identity a manager-dispatched Job records, and
// refuses a Job that records none.
func managerIdentityOf(job *batchv1.Job) (managerIdentity, error) {
	controllerImage, controllerRevision, runnerImage := workload.ManagerIdentityOf(job)
	if controllerImage == "" || controllerRevision == "" || runnerImage == "" {
		return managerIdentity{}, errors.New("manager Job records no complete manager identity")
	}
	return managerIdentity{
		controllerImage:    controllerImage,
		controllerRevision: controllerRevision,
		runnerImage:        runnerImage,
	}, nil
}

func buildFixture(
	schema *operatorv1alpha1.PtahSchema,
	manager managerIdentity,
	planData []byte,
	policyUID string,
	policyData []byte,
	databaseURL string,
	now time.Time,
) (fixtureBundle, error) {
	if schema == nil || schema.Name == "" || schema.Namespace == "" || schema.UID == "" {
		return fixtureBundle{}, errors.New("schema must have a namespace, name, and UID")
	}
	if schema.Status.ExecutionBinding == nil {
		return fixtureBundle{}, errors.New("schema lacks a predecessor execution binding")
	}
	binding := schema.Status.ExecutionBinding.DeepCopy()
	// The plan records the manager that published it, and the admission guard
	// over plan writes requires the record. The binding carries what the plan
	// binds; a binding without it belongs to a release this fixture cannot
	// describe.
	if binding.ControllerStateVersion == 0 || binding.PtahVersion == "" || binding.ExecutorImage == "" ||
		binding.RunnerProtocolVersion == 0 {
		return fixtureBundle{}, errors.New("schema execution binding is incomplete")
	}
	if manager.controllerImage == "" || manager.controllerRevision == "" || manager.runnerImage == "" {
		return fixtureBundle{}, errors.New("the publishing manager's identity is incomplete")
	}
	if strings.TrimSpace(policyUID) == "" || len(policyData) == 0 {
		return fixtureBundle{}, errors.New("verification policy identity and bytes are required")
	}
	decoded, err := dataplane.DecodePlan(planData, string(schema.Spec.Target.Engine))
	if err != nil {
		return fixtureBundle{}, fmt.Errorf("validate plan: %w", err)
	}
	if decoded.Destructive {
		return fixtureBundle{}, errors.New("upgrade fixture plan must be non-destructive")
	}
	// The fixture writes the plan as ReadyToApply under Always, which a plan
	// that changes privileges never is.
	if len(decoded.PrivilegeChanges) > 0 {
		return fixtureBundle{}, errors.New("upgrade fixture plan must change no privilege")
	}
	coordinationDigest, err := coordination.Digest(schema.Namespace, schema.Spec.Target)
	if err != nil {
		return fixtureBundle{}, fmt.Errorf("derive coordination digest: %w", err)
	}
	policyFingerprint, err := predecessorPolicyFingerprint(schema)
	if err != nil {
		return fixtureBundle{}, err
	}
	contentDigest := fingerprint.DigestBytes(planData)
	desiredReference, err := ocireference.Parse(schema.Spec.Desired.OCIRef)
	if err != nil || !desiredReference.IsDigest {
		return fixtureBundle{}, errors.New("schema desired reference must be digest-pinned")
	}
	artifactDigest := desiredReference.Selector
	// The runner recomputes this from the database URL it resolves and refuses
	// the operation when it differs from what planning recorded, which is the
	// guard that stops a plan being applied to a target it was not planned
	// against. A placeholder string stood here, so the two could never agree:
	// every predecessor Apply exited with "database target identity changed
	// after planning" before opening a single connection, and the barrier it
	// was supposed to block on saw no contention at all.
	//
	// It is computed with runner.TargetIdentityDigest, the same function the
	// runner uses, rather than restated -- a second spelling of this identity
	// is exactly what failed here.
	targetIdentityDigest, err := runner.TargetIdentityDigest(databaseURL)
	if err != nil {
		return fixtureBundle{}, fmt.Errorf("derive predecessor Apply target identity: %w", err)
	}
	verificationPolicyDigest := fingerprint.DigestBytes(policyData)
	planFingerprint, err := (fingerprint.PlanBinding{
		ContractVersion:          fingerprint.CurrentPlanContractVersion,
		SchemaUID:                string(schema.UID),
		PlanContentDigest:        contentDigest,
		ArtifactDigest:           artifactDigest,
		CoordinationDigest:       coordinationDigest,
		TargetIdentityDigest:     targetIdentityDigest,
		ActualStateFingerprint:   decoded.FromFingerprint,
		DesiredStateFingerprint:  decoded.ToFingerprint,
		PolicyFingerprint:        policyFingerprint,
		VerificationPolicyUID:    policyUID,
		VerificationPolicyDigest: verificationPolicyDigest,
		ExecutionBindingID:       binding.Epoch,
		ControllerStateVersion:   binding.ControllerStateVersion,
		PtahVersion:              binding.PtahVersion,
		ExecutorImage:            binding.ExecutorImage,
		RunnerProtocolVersion:    binding.RunnerProtocolVersion,
	}).Fingerprint()
	if err != nil {
		return fixtureBundle{}, fmt.Errorf("fingerprint plan: %w", err)
	}
	planName := "ptah-plan-" + strings.TrimPrefix(planFingerprint, "sha256:")[:24]
	chunkName := planName + "-000"
	controller := true
	blockDeletion := true
	plan := operatorv1alpha1.PtahSchemaPlan{
		TypeMeta: metav1.TypeMeta{
			APIVersion: operatorv1alpha1.GroupVersion.String(),
			Kind:       "PtahSchemaPlan",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: schema.Namespace,
			Name:      planName,
			Labels:    map[string]string{planstore.LabelSchema: schema.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         operatorv1alpha1.GroupVersion.String(),
				Kind:               "PtahSchema",
				Name:               schema.Name,
				UID:                schema.UID,
				Controller:         &controller,
				BlockOwnerDeletion: &blockDeletion,
			}},
		},
		Spec: operatorv1alpha1.PtahSchemaPlanSpec{
			ContractVersion:          fingerprint.CurrentPlanContractVersion,
			SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
			Fingerprint:              planFingerprint,
			ContentDigest:            contentDigest,
			Size:                     int64(len(planData)),
			ArtifactDigest:           artifactDigest,
			CoordinationDigest:       coordinationDigest,
			TargetIdentityDigest:     targetIdentityDigest,
			ActualStateFingerprint:   decoded.FromFingerprint,
			DesiredStateFingerprint:  decoded.ToFingerprint,
			PolicyFingerprint:        policyFingerprint,
			VerificationPolicyUID:    types.UID(policyUID),
			VerificationPolicyDigest: verificationPolicyDigest,
			ExecutionBindingID:       binding.Epoch,
			ControllerImage:          manager.controllerImage,
			ControllerRevision:       manager.controllerRevision,
			ControllerStateVersion:   binding.ControllerStateVersion,
			PtahVersion:              binding.PtahVersion,
			ExecutorImage:            binding.ExecutorImage,
			RunnerImage:              manager.runnerImage,
			RunnerProtocolVersion:    binding.RunnerProtocolVersion,
			Dialect:                  decoded.Dialect,
			Destructive:              false,
			StatementCount:           int32(len(decoded.Statements)),
			Chunks: []operatorv1alpha1.PlanChunkReference{{
				Name: chunkName, Key: planstore.ChunkDataKey, Index: 0,
				Digest: contentDigest, Size: int32(len(planData)),
			}},
		},
	}
	transitionTime := metav1.NewTime(now)
	nextReconciliation := metav1.NewTime(now.Add(24 * time.Hour))
	status := operatorv1alpha1.PtahSchemaStatus{
		ObservedGeneration:     schema.Generation,
		Phase:                  operatorv1alpha1.PhaseReadyToApply,
		ExecutionBinding:       binding,
		NextReconciliationTime: &nextReconciliation,
		Source: operatorv1alpha1.SchemaSourceStatus{
			RequestedReference:       schema.Spec.Desired.OCIRef,
			ResolvedReference:        schema.Spec.Desired.OCIRef,
			Digest:                   artifactDigest,
			MediaType:                "application/vnd.oci.image.manifest.v1+json",
			ArtifactType:             dataplane.SchemaArtifactType,
			Size:                     1,
			Verified:                 true,
			VerificationPolicyUID:    types.UID(policyUID),
			VerificationPolicyDigest: verificationPolicyDigest,
			ResolvedAt:               &transitionTime,
			VerifiedAt:               &transitionTime,
		},
		Target: operatorv1alpha1.TargetStatus{
			Engine:             schema.Spec.Target.Engine,
			CoordinationDigest: coordinationDigest,
			IdentityDigest:     targetIdentityDigest,
			DriftReportDigest:  fingerprint.DigestBytes([]byte("predecessor Apply upgrade drift report")),
			LastObservedAt:     &transitionTime,
		},
		Plan: &operatorv1alpha1.CurrentPlanStatus{
			Name:                     planName,
			Fingerprint:              planFingerprint,
			ContentDigest:            contentDigest,
			ArtifactDigest:           artifactDigest,
			CoordinationDigest:       coordinationDigest,
			TargetIdentityDigest:     targetIdentityDigest,
			ActualStateFingerprint:   decoded.FromFingerprint,
			DesiredStateFingerprint:  decoded.ToFingerprint,
			PolicyFingerprint:        policyFingerprint,
			VerificationPolicyUID:    types.UID(policyUID),
			VerificationPolicyDigest: verificationPolicyDigest,
			ExecutionBindingID:       binding.Epoch,
			ControllerImage:          manager.controllerImage,
			ControllerRevision:       manager.controllerRevision,
			ControllerStateVersion:   binding.ControllerStateVersion,
			PtahVersion:              binding.PtahVersion,
			ExecutorImage:            binding.ExecutorImage,
			RunnerImage:              manager.runnerImage,
			RunnerProtocolVersion:    binding.RunnerProtocolVersion,
			Destructive:              false,
			StatementCount:           int32(len(decoded.Statements)),
			CreatedAt:                transitionTime,
		},
		Conditions: []metav1.Condition{
			{
				Type: "PlanReady", Status: metav1.ConditionTrue, Reason: "CurrentPlan",
				Message: "Exact predecessor plan is ready", ObservedGeneration: schema.Generation,
				LastTransitionTime: transitionTime,
			},
			{
				Type: "ApprovalRequired", Status: metav1.ConditionFalse, Reason: "NotRequired",
				Message: "Policy permits this non-destructive plan", ObservedGeneration: schema.Generation,
				LastTransitionTime: transitionTime,
			},
			{
				Type: "Ready", Status: metav1.ConditionFalse, Reason: "ApplyPending",
				Message: "Exact predecessor plan is ready to apply", ObservedGeneration: schema.Generation,
				LastTransitionTime: transitionTime,
			},
		},
	}
	return fixtureBundle{Plan: plan, SchemaStatus: status}, nil
}

// predecessorPolicyFingerprint asks the controller for the policy binding it
// would record. A plan the controller cannot reproduce is refused by the write
// webhook before it is stored, so a second spelling of this fingerprint here
// would be a fixture that compiles and never works.
func predecessorPolicyFingerprint(schema *operatorv1alpha1.PtahSchema) (string, error) {
	if schema == nil {
		return "", errors.New("schema is required for policy fingerprint")
	}
	return controller.PolicyFingerprint(schema)
}
