package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/pkg/planread/planreadtest"
)

// admitting is an API server that stamps the approver the way the mutating
// webhook does, or answers the create with a chosen error.
type admitting struct {
	client.Client
	refuse  error
	created []client.Object
	options []client.CreateOption
}

func (a *admitting) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	a.created = append(a.created, object)
	a.options = append(a.options, options...)
	if a.refuse != nil {
		return a.refuse
	}
	if approval, ok := object.(*operatorv1alpha1.PtahSchemaApproval); ok {
		approval.Spec.Approver.Username = "alice@example.com"
		approval.Spec.ApprovedAt = metav1.NewTime(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
	}
	return a.Client.Create(ctx, object, options...)
}

// approvable stores a schema whose plan waits for a person and returns the
// API server approve talks to, with the plan's fingerprint.
func approvable(t *testing.T, change func(*operatorv1alpha1.PtahSchemaStatus)) (*admitting, string) {
	t.Helper()
	lab := planreadtest.NewLab(t, "storefront")
	plan, _ := lab.Publish(t, []string{"ALTER TABLE orders DROP COLUMN legacy_note;"}, "drop")
	lab.SetCurrent(t, plan)
	schema := &operatorv1alpha1.PtahSchema{}
	if err := lab.Client().Get(context.Background(), client.ObjectKey{Namespace: planreadtest.Namespace, Name: "storefront"}, schema); err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
	meta.SetStatusCondition(&schema.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionTrue, Reason: "Waiting",
		Message: "Create an approval bound to the current plan fingerprint",
	})
	if change != nil {
		change(&schema.Status)
	}
	if err := lab.Client().Status().Update(context.Background(), schema); err != nil {
		t.Fatalf("update the schema status: %v", err)
	}
	return &admitting{Client: lab.Client()}, plan.Spec.Fingerprint
}

func runApprove(t *testing.T, api *admitting, input string, interactive bool, arguments ...string) (int, string, string) {
	t.Helper()
	previous := connector
	connector = func(string, string, string, time.Duration) (client.Client, string, error) {
		return api, planreadtest.Namespace, nil
	}
	t.Cleanup(func() { connector = previous })
	var stdout, stderr bytes.Buffer
	code := approveCommand(context.Background(), arguments, strings.NewReader(input), interactive, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// The commands share the package-level connector, so these tests do not run
// in parallel.

func TestApproveShowsThePlanAndApprovesItOnYes(t *testing.T) {
	api, fingerprint := approvable(t, nil)

	code, stdout, stderr := runApprove(t, api, "yes\n", true, "schema/storefront")

	if code != exitOK || len(api.created) != 1 {
		t.Fatalf("approve = %d after %d creates; stderr %q", code, len(api.created), stderr)
	}
	for _, want := range []string{"ALTER TABLE orders DROP COLUMN legacy_note;", fingerprint,
		"recorded by admission as alice@example.com at 2026-11-01T10:00:00Z", "It binds plan"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("approve printed %q, missing %q", stdout, want)
		}
	}
	if !strings.Contains(stderr, "Type yes to approve") {
		t.Fatalf("approve did not ask: stderr %q", stderr)
	}
}

func TestApproveSendsNothingWithoutAYes(t *testing.T) {
	for _, answer := range []string{"\n", "no\n", "y\n", "YES please\n", ""} {
		api, _ := approvable(t, nil)

		code, _, stderr := runApprove(t, api, answer, true, "schema/storefront")

		if code != exitNotApproved || len(api.created) != 0 || !strings.Contains(stderr, "Nothing was approved") {
			t.Fatalf("approve after %q = %d, %d creates, stderr %q", answer, code, len(api.created), stderr)
		}
	}
}

func TestApproveWithoutATerminalNeedsTheReviewedFingerprint(t *testing.T) {
	api, fingerprint := approvable(t, nil)

	code, _, stderr := runApprove(t, api, "yes\n", false, "schema/storefront")

	if code != exitUsage || len(api.created) != 0 || !strings.Contains(stderr, "--plan-fingerprint") {
		t.Fatalf("approve without a terminal = %d, %d creates, stderr %q", code, len(api.created), stderr)
	}

	code, stdout, stderr := runApprove(t, api, "", false, "schema/storefront", "--plan-fingerprint", fingerprint)

	if code != exitOK || len(api.created) != 1 || strings.Contains(stdout, "ALTER TABLE") {
		t.Fatalf("approve with the fingerprint = %d, %d creates, stdout %q, stderr %q", code, len(api.created), stdout, stderr)
	}
}

func TestApproveRefusesAFingerprintThatIsNotTheCurrentPlans(t *testing.T) {
	api, _ := approvable(t, nil)

	code, _, stderr := runApprove(t, api, "", false, "schema/storefront", "--plan-fingerprint", "sha256:reviewed-yesterday")

	if code != exitNotApproved || len(api.created) != 0 || !strings.Contains(stderr, "not the sha256:reviewed-yesterday given") {
		t.Fatalf("approve = %d, %d creates, stderr %q", code, len(api.created), stderr)
	}
}

func TestApproveSaysWhyNothingCanBeApprovedNow(t *testing.T) {
	api, fingerprint := approvable(t, func(status *operatorv1alpha1.PtahSchemaStatus) {
		status.Phase = operatorv1alpha1.PhaseObserving
		status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{Type: operatorv1alpha1.OperationObserve, ID: "op"}
	})

	code, _, stderr := runApprove(t, api, "", false, "schema/storefront", "--plan-fingerprint", fingerprint)

	if code != exitNotApproved || len(api.created) != 0 || !strings.Contains(stderr, "running Observe") {
		t.Fatalf("approve = %d, %d creates, stderr %q", code, len(api.created), stderr)
	}
}

func TestApprovePrintsTheManifestOnAClientDryRun(t *testing.T) {
	api, fingerprint := approvable(t, nil)

	code, stdout, stderr := runApprove(t, api, "", false, "schema/storefront", "--dry-run", "client")

	if code != exitOK || len(api.created) != 0 {
		t.Fatalf("approve --dry-run=client = %d, %d creates, stderr %q", code, len(api.created), stderr)
	}
	for _, want := range []string{"kind: PtahSchemaApproval", "generateName: storefront-", "planFingerprint: " + fingerprint, "schemaRef:"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the manifest %q is missing %q", stdout, want)
		}
	}
	if strings.Contains(stdout, "alice") {
		t.Fatalf("the manifest carries an identity admission did not stamp: %q", stdout)
	}
}

