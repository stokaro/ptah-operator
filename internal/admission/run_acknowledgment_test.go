package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const unresolvedOperationID = "sha256:9999999999999999999999999999999999999999999999999999999999999999"

// runAcknowledgmentFixture returns a handler whose reader holds a migration
// with an unresolved run, plus the acknowledgment a person would submit for
// it: the migration and the run, and nothing else.
func runAcknowledgmentFixture(
	t *testing.T,
	mutate bool,
	arrange func(*operatorv1alpha1.PtahMigration),
) (*RunAcknowledgmentHandler, *operatorv1alpha1.PtahMigrationRunAcknowledgment) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	migration := &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app", UID: "migration-uid"},
	}
	migration.Status.UnresolvedRun = &operatorv1alpha1.UnresolvedMigrationRunStatus{
		Outcome:     operatorv1alpha1.MigrationRunOutcomeUnknown,
		OperationID: unresolvedOperationID,
		PlanRef:     operatorv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-app", UID: "plan-uid"},
		RecordedAt:  metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
	}
	if arrange != nil {
		arrange(migration)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(migration).Build()
	handler := &RunAcknowledgmentHandler{
		Reader:  reader,
		Decoder: cradmission.NewDecoder(scheme),
		Clock:   fixedClock{value: time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)},
		Mutate:  mutate,
	}
	acknowledgment := &operatorv1alpha1.PtahMigrationRunAcknowledgment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahMigrationRunAcknowledgment",
		},
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app-run-accounted-for"},
		Spec: operatorv1alpha1.PtahMigrationRunAcknowledgmentSpec{
			MigrationRef: operatorv1alpha1.ImmutableObjectReference{Name: migration.Name, UID: migration.UID},
			OperationID:  unresolvedOperationID,
		},
	}
	return handler, acknowledgment
}

func runAcknowledgmentRequest(
	t *testing.T,
	acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment,
	operation admissionv1.Operation,
) cradmission.Request {
	t.Helper()

	raw, err := json.Marshal(acknowledgment)
	if err != nil {
		t.Fatal(err)
	}
	return cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "admission-uid", Namespace: acknowledgment.Namespace, Name: acknowledgment.Name,
		Operation: operation, Object: runtime.RawExtension{Raw: raw},
	}}
}

// The mutating pass writes who acknowledged and when, and nothing else: the
// decision is the person's own.
func TestRunAcknowledgmentCreateStampsTheRequesterAndNothingElse(t *testing.T) {
	t.Parallel()

	handler, acknowledgment := runAcknowledgmentFixture(t, true, nil)
	// Whatever the client claims about itself is replaced.
	acknowledgment.Spec.AcknowledgedBy = operatorv1alpha1.ApprovalIdentity{Username: "someone-else"}
	request := runAcknowledgmentRequest(t, acknowledgment, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{
		Username: "dba@example.test", UID: "dba-uid", Groups: []string{"dba", "dba", "system:authenticated"},
	}
	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("an acknowledgment of the unresolved run was denied: %#v", response.Result)
	}
	if len(response.Patches) == 0 {
		t.Fatal("no identity was stamped")
	}
	for _, patch := range response.Patches {
		if !strings.HasPrefix(patch.Path, "/spec/acknowledgedBy") &&
			patch.Path != "/spec/acknowledgedAt" && patch.Path != "/spec/mutationRequestUID" {
			t.Fatalf("the mutating pass wrote %s, which is not the acknowledger's identity: %#v", patch.Path, response.Patches)
		}
	}
	patchJSON, err := json.Marshal(response.Patches)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"dba@example.test", "dba-uid", "system:authenticated", "admission-uid", "2026-09-01T13:00:00Z"} {
		if !containsJSON(patchJSON, want) {
			t.Fatalf("the stamped acknowledgment %s does not carry %q", patchJSON, want)
		}
	}
	if strings.Contains(string(patchJSON), "someone-else") {
		t.Fatalf("the identity the client wrote survived the stamp: %s", patchJSON)
	}
	if strings.Count(string(patchJSON), `"dba"`) != 1 {
		t.Fatalf("duplicate groups were not normalized away: %s", patchJSON)
	}
}

