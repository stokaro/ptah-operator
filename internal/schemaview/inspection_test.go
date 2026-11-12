package schemaview_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/schemaview"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// The approvals listed are the ones that name this schema by name and UID,
// read from the native reading of a schema that recovered from an unknown
// Apply and the approval that Apply consumed. The near misses are copies of
// that approval changed the way a wrong filter would let through.
func TestLoadListsTheApprovalsThatNameThisSchema(t *testing.T) {
	t.Parallel()

	recovered := recoveredSchema(t)
	original := originalApproval(t)
	approval := func(name string, minutes int, change func(*operatorv1alpha1.PtahSchemaApproval)) *operatorv1alpha1.PtahSchemaApproval {
		copied := original.DeepCopy()
		copied.Name = name
		copied.CreationTimestamp = metav1.NewTime(original.CreationTimestamp.Add(time.Duration(minutes) * time.Minute))
		change(copied)
		return copied
	}
	objects := []client.Object{
		recovered, original,
		// A schema deleted and recreated under the same name is another object.
		approval("predecessor", 1, func(a *operatorv1alpha1.PtahSchemaApproval) { a.Spec.SchemaRef.UID = "predecessor-uid" }),
		approval("other-schema", 2, func(a *operatorv1alpha1.PtahSchemaApproval) { a.Spec.SchemaRef.Name = "warehouse" }),
		// These name this schema, and a plan that is not its current one.
		approval("replaced-plan", 3, func(a *operatorv1alpha1.PtahSchemaApproval) { a.Spec.PlanRef.UID = "replaced-plan-uid" }),
		approval("other-fingerprint", 4, func(a *operatorv1alpha1.PtahSchemaApproval) {
			a.Spec.PlanFingerprint = "sha256:" + strings.Repeat("0", 64)
		}),
		approval("other-plan-name", 5, func(a *operatorv1alpha1.PtahSchemaApproval) { a.Spec.PlanRef.Name = "ptah-plan-other" }),
	}

	view, err := schemaview.Load(context.Background(), readerWith(t, objects...), recovered.Namespace, recovered.Name)
	if err != nil {
		t.Fatal(err)
	}
	if view.ApprovalsUnreadable != nil || view.Approvals == nil {
		t.Fatalf("approvals = %+v, unreadable = %+v", view.Approvals, view.ApprovalsUnreadable)
	}
	current := map[string]bool{}
	var names []string
	for _, item := range view.Approvals.Items {
		names = append(names, item.Name)
		current[item.Name] = item.CurrentPlan
	}
	if want := []string{"other-plan-name", "other-fingerprint", "replaced-plan", original.Name}; !slices.Equal(names, want) {
		t.Fatalf("approvals = %v, want %v, newest first", names, want)
	}
	if view.Approvals.Total != 4 {
		t.Fatalf("Total = %d, want 4", view.Approvals.Total)
	}
	for name, want := range map[string]bool{
		original.Name: true, "replaced-plan": false, "other-fingerprint": false, "other-plan-name": false,
	} {
		if current[name] != want {
			t.Errorf("%s: CurrentPlan = %t, want %t", name, current[name], want)
		}
	}

	// The approval the reading recorded was consumed and then made stale,
	// and the view says so in the controller's own words.
	found := view.Approvals.Items[3]
	if found.Approver.Username != "kubernetes-admin" || found.PlanRef.Name != "ptah-plan-9e719473095ef87d3e429aaf" {
		t.Fatalf("approval = %+v", found)
	}
	for _, want := range []string{"Accepted=False", "Consumed=True", "Stale=True"} {
		if !hasCondition(found.Conditions, want) {
			t.Fatalf("approval conditions %+v do not carry %s", found.Conditions, want)
		}
	}
	var text bytes.Buffer
	if err := schemaview.Render(&text, view, schemaview.Text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\nApprovals, newest first:\n",
		"  " + original.Name + "\n    approved by kubernetes-admin at 2026-10-01T08:34:14Z\n" +
			"    plan ptah-plan-9e719473095ef87d3e429aaf, the current plan\n",
		"Stale     True    PlanNoLongerCurrent  2026-10-01T08:39:54Z  The approved plan no longer matches the current database state",
		"Consumed  True    DispatchCommitted    2026-10-01T08:34:14Z  The exact plan was committed to one Apply Job dispatch",
		"  replaced-plan\n",
		"    plan ptah-plan-9e719473095ef87d3e429aaf, not the current plan\n",
	} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("text view is missing %q:\n%s", want, text.String())
		}
	}
	for _, unwanted := range []string{"predecessor", "other-schema"} {
		if strings.Contains(text.String(), unwanted) {
			t.Fatalf("text view lists %s, which names another schema:\n%s", unwanted, text.String())
		}
	}
}

