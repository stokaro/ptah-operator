package controller

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planseal"
	"github.com/stokaro/ptah-operator/internal/resultconsumer"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type durableTestAPI struct {
	client.Client
	sequence atomic.Int64
}

func (c *durableTestAPI) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	object.SetUID(types.UID(fmt.Sprintf("record-%d", c.sequence.Add(1))))
	return c.Client.Create(ctx, object, opts...)
}

type forbiddenResultLogs struct{ t *testing.T }

func (r forbiddenResultLogs) Read(context.Context, string, string, string) ([]byte, error) {
	r.t.Error("durable Job attempted log fallback")
	return nil, errors.New("logs unavailable")
}
func controllerConsumer(t *testing.T, api client.Client) *resultconsumer.Reader {
	t.Helper()
	r, err := resultconsumer.New(resultconsumer.StoreLoader{Store: resultstore.Store{Reader: api}}, resultconsumer.Options{Workers: 1, Entries: 2, Timeout: time.Second, Retention: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("consumer did not stop")
		}
	})
	return r
}
func pollEvidence(t *testing.T, read func() (terminalEvidence, error)) (terminalEvidence, error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		e, err := read()
		if !errors.Is(err, errResultReadCooling) {
			return e, err
		}
		if time.Now().After(deadline) {
			t.Fatal("durable read did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBothControllersReadDurableResultsAfterPodDeletion(t *testing.T) {
	for _, name := range []string{"schema-resolve", "schema-verify-admitted", "schema-observe", "schema-plan-dev-fence-scheduling", "schema-apply-admitted-scheduling", "migration-resolve", "migration-verify-admitted", "migration-history", "migration-apply-admitted-scheduling"} {
		t.Run(name, func(t *testing.T) {
			f := resulttest.New(t, name)
			apiClient := &durableTestAPI{Client: f.Client(t)}
			result := runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.Operation(f.Identity.Binding.Operation), OperationID: f.Identity.Binding.OperationID, ChildExitCode: -1, Error: &runner.ResultError{Code: "refused", Message: "refused before dispatch"}}
			payload, err := resultdelivery.Encode(f.Identity, result)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (resultstore.Store{Client: apiClient, Reader: apiClient}).Publish(t.Context(), f.Identity.Binding, payload, fingerprint.DigestBytes(payload)); err != nil {
				t.Fatal(err)
			}
			if err := apiClient.Delete(t.Context(), f.Pod); err != nil {
				t.Fatal(err)
			}
			consumer := controllerConsumer(t, apiClient)
			var read func() (terminalEvidence, error)
			switch subject := f.Subject.(type) {
			case *api.PtahSchema:
				r := &SchemaReconciler{Client: apiClient, APIReader: apiClient, Results: consumer, Logs: forbiddenResultLogs{t}}
				read = func() (terminalEvidence, error) { return r.terminalLogs(t.Context(), subject, f.Job) }
			case *api.PtahMigration:
				r := &MigrationReconciler{Client: apiClient, APIReader: apiClient, Results: consumer, Logs: forbiddenResultLogs{t}}
				read = func() (terminalEvidence, error) { return r.migrationTerminalLogs(t.Context(), subject, f.Job) }
			default:
				t.Fatal("unknown fixture")
			}
			evidence, err := pollEvidence(t, read)
			if err != nil {
				t.Fatal(err)
			}
			got, err := evidence.parseResult(result.Operation, result.OperationID)
			if err != nil || got.Error == nil || got.Error.Code != "refused" || !evidence.Durable {
				t.Fatalf("durable result not consumed: %v", err)
			}
			if evidence.Trusted || evidence.PodCount != 1 || len(evidence.PodUIDs) != 1 || evidence.PodUIDs[0] != f.Pod.UID {
				t.Fatal("historical identity was lost or treated as live termination evidence")
			}
		})
	}
}

func TestDurableResultNeverUsesLogsOrTerminationSummary(t *testing.T) {
	f := resulttest.New(t, "migration-apply-admitted-scheduling")
	f.Pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: executorContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "a termination summary is not a durable receipt"}}}}
	apiClient := f.Client(t)
	subject := f.Subject.(*api.PtahMigration)
	r := &MigrationReconciler{Client: apiClient, APIReader: apiClient, Results: controllerConsumer(t, apiClient), Logs: forbiddenResultLogs{t}}
	evidence, err := pollEvidence(t, func() (terminalEvidence, error) { return r.migrationTerminalLogs(t.Context(), subject, f.Job) })
	if err != nil {
		t.Fatal(err)
	}
	_, parseErr := evidence.parseResult(runner.OperationMigrationApply, f.Identity.Binding.OperationID)
	if !errors.Is(parseErr, runner.ErrFrameNotFound) {
		t.Fatalf("missing receipt not reported: %v", parseErr)
	}
	if _, standsIn, err := terminationSummaryStandIn(evidence, parseErr, runner.OperationMigrationApply, f.Identity.Binding.OperationID); standsIn || err != nil {
		t.Fatal("summary substituted for durable evidence")
	}
	r.Results = nil
	if _, err := r.migrationTerminalLogs(t.Context(), subject, f.Job); err == nil {
		t.Fatal("unconfigured consumer silently used legacy logs")
	}
}