// The validating pass admits exactly the stamp the mutating pass would have
// written for this requester, and refuses one written for anybody else.
func TestRunAcknowledgmentValidationHoldsTheStampToTheRequester(t *testing.T) {
	t.Parallel()

	requester := authenticationv1.UserInfo{Username: "dba@example.test", UID: "dba-uid", Groups: []string{"dba"}}
	stamped := func(acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment) {
		acknowledgment.Spec.AcknowledgedBy = operatorv1alpha1.ApprovalIdentity{
			Username: requester.Username, UID: requester.UID, Groups: []string{"dba"},
		}
		acknowledgment.Spec.AcknowledgedAt = metav1.NewTime(time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC))
		acknowledgment.Spec.MutationRequestUID = "mutating-admission-uid"
	}
	rows := []struct {
		name    string
		arrange func(*operatorv1alpha1.PtahMigrationRunAcknowledgment)
		want    string
	}{
		{name: "the requester's own stamp", arrange: stamped},
		{
			name: "somebody else's username",
			arrange: func(acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment) {
				stamped(acknowledgment)
				acknowledgment.Spec.AcknowledgedBy.Username = "manager"
			},
			want: "do not match the authenticated request",
		},
		{
			name: "a group the requester does not carry",
			arrange: func(acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment) {
				stamped(acknowledgment)
				acknowledgment.Spec.AcknowledgedBy.Groups = []string{"dba", "system:masters"}
			},
			want: "do not match the authenticated request",
		},
		{
			name: "no mutating stamp",
			arrange: func(acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment) {
				stamped(acknowledgment)
				acknowledgment.Spec.MutationRequestUID = ""
			},
			want: "not stamped by the mutating admission webhook",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			handler, acknowledgment := runAcknowledgmentFixture(t, false, nil)
			row.arrange(acknowledgment)
			request := runAcknowledgmentRequest(t, acknowledgment, admissionv1.Create)
			request.UserInfo = requester
			response := handler.Handle(context.Background(), request)
			if row.want == "" {
				if !response.Allowed {
					t.Fatalf("the requester's own stamp was refused: %#v", response.Result)
				}
				return
			}
			if response.Allowed {
				t.Fatalf("an acknowledgment carrying %s was admitted", row.name)
			}
			if response.Result == nil || !strings.Contains(response.Result.Message, row.want) {
				t.Fatalf("denial = %#v, want one mentioning %q", response.Result, row.want)
			}
		})
	}
}

// An acknowledgment settles one run of one object. Each row moves one of the
// two off the run the migration records, in both passes.
func TestRunAcknowledgmentRefusesAnythingButTheRecordedRun(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name           string
		want           string
		migration      func(*operatorv1alpha1.PtahMigration)
		acknowledgment func(*operatorv1alpha1.PtahMigrationRunAcknowledgment)
	}{
		{
			name: "another run",
			want: "not the one this acknowledgment names",
			acknowledgment: func(acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment) {
				acknowledgment.Spec.OperationID = "sha256:" + strings.Repeat("1", 64)
			},
		},
		{
			name: "a migration recreated under the same name",
			want: "referenced migration UID does not match",
			acknowledgment: func(acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment) {
				acknowledgment.Spec.MigrationRef.UID = "restored-migration-uid"
			},
		},
		{
			name: "a migration with nothing unresolved",
			want: "records no unresolved run",
			migration: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.UnresolvedRun = nil
			},
		},
		{
			name: "a migration that does not exist",
			want: "not found",
			acknowledgment: func(acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment) {
				acknowledgment.Spec.MigrationRef.Name = "missing"
			},
		},
		{
			name: "no operation",
			want: "must name the operationID",
			acknowledgment: func(acknowledgment *operatorv1alpha1.PtahMigrationRunAcknowledgment) {
				acknowledgment.Spec.OperationID = ""
			},
		},
	}
	requester := authenticationv1.UserInfo{Username: "dba@example.test", UID: "dba-uid"}
	for _, row := range rows {
		for _, mutate := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/mutate=%t", row.name, mutate), func(t *testing.T) {
				t.Parallel()
				handler, acknowledgment := runAcknowledgmentFixture(t, mutate, row.migration)
				if row.acknowledgment != nil {
					row.acknowledgment(acknowledgment)
				}
				if !mutate {
					// The validating pass sees what the mutating pass wrote.
					handler.stamp(acknowledgment, requester, "mutating-admission-uid")
				}
				request := runAcknowledgmentRequest(t, acknowledgment, admissionv1.Create)
				request.UserInfo = requester
				response := handler.Handle(context.Background(), request)
				if response.Allowed {
					t.Fatalf("an acknowledgment of %s was admitted", row.name)
				}
				if response.Result == nil || !strings.Contains(response.Result.Message, row.want) {
					t.Fatalf("denial = %#v, want one mentioning %q", response.Result, row.want)
				}
			})
		}
	}
}

// Once made, the decision cannot be rewritten; its metadata can.
func TestRunAcknowledgmentSpecIsImmutable(t *testing.T) {
	t.Parallel()

	handler, acknowledgment := runAcknowledgmentFixture(t, false, nil)
	handler.stamp(acknowledgment, authenticationv1.UserInfo{Username: "dba@example.test"}, "mutating-admission-uid")
	previous, err := json.Marshal(acknowledgment)
	if err != nil {
		t.Fatal(err)
	}

	labeled := acknowledgment.DeepCopy()
	labeled.Labels = map[string]string{"team": "dba"}
	request := runAcknowledgmentRequest(t, labeled, admissionv1.Update)
	request.OldObject = runtime.RawExtension{Raw: previous}
	if response := handler.Handle(context.Background(), request); !response.Allowed {
		t.Fatalf("a metadata update was refused: %#v", response.Result)
	}

	rewritten := acknowledgment.DeepCopy()
	rewritten.Spec.AcknowledgedBy.Username = "someone-else"
	request = runAcknowledgmentRequest(t, rewritten, admissionv1.Update)
	request.OldObject = runtime.RawExtension{Raw: previous}
	response := handler.Handle(context.Background(), request)
	if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "immutable") {
		t.Fatalf("a rewritten acknowledgment was answered %#v", response.Result)
	}
}