// A schema with no current plan has no approval of its current plan, whatever
// the approval names.
func TestLoadFindsNoCurrentPlanWhenTheSchemaHasNone(t *testing.T) {
	t.Parallel()

	recovered := recoveredSchema(t)
	recovered.Status.Plan = nil
	view, err := schemaview.Load(context.Background(), readerWith(t, recovered, originalApproval(t)), recovered.Namespace, recovered.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Approvals.Items) != 1 || view.Approvals.Items[0].CurrentPlan {
		t.Fatalf("approvals = %+v, want one, of no current plan", view.Approvals)
	}
}

// Once a schema converges it has no current plan, and the approval its last
// apply consumed names the applied plan. The near misses name a plan that
// shares the applied plan's name but not its UID or fingerprint, or another
// plan altogether.
func TestLoadMarksTheApprovalOfTheAppliedPlan(t *testing.T) {
	t.Parallel()

	fingerprint := "sha256:" + strings.Repeat("e", 64)
	converged := fixture()
	converged.Status.Plan = nil
	converged.Status.Applied.PlanRef = operatorv1alpha1.ImmutableObjectReference{Name: "ptah-plan-earlier", UID: "earlier-plan-uid"}
	converged.Status.Applied.PlanFingerprint = fingerprint
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	approval := func(name string, minutes int, plan operatorv1alpha1.ImmutableObjectReference, planFingerprint string) client.Object {
		return &operatorv1alpha1.PtahSchemaApproval{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a", Name: name,
				CreationTimestamp: metav1.NewTime(created.Add(time.Duration(minutes) * time.Minute)),
			},
			Spec: operatorv1alpha1.PtahSchemaApprovalSpec{
				SchemaRef: operatorv1alpha1.ImmutableObjectReference{Name: "storefront", UID: converged.UID},
				PlanRef:   plan, PlanFingerprint: planFingerprint,
			},
			Status: operatorv1alpha1.PtahSchemaApprovalStatus{Conditions: []metav1.Condition{{
				Type: operatorv1alpha1.ConditionApprovalConsumed, Status: metav1.ConditionTrue,
				Reason: string(operatorv1alpha1.ReasonDispatchCommitted), Message: "The exact plan was committed to one Apply Job dispatch",
			}}},
		}
	}
	applied := converged.Status.Applied.PlanRef
	view, err := schemaview.Load(context.Background(), readerWith(t, converged,
		approval("applied", 1, applied, fingerprint),
		approval("recreated-plan", 2, operatorv1alpha1.ImmutableObjectReference{Name: applied.Name, UID: "recreated-uid"}, fingerprint),
		approval("other-fingerprint", 3, applied, "sha256:"+strings.Repeat("f", 64)),
		approval("other-plan", 4, operatorv1alpha1.ImmutableObjectReference{Name: "ptah-plan-other", UID: applied.UID}, fingerprint),
	), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	marked := map[string]bool{}
	for _, item := range view.Approvals.Items {
		marked[item.Name] = item.AppliedPlan
		if item.CurrentPlan {
			t.Fatalf("%s names the current plan of a schema that has none", item.Name)
		}
	}
	if len(marked) != 4 || !marked["applied"] || marked["recreated-plan"] || marked["other-fingerprint"] || marked["other-plan"] {
		t.Fatalf("applied plan = %v", marked)
	}
	var text bytes.Buffer
	if err := schemaview.Render(&text, view, schemaview.Text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  applied\n    approved by", "    plan ptah-plan-earlier, the applied plan\n",
		"  recreated-plan\n", "    plan ptah-plan-earlier, not the current plan\n",
	} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("text view is missing %q:\n%s", want, text.String())
		}
	}

	// A schema with no applied plan has no approval of it.
	converged.Status.Applied = nil
	view, err = schemaview.Load(context.Background(), readerWith(t, converged, approval("applied", 1, applied, fingerprint)), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Approvals.Items) != 1 || view.Approvals.Items[0].AppliedPlan {
		t.Fatalf("approvals = %+v, want one of no applied plan", view.Approvals.Items)
	}
}

