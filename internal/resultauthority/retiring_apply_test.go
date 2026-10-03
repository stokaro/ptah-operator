package resultauthority

import (
	"errors"
	"strings"
	"testing"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"k8s.io/apimachinery/pkg/types"
)

func retiringApplyFixture(t *testing.T) *resulttest.Fixture {
	t.Helper()
	f := resulttest.New(t, "schema-apply-admitted-scheduling")
	schema := f.Subject.(*api.PtahSchema)
	op := schema.Status.ActiveOperation
	schema.Status.PendingObservation = &api.PendingObservationStatus{
		Outcome: api.PendingObservationOutcomeUnknown, ApplyOperationID: op.ID,
		ApplyJobName: op.JobName, ApplyJobUID: op.JobUID, ApplyGeneration: schema.Generation,
		ApplyPodCount: 1, ApplyPodUIDs: []types.UID{f.Pod.UID},
		AdmissionSnapshot: op.AdmissionSnapshot, Plan: *schema.Status.Plan, Target: *op.Target,
	}
	schema.Status.PendingBindingRetirement = &api.BindingRetirementStatus{
		RetiredEpoch: op.ExecutionBindingID,
		Job:          &api.RetiredJobStatus{Operation: api.OperationApply, Name: op.JobName, UID: op.JobUID},
	}
	schema.Status.ActiveOperation = nil
	schema.Status.Plan = nil
	schema.Status.ExecutionBinding.Epoch = "v1-" + strings.Repeat("4", 32)
	return f
}

func TestRunningSchemaApplyCanDeliverAfterBindingRotation(t *testing.T) {
	f := retiringApplyFixture(t)
	if err := (Authorizer{Reader: f.Client(t)}).Check(t.Context(), f.Identity); err != nil {
		t.Fatalf("original running Apply lost delivery authority after epoch retirement: %v", err)
	}
}

func TestRetiringSchemaApplyStillRequiresItsExactClaim(t *testing.T) {
	for name, change := range map[string]func(*resulttest.Fixture){
		"no retirement":  func(f *resulttest.Fixture) { f.Subject.(*api.PtahSchema).Status.PendingBindingRetirement = nil },
		"no retired Job": func(f *resulttest.Fixture) { f.Subject.(*api.PtahSchema).Status.PendingBindingRetirement.Job = nil },
		"retired Job UID": func(f *resulttest.Fixture) {
			f.Subject.(*api.PtahSchema).Status.PendingBindingRetirement.Job.UID = "other"
		},
		"retired operation": func(f *resulttest.Fixture) {
			f.Subject.(*api.PtahSchema).Status.PendingBindingRetirement.Job.Operation = api.OperationObserve
		},
		"retired epoch": func(f *resulttest.Fixture) {
			f.Subject.(*api.PtahSchema).Status.PendingBindingRetirement.RetiredEpoch = "v1-" + strings.Repeat("5", 32)
		},
		"not rotated": func(f *resulttest.Fixture) {
			f.Subject.(*api.PtahSchema).Status.ExecutionBinding.Epoch = f.Identity.Binding.ExecutionBindingID
		},
		"pending generation": func(f *resulttest.Fixture) { f.Subject.(*api.PtahSchema).Status.PendingObservation.ApplyGeneration++ },
		"pending Pod UID": func(f *resulttest.Fixture) {
			f.Subject.(*api.PtahSchema).Status.PendingObservation.ApplyPodUIDs[0] = "other"
		},
		"ambiguous Pods":     func(f *resulttest.Fixture) { f.Subject.(*api.PtahSchema).Status.PendingObservation.ApplyPodCount = 2 },
		"different input":    func(f *resulttest.Fixture) { f.Identity.Binding.InputFingerprint = "sha256:" + strings.Repeat("9", 64) },
		"different template": func(f *resulttest.Fixture) { f.Job.Spec.Template.Spec.Containers[0].Image = "other" },
		"proof cleared":      func(f *resulttest.Fixture) { f.Subject.(*api.PtahSchema).Status.PendingObservation = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := retiringApplyFixture(t)
			change(f)
			if err := (Authorizer{Reader: f.Client(t)}).Check(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatalf("changed retirement authorized delivery: %v", err)
			}
		})
	}
}
