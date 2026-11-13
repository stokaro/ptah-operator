package webhook_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/approve"
	"github.com/stokaro/ptah-operator/internal/desiredspec"
	"github.com/stokaro/ptah-operator/test/envtest/internal/harness"
)

// kubectl ptah approve hands its decision to approve.Submit, which reads the
// schema again and creates the approval as the person running the command.
// The unit tests answer that create with errors written by hand; here the API
// server answers it, as the identity that sends it: its own authorizer, the
// chart's approver ClusterRole and the chart's webhook configurations in front
// of the manager's handlers. What each row proves is that an answer the
// command classifies is the answer the API server gives.
func TestApproveCommandGetsTheAPIServersAnswer(t *testing.T) {
	plane.Require(t)
	ctx := context.Background()
	installApproverRole(t)

	t.Run("an approver bound to the chart's role is admitted and stamped", func(t *testing.T) {
		t.Parallel()
		fixture := newApprovalFixture(t)
		const person = "carol@example.com"
		bindApproverRole(t, fixture.namespace, person)

		result := approve.Submit(ctx, clientAs(t, person), fixture.review(t), approve.Options{})

		if result.Outcome != approve.Admitted {
			t.Fatalf("Submit() as %s = %s: %s", person, result.Outcome, result.Message)
		}
		if approver, at := result.Approver(); approver != person || at == nil || at.IsZero() {
			t.Fatalf("the answer carries approver %q at %v, want %s stamped by admission", approver, at, person)
		}
		if kind := result.Approval.GetObjectKind().GroupVersionKind().Kind; kind != "PtahSchemaApproval" {
			t.Fatalf("the answer reads as kind %q", kind)
		}
		stored := &operatorv1alpha1.PtahSchemaApproval{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(result.Approval), stored); err != nil {
			t.Fatalf("read the approval %s back: %v", client.ObjectKeyFromObject(result.Approval), err)
		}
		want := fixture.review(t)
		if stored.Spec.Approver.Username != person || stored.Spec.SchemaRef.UID != want.ResourceUID ||
			stored.Spec.PlanRef.UID != want.PlanUID || stored.Spec.PlanFingerprint != want.Fingerprint ||
			!strings.HasPrefix(stored.Name, fixture.schema.Name+"-") {
			t.Fatalf("stored %s with %+v, want the reviewed plan approved by %s", stored.Name, stored.Spec, person)
		}
	})

	// approverClusterRole.subjects, which kubectl ptah install --approver
	// fills, binds the role for the installation: the chart's own
	// ClusterRoleBinding, as it renders, in a namespace nobody named.
	t.Run("an approver the chart binds is admitted in any namespace", func(t *testing.T) {
		t.Parallel()
		fixture := newApprovalFixture(t)
		const person = "frank@example.com"
		installChartApproverBinding(t, fixture.namespace, person)

		result := approve.Submit(ctx, clientAs(t, person), fixture.review(t), approve.Options{})

		if result.Outcome != approve.Admitted {
			t.Fatalf("Submit() as %s = %s: %s", person, result.Outcome, result.Message)
		}
		if approver, at := result.Approver(); approver != person || at == nil || at.IsZero() {
			t.Fatalf("the answer carries approver %q at %v, want %s stamped by admission", approver, at, person)
		}
	})

	t.Run("an identity without create is refused by RBAC in its own words", func(t *testing.T) {
		t.Parallel()
		fixture := newApprovalFixture(t)
		const person = "dave@example.com"
		// The read half of the approver's grant and nothing else: Submit
		// reads the schema again, then the create is RBAC's to refuse.
		grant(t, fixture.namespace, "schema-reader", userSubject(person), rbacv1.PolicyRule{
			APIGroups: []string{operatorv1alpha1.GroupVersion.Group}, Resources: []string{"ptahschemas"}, Verbs: []string{"get"},
		})

		result := approve.Submit(ctx, clientAs(t, person), fixture.review(t), approve.Options{})

		refusal := `User "` + person + `" cannot create resource "ptahschemaapprovals" in API group "operator.ptah.run" in the namespace "` + fixture.namespace + `"`
		if result.Outcome != approve.Forbidden || !strings.Contains(result.Message, refusal) {
			t.Fatalf("Submit() as %s = %s: %q; want forbidden naming %q", person, result.Outcome, result.Message, refusal)
		}
		requireNoApprovals(t, fixture.namespace)
	})

	t.Run("a server dry run is judged, named and not stored", func(t *testing.T) {
		t.Parallel()
		fixture := newApprovalFixture(t)
		const person = "erin@example.com"
		bindApproverRole(t, fixture.namespace, person)

		result := approve.Submit(ctx, clientAs(t, person), fixture.review(t), approve.Options{ServerDryRun: true})

		if result.Outcome != approve.Previewed {
			t.Fatalf("Submit() as %s with a server dry run = %s: %s", person, result.Outcome, result.Message)
		}
		name := result.Approval.GetName()
		if approver, _ := result.Approver(); approver != person || !strings.HasPrefix(name, fixture.schema.Name+"-") || len(name) <= len(fixture.schema.Name)+1 {
			t.Fatalf("the dry run answered %q stamped %q, want a name the API server generated from %s- and %s", name, approver, fixture.schema.Name, person)
		}
		if err := admin.Get(ctx, client.ObjectKey{Namespace: fixture.namespace, Name: name}, &operatorv1alpha1.PtahSchemaApproval{}); !apierrors.IsNotFound(err) {
			t.Fatalf("read %s/%s after the dry run: %v, want not found", fixture.namespace, name, err)
		}
		requireNoApprovals(t, fixture.namespace)
	})

	// Submit reads the schema again just before it creates the approval, and
	// what changes after that read is admission's to judge. These rows change
	// it there, as the operator would, and the answer is admission's refusal.
	for _, test := range []struct {
		name    string
		change  func(*testing.T, approvalFixture)
		outcome func(error) bool
		says    string
	}{
		{"admission refuses a plan the schema stopped waiting on", func(t *testing.T, fixture approvalFixture) {
			schema := &operatorv1alpha1.PtahSchema{}
			if err := admin.Get(ctx, client.ObjectKeyFromObject(fixture.schema), schema); err != nil {
				t.Errorf("read the schema: %v", err)
				return
			}
			schema.Status.Phase = operatorv1alpha1.PhasePlanning
			if err := admin.Status().Update(ctx, schema); err != nil {
				t.Errorf("move the schema to Planning: %v", err)
			}
		}, apierrors.IsForbidden, "referenced schema is not awaiting approval"},
		{"admission answers 404 for a plan deleted since the read", func(t *testing.T, fixture approvalFixture) {
			if err := admin.Delete(ctx, fixture.plan); err != nil {
				t.Errorf("delete the plan: %v", err)
			}
		}, apierrors.IsNotFound, "read referenced plan"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newApprovalFixture(t)
			const person = "frank@example.com"
			bindApproverRole(t, fixture.namespace, person)
			api := &afterTheRead{Client: clientAs(t, person), change: func() { test.change(t, fixture) }}

			result := approve.Submit(ctx, api, fixture.review(t), approve.Options{})

			if !test.outcome(api.answer) {
				t.Fatalf("the API server answered %v, not the status this row is about", api.answer)
			}
			denied := `admission webhook "mapproval.operator.ptah.run" denied the request`
			if result.Outcome != approve.Refused || !strings.Contains(result.Message, denied) || !strings.Contains(result.Message, test.says) {
				t.Fatalf("Submit() = %s: %q; want refused naming %q and %q", result.Outcome, result.Message, denied, test.says)
			}
			requireNoApprovals(t, fixture.namespace)
		})
	}
}