func TestApproveAsksAdmissionOnAServerDryRun(t *testing.T) {
	api, fingerprint := approvable(t, nil)

	code, stdout, _ := runApprove(t, api, "", false, "schema/storefront", "--plan-fingerprint", fingerprint, "--dry-run", "server")

	if code != exitOK || len(api.created) != 1 || !strings.Contains(stdout, "nothing was stored") {
		t.Fatalf("approve --dry-run=server = %d, %d creates, stdout %q", code, len(api.created), stdout)
	}
	if len(api.options) != 1 || api.options[0] != client.DryRunAll {
		t.Fatalf("the create was sent with %v, want a server dry run", api.options)
	}
}

func TestApproveExitsByWhatTheAPIServerSaid(t *testing.T) {
	for _, test := range []struct {
		name   string
		refuse error
		code   int
		says   string
	}{
		{"RBAC", &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusForbidden,
			Reason:  metav1.StatusReasonForbidden,
			Message: `ptahschemaapprovals.operator.ptah.run is forbidden: User "bob" cannot create resource "ptahschemaapprovals" in API group "operator.ptah.run" in the namespace "application"`}},
			exitRefused, `User "bob" cannot create`},
		{"admission", &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusForbidden,
			Reason:  metav1.StatusReasonForbidden,
			Message: `admission webhook "vptahschemaapproval.operator.ptah.run" denied the request: the author of the change may not approve it`}},
			exitRefused, "may not approve it"},
		{"no answer", context.DeadlineExceeded, exitUnknown, "may have been stored"},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, fingerprint := approvable(t, nil)
			api.refuse = test.refuse

			code, _, stderr := runApprove(t, api, "", false, "schema/storefront", "--plan-fingerprint", fingerprint)

			if code != test.code || !strings.Contains(stderr, test.says) {
				t.Fatalf("approve = %d, stderr %q; want %d naming %q", code, stderr, test.code, test.says)
			}
		})
	}
}

func TestApproveNamesTheResourceItApproves(t *testing.T) {
	api, _ := approvable(t, nil)
	for _, arguments := range [][]string{{"storefront"}, {"ptahschema/storefront"}, {}, {"schema/storefront", "schema/other"}} {
		if code, _, _ := runApprove(t, api, "", false, arguments...); code != exitUsage {
			t.Fatalf("approve %q = %d, want usage", arguments, code)
		}
	}
	if code, _, _ := runApprove(t, api, "", false, "schema/absent", "--plan-fingerprint", "sha256:x"); code != exitAbsent {
		t.Fatalf("approve of no schema = %d, want absent", code)
	}
	if code, _, _ := runApprove(t, api, "", false, "migration/storefront", "--plan-fingerprint", "sha256:x"); code != exitAbsent {
		t.Fatalf("approve of a migration named like a schema = %d, want absent", code)
	}
	if len(api.created) != 0 {
		t.Fatalf("%d approvals were sent for commands that named nothing approvable", len(api.created))
	}
}