// Every "Label: value" line starts its value in one column, the longest
// label a condition read first can carry included.
func TestRenderTextAlignsEveryLabel(t *testing.T) {
	t.Parallel()

	held := fixture()
	held.Spec.Suspend = true
	held.Spec.Policy.ProtectedTables = []string{"payments"}
	held.Status.Conditions = append(held.Status.Conditions,
		metav1.Condition{Type: "ReconciliationFailed", Status: metav1.ConditionTrue, Reason: "OperationFailed", Message: "the Observe Job failed"},
		metav1.Condition{Type: "Suspended", Status: metav1.ConditionTrue, Reason: "Requested", Message: "New database operations are suspended"},
	)
	view, err := schemaview.Load(context.Background(), readerWith(t, held), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	var text bytes.Buffer
	if err := schemaview.Render(&text, view, schemaview.Text); err != nil {
		t.Fatal(err)
	}
	// The header, up to the first blank line: what follows it are sections,
	// whose first line says how many entries follow rather than a field.
	header, _, _ := strings.Cut(text.String(), "\n\n")
	columns := map[int][]string{}
	for _, line := range strings.Split(header, "\n") {
		match := labeledLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		columns[len(match[1])+len(":")+len(match[2])] = append(columns[len(match[1])+len(":")+len(match[2])], match[1])
	}
	if len(columns) != 1 {
		t.Fatalf("values start in %d columns, by label %v:\n%s", len(columns), columns, text.String())
	}
	for _, labels := range columns {
		for _, want := range []string{"Schema", "ReconciliationFailed", "Suspended", "Suspend", "Protected tables", "Reference data"} {
			if !slices.Contains(labels, want) {
				t.Fatalf("the alignment check read no %q line, so it held nothing about it: %v", want, labels)
			}
		}
	}
}

// Reading a schema needs get on it and nothing else, so a reader who may not
// list approvals still gets the schema, and one line that says why the
// approvals are missing. Any other failure of the list is reported the same
// way and fails nothing.
func TestLoadKeepsTheSchemaWhenTheApprovalsCannotBeListed(t *testing.T) {
	t.Parallel()

	resource := schema.GroupResource{Group: "operator.ptah.run", Resource: "ptahschemaapprovals"}
	forbidden := apierrors.NewForbidden(resource, "", errors.New(
		`User "plan-reader" cannot list resource "ptahschemaapprovals" in API group "operator.ptah.run" in the namespace "team-a"`))
	for _, test := range []struct {
		name      string
		err       error
		forbidden bool
		line      string
	}{
		{
			name: "forbidden", err: forbidden, forbidden: true,
			line: "\nApprovals: not shown; listing ptahschemaapprovals is forbidden to you (RBAC): " + forbidden.Error() + "\n",
		},
		{
			name: "unavailable", err: apierrors.NewServiceUnavailable("the API server is shutting down"),
			line: "\nApprovals: not shown; listing ptahschemaapprovals failed: the API server is shutting down\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader := readerRefusingList(t, test.err, fixture())

			view, err := schemaview.Load(context.Background(), reader, "team-a", "storefront")
			if err != nil {
				t.Fatalf("Load() = %v; a list that failed must not fail the view", err)
			}
			if view.Approvals != nil || view.ApprovalsUnreadable == nil ||
				view.ApprovalsUnreadable.Forbidden != test.forbidden || view.Phase != "AwaitingApproval" {
				t.Fatalf("view = %+v, unreadable = %+v", view, view.ApprovalsUnreadable)
			}

			var text, document bytes.Buffer
			if err := schemaview.Render(&text, view, schemaview.Text); err != nil {
				t.Fatal(err)
			}
			if err := schemaview.Render(&document, view, schemaview.JSON); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(text.String(), test.line) || strings.Count(text.String(), "Approvals") != 1 {
				t.Fatalf("text view does not say once why the approvals are missing; want %q:\n%s", test.line, text.String())
			}
			var decoded map[string]any
			if err := json.Unmarshal(document.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if _, listed := decoded["approvals"]; listed {
				t.Fatalf("json view carries an approval list it could not read:\n%s", document.String())
			}
			unreadable, ok := decoded["approvalsUnreadable"].(map[string]any)
			if !ok || unreadable["forbidden"] != test.forbidden || unreadable["message"] != test.err.Error() {
				t.Fatalf("json view does not say why the approvals are missing:\n%s", document.String())
			}
		})
	}
}

// An empty list is an answer of its own, and the JSON says it as one.
func TestRenderSaysWhenNoApprovalNamesTheSchema(t *testing.T) {
	t.Parallel()

	view, err := schemaview.Load(context.Background(), readerWith(t, fixture()), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	var text, document bytes.Buffer
	if err := schemaview.Render(&text, view, schemaview.Text); err != nil {
		t.Fatal(err)
	}
	if err := schemaview.Render(&document, view, schemaview.JSON); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "\nApprovals: none\n") {
		t.Fatalf("text view does not say there are no approvals:\n%s", text.String())
	}
	if !strings.Contains(document.String(), `"approvals": {`+"\n"+`    "total": 0,`+"\n"+`    "items": []`) ||
		strings.Contains(document.String(), "approvalsUnreadable") {
		t.Fatalf("json view does not carry an empty list:\n%s", document.String())
	}
}

// A schema collects an approval for every plan anybody approved. The view
// prints the newest few and says how many it left out.
func TestRenderBoundsTheApprovals(t *testing.T) {
	t.Parallel()

	schemaObject := fixture()
	objects := []client.Object{schemaObject}
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for index := range statusview.MaxDecisions + 2 {
		objects = append(objects, &operatorv1alpha1.PtahSchemaApproval{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a", Name: fmt.Sprintf("approval-%d", index),
				CreationTimestamp: metav1.NewTime(created.Add(time.Duration(index) * time.Hour)),
			},
			Spec: operatorv1alpha1.PtahSchemaApprovalSpec{
				SchemaRef: operatorv1alpha1.ImmutableObjectReference{Name: "storefront", UID: schemaObject.UID},
				PlanRef:   operatorv1alpha1.ImmutableObjectReference{Name: "ptah-plan-old", UID: "old-plan-uid"},
			},
		})
	}
	view, err := schemaview.Load(context.Background(), readerWith(t, objects...), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	var text bytes.Buffer
	if err := schemaview.Render(&text, view, schemaview.Text); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("\nApprovals, the newest %d of %d; kubectl ptah history schema/storefront -n team-a lists all %d:\n",
		statusview.MaxDecisions, statusview.MaxDecisions+2, statusview.MaxDecisions+2)
	if !strings.Contains(text.String(), want) {
		t.Fatalf("text view does not say the list was cut; want %q:\n%s", want, text.String())
	}
	if !strings.Contains(text.String(), "  approval-6\n") || strings.Contains(text.String(), "  approval-1\n") {
		t.Fatalf("text view does not keep the newest:\n%s", text.String())
	}
	if got := strings.Count(text.String(), "    no condition written yet\n"); got != statusview.MaxDecisions {
		t.Fatalf("text view prints %d approvals, want %d:\n%s", got, statusview.MaxDecisions, text.String())
	}
}

// The text view says why the schema stands where it does without a reader
// knowing a field path: Ready and its message first, the policy the spec
// asks for, the controller's verdict on the plan, and then every condition,
// the inactive and completed ones included.
func TestRenderTextSaysWhyTheSchemaWaitsAndShowsEveryCondition(t *testing.T) {
	t.Parallel()

	recovered := recoveredSchema(t)
	view, err := schemaview.Load(context.Background(), readerWith(t, recovered), recovered.Namespace, recovered.Name)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := schemaview.Render(&out, view, schemaview.Text); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"Phase:                AwaitingApproval\n" +
			"Ready:                False, AwaitingApproval: Plan is waiting for approval\n" +
			"Policy:               apply OnApproval, allowDestructive true, driftSeverity all\n",
		"  not approved\n  ApprovalRequired: True, Waiting: Create an approval bound to the current plan fingerprint\n",
		"\nConditions:\n  TYPE                  STATUS  REASON            SINCE                 MESSAGE\n",
		// Suspension that is not in force, and an Apply that is over, are
		// read from rows of their own.
		"  Suspended             False   Active            2026-10-01T08:39:44Z  Reconciliation is active\n",
		"  Applying              False   OutcomeUnknown    2026-10-01T08:34:19Z  Apply outcome is uncertain; only read-only observation is permitted (written for generation 2)\n",
		"  InSync                False   ScopedChanges     2026-10-01T08:40:40Z  The authoritative managed scope differs from the verified artifact\n",
		"kubectl ptah plan e2e-alert-schema-postgresql -n " + recovered.Namespace + " prints the SQL this plan holds.\n",
		// The view lists the newest decisions; the history is where the rest
		// and the journal are, and the view says so whatever it listed.
		"kubectl ptah history schema/e2e-alert-schema-postgresql -n " + recovered.Namespace +
			" prints its lifecycle history and every approval that names it.\n",
		"kubectl events -n " + recovered.Namespace + " --for ptahschema/e2e-alert-schema-postgresql prints its Events.\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text view is missing %q:\n%s", want, text)
		}
	}
	if got := strings.Count(text, "\n  "+"EngineSupported"); got != 1 {
		t.Fatalf("EngineSupported appears %d times in the table:\n%s", got, text)
	}
	for _, condition := range recovered.Status.Conditions {
		if !strings.Contains(text, condition.Message) {
			t.Fatalf("text view drops the message of %s:\n%s", condition.Type, text)
		}
	}
	// Leading lines are Ready and the holds that are True; Suspended=False
	// is in the table and not above it.
	if strings.Contains(text, "\nSuspended:") || strings.Contains(text, "\nSuspend:") {
		t.Fatalf("text view reads an inactive suspension as one:\n%s", text)
	}
}

