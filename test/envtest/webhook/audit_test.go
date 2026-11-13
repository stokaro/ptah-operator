package webhook_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/envtest/internal/harness"
)

// auditPolicy logs the creates the stamping webhooks answer at Metadata, and
// nothing else. Metadata is the level that keeps no body: at RequestResponse
// the stamp is in the stored object as well, so an annotation found there says
// nothing about the level an installation is likeliest to keep for every write.
const auditPolicy = `apiVersion: audit.k8s.io/v1
kind: Policy
omitStages: ["RequestReceived"]
rules:
  - level: Metadata
    verbs: ["create"]
    resources:
      - group: operator.ptah.run
        resources: ["ptahschemaapprovals", "ptahmigrationapprovals", "ptahmigrationrunacknowledgments"]
  - level: None
`

// auditLogPath is the file the package's API server writes its audit log to,
// one JSON event per line.
var auditLogPath string

// enableAuditLog points the package's API server at auditPolicy and a log
// file in a directory of its own, which it returns for the caller to remove
// once the control plane has stopped.
func enableAuditLog() (string, error) {
	directory, err := os.MkdirTemp("", "ptah-envtest-audit-")
	if err != nil {
		return "", err
	}
	policy := filepath.Join(directory, "policy.yaml")
	if err := os.WriteFile(policy, []byte(auditPolicy), 0o600); err != nil {
		_ = os.RemoveAll(directory)
		return "", err
	}
	auditLogPath = filepath.Join(directory, "audit.log")
	plane.Environment.ControlPlane.GetAPIServer().Configure().
		Set("audit-policy-file", policy).
		Set("audit-log-path", auditLogPath)
	return directory, nil
}

// The stamped mutationRequestUID is the UID of the review the mutating webhook
// answered, which the audit event does not otherwise hold anywhere. Each
// stamping webhook returns it, and the username it stamped, as audit
// annotations, and this is the API server recording them on the create's
// event at Metadata under the webhook's name: the keys the audit page names.
func TestStampingWebhooksNameTheirStampInTheAuditLog(t *testing.T) {
	plane.Require(t)
	ctx := context.Background()
	page, err := os.ReadFile(filepath.Join(harness.RepositoryRoot(), "docs", "site", "src", "content", "docs", "use", "audit.md"))
	if err != nil {
		t.Fatalf("read the audit page: %v", err)
	}

	rows := []struct {
		kind, resource, webhook string
		// identityField is the spec field the stamped username is stored under.
		identityField string
		// object is the create a person sends, in a namespace of its own.
		object func(t *testing.T) *unstructured.Unstructured
	}{
		{
			kind: "PtahSchemaApproval", resource: "ptahschemaapprovals",
			webhook: "mapproval.operator.ptah.run", identityField: "approver",
			object: func(t *testing.T) *unstructured.Unstructured {
				fixture := newApprovalFixture(t)
				return fixture.approval("audited-approval", fixture.plan.Spec.Fingerprint)
			},
		},
		{
			kind: "PtahMigrationApproval", resource: "ptahmigrationapprovals",
			webhook: "mmigrationapproval.operator.ptah.run", identityField: "approver",
			object: func(t *testing.T) *unstructured.Unstructured {
				return newMigrationApprovalFixture(t).approval("audited-approval")
			},
		},
		{
			kind: "PtahMigrationRunAcknowledgment", resource: "ptahmigrationrunacknowledgments",
			webhook: acknowledgmentWebhook, identityField: "acknowledgedBy",
			object: func(t *testing.T) *unstructured.Unstructured {
				fixture := newAcknowledgmentFixture(t)
				return fixture.acknowledgment("audited-acknowledgment", fixture.migration.Status.UnresolvedRun.OperationID)
			},
		},
	}
	for _, row := range rows {
		t.Run(row.kind, func(t *testing.T) {
			t.Parallel()
			object := row.object(t)
			key := client.ObjectKeyFromObject(object)
			// The approver is impersonated, so the event's user is the
			// administrator and the stamp names somebody else.
			if err := approverClient(t).Create(ctx, object); err != nil {
				t.Fatalf("create %s %s as %s: %v", row.kind, key, approverName, err)
			}
			stored := &unstructured.Unstructured{}
			stored.SetGroupVersionKind(operatorv1alpha1.GroupVersion.WithKind(row.kind))
			if err := admin.Get(ctx, key, stored); err != nil {
				t.Fatalf("read %s %s back: %v", row.kind, key, err)
			}
			stampedUID, _, _ := unstructured.NestedString(stored.Object, "spec", "mutationRequestUID")
			stampedUsername, _, _ := unstructured.NestedString(stored.Object, "spec", row.identityField, "username")
			if stampedUID == "" || stampedUsername != approverName {
				t.Fatalf("%s %s stores mutationRequestUID %q and %s.username %q; want a UID and %q",
					row.kind, key, stampedUID, row.identityField, stampedUsername, approverName)
			}

			event := awaitCreateEvent(t, row.resource, key)
			if event.Level != auditv1.LevelMetadata || event.RequestObject != nil || event.ResponseObject != nil {
				t.Fatalf("the create event of %s %s is at level %s with a body (request %t, response %t); the row is about Metadata",
					row.kind, key, event.Level, event.RequestObject != nil, event.ResponseObject != nil)
			}
			if event.ResponseStatus == nil || event.ResponseStatus.Code != 201 {
				t.Fatalf("the create event of %s %s answered %+v; want 201, the create the stamp was read back from",
					row.kind, key, event.ResponseStatus)
			}
			uidKey, usernameKey := row.webhook+"/mutationRequestUID", row.webhook+"/username"
			if event.Annotations[uidKey] != stampedUID || event.Annotations[usernameKey] != stampedUsername {
				t.Fatalf("the create event %s of %s %s carries annotations %v; want %s=%s and %s=%s",
					event.AuditID, row.kind, key, event.Annotations, uidKey, stampedUID, usernameKey, stampedUsername)
			}
			if event.ImpersonatedUser == nil || event.ImpersonatedUser.Username != stampedUsername ||
				event.User.Username == stampedUsername {
				t.Fatalf("the create event of %s %s names user %q and impersonatedUser %+v; want the stamped %q impersonated by someone else",
					row.kind, key, event.User.Username, event.ImpersonatedUser, stampedUsername)
			}
			for _, documented := range []string{uidKey, usernameKey} {
				if !bytes.Contains(page, []byte("`"+documented+"`")) {
					t.Errorf("the audit page does not name the key %s the API server recorded", documented)
				}
			}
		})
	}
}