// afterTheRead is the person's client with the operator moving between
// Submit's last read and its create. It keeps the API server's answer.
type afterTheRead struct {
	client.Client
	change func()
	answer error
}

func (a *afterTheRead) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	a.change()
	a.answer = a.Client.Create(ctx, object, options...)
	return a.answer
}

// review is what approve.Load reads for the fixture's identities and spec:
// Submit reads only those again, and the fixture's plan has no stored chunk
// for Load's content read.
func (fixture approvalFixture) review(t *testing.T) approve.Review {
	t.Helper()
	// The spec as the API server sends it, which is what approve compares.
	raw := &unstructured.Unstructured{}
	raw.SetGroupVersionKind(operatorv1alpha1.GroupVersion.WithKind("PtahSchema"))
	if err := admin.Get(context.Background(), client.ObjectKeyFromObject(fixture.schema), raw); err != nil {
		t.Fatalf("read the fixture's schema: %v", err)
	}
	spec, err := desiredspec.Of(raw.Object["spec"])
	if err != nil {
		t.Fatalf("read the fixture's spec: %v", err)
	}
	return approve.Review{
		Family: approve.Schema, Namespace: fixture.namespace, Name: fixture.schema.Name, ResourceUID: fixture.schema.UID,
		Spec:     spec,
		PlanName: fixture.plan.Name, PlanUID: fixture.plan.UID, Fingerprint: fixture.plan.Spec.Fingerprint,
	}
}

