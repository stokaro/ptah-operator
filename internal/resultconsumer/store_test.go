package resultconsumer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/plancontract"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type identifiedAPI struct {
	client.Client
	sequence atomic.Int64
}

func (c *identifiedAPI) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	object.SetUID(types.UID(fmt.Sprintf("result-%d", c.sequence.Add(1))))
	return c.Client.Create(ctx, object, opts...)
}

type recordsOnly struct{ client.Reader }

func (r recordsOnly) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if _, ok := object.(*api.PtahResultRecord); !ok {
		return fmt.Errorf("non-record read forbidden")
	}
	return r.Reader.Get(ctx, key, object, opts...)
}
func (r recordsOnly) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return fmt.Errorf("list forbidden")
}
func requestFor(identity resultdelivery.Identity) Request {
	b := identity.Binding
	return Request{Namespace: b.Namespace, Kind: b.Kind, Name: b.Name, UID: b.UID, Generation: b.Generation, ExecutionBindingID: b.ExecutionBindingID, InputFingerprint: b.InputFingerprint, Operation: b.Operation, OperationID: b.OperationID, JobName: b.JobName, JobUID: b.JobUID, Engine: identity.Engine}
}
func hash(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }

func TestStoreLoaderSurvivesPodAndJobLossForEveryOperation(t *testing.T) {
	for _, name := range []string{"schema-resolve", "schema-verify-admitted", "schema-observe", "schema-plan-dev-fence-scheduling", "schema-apply-admitted-scheduling", "migration-resolve", "migration-verify-admitted", "migration-history", "migration-apply-admitted-scheduling"} {
		t.Run(name, func(t *testing.T) {
			f := resulttest.New(t, name)
			apiClient := &identifiedAPI{Client: f.Client(t)}
			store := resultstore.Store{Client: apiClient, Reader: apiClient}
			value := runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.Operation(f.Identity.Binding.Operation), OperationID: f.Identity.Binding.OperationID, ChildExitCode: -1, Error: &runner.ResultError{Code: "refused", Message: "refused before dispatch"}}
			payload, err := resultdelivery.Encode(f.Identity, value)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := store.Publish(t.Context(), f.Identity.Binding, payload, hash(payload))
			if err != nil {
				t.Fatal(err)
			}
			if err := apiClient.Delete(t.Context(), f.Pod); err != nil {
				t.Fatal(err)
			}
			if err := apiClient.Delete(t.Context(), f.Job); err != nil {
				t.Fatal(err)
			}
			// A fresh loader has neither credentials nor a process seal key. Its API
			// identity cannot read anything except durable result records.
			loader := StoreLoader{Store: resultstore.Store{Reader: recordsOnly{Reader: apiClient}}}
			loaded, err := loader.Load(t.Context(), requestFor(f.Identity))
			if err != nil || loaded.Receipt != receipt || loaded.Binding != f.Identity.Binding || loaded.Value.Error == nil || loaded.Value.Error.Code != "refused" {
				t.Fatalf("restart lost result: %v", err)
			}
		})
	}
}

func TestMaximumPlanLoadsWithoutProcessKey(t *testing.T) {
	f := resulttest.New(t, "schema-plan-dev-fence-scheduling")
	apiClient := &identifiedAPI{Client: f.Client(t)}
	store := resultstore.Store{Client: apiClient, Reader: apiClient}
	before := `{"format_version":1,"name":"plan","dialect":"postgresql","from_fingerprint":"before","to_fingerprint":"after","statements":[{"sql":"CREATE TABLE public.example (id integer)","severity":"safe","reason":"`
	after := `"}]}`
	plan := before + strings.Repeat("<", int(plancontract.MaxExecutableBytes)-len(before)-len(after)) + after
	value := runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationPlan, OperationID: f.Identity.Binding.OperationID, Stdout: plan, PlanContentDigest: hash([]byte(plan)), PlanOutcome: runner.PlanOutcomeChanges, CoordinationDigest: hash([]byte("realm"))}
	payload, err := resultdelivery.Encode(f.Identity, value)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := store.Publish(t.Context(), f.Identity.Binding, payload, hash(payload))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := (StoreLoader{Store: resultstore.Store{Reader: recordsOnly{Reader: apiClient}}}).Load(t.Context(), requestFor(f.Identity))
	if err != nil || loaded.Receipt != receipt || loaded.Value.Stdout != plan {
		t.Fatalf("maximum plan lost bytes: %v", err)
	}
}

func TestStoreLoaderRefusesEveryChangedClaimField(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	apiClient := &identifiedAPI{Client: f.Client(t)}
	store := resultstore.Store{Client: apiClient, Reader: apiClient}
	payload, err := resultdelivery.Encode(f.Identity, runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationObserve, OperationID: f.Identity.Binding.OperationID, ChildExitCode: -1, Error: &runner.ResultError{Code: "refused", Message: "refused"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(t.Context(), f.Identity.Binding, payload, hash(payload)); err != nil {
		t.Fatal(err)
	}
	loader := StoreLoader{Store: store}
	for name, change := range map[string]func(*Request){
		"namespace": func(r *Request) { r.Namespace = "other" }, "kind": func(r *Request) { r.Kind = "PtahMigration" }, "name": func(r *Request) { r.Name = "other" }, "UID": func(r *Request) { r.UID = "other" }, "generation": func(r *Request) { r.Generation++ }, "epoch": func(r *Request) { r.ExecutionBindingID = "v1-" + strings.Repeat("f", 32) }, "inputs": func(r *Request) { r.InputFingerprint = hash([]byte("other")) }, "operation": func(r *Request) { r.Operation = "plan" }, "operation ID": func(r *Request) { r.OperationID = "other" }, "Job name": func(r *Request) { r.JobName = "other" }, "Job UID": func(r *Request) { r.JobUID = "other" }, "invalid engine": func(r *Request) { r.Engine = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			r := requestFor(f.Identity)
			change(&r)
			loaded, err := loader.Load(t.Context(), r)
			if err == nil || loaded.Receipt.UID != "" || loaded.Value.Operation != "" {
				t.Fatal("changed claim obtained result")
			}
		})
	}
}
