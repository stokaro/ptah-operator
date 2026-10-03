package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
)

type operationResultAPI struct {
	client.Client
	sequence int
}

func (c *operationResultAPI) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	c.sequence++
	object.SetUID(types.UID(fmt.Sprintf("result-%d", c.sequence)))
	return c.Client.Create(ctx, object, opts...)
}

func operationResultFixture(t *testing.T, name string) (*resulttest.Fixture, *operationResultAPI, runner.Result) {
	t.Helper()
	f := resulttest.New(t, name)
	value := runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.Operation(f.Identity.Binding.Operation),
		OperationID: f.Identity.Binding.OperationID, ChildExitCode: -1, Error: &runner.ResultError{Code: "refused", Message: "refused before dispatch"}}
	return f, &operationResultAPI{Client: f.Client(t)}, value
}

func publishOperationResult(t *testing.T, c *operationResultAPI, f *resulttest.Fixture, value runner.Result) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (resultstore.Store{Client: c, Reader: c}).Publish(t.Context(), f.Identity.Binding, payload,
		fmt.Sprintf("sha256:%x", sha256.Sum256(payload))); err != nil {
		t.Fatal(err)
	}
}

func TestRecordedMigrationApplyNeedsItsReceiptWithoutLogFrame(t *testing.T) {
	for _, outcome := range []api.MigrationRunOutcome{api.MigrationRunOutcomeFailed, api.MigrationRunOutcomeApplied} {
		t.Run(string(outcome), func(t *testing.T) {
			f, c, value := operationResultFixture(t, "migration-apply-admitted-scheduling")
			applied := []int64{1}
			if outcome == api.MigrationRunOutcomeApplied {
				applied = []int64{1, 2, 3}
			}
			value.MigrationRun = &dataplane.MigrationRunReport{ContractVersion: dataplane.SupportedMigrationRunContract,
				Direction: "up", Outcome: strings.ToLower(string(outcome)), Planned: []int64{1, 2, 3}, Applied: applied}
			run := &api.MigrationRunStatus{JobName: f.Job.Name, JobUID: f.Job.UID, Outcome: outcome, AppliedVersions: applied}
			// The migration CI stopped here after all database/status checks had
			// passed: durable Jobs intentionally emit no correctness frame.
			logs := []byte("ptah-runner: diagnostic output only\n")
			if _, err := readRecordedMigrationApply(t.Context(), c, f.Job, f.Pod, run, logs); !operationResultPending(err) {
				t.Fatalf("absent receipt did not remain incomplete: %v", err)
			}
			publishOperationResult(t, c, f, value)
			got, err := readRecordedMigrationApply(t.Context(), c, f.Job, f.Pod, run, logs)
			if err != nil || !reflect.DeepEqual(got, value) {
				t.Fatalf("complete result with no stdout frame was refused: %v", err)
			}
			for name, mutate := range map[string]func(*api.MigrationRunStatus){
				"another Job UID":          func(r *api.MigrationRunStatus) { r.JobUID = "other" },
				"another Job name":         func(r *api.MigrationRunStatus) { r.JobName = "other" },
				"unknown outcome":          func(r *api.MigrationRunStatus) { r.Outcome = api.MigrationRunOutcomeUnknown },
				"missing applied versions": func(r *api.MigrationRunStatus) { r.AppliedVersions = nil },
				"another applied version":  func(r *api.MigrationRunStatus) { r.AppliedVersions = []int64{9} },
			} {
				t.Run(name, func(t *testing.T) {
					changed := run.DeepCopy()
					mutate(changed)
					if _, err := readRecordedMigrationApply(t.Context(), c, f.Job, f.Pod, changed, logs); err == nil {
						t.Fatal("receipt accepted a different recorded run")
					}
				})
			}
		})
	}
}

func TestReadOperationResultUsesDurableReceipt(t *testing.T) {
	for _, name := range []string{"schema-resolve", "migration-history", "migration-apply-admitted-scheduling"} {
		t.Run(name, func(t *testing.T) {
			f, c, want := operationResultFixture(t, name)
			publishOperationResult(t, c, f, want)
			for _, object := range []client.Object{f.Job, f.Pod} {
				if err := c.Delete(t.Context(), object); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readOperationResult(t.Context(), c, f.Job, f.Pod, want.Operation, want.OperationID, nil)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("archived workload, absent logs: result matches=%v, err=%v", reflect.DeepEqual(got, want), err)
			}
		})
	}
}