// A destructive plan under a policy that refuses one is blocked, and the view
// says so from the conditions the controller writes for it
// (setPlanPolicyStatus in internal/controller): Ready names the policy, and
// ApprovalRequired, False, says what to change.
func TestRenderTextSaysWhyADestructivePlanIsBlocked(t *testing.T) {
	t.Parallel()

	blocked := fixture()
	blocked.Generation = 4
	blocked.Status.ObservedGeneration = 4
	blocked.Spec.Policy = operatorv1alpha1.ReconciliationPolicy{
		Apply: operatorv1alpha1.ApplyPolicyAlways, AllowDestructive: false, DriftSeverity: "all",
		ProtectedTables: []string{"payments", "billing.invoices"},
	}
	blocked.Status.Phase = operatorv1alpha1.PhaseBlocked
	blocked.Status.Plan.Destructive = true
	blocked.Status.Conditions = []metav1.Condition{
		{
			Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionFalse, ObservedGeneration: 4,
			Reason: string(operatorv1alpha1.ReasonDestructiveChangesDisabled),
			Message: "Plan contains destructive changes and policy disallows them; read them with kubectl ptah plan, " +
				"then either narrow the desired schema or set spec.policy.allowDestructive",
		},
		{
			Type: operatorv1alpha1.ConditionReady, Status: metav1.ConditionFalse, ObservedGeneration: 4,
			Reason: string(operatorv1alpha1.ReasonPolicyBlocked), Message: "Plan is blocked by destructive-change policy",
		},
	}
	view, err := schemaview.Load(context.Background(), readerWith(t, blocked), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := schemaview.Render(&out, view, schemaview.Text); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"Phase:                Blocked\n" +
			"Ready:                False, PolicyBlocked: Plan is blocked by destructive-change policy\n" +
			"Policy:               apply Always, allowDestructive false, driftSeverity all\n" +
			"Protected tables:     payments, billing.invoices\n",
		"  destructive\n  not approved\n" +
			"  ApprovalRequired: False, DestructiveChangesDisabled: Plan contains destructive changes and policy " +
			"disallows them; read them with kubectl ptah plan, then either narrow the desired schema or set " +
			"spec.policy.allowDestructive\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text view is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "written for generation") || strings.Contains(text, "Status for:") {
		t.Fatalf("a status written for the current generation was called stale:\n%s", text)
	}
}

