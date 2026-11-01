package crd_test

import (
	"context"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/migrationview"
	"github.com/stokaro/ptah-operator/internal/schemaview"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// `kubectl ptah schema` and `kubectl ptah migration` need the get the docs
// name -- on the schema; on the migration and the plan its status names -- and
// list the approvals and acknowledgments that name a resource only where the
// reader may. Here the API server's own authorizer refuses those lists to a
// reader granted that get alone: the views still load, the migration's plan
// included, and record the refusal as RBAC's. Granting list then shows the same
// reader the lists, so what the first reading recorded was the authorizer's
// answer and not something the views say regardless.
func TestPluginViewsNeedGetAndListDecisionsOnlyWhereAllowed(t *testing.T) {
	plane.Require(t)
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	namespace := newNamespace(t, "plugin-reader")
	storedMigration := migrationBase(namespace)()
	for _, object := range []client.Object{schemaBase(namespace)(), storedMigration} {
		if err := api.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	// The plan the migration's status names, written as the administrator
	// the way the manager would, so the reader's view has a plan to get.
	plan := resource("PtahMigrationPlan", namespace, "ptah-mplan-19581e27de7ced00ff1ce50b", migrationPlanSpec())
	set(t, plan, string(storedMigration.GetUID()), "spec", "migrationRef", "uid")
	if err := api.Create(ctx, plan); err != nil {
		t.Fatal(err)
	}
	set(t, storedMigration, map[string]any{"name": plan.GetName(), "uid": string(plan.GetUID())}, "status", "plan")
	if err := api.Status().Update(ctx, storedMigration); err != nil {
		t.Fatal(err)
	}
	reader := namespace + "-reader"
	grant(ctx, t, namespace, reader, "get", []string{"ptahschemas", "ptahmigrations", "ptahmigrationplans"}, "get")
	scheme := runtime.NewScheme()
	if err := ptahv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	// The plugin's own client: no cache, every read straight to the API
	// server, as the reader.
	asReader, err := client.New(plane.Impersonate(reader), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	var schema schemaview.View
	var migration migrationview.View
	// RBAC reaches the authorizer through its own informers, so the first
	// reads may come before the grant does.
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		var schemaErr, migrationErr error
		schema, schemaErr = schemaview.Load(ctx, asReader, namespace, "application")
		migration, migrationErr = migrationview.Load(ctx, asReader, namespace, "orders")
		return schemaErr == nil && migrationErr == nil, nil
	}); err != nil {
		t.Fatalf("the reader granted get could not read the resources: %v", err)
	}
	if migration.Plan == nil || migration.Plan.Name != plan.GetName() || len(migration.Plan.Migrations) != 2 {
		t.Fatalf("the reader's migration view carries plan %+v, want %s with its two migrations", migration.Plan, plan.GetName())
	}
	for _, list := range []struct {
		resource   string
		listed     bool
		unreadable *statusview.Unreadable
	}{
		{"ptahschemaapprovals", schema.Approvals != nil, schema.ApprovalsUnreadable},
		{"ptahmigrationapprovals", migration.Approvals != nil, migration.ApprovalsUnreadable},
		{"ptahmigrationrunacknowledgments", migration.Acknowledgments != nil, migration.AcknowledgmentsUnreadable},
	} {
		if list.listed || list.unreadable == nil || !list.unreadable.Forbidden ||
			!strings.Contains(list.unreadable.Message, `cannot list resource "`+list.resource+`"`) {
			t.Fatalf("%s: listed %t, unreadable %+v; want the authorizer's refusal", list.resource, list.listed, list.unreadable)
		}
	}

	grant(ctx, t, namespace, reader, "list", []string{"ptahschemaapprovals", "ptahmigrationapprovals", "ptahmigrationrunacknowledgments"}, "list")
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		var schemaErr, migrationErr error
		schema, schemaErr = schemaview.Load(ctx, asReader, namespace, "application")
		migration, migrationErr = migrationview.Load(ctx, asReader, namespace, "orders")
		if schemaErr != nil || migrationErr != nil {
			return false, nil
		}
		return schema.Approvals != nil && migration.Approvals != nil && migration.Acknowledgments != nil, nil
	}); err != nil {
		t.Fatalf("the reader granted list still cannot list the decisions: schema %+v, migration %+v, %+v: %v",
			schema.ApprovalsUnreadable, migration.ApprovalsUnreadable, migration.AcknowledgmentsUnreadable, err)
	}
}

// grant gives user the verb on the resources in the namespace, through a Role
// and a RoleBinding named for what they grant.
func grant(ctx context.Context, t *testing.T, namespace, user, name string, resources []string, verb string) {
	t.Helper()
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{ptahv1.GroupVersion.Group}, Resources: resources, Verbs: []string{verb}}},
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
	}
	for _, object := range []client.Object{role, binding} {
		if err := api.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
}