func TestDurableResultDoesNotWaitForAnExecutorInAFailedPod(t *testing.T) {
	for _, name := range []string{"schema-observe", "schema-apply-admitted-scheduling", "migration-history", "migration-apply-admitted-scheduling"} {
		for _, phase := range []corev1.PodPhase{corev1.PodRunning, corev1.PodFailed} {
			t.Run(name+"/"+string(phase), func(t *testing.T) {
				f := resulttest.New(t, name)
				f.Pod.Status.Phase = phase
				f.Pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: executorContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}
				f.Pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: f.Pod.Spec.InitContainers[0].Name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2}}}}
				apiClient := f.Client(t)
				consumer := controllerConsumer(t, apiClient)
				var read func() (terminalEvidence, error)
				switch subject := f.Subject.(type) {
				case *api.PtahSchema:
					r := &SchemaReconciler{Client: apiClient, APIReader: apiClient, Results: consumer, Logs: forbiddenResultLogs{t}}
					read = func() (terminalEvidence, error) { return r.terminalLogs(t.Context(), subject, f.Job) }
				case *api.PtahMigration:
					r := &MigrationReconciler{Client: apiClient, APIReader: apiClient, Results: consumer, Logs: forbiddenResultLogs{t}}
					read = func() (terminalEvidence, error) { return r.migrationTerminalLogs(t.Context(), subject, f.Job) }
				}
				evidence, err := pollEvidence(t, read)
				if phase == corev1.PodRunning {
					if !errors.Is(err, errTerminalPodPending) {
						t.Fatalf("a nonterminal Pod stopped waiting: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("failed init Pod remained pending instead of exposing the missing receipt: %v", err)
				}
				_, err = evidence.parseResult(runner.Operation(f.Identity.Binding.Operation), f.Identity.Binding.OperationID)
				if !errors.Is(err, runner.ErrFrameNotFound) || evidence.Trusted || !evidence.Durable || evidence.PodCount != 1 || len(evidence.PodUIDs) != 1 || evidence.PodUIDs[0] != f.Pod.UID {
					t.Fatalf("failed init lost its exact missing-result evidence: evidence=%+v error=%v", evidence, err)
				}
			})
		}
	}
}

func TestDurableConsumerStillRefusesReplacementPod(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	f.Pod.UID = "replacement"
	f.Pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: executorContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
	apiClient := &durableTestAPI{Client: f.Client(t)}
	result := runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationObserve, OperationID: f.Identity.Binding.OperationID, ChildExitCode: -1, Error: &runner.ResultError{Code: "refused", Message: "refused"}}
	payload, err := resultdelivery.Encode(f.Identity, result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (resultstore.Store{Client: apiClient, Reader: apiClient}).Publish(t.Context(), f.Identity.Binding, payload, fingerprint.DigestBytes(payload)); err != nil {
		t.Fatal(err)
	}
	r := &SchemaReconciler{Client: apiClient, APIReader: apiClient, Results: controllerConsumer(t, apiClient), Logs: forbiddenResultLogs{t}}
	if _, err := pollEvidence(t, func() (terminalEvidence, error) {
		return r.terminalLogs(t.Context(), f.Subject.(*api.PtahSchema), f.Job)
	}); !errors.Is(err, errTerminalPodIntent) {
		t.Fatalf("replacement Pod accepted: %v", err)
	}
}

func TestDurablePlanPublishesAfterManagerKeyLoss(t *testing.T) {
	oldKey := mustGenerateTestSealKey()
	r, apiClient, schema := planSealMismatchFixture(t, oldKey.PublicKey(), planSealPublicKeyDigest(oldKey.PublicKey()))
	r.SealKey = planseal.KeyPair{}
	plan := safetyPlanDocumentWithStatement(t, "observed-state", "INSERT INTO public.customers (email) VALUES ('restart-witness@example.com')")
	result := runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationPlan, OperationID: schema.Status.ActiveOperation.ID, Stdout: string(plan), PlanOutcome: runner.PlanOutcomeChanges, PlanContentDigest: fingerprint.DigestBytes(plan), CoordinationDigest: schema.Status.Target.CoordinationDigest, TargetIdentityDigest: schema.Status.Target.IdentityDigest}
	stored := &api.PtahSchema{}
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(schema), stored); err != nil {
		t.Fatal(err)
	}
	if _, err := r.consumeResultWithTransport(t.Context(), stored, nil, result, []types.UID{"original-pod"}, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(schema), stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Plan == nil || stored.Status.Plan.ContentDigest != fingerprint.DigestBytes(plan) || stored.Status.ActiveOperation != nil {
		t.Fatalf("durable plan was not published after key loss: phase=%s plan=%#v", stored.Status.Phase, stored.Status.Plan)
	}
}