// A status written for an older spec says so, because the policy printed
// above it is the newer spec's.
func TestRenderTextSaysWhenTheStatusIsForAnOlderSpec(t *testing.T) {
	t.Parallel()

	recovered := recoveredSchema(t)
	recovered.Generation = 4
	view, err := schemaview.Load(context.Background(), readerWith(t, recovered), recovered.Namespace, recovered.Name)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := schemaview.Render(&out, view, schemaview.Text); err != nil {
		t.Fatal(err)
	}
	if want := "Status for:           generation 3; the spec is at generation 4\n"; !strings.Contains(out.String(), want) {
		t.Fatalf("text view is missing %q:\n%s", want, out.String())
	}
}

// The JSON view keeps every field it carried before the conditions, the
// policy and the approvals were added, under the same names.
func TestRenderJSONKeepsItsEarlierFields(t *testing.T) {
	t.Parallel()

	schemaObject := fixture()
	schemaObject.Spec.Suspend = true
	view, err := schemaview.Load(context.Background(), readerWith(t, schemaObject), "team-a", "storefront")
	if err != nil {
		t.Fatal(err)
	}
	var document bytes.Buffer
	if err := schemaview.Render(&document, view, schemaview.JSON); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(document.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{
		{"namespace"}, {"schema"}, {"phase"}, {"suspended"},
		{"source", "kind"}, {"source", "reference"}, {"source", "digest"}, {"source", "verified"},
		{"observation", "observedAt"}, {"observation", "drift"}, {"observation", "highestSeverity"},
		{"observation", "findingCount"}, {"observation", "findings"}, {"observation", "referenceData"},
		{"plan", "name"}, {"plan", "fingerprint"}, {"plan", "statementCount"}, {"plan", "createdAt"}, {"plan", "approved"},
		{"applied", "planName"}, {"applied", "planFingerprint"}, {"applied", "ptahVersion"}, {"applied", "completedAt"},
	} {
		if _, ok := lookup(decoded, path); !ok {
			t.Errorf("json view lost %s:\n%s", strings.Join(path, "."), document.String())
		}
	}
	conditions, ok := decoded["conditions"].([]any)
	if !ok || len(conditions) == 0 {
		t.Fatalf("json view lost its conditions:\n%s", document.String())
	}
	for _, key := range []string{"type", "status", "reason"} {
		if _, ok := conditions[0].(map[string]any)[key]; !ok {
			t.Errorf("a condition lost %s:\n%s", key, document.String())
		}
	}
}

// labeledLine is a top-level "Label:  value" line: a label at the start of the
// line, a colon, the padding, and a value.
var labeledLine = regexp.MustCompile(`^([A-Z][A-Za-z ]*):( +)[^ ]`)

func lookup(document map[string]any, path []string) (any, bool) {
	var current any = document
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		if current, ok = object[key]; !ok {
			return nil, false
		}
	}
	return current, true
}

