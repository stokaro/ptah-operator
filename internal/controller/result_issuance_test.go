package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type issuanceProbe struct {
	calls    int
	identity resultdelivery.Identity
	budget   time.Duration
	wait     bool
	foreign  bool
}

func (p *issuanceProbe) Ensure(ctx context.Context, identity resultdelivery.Identity) (resultcredentials.Credential, error) {
	p.calls++
	p.identity = identity
	if deadline, ok := ctx.Deadline(); ok {
		p.budget = time.Until(deadline)
	}
	if p.wait {
		<-ctx.Done()
		return resultcredentials.Credential{}, ctx.Err()
	}
	name := jobconfig.CredentialName(identity.Binding.UID, identity.Binding.OperationID, identity.Binding.JobName)
	if p.foreign {
		name = "foreign"
	}
	return resultcredentials.Credential{Name: name, UID: "canonical-credential"}, nil
}
func invokeIssuance(ctx context.Context, f *resulttest.Fixture, reader client.Reader, issuer ResultCredentialIssuer) (bool, error) {
	return invokeIssuanceWithHints(ctx, f, reader, issuer, nil)
}

func invokeIssuanceWithHints(ctx context.Context, f *resulttest.Fixture, reader client.Reader, issuer ResultCredentialIssuer, hints *resultEnrollmentHints) (bool, error) {
	b := f.Identity.Binding
	var snapshot *api.PodAdmissionSnapshot
	switch subject := f.Subject.(type) {
	case *api.PtahSchema:
		snapshot = subject.Status.ActiveOperation.AdmissionSnapshot
	case *api.PtahMigration:
		snapshot = subject.Status.ActiveOperation.AdmissionSnapshot
	}
	return issueResultCredential(ctx, reader, issuer, hints, f.Subject, b.Kind, f.Job, snapshot, b.ExecutionBindingID, b.InputFingerprint, b.Operation, b.OperationID, f.Identity.Engine)
}
func TestCredentialIssuanceUsesExactAdoptedPodForAllOperations(t *testing.T) {
	for _, name := range []string{"schema-resolve", "schema-verify-admitted", "schema-observe", "schema-plan-dev-fence-scheduling", "schema-apply-admitted-scheduling", "migration-resolve", "migration-verify-admitted", "migration-history", "migration-apply-admitted-scheduling"} {
		t.Run(name, func(t *testing.T) {
			f := resulttest.New(t, name)
			probe := &issuanceProbe{}
			ready, err := invokeIssuance(t.Context(), f, f.Client(t), probe)
			if err != nil || !ready || probe.calls != 1 || probe.identity != f.Identity {
				t.Fatalf("issuance binding: ready=%v calls=%d err=%v", ready, probe.calls, err)
			}
			if probe.budget <= 0 || probe.budget > resultIssuanceTimeout {
				t.Fatalf("issuance budget is %s", probe.budget)
			}
		})
	}
}
func TestCredentialIssuanceRefusesAmbiguousOrChangedInputs(t *testing.T) {
	for _, scenario := range []string{"missing Pod", "multiple Pods", "changed Pod workload", "missing Pod UID", "changed generation", "missing issuer", "foreign credential"} {
		t.Run(scenario, func(t *testing.T) {
			f := resulttest.New(t, "schema-observe")
			if scenario == "changed Pod workload" {
				f.Pod.Spec.Containers[0].Image = "foreign"
			}
			if scenario == "missing Pod UID" {
				f.Pod.UID = ""
			}
			if scenario == "changed generation" {
				f.Subject.SetGeneration(f.Subject.GetGeneration() + 1)
			}
			apiClient := f.Client(t)
			probe := &issuanceProbe{}
			if scenario == "missing Pod" {
				if err := apiClient.Delete(t.Context(), f.Pod); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "multiple Pods" {
				other := f.Pod.DeepCopy()
				other.Name += "-other"
				other.UID = "other"
				other.ResourceVersion = ""
				if err := apiClient.Create(t.Context(), other); err != nil {
					t.Fatal(err)
				}
			}
			var issuer ResultCredentialIssuer = probe
			if scenario == "missing issuer" {
				issuer = nil
			}
			if scenario == "foreign credential" {
				probe.foreign = true
			}
			ready, err := invokeIssuance(t.Context(), f, apiClient, issuer)
			if ready {
				t.Fatal("unsafe issuance was reported ready")
			}
			if scenario == "missing Pod" {
				if err != nil {
					t.Fatalf("pending Pod is not an error: %v", err)
				}
			} else if err == nil {
				t.Fatal("changed binding was not refused")
			}
			if scenario != "foreign credential" && probe.calls != 0 {
				t.Fatal("invalid inputs reached signer")
			}
		})
	}
}
func TestCredentialIssuanceHonorsCancellation(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	probe := &issuanceProbe{wait: true}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	ready, err := invokeIssuance(ctx, f, f.Client(t), probe)
	if ready || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("canceled issuance did not stop: %v", err)
	}
}