// installApproverRole creates the chart's approver ClusterRole as rendered.
// The chart creates it and binds it to nobody.
func installApproverRole(t *testing.T) {
	t.Helper()
	if chartApproverRole == nil {
		t.Fatal("the chart rendered no approver ClusterRole")
	}
	role := chartApproverRole.DeepCopy()
	if err := admin.Create(context.Background(), role); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create ClusterRole %s: %v", role.Name, err)
	}
}

// bindApproverRole binds the chart's approver ClusterRole to user in
// namespace, the way the docs tell an installation to, and returns once the
// authorizer lets user create a schema approval there.
func bindApproverRole(t *testing.T, namespace, user string) {
	t.Helper()
	ctx := context.Background()
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "approvers-" + strings.ReplaceAll(user, "@", "-at-")},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: chartApproverRole.Name},
		Subjects:   []rbacv1.Subject{userSubject(user)},
	}
	if err := admin.Create(ctx, binding); err != nil {
		t.Fatalf("create RoleBinding %s/%s: %v", namespace, binding.Name, err)
	}
	if err := eventually(10*time.Second, func() error {
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: user,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace, Verb: "create", Group: operatorv1alpha1.GroupVersion.Group, Resource: "ptahschemaapprovals",
			},
		}}
		if err := admin.Create(ctx, review); err != nil {
			return err
		}
		if !review.Status.Allowed {
			return errors.New("the authorizer does not yet let " + user + " create ptahschemaapprovals in " + namespace)
		}
		return nil
	}); err != nil {
		t.Fatalf("wait for RoleBinding %s/%s to take effect: %v", namespace, binding.Name, err)
	}
}