func hasCondition(conditions []schemaview.ConditionView, typeAndStatus string) bool {
	for _, condition := range conditions {
		if condition.Type+"="+condition.Status == typeAndStatus {
			return true
		}
	}
	return false
}

func readerRefusingList(t *testing.T, refusal error, objects ...client.Object) client.Reader {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return refusal
			},
		}).Build()
}

// recoveredSchema is the native reading of a PostgreSQL schema that recovered
// from an Apply whose outcome was unknown and waits for a new approval of the
// same immutable plan. testdata/e2e/readings/README.md says where it is from.
func recoveredSchema(t *testing.T) *operatorv1alpha1.PtahSchema {
	t.Helper()
	recovered := &operatorv1alpha1.PtahSchema{}
	readReading(t, "alert-schema-recovered.json", recovered)
	recovered.ResourceVersion = ""
	return recovered
}

// originalApproval is the native reading of the approval that recovered
// schema's Apply consumed, made stale afterwards.
func originalApproval(t *testing.T) *operatorv1alpha1.PtahSchemaApproval {
	t.Helper()
	approval := &operatorv1alpha1.PtahSchemaApproval{}
	readReading(t, "alert-schema-original-approval.json", approval)
	approval.ResourceVersion = ""
	if approval.Spec.SchemaRef.UID != types.UID("95be094d-0ff0-45e1-bff2-37b8067ba546") {
		t.Fatalf("the approval reading names schema %q", approval.Spec.SchemaRef.UID)
	}
	return approval
}

func readReading(t *testing.T, name string, into any) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, into); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}