// awaitCreateEvent waits for the one ResponseComplete event of the create of
// key in resource. The log backend writes an event once the request has
// ended, which can be after the client has its answer.
func awaitCreateEvent(t *testing.T, resource string, key client.ObjectKey) auditv1.Event {
	t.Helper()
	var matched []auditv1.Event
	var read int
	err := eventually(30*time.Second, func() error {
		events, err := readAuditLog()
		if err != nil {
			return err
		}
		read, matched = len(events), nil
		for _, event := range events {
			if isCreateEvent(event, resource, key) {
				matched = append(matched, event)
			}
		}
		if len(matched) != 1 {
			return fmt.Errorf("%d of the %d events in %s are the completed create of %s %s, want 1",
				len(matched), read, auditLogPath, resource, key)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("wait for the audit event of the create: %v", err)
	}
	return matched[0]
}

// The filter takes the completed create of the object and refuses each event
// beside it in the log: an earlier stage, another verb, another kind, a
// subresource, and the same name in another namespace. It needs no control
// plane.
func TestIsCreateEventRefusesEveryOtherEvent(t *testing.T) {
	t.Parallel()
	key := client.ObjectKey{Namespace: "approvals-a", Name: "audited-approval"}
	create := auditv1.Event{
		Stage: auditv1.StageResponseComplete, Verb: "create",
		ObjectRef: &auditv1.ObjectReference{
			APIGroup: operatorv1alpha1.GroupVersion.Group, Resource: "ptahschemaapprovals",
			Namespace: key.Namespace, Name: key.Name,
		},
	}
	if !isCreateEvent(create, "ptahschemaapprovals", key) {
		t.Fatal("the filter refuses the completed create it waits for")
	}
	refused := map[string]func(*auditv1.Event){
		"the request received stage": func(event *auditv1.Event) { event.Stage = auditv1.StageRequestReceived },
		"a get":                      func(event *auditv1.Event) { event.Verb = "get" },
		"no object":                  func(event *auditv1.Event) { event.ObjectRef = nil },
		"another group":              func(event *auditv1.Event) { event.ObjectRef.APIGroup = "batch" },
		"another kind":               func(event *auditv1.Event) { event.ObjectRef.Resource = "ptahmigrationapprovals" },
		"the status subresource":     func(event *auditv1.Event) { event.ObjectRef.Subresource = "status" },
		"another namespace":          func(event *auditv1.Event) { event.ObjectRef.Namespace = "approvals-b" },
		"another name":               func(event *auditv1.Event) { event.ObjectRef.Name = "another-approval" },
	}
	for name, change := range refused {
		event := create
		reference := *create.ObjectRef
		event.ObjectRef = &reference
		change(&event)
		if isCreateEvent(event, "ptahschemaapprovals", key) {
			t.Errorf("the filter takes %s", name)
		}
	}
}

func isCreateEvent(event auditv1.Event, resource string, key client.ObjectKey) bool {
	return event.Stage == auditv1.StageResponseComplete && event.Verb == "create" && event.ObjectRef != nil &&
		event.ObjectRef.APIGroup == operatorv1alpha1.GroupVersion.Group && event.ObjectRef.Resource == resource &&
		event.ObjectRef.Subresource == "" && event.ObjectRef.Namespace == key.Namespace && event.ObjectRef.Name == key.Name
}

// readAuditLog decodes every event the API server has written so far. A line
// it is still writing fails the read, and the caller reads again.
func readAuditLog() ([]auditv1.Event, error) {
	file, err := os.Open(auditLogPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var events []auditv1.Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event auditv1.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("decode event %d of %s: %w", len(events)+1, auditLogPath, err)
		}
		events = append(events, event)
	}
	return events, scanner.Err()
}