// installChartApproverBinding renders the chart with user as its one approver,
// creates the ClusterRoleBinding that render holds, and returns once the
// authorizer lets user create a schema approval in namespace. The binding goes
// with the test.
func installChartApproverBinding(t *testing.T, namespace, user string) {
	t.Helper()
	ctx := context.Background()
	release := harness.DefaultRelease()
	release.Values = append(release.Values,
		[2]string{"approverClusterRole.subjects[0].kind", "User"},
		[2]string{"approverClusterRole.subjects[0].name", user})
	objects, err := harness.RenderChart(ctx, release)
	if err != nil {
		t.Fatalf("render the chart with %s as its approver: %v", user, err)
	}
	var bindings []*rbacv1.ClusterRoleBinding
	for _, object := range objects {
		if object.GetKind() != "ClusterRoleBinding" || object.GetName() != chartApproverRole.Name {
			continue
		}
		binding := &rbacv1.ClusterRoleBinding{}
		if err := fromUnstructured(object, binding); err != nil {
			t.Fatal(err)
		}
		bindings = append(bindings, binding)
	}
	if len(bindings) != 1 {
		t.Fatalf("the chart rendered %d ClusterRoleBindings named %s for approver %s, want one", len(bindings), chartApproverRole.Name, user)
	}
	binding := bindings[0]
	if binding.RoleRef.Name != chartApproverRole.Name {
		t.Fatalf("the chart's approver binding refers to %+v, not the approver role %s", binding.RoleRef, chartApproverRole.Name)
	}
	if err := admin.Create(ctx, binding); err != nil {
		t.Fatalf("create ClusterRoleBinding %s: %v", binding.Name, err)
	}
	t.Cleanup(func() {
		if err := admin.Delete(context.Background(), binding); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete ClusterRoleBinding %s: %v", binding.Name, err)
		}
	})
	if err := eventually(10*time.Second, func() error {
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: user,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace, Verb: "create", Group: operatorv1alpha1.GroupVersion.Group, Resource: "ptahschemaapprovals",
			},
		}}
		if err := admin.Create(ctx, review); err != nil {
			return err
		}
		if !review.Status.Allowed {
			return errors.New("the authorizer does not yet let " + user + " create ptahschemaapprovals in " + namespace)
		}
		return nil
	}); err != nil {
		t.Fatalf("wait for the chart's ClusterRoleBinding %s to take effect: %v", binding.Name, err)
	}
}

// One printed file approves a plan again, which a fixed name would refuse: the
// API server generates a name each time, and admission stamps each approval.
// A name given on the command line is the caller's to keep unique.
func TestPrintedApprovalCanBeCreatedTwice(t *testing.T) {
	plane.Require(t)
	ctx := context.Background()
	installApproverRole(t)
	fixture := newApprovalFixture(t)
	const person = "erin@example.com"
	bindApproverRole(t, fixture.namespace, person)
	api := clientAs(t, person)
	review := fixture.review(t)

	first, second := approve.Manifest(review, ""), approve.Manifest(review, "")
	for _, printed := range []*unstructured.Unstructured{first, second} {
		if err := api.Create(ctx, printed); err != nil {
			t.Fatalf("create the printed approval: %v", err)
		}
		if !strings.HasPrefix(printed.GetName(), fixture.schema.Name+"-") {
			t.Fatalf("the printed approval was stored as %q, not under a name generated from %s-", printed.GetName(), fixture.schema.Name)
		}
		stored := &operatorv1alpha1.PtahSchemaApproval{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(printed), stored); err != nil {
			t.Fatalf("read the approval %s back: %v", printed.GetName(), err)
		}
		if stored.Spec.Approver.Username != person || stored.Spec.PlanRef.UID != review.PlanUID {
			t.Fatalf("stored %s with %+v, want the reviewed plan approved by %s", stored.Name, stored.Spec, person)
		}
	}
	if first.GetName() == second.GetName() {
		t.Fatalf("both creates were stored as %s", first.GetName())
	}

	named := "reviewed-by-erin"
	if err := api.Create(ctx, approve.Manifest(review, named)); err != nil {
		t.Fatalf("create the approval named %s: %v", named, err)
	}
	if err := api.Create(ctx, approve.Manifest(review, named)); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("a second create of the approval named %s = %v, want AlreadyExists", named, err)
	}
}

// requireNoApprovals holds namespace to no stored approval, and says what the
// list read so an empty answer is not mistaken for no answer.
func requireNoApprovals(t *testing.T, namespace string) {
	t.Helper()
	approvals := &operatorv1alpha1.PtahSchemaApprovalList{}
	if err := admin.List(context.Background(), approvals, client.InNamespace(namespace)); err != nil {
		t.Fatalf("list the approvals in %s: %v", namespace, err)
	}
	if len(approvals.Items) != 0 {
		t.Fatalf("%d approvals are stored in %s, the first %s; want none", len(approvals.Items), namespace, approvals.Items[0].Name)
	}
}
