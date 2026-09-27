package policyenv_test

import (
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// policyRefusal is the error a client receives when a policy denies a
// request, worded as kube-apiserver words it.
func policyRefusal(policy, message string) error {
	return apierrors.NewForbidden(
		schema.GroupResource{Resource: "configmaps"}, "settings",
		errors.New("ValidatingAdmissionPolicy '"+policy+"' with binding '"+policy+"' denied request: "+message),
	)
}

func TestDecideAttributesARefusalToThePolicyThatMadeIt(t *testing.T) {
	t.Parallel()

	verdict := policyenv.Decide(policyRefusal("guard-a", "rejected an unsafe shape"))
	if verdict.Admitted || verdict.Policy != "guard-a" || verdict.Binding != "guard-a" ||
		verdict.Message != "rejected an unsafe shape" {
		t.Fatalf("Decide() = %+v, want a refusal by guard-a with its message", verdict)
	}
	if verdict := policyenv.Decide(nil); !verdict.Admitted {
		t.Fatalf("Decide(nil) = %+v, want admitted", verdict)
	}
	webhook := apierrors.NewForbidden(schema.GroupResource{Resource: "jobs"}, "job",
		errors.New(`admission webhook "vcontrollerwrite.operator.ptah.run" denied the request: outside the intent`))
	if verdict := policyenv.Decide(webhook); verdict.Admitted || verdict.Policy != "" {
		t.Fatalf("Decide(webhook denial) = %+v, want a refusal no policy made", verdict)
	}
}

// The matcher decides every row, so it has to be shown to refuse what a row
// did not ask for: each case is a verdict that must fail the row beside it.
func TestTheRowMatcherRefusesAVerdictTheRowDidNotAskFor(t *testing.T) {
	t.Parallel()

	refused := policyenv.Row{Name: "refused", Deny: []string{"guard-a", "guard-b"}, Message: "rejected an unsafe shape"}
	admitted := policyenv.Row{Name: "admitted"}
	tests := []struct {
		name    string
		row     policyenv.Row
		verdict policyenv.Verdict
		wantErr bool
	}{
		{name: "the named policy refused it", row: refused, verdict: policyenv.Decide(policyRefusal("guard-a", "rejected an unsafe shape: spec"))},
		{name: "the other named policy refused it", row: refused, verdict: policyenv.Decide(policyRefusal("guard-b", "rejected an unsafe shape"))},
		{name: "a refusal row the API server admitted", row: refused, verdict: policyenv.Decide(nil), wantErr: true},
		{name: "a policy the row does not name refused it", row: refused, verdict: policyenv.Decide(policyRefusal("guard-c", "rejected an unsafe shape")), wantErr: true},
		{name: "the named policy refused it for another reason", row: refused, verdict: policyenv.Decide(policyRefusal("guard-a", "rejected an inactive release identity")), wantErr: true},
		{
			name: "something other than a policy refused it", row: refused,
			verdict: policyenv.Decide(apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "settings",
				errors.New(`User "someone" cannot update resource "configmaps"`))),
			wantErr: true,
		},
		{name: "an admitted row the API server admitted", row: admitted, verdict: policyenv.Decide(nil)},
		{name: "an admitted row a policy refused", row: admitted, verdict: policyenv.Decide(policyRefusal("guard-a", "rejected")), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.row.Judge(test.verdict)
			if test.wantErr && err == nil {
				t.Fatalf("the matcher accepted %s for row %q", test.verdict, test.row.Name)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("the matcher refused a verdict the row asked for: %v", err)
			}
		})
	}
}