func TestReadOperationResultNeverFallsBackToLogs(t *testing.T) {
	f, c, want := operationResultFixture(t, "migration-history")
	logs, err := runner.MarshalFrame(want)
	if err != nil {
		t.Fatal(err)
	}
	_, err = readOperationResult(t.Context(), c, f.Job, f.Pod, want.Operation, want.OperationID, logs)
	if !errors.Is(err, resultstore.ErrIncomplete) || !operationResultPending(err) {
		t.Fatalf("missing receipt with a valid log frame: %v", err)
	}
	publishOperationResult(t, c, f, want)
	var records api.PtahResultRecordList
	if err := c.List(t.Context(), &records, client.MatchingLabels{resultstore.LabelRecord: "chunk"}); err != nil || len(records.Items) != 1 {
		t.Fatalf("read the exact result chunk: count=%d, err=%v", len(records.Items), err)
	}
	records.Items[0].Spec.Data = []byte("corrupt")
	if err := c.Update(t.Context(), &records.Items[0]); err != nil {
		t.Fatal(err)
	}
	_, err = readOperationResult(t.Context(), c, f.Job, f.Pod, want.Operation, want.OperationID, logs)
	if err == nil || operationResultPending(err) {
		t.Fatalf("corrupt receipt must be refused, not retried or read from logs: %v", err)
	}
}

func TestReadOperationResultPreservesPlanBytesAndEngine(t *testing.T) {
	f, c, _ := operationResultFixture(t, "schema-plan-dev-fence-scheduling")
	plan := `{"format_version":1,"name":"plan","dialect":"postgresql","from_fingerprint":"before","to_fingerprint":"after","statements":[{"sql":"CREATE TABLE public.example (id integer)","severity":"safe"}]}`
	value := runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationPlan,
		OperationID: f.Identity.Binding.OperationID, Stdout: plan, PlanOutcome: runner.PlanOutcomeChanges,
		PlanContentDigest: sha256Digest([]byte(plan)), CoordinationDigest: sha256Digest([]byte("realm"))}
	publishOperationResult(t, c, f, value)
	got, err := readOperationResult(t.Context(), c, f.Job, f.Pod, value.Operation, value.OperationID, nil)
	if err != nil || got.Stdout != plan || got.PlanContentDigest != value.PlanContentDigest {
		t.Fatalf("durable plan bytes or digest changed: %v", err)
	}
	for i := range f.Job.Spec.Template.Spec.Containers[0].Env {
		if f.Job.Spec.Template.Spec.Containers[0].Env[i].Name == "PTAH_EXPECTED_DATABASE_ENGINE" {
			f.Job.Spec.Template.Spec.Containers[0].Env[i].Value = "MySQL"
		}
	}
	if _, err := readOperationResult(t.Context(), c, f.Job, f.Pod, value.Operation, value.OperationID, nil); err == nil {
		t.Fatal("accepted a PostgreSQL plan for a MySQL workload")
	}
}

func TestReadOperationResultRejectsForeignEvidence(t *testing.T) {
	for _, field := range []string{"operation", "operation-id", "job-uid", "pod-uid", "generation", "binding", "payload-protocol", "projection"} {
		t.Run(field, func(t *testing.T) {
			f, c, value := operationResultFixture(t, "migration-history")
			operation, operationID := value.Operation, value.OperationID
			if field == "payload-protocol" {
				value.ProtocolVersion++
			}
			publishOperationResult(t, c, f, value)
			switch field {
			case "operation":
				operation = runner.OperationMigrationApply
			case "operation-id":
				operationID += "-foreign"
			case "job-uid":
				f.Job.UID = "replacement"
			case "pod-uid":
				f.Pod.UID = "replacement"
			case "generation":
				for i := range f.Job.Spec.Template.Spec.Containers[0].Env {
					if f.Job.Spec.Template.Spec.Containers[0].Env[i].Name == jobconfig.Generation {
						f.Job.Spec.Template.Spec.Containers[0].Env[i].Value = "3"
					}
				}
			case "binding":
				f.Job.Annotations[annotationBindingID] = "v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			case "projection":
				f.Job.Spec.Template.Spec.Containers[0].Args = nil
			}
			_, err := readOperationResult(t.Context(), c, f.Job, f.Pod, operation, operationID, nil)
			if err == nil || operationResultPending(err) {
				t.Fatalf("changed %s was not permanently refused: %v", field, err)
			}
		})
	}
}

func TestReadOperationResultLegacyJobStillUsesExactFrame(t *testing.T) {
	f, _, value := operationResultFixture(t, "migration-history")
	job := f.Job.DeepCopy()
	job.Spec.Template.Spec.Containers = nil
	job.Spec.Template.Spec.Volumes = nil
	logs, err := runner.MarshalFrame(value)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readOperationResult(t.Context(), nil, job, f.Pod, value.Operation, value.OperationID, logs)
	if err != nil || !reflect.DeepEqual(got, value) {
		t.Fatalf("legacy result matches=%v, err=%v", reflect.DeepEqual(got, value), err)
	}
	if _, err := readOperationResult(t.Context(), nil, job, f.Pod, value.Operation, value.OperationID, append(logs, logs...)); err == nil {
		t.Fatal("legacy reader accepted duplicate frames")
	}
}
