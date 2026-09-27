package admission

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// stampSchemaWriter records writer as the fixture's schema's last spec
// writer, exactly as SchemaSpecWriterHandler would have. It mutates through
// the fixture's own reader, the same way the plan and status mutations
// elsewhere in this package do.
func stampSchemaWriter(t *testing.T, handler *ApprovalHandler, namespace, name string, writer authenticationv1.UserInfo) {
	t.Helper()
	api, ok := handler.Reader.(client.Client)
	if !ok {
		t.Fatal("approval fixture reader is not mutable")
	}
	schema := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, schema); err != nil {
		t.Fatal(err)
	}
	stampSpecWriter(schema, writer)
	if err := api.Update(context.Background(), schema); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalFourEyesRefusesTheAuthorsOwnApproval(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, true)
	author := authenticationv1.UserInfo{Username: "alice@example.com", UID: "idp-123"}
	stampSchemaWriter(t, handler, approval.Namespace, approval.Spec.SchemaRef.Name, author)
	// The installation turns the control on for the whole manager; a schema
	// carries no field of its own that could turn it back off.
	handler.RequireDistinctApprover = true

	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = author
	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() admitted an approval from the schema's own last spec writer")
	}
	if response.Result == nil || !strings.Contains(response.Result.Message, "requires a distinct approver") {
		t.Fatalf("Handle() response = %#v, want a four-eyes refusal", response.Result)
	}
}

func TestApprovalFourEyesAdmitsADistinctApprover(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, true)
	author := authenticationv1.UserInfo{Username: "alice@example.com", UID: "idp-123"}
	stampSchemaWriter(t, handler, approval.Namespace, approval.Spec.SchemaRef.Name, author)
	handler.RequireDistinctApprover = true

	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{Username: "bob@example.com", UID: "idp-456"}
	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() refused a distinct approver: %#v", response.Result)
	}
}

func TestApprovalFourEyesOffAdmitsTheAuthorsApproval(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, true)
	author := authenticationv1.UserInfo{Username: "alice@example.com", UID: "idp-123"}
	// Stamp the writer without turning the control on: it defaults to false,
	// and an installation that never sets the manager's flag keeps admitting
	// the approvals it always did.
	stampSchemaWriter(t, handler, approval.Namespace, approval.Spec.SchemaRef.Name, author)

	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = author
	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() refused a self-approval with the four-eyes control off: %#v", response.Result)
	}
}

func TestApprovalFourEyesRefusesWhenNoWriterIsRecorded(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, true)
	handler.RequireDistinctApprover = true

	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{Username: "alice@example.com", UID: "idp-123"}
	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() admitted an approval although no spec writer was recorded")
	}
	if response.Result == nil || !strings.Contains(response.Result.Message, "no spec writer is recorded") {
		t.Fatalf("Handle() response = %#v, want the missing-writer refusal", response.Result)
	}
}

// stampMigrationWriter is stampSchemaWriter's counterpart for PtahMigration.
func stampMigrationWriter(t *testing.T, handler *MigrationApprovalHandler, namespace, name string, writer authenticationv1.UserInfo) {
	t.Helper()
	api, ok := handler.Reader.(client.Client)
	if !ok {
		t.Fatal("migration approval fixture reader is not mutable")
	}
	migration := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, migration); err != nil {
		t.Fatal(err)
	}
	stampSpecWriter(migration, writer)
	if err := api.Update(context.Background(), migration); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationApprovalFourEyesRefusesTheAuthorsOwnApproval(t *testing.T) {
	t.Parallel()

	handler, approval := migrationApprovalFixture(t, true, nil)
	author := authenticationv1.UserInfo{Username: "alice@example.com", UID: "idp-123"}
	stampMigrationWriter(t, handler, approval.Namespace, approval.Spec.MigrationRef.Name, author)
	handler.RequireDistinctApprover = true

	request := migrationApprovalRequest(t, approval, admissionv1.Create)
	request.UserInfo = author
	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() admitted an approval from the migration's own last spec writer")
	}
	if response.Result == nil || !strings.Contains(response.Result.Message, "requires a distinct approver") {
		t.Fatalf("Handle() response = %#v, want a four-eyes refusal", response.Result)
	}
}

func TestMigrationApprovalFourEyesAdmitsADistinctApprover(t *testing.T) {
	t.Parallel()

	handler, approval := migrationApprovalFixture(t, true, nil)
	author := authenticationv1.UserInfo{Username: "alice@example.com", UID: "idp-123"}
	stampMigrationWriter(t, handler, approval.Namespace, approval.Spec.MigrationRef.Name, author)
	handler.RequireDistinctApprover = true

	request := migrationApprovalRequest(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{Username: "bob@example.com", UID: "idp-456"}
	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() refused a distinct approver: %#v", response.Result)
	}
}

func TestMigrationApprovalFourEyesOffAdmitsTheAuthorsApproval(t *testing.T) {
	t.Parallel()

	handler, approval := migrationApprovalFixture(t, true, nil)
	author := authenticationv1.UserInfo{Username: "alice@example.com", UID: "idp-123"}
	stampMigrationWriter(t, handler, approval.Namespace, approval.Spec.MigrationRef.Name, author)

	request := migrationApprovalRequest(t, approval, admissionv1.Create)
	request.UserInfo = author
	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() refused a self-approval with the four-eyes control off: %#v", response.Result)
	}
}

func TestMigrationApprovalFourEyesRefusesWhenNoWriterIsRecorded(t *testing.T) {
	t.Parallel()

	handler, approval := migrationApprovalFixture(t, true, nil)
	handler.RequireDistinctApprover = true

	request := migrationApprovalRequest(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{Username: "alice@example.com", UID: "idp-123"}
	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() admitted an approval although no spec writer was recorded")
	}
	if response.Result == nil || !strings.Contains(response.Result.Message, "no spec writer is recorded") {
		t.Fatalf("Handle() response = %#v, want the missing-writer refusal", response.Result)
	}
}
